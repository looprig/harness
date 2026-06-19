package loop

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/inventivepotter/urvi/internal/agent/loop/command"
	"github.com/inventivepotter/urvi/internal/agent/loop/event"
	"github.com/inventivepotter/urvi/internal/content"
	"github.com/inventivepotter/urvi/internal/uuid"
)

// Loop is the handle to a running agent loop for internal packages.
// Commands is unbuffered — sends block until the actor is ready. Callers must
// never close Commands; stop the actor with Shutdown. (Closing it would exit the
// actor through the `!ok` path, skipping terminal delivery and shutdown acks.)
// Done is closed when the actor has fully exited.
// Direct callers must honor the command contracts, including a buffered(1) Ack on
// every submit (UserInput/SubagentResult) and CancelQueuedInput, and a paired
// Events/Abandoned pair when supplying a per-turn stream.
// Reply channels (the Ack on each command) must be buffered (capacity >= 1): the
// actor replies through tryAck (a non-blocking send), so an unbuffered or unread
// Ack drops the reply rather than stalling the actor.
type Loop struct {
	Commands chan<- command.Command
	Done     <-chan struct{}

	// gateReg is the actor's gate-registration seam. A parked runner (or
	// RequestUserInput on its behalf) sends a gateRegistration here and waits for
	// the ack; listen installs the gate in loopState.pendingGates before closing
	// the ack (install-before-emit). It is unexported: only in-package callers (the
	// runner via the turn-launch ctx injection, and tests) register gates. The
	// actor is the sole reader.
	gateReg chan<- gateRegistration
}

// idGenerator mints a fresh UUID. It defaults to uuid.New; tests inject a
// failing generator to exercise the crypto/rand failure branches.
type idGenerator func() (uuid.UUID, error)

// eventPublisher is the loop's narrow consumer of the session-level event
// fan-in. The loop depends on this small interface (Dependency Inversion /
// Interface Segregation) rather than on the concrete session type, so only the
// session owns buffering, shutdown, close, and sequence policy. A parent or
// primary loop must not forward child-loop events; identity is metadata, not
// the transport path.
type eventPublisher interface {
	PublishEvent(context.Context, event.Event) error
}

// Provenance identifies the parent turn/step that spawned a loop. The zero value
// means "no parent" (the primary loop). It is the (LoopID, TurnID, StepID) tuple
// the loop stamps onto the Parent* fields of every event it emits. It lives in
// the loop package because both loopState and AgentSession's registry use it.
type Provenance struct {
	LoopID uuid.UUID // parent loop; zero for the primary loop
	TurnID uuid.UUID // the parent turn that spawned this loop
	StepID uuid.UUID // the parent step (optional finer grain)
}

const defaultDrainTimeout = 5 * time.Second

// resolveDrainTimeout applies the default when the caller leaves DrainTimeout unset.
func resolveDrainTimeout(d time.Duration) time.Duration {
	if d <= 0 {
		return defaultDrainTimeout
	}
	return d
}

// New constructs a loop and starts its actor goroutine. loopCtx is the loop's
// lifetime (derived by the session from its sessionCtx); it is NOT a turn
// lifetime. sessionID is shared by every loop in the session; loopID is unique
// to this loop. parent is the provenance of the turn/step that spawned this loop
// (zero for the primary loop). events is the session-level event publisher the
// loop depends on (Dependency Inversion); it must be non-nil.
func New(loopCtx context.Context, sessionID, loopID uuid.UUID, parent Provenance, events eventPublisher, cfg Config) (*Loop, error) {
	if cfg.Client == nil {
		return nil, &ConfigError{Kind: ConfigMissingClient}
	}
	if err := cfg.Model.Validate(); err != nil {
		return nil, &ConfigError{Kind: ConfigInvalidModel, Cause: err}
	}
	if events == nil {
		return nil, &ConfigError{Kind: ConfigMissingPublisher}
	}
	cfg.DrainTimeout = resolveDrainTimeout(cfg.DrainTimeout)
	cfg.Tools = resolveToolSetCaps(cfg.Tools)
	if cfg.idGen == nil {
		cfg.idGen = uuid.New
	}
	commands := make(chan command.Command)
	done := make(chan struct{})
	// gateReg is unbuffered: registration is synchronous (the runner blocks on the
	// ack), and the actor is the sole reader, selecting on it alongside commands.
	gateReg := make(chan gateRegistration)
	state := newLoopState(sessionID, loopID, parent, events)
	go listen(loopCtx, cfg, commands, gateReg, done, state)
	return &Loop{Commands: commands, Done: done, gateReg: gateReg}, nil
}

// projectForSink derives the SINK-side form of an event from the (full-fidelity)
// stream event. It returns the event to envelope for sinks and the CallID for the
// envelope. SECURITY: any event carrying sensitive payload implements
// event.Redactable; this replaces it with its redacted SinkProjection so tool
// args, file content, URLs/headers, questions, choice strings, and result
// previews NEVER reach a sink. Events without sensitive payload pass through. The
// CallID is read from the ORIGINAL (full) event — projection may change the
// concrete type — so the envelope self-identifies the tool call it pertains to.
// The per-turn stream is never touched by this function.
func projectForSink(ev event.Event) (event.Event, uuid.UUID) {
	var callID uuid.UUID
	switch e := ev.(type) {
	case event.PermissionRequested:
		callID = e.CallID
	case event.UserInputRequested:
		callID = e.CallID
	case event.ToolCallStarted:
		callID = e.CallID
	case event.ToolCallCompleted:
		callID = e.CallID
	}
	if r, ok := ev.(event.Redactable); ok {
		return r.SinkProjection(), callID
	}
	return ev, callID
}

type loopStatus int

const (
	loopIdle loopStatus = iota
	loopRunning
	loopShuttingDown
)

// inboxCap bounds loopState.inbox. A submit that arrives while the queue is full
// is rejected with TurnRejected{RejectQueueFull} (a length check, never a blocking
// send), so the actor never blocks on a queue push. WHY a bound at all: it caps the
// memory held by accepted-but-unresolved submits so a producer that floods the loop
// cannot grow the inbox without limit. 64 is a generous default, not a tuned value.
const inboxCap = 64

// queuedInput is an accepted-but-unresolved submit sitting in loopState.inbox.
// inputID is the submit command's Header.ID (so CancelQueuedInput can remove it by
// id while it is still queued). triggeredBy is the producing subagent loop id for a
// SubagentResult (zero for a UserInput); the events caused by this queued input
// (TurnStarted/TurnFoldedInto/InputCancelled) stamp it as Header.TriggeredByLoopID,
// which releases the parent's quiescence wake token. triggeredBy is stored now and
// USED for quiescence in a later phase.
type queuedInput struct {
	inputID     uuid.UUID
	triggeredBy uuid.UUID
	msg         *content.UserMessage
}

type loopState struct {
	// id is this loop's id. In multi-agent sessions each subagent loop gets its
	// own loop id.
	id uuid.UUID

	// sessionID is shared by every loop participating in the same session.
	sessionID uuid.UUID

	// parent is the provenance of whatever spawned this loop (zero for the
	// primary loop). The loop knows its PARENT so it can later stamp Parent* on
	// the events it emits; it never tracks its CHILDREN. The session owns the
	// loop registry and turn tree (SRP).
	parent Provenance

	// events is the session-level event publisher (Dependency Inversion). Stored
	// at construction; Phase 3 does not yet call it — event publication wiring is
	// Phase 4. The existing per-turn channel + cfg.Sinks paths are unchanged.
	events eventPublisher

	turnIndex     event.TurnIndex
	turnID        uuid.UUID // entity id for the active turn; zero when idle
	causationID   uuid.UUID // active submit command's Header.ID; zero when idle
	status        loopStatus
	cancelTurn    context.CancelFunc
	turnDone      <-chan struct{}         // active turnCtx.Done(); nil when idle. It is closed when the turn ctx is cancelled (Interrupt/Shutdown processed on an earlier loop iteration, or root-ctx). emitTurn escapes on it so a turn cancelled BEFORE the actor parked here can still ack and free the parked runTurn. While the actor is parked in emitTurn it cannot process a new Interrupt — turnAbandoned / ctx.Done() are the escapes for that case.
	turnEvents    chan<- event.Event      // current turn's channel; nil for a fan-in-only turn; actor closes it when non-nil
	turnAbandoned <-chan struct{}         // paired with turnEvents; nil for a fan-in-only turn; closed when the caller stops reading
	msgs          content.AgenticMessages // conversation history across turns

	// inbox is the actor-owned pending-input queue for accepted
	// UserInput/SubagentResult that could not start immediately (a turn was
	// running). Only the actor (listen) appends/removes/clears it — no locks. On
	// going idle the actor pops the first entry to start the next turn; on an
	// abnormal terminal it returns the remaining entries via InputCancelled and
	// starts nothing. Bounded by inboxCap (a full inbox rejects with QueueFull).
	// Fold-into-a-running-turn is a later phase; in this phase queued input
	// resolves only by starting a later turn, by CancelQueuedInput, or by
	// abnormal-terminal return.
	inbox []queuedInput

	// draining holds inbox entries the actor popped for a tool-continuation drain but
	// whose TurnFoldedInto has not yet been committed. The drain handshake moves
	// entries here (out of inbox) at the drain point and replies them to runTurn;
	// runTurn then commits a TurnFoldedInto per entry, and the commit point removes
	// the matching entry from draining (it is now resolved). If the turn ends
	// abnormally between the drain and a fold commit, these entries are no longer in
	// inbox, so the abnormal-terminal path returns them from draining too (every
	// removed entry is resolved exactly once). Actor-owned, like inbox.
	draining []queuedInput

	shutdownAcks []chan<- error

	// pendingGates maps a tool call's CallID to the gate a parked runner is blocked
	// on. Owned SOLELY by listen/the actor — a turn goroutine never touches it. A
	// control command (Approve/Deny/ProvideUserInput) is routed to the matching
	// gate by CallID AND kind, then the entry is deleted. Cleared on turn end.
	pendingGates map[uuid.UUID]gate
}

// newLoopState builds the actor-owned loop state with its identity (sessionID,
// loopID, parent provenance) and the session event publisher. pendingGates is
// initialized so the actor can route gate commands without a nil-map panic.
func newLoopState(sessionID, loopID uuid.UUID, parent Provenance, events eventPublisher) loopState {
	return loopState{
		id:           loopID,
		sessionID:    sessionID,
		parent:       parent,
		events:       events,
		pendingGates: make(map[uuid.UUID]gate),
	}
}

// turnResult is the turn goroutine's hand-back to the actor. The conversation is
// committed INCREMENTALLY through the per-step commit handshake (commitRequest), so
// turnResult no longer carries the whole-turn message slice: it carries only the
// turn terminal for the actor to deliver. A failed/interrupted turn discards only
// the in-flight incomplete step; committed steps already live in loopState.msgs.
type turnResult struct {
	terminal event.Event // TurnDone, TurnFailed, or TurnInterrupted
}

// cancelReasonFor maps an abnormal turn terminal to the CancelReason stamped on
// the InputCancelled events that return still-queued input. A TurnInterrupted maps
// to CancelTurnInterrupted; anything else (TurnFailed, and a shutdown that ended a
// TurnDone) maps to CancelTurnFailed — the queued input never started, so from the
// client's view it was not completed.
func cancelReasonFor(terminal event.Event) event.CancelReason {
	if _, ok := terminal.(event.TurnInterrupted); ok {
		return event.CancelTurnInterrupted
	}
	return event.CancelTurnFailed
}

// commitRequest is one per-step commit handshake from the turn goroutine to the
// actor: the finalized step group to append to loopState.msgs and the Enduring
// StepDone event to emit at the same actor-owned point. ack is buffered(1); the
// actor closes it after committing+emitting so the parked runTurn unblocks. The
// turn goroutine selects on ack AND turnCtx.Done so an Interrupt/Shutdown frees it.
type commitRequest struct {
	commit turnCommit
	ack    chan<- struct{}
}

// drainRequest is the tool-continuation drain handshake from the turn goroutine to
// the actor. The actor (the inbox's sole owner) pops + clears the inbox in order,
// moves the popped entries into loopState.draining (so an abnormal terminal still
// returns them), converts each to a foldedMsg, and sends them on reply. reply is
// buffered(1): the actor never blocks sending it, and a runTurn that escapes on
// turnCtx.Done (an Interrupt during the handshake) leaves the reply in the buffer
// harmlessly — the moved entries are resolved by returnQueuedInbox via the draining
// buffer on the resulting abnormal terminal, so nothing is stranded.
type drainRequest struct {
	reply chan<- []foldedMsg
}

// listen takes gateReg as a bidirectional channel: the actor is its sole reader
// (the select below), and the per-turn goroutine hands the SEND side to runTurn
// → RunBatch so a parked tool can register a gate. (A receive-only handle could
// not be narrowed to the send-only direction runTurn requires.)
func listen(ctx context.Context, cfg Config, commands <-chan command.Command, gateReg chan gateRegistration, done chan struct{}, state loopState) {
	defer close(done)

	internal := make(chan turnResult, 1)
	// commits is the per-step commit handshake channel. The turn goroutine sends a
	// commitRequest per completed step; the actor (the sole reader, in its select)
	// appends the group to loopState.msgs and emits the StepDone at the same point,
	// then acks. Unbuffered: the handshake is synchronous (runTurn blocks on the
	// ack), which serializes the per-turn stream so a step's TokenDeltas (emitted by
	// runTurn) all precede that step's StepDone (emitted here by the actor).
	commits := make(chan commitRequest)
	// drains is the tool-continuation drain handshake channel. The turn goroutine
	// sends one drainRequest per tool-continuation boundary; the actor (the sole
	// reader, in its select) pops + clears the inbox into loopState.draining and
	// replies the foldedMsgs. Unbuffered: the handshake is synchronous, so the actor
	// is the only goroutine touching inbox/draining when it runs.
	drains := make(chan drainRequest)

	publish := func(ev event.Event) {
		// SECURITY: the sink path is redacted; the per-turn stream (handled
		// separately by emit/deliverAndClose) keeps full fidelity. projectForSink
		// replaces any event.Redactable with its SinkProjection so tool args, file
		// content, URLs/headers, questions, and result previews never reach a sink,
		// and extracts the CallID for the envelope from the ORIGINAL event.
		sinkEv, callID := projectForSink(ev)
		// EventID is fresh per emitted event. A crypto/rand failure here is a
		// system-level fault; it must not abort turn execution (the bare per-turn
		// event is delivered separately), so log it and emit the envelope with a
		// zero EventID rather than dropping the sink copy.
		eventID, err := cfg.idGen()
		if err != nil {
			slog.Error("event id generation failed; emitting envelope with zero EventID", "error", err)
		}
		env := event.EventEnvelope{
			SessionID:   state.sessionID,
			TurnID:      state.turnID,
			EventID:     eventID,
			CausationID: state.causationID,
			CallID:      callID,
			Event:       sinkEv,
		}
		// TurnIndex is read from the ORIGINAL event; projection preserves it but
		// may change the concrete type (e.g. UserInputRequested → ...Sink).
		switch e := ev.(type) {
		case event.TurnStarted:
			env.TurnIndex = e.TurnIndex
		case event.TokenDelta:
			env.TurnIndex = e.TurnIndex
		case event.TurnDone:
			env.TurnIndex = e.TurnIndex
		case event.TurnFailed:
			env.TurnIndex = e.TurnIndex
		case event.TurnInterrupted:
			env.TurnIndex = e.TurnIndex
		}
		for _, sink := range cfg.Sinks {
			func() {
				defer func() {
					if r := recover(); r != nil {
						slog.Warn("event sink panicked", "panic", r)
					}
				}()
				sink.OnEvent(ctx, env)
			}()
		}
	}

	publish(event.SessionStarted{Header: event.Header{SessionID: state.sessionID}})

	// emitTurn is the ACTOR-side per-turn emit, used at the commit point to emit the
	// initial TurnStarted and each step's StepDone. It mirrors the turn goroutine's
	// emit closure (publish to sinks, then send to the per-turn channel) so both
	// reach the same per-turn stream. The blocking commit handshake guarantees the
	// turn goroutine is parked in cfg.commit while the actor calls this, so there are
	// never two concurrent writers to state.turnEvents — a step's TokenDeltas (from
	// the turn goroutine) all precede that step's StepDone (from here). The three
	// escapes (turnAbandoned / ctx.Done / nil channel) keep the actor from wedging if
	// the caller stopped reading or the loop is dying.
	emitTurn := func(ev event.Event) {
		publish(ev)
		if state.turnEvents == nil {
			return
		}
		select {
		case state.turnEvents <- ev:
		case <-state.turnAbandoned:
		case <-ctx.Done():
		case <-state.turnDone: // turn ctx already cancelled (Interrupt/Shutdown handled before the actor parked here, or root-ctx): stop blocking on a stalled consumer so the commit point can ack and free runTurn. A NEW Interrupt arriving while parked here cannot be processed — turnAbandoned / ctx.Done() cover that.
		}
	}

	// deliverAndClose publishes the terminal event, sends it to the per-turn
	// channel unless the caller abandoned the stream, and closes the channel.
	// Always called by the actor, never by the turn goroutine, and only after the
	// turn goroutine has sent its result on `internal` (so closing turnEvents can
	// never race a concurrent emit).
	//
	// Three escapes, so the actor can never wedge here:
	//   - turnEvents <- terminal: the normal path, caller reads the terminal.
	//   - turnAbandoned: Invoke closes it via defer after receiving the terminal;
	//     Stream.Close closes it explicitly.
	//   - ctx.Done: a buggy caller that never reads and never closes Abandoned
	//     (e.g. a leaked Stream reader) must not pin the actor forever. A root-ctx
	//     cancel always frees it. Without this case such a caller would wedge the
	//     actor outside its select loop, where neither Shutdown nor root-ctx
	//     cancel could reach it.
	deliverAndClose := func(terminal event.Event) {
		publish(terminal)
		// A fan-in-only turn (nil turnEvents) has no per-turn stream to deliver to
		// or close: publish to sinks/fan-in is the whole delivery. Sending on a nil
		// channel would block forever and close(nil) would panic, so skip both.
		if state.turnEvents != nil {
			select {
			case state.turnEvents <- terminal:
			case <-state.turnAbandoned: // caller abandoned; terminal already in sinks
			case <-ctx.Done(): // hard loop kill; terminal already in sinks
			}
			close(state.turnEvents)
		}
		state.turnEvents = nil
		state.turnAbandoned = nil
		state.turnDone = nil
	}

	forceAbandon := func() {
		abandoned := make(chan struct{})
		close(abandoned)
		state.turnAbandoned = abandoned
	}

	ackShutdowns := func(err error) {
		for _, ack := range state.shutdownAcks {
			ack <- err
		}
		state.shutdownAcks = nil
	}

	// routeControl delivers a control command (Approve/Deny/ProvideUserInput) to
	// the parked runner blocked on its CallID, but ONLY if a gate is open for that
	// CallID AND the gate kind accepts this command kind. On a match it delivers
	// once (the gate's reply channel is buffered(1) and the runner is its sole
	// reader, so the send never blocks the actor) and deletes the gate so a
	// duplicate cannot deliver twice. Any miss — no gate (wrong/unknown CallID,
	// stale or duplicate command) or a kind mismatch — is silently DROPPED
	// (fail-safe): the actor never blocks and never panics.
	routeControl := func(cmd command.Command, callID uuid.UUID) {
		g, ok := state.pendingGates[callID]
		if !ok || !accepts(g.kind, cmd) {
			return
		}
		g.reply <- cmd
		delete(state.pendingGates, callID)
	}

	// clearGates drops every open gate at turn end / cancellation. A parked runner
	// is already unblocking via <-ctx.Done() (the turn ctx is cancelled), so the
	// reply channels are simply abandoned; the actor must not hold stale entries
	// that a late control command for a finished turn could match.
	clearGates := func() {
		if len(state.pendingGates) > 0 {
			state.pendingGates = make(map[uuid.UUID]gate)
		}
	}

	// startTurn begins a turn FROM an accepted submit (qi). It is the single
	// commit-then-start path shared by an idle submit and the on-idle inbox pop. It
	// mints the TurnID, installs the active-turn fields (events/abandoned may be nil
	// for a fan-in-only turn), commits qi.msg into loopState.msgs and emits
	// event.TurnStarted{InputID, Message, CausationID=inputID} at the same
	// actor-owned point, then launches runTurn. It returns the new TurnID; on an
	// id-gen failure it returns a non-nil error and starts nothing (the caller
	// decides how to surface it). The actor is the sole caller, so it always runs
	// with state.status idle.
	startTurn := func(qi queuedInput, events chan<- event.Event, abandoned <-chan struct{}) (uuid.UUID, error) {
		turnID, err := cfg.idGen()
		if err != nil {
			return uuid.UUID{}, &IDGenerationError{Cause: err}
		}
		state.turnIndex++
		state.turnID = turnID
		state.causationID = qi.inputID
		state.status = loopRunning
		state.turnEvents = events
		state.turnAbandoned = abandoned
		// The turn ctx derives from the loop ctx (listen's ctx is loopCtx): submit
		// commands carry no context, so a turn's lifetime is bounded by the loop's,
		// not by any caller's API-call ctx.
		turnCtx, cancel := context.WithCancel(ctx)
		state.cancelTurn = cancel
		state.turnDone = turnCtx.Done()
		idx := state.turnIndex

		// base is a defensive CLONE of pre-turn history with its OWN backing array,
		// taken BEFORE the initial UserMessage is committed (runTurn reads it
		// concurrently while the actor keeps appending committed step groups).
		base := cloneMessages(state.msgs)

		// Loop-owned incremental commit: commit the initial UserMessage and emit
		// TurnStarted (Message + CausationID = inputID + InputID = inputID) at the
		// SAME actor-owned point, BEFORE runTurn starts. TriggeredByLoopID carries
		// qi.triggeredBy (set for a SubagentResult, zero for a UserInput).
		state.msgs = append(state.msgs, qi.msg)
		emitTurn(event.TurnStarted{
			Header: event.Header{
				SessionID:         state.sessionID,
				LoopID:            state.id,
				TurnID:            state.turnID,
				CausationID:       state.causationID,
				TriggeredByLoopID: qi.triggeredBy,
			},
			TurnIndex: idx,
			InputID:   qi.inputID,
			Message:   qi.msg,
		})

		ts := newTurnState(state.sessionID, state.id, state.turnID, idx, state.causationID, qi.msg)

		commit := func(cctx context.Context, tc turnCommit) error {
			ack := make(chan struct{}, 1)
			req := commitRequest{commit: tc, ack: ack}
			select {
			case commits <- req:
			case <-cctx.Done():
				return &CommitError{Reason: CommitTurnCancelled, Cause: cctx.Err()}
			}
			select {
			case <-ack:
				return nil
			case <-cctx.Done():
				return &CommitError{Reason: CommitTurnCancelled, Cause: cctx.Err()}
			}
		}

		// drainPending is the tool-continuation drain handshake. It is ctx-cancellable
		// exactly like commit: it selects on the buffered(1) reply AND cctx.Done, so an
		// Interrupt/Shutdown during the drain frees the parked runTurn instead of
		// wedging it. The reply is buffered, so the actor's send never blocks even if
		// runTurn has already escaped on cctx.Done (the moved-out inbox entries are then
		// resolved from loopState.draining by the abnormal-terminal return path).
		drainPending := func(cctx context.Context) ([]foldedMsg, error) {
			reply := make(chan []foldedMsg, 1)
			req := drainRequest{reply: reply}
			select {
			case drains <- req:
			case <-cctx.Done():
				return nil, &CommitError{Reason: CommitTurnCancelled, Cause: cctx.Err()}
			}
			select {
			case batch := <-reply:
				return batch, nil
			case <-cctx.Done():
				return nil, &CommitError{Reason: CommitTurnCancelled, Cause: cctx.Err()}
			}
		}

		// emit is the turn goroutine's per-turn emit. It is nil-safe: a fan-in-only
		// turn (nil events) publishes to sinks/fan-in only and never sends on a nil
		// channel. The escapes (abandoned, turnCtx.Done) keep emit from pinning the
		// turn goroutine when the consumer stops reading.
		emit := func(ev event.Event) {
			publish(ev)
			if events == nil {
				return
			}
			select {
			case events <- ev:
			case <-abandoned:
			case <-turnCtx.Done():
			}
		}

		turnCfg := turnConfig{
			base:         base,
			model:        cfg.Model,
			tools:        cfg.Tools,
			client:       cfg.Client,
			gateReg:      gateReg,
			idGen:        cfg.idGen,
			commit:       commit,
			drainPending: drainPending,
			emit:         emit,
		}

		go func() {
			defer cancel()
			defer func() {
				if r := recover(); r != nil {
					slog.Error("turn goroutine panicked", "panic", r)
					internal <- turnResult{
						terminal: event.TurnFailed{TurnIndex: idx, Err: &event.TurnPanicError{Detail: fmt.Sprintf("%v", r)}},
					}
				}
			}()
			terminal := runTurn(turnCtx, turnCfg, ts)
			internal <- turnResult{terminal: terminal}
		}()
		return turnID, nil
	}

	// userMessageFromBlocks wraps submit blocks into the committed UserMessage form.
	userMessageFromBlocks := func(blocks []content.Block) *content.UserMessage {
		return &content.UserMessage{Message: content.Message{Role: content.RoleUser, Blocks: blocks}}
	}

	// decideSubmit resolves a UserInput/SubagentResult against the actor's OWN live
	// state (race-free), replying its Disposition through ack via tryAck. queueable
	// is false for a StartOnly UserInput (and, later, non-queueable internal turns):
	// such a submit must start or be rejected. events/abandoned are the optional
	// per-turn stream (nil for a fan-in-only submit). On an id-gen failure while
	// starting, the rejected turn's Events channel is closed and TurnRejected{Busy}
	// is mapped to a TurnRejected so the caller always unblocks. A crypto/rand
	// failure means the actor cannot mint the TurnID — a transient system fault — so
	// the loop declines the work (fail-secure): it closes the per-turn stream, logs
	// the typed IDGenerationError, and replies TurnRejected{RejectInternal} (a
	// transient internal failure; the loop is healthy and the caller MAY retry —
	// distinct from RejectShuttingDown, which says the loop is going away).
	decideSubmit := func(qi queuedInput, queueable bool, events chan<- event.Event, abandoned <-chan struct{}, ack chan<- command.Disposition) {
		switch {
		case state.status == loopShuttingDown:
			tryAck[command.Disposition](ack, command.TurnRejected{Reason: command.RejectShuttingDown})
			if events != nil {
				close(events)
			}
		case len(state.inbox) >= inboxCap:
			tryAck[command.Disposition](ack, command.TurnRejected{Reason: command.RejectQueueFull})
			if events != nil {
				close(events)
			}
		case state.status == loopRunning && !queueable:
			tryAck[command.Disposition](ack, command.TurnRejected{Reason: command.RejectBusy})
			if events != nil {
				close(events)
			}
		case state.status == loopRunning:
			// Queueable + busy: accept into the inbox (ordered). A queued submit has
			// no per-turn stream of its own — it resolves on the fan-in — so an
			// events channel supplied here (only StartOnly sets one, and StartOnly is
			// not queueable) cannot reach this branch; nothing to close.
			state.inbox = append(state.inbox, qi)
			tryAck[command.Disposition](ack, command.InputQueued{InputID: qi.inputID})
		default: // idle
			turnID, err := startTurn(qi, events, abandoned)
			if err != nil {
				// Fail-secure: cannot mint a TurnID. Decline the work, close the
				// per-turn stream, and reply a rejection so the caller unblocks. The
				// loop is healthy (only id-gen failed), so this is RejectInternal — a
				// transient failure the caller MAY retry, NOT RejectShuttingDown.
				slog.Error("turn id generation failed; rejecting submit", "error", err)
				tryAck[command.Disposition](ack, command.TurnRejected{Reason: command.RejectInternal})
				if events != nil {
					close(events)
				}
				return
			}
			tryAck[command.Disposition](ack, command.Started{TurnID: turnID, InputID: qi.inputID})
		}
	}

	// returnEntry resolves ONE removed-from-inbox entry as returned: it emits the
	// single event.InputCancelled{reason} that a client observes for qi. It is the
	// ONE place a return is emitted, so the "every removal is resolved exactly once"
	// invariant has a single owner. turnID is the turn whose end caused the return
	// (zero for a pure retract outside a turn); it lands on Header.TurnID.
	returnEntry := func(qi queuedInput, reason event.CancelReason, turnID uuid.UUID) {
		publish(event.InputCancelled{
			Header: event.Header{
				SessionID:         state.sessionID,
				LoopID:            state.id,
				TurnID:            turnID,
				CausationID:       qi.inputID,
				TriggeredByLoopID: qi.triggeredBy,
			},
			InputID: qi.inputID,
			Reason:  reason,
			Message: qi.msg,
		})
	}

	// returnQueuedInbox returns every still-unresolved queued entry via returnEntry
	// after an abnormal terminal (TurnFailed/TurnInterrupted). It covers BOTH the inbox
	// (entries never drained) AND the draining buffer (entries popped for a fold whose
	// TurnFoldedInto never committed because the turn ended first) — without the
	// draining sweep those popped entries would be silently stranded (no
	// TurnFoldedInto, no InputCancelled). The actor does NOT auto-start a new turn from
	// a returned entry — the client decides whether to resend. endedTurnID is the turn
	// that ended (the cause of the return). The draining entries are returned BEFORE
	// the inbox entries, preserving their original receive order (drained entries were
	// queued earliest).
	returnQueuedInbox := func(reason event.CancelReason, endedTurnID uuid.UUID) {
		for _, qi := range state.draining {
			returnEntry(qi, reason, endedTurnID)
		}
		state.draining = nil
		for _, qi := range state.inbox {
			returnEntry(qi, reason, endedTurnID)
		}
		state.inbox = nil
	}

	// popFront removes and returns the first queued entry, the single place the
	// inbox-front splice lives. The bool is false when the inbox is empty.
	//
	// Inbox-exit invariant: every entry that popFront (or any other path) REMOVES
	// from state.inbox MUST be resolved exactly once — either it reaches a successful
	// startTurn (it becomes a turn) or it reaches returnEntry (it is returned via
	// event.InputCancelled). A removed entry that reaches neither is silently
	// stranded; do not add a removal path that can skip both.
	popFront := func() (queuedInput, bool) {
		if len(state.inbox) == 0 {
			return queuedInput{}, false
		}
		qi := state.inbox[0]
		state.inbox = state.inbox[1:]
		return qi, true
	}

	// drainInbox is the tool-continuation drain: it pops + clears the ENTIRE inbox in
	// order, MOVES the popped entries into state.draining (so they are still resolved
	// if the turn ends abnormally before their TurnFoldedInto commits), and returns one
	// foldedMsg per entry for runTurn to fold. It is the single place the inbox→draining
	// move lives. Each moved entry leaves draining only via the commit point (its
	// TurnFoldedInto resolves it) or via returnQueuedInbox (an abnormal terminal), so the
	// inbox-exit invariant — every removed entry is resolved exactly once — still holds.
	drainInbox := func() []foldedMsg {
		if len(state.inbox) == 0 {
			return nil
		}
		batch := make([]foldedMsg, 0, len(state.inbox))
		for _, qi := range state.inbox {
			batch = append(batch, foldedMsg{inputID: qi.inputID, triggeredBy: qi.triggeredBy, msg: qi.msg})
		}
		state.draining = append(state.draining, state.inbox...)
		state.inbox = nil
		return batch
	}

	// removeDraining drops the entry with inputID from state.draining at its
	// TurnFoldedInto commit point (it is now resolved by that event). It is a no-op if
	// the id is absent (defensive — a fold is committed exactly once).
	removeDraining := func(inputID uuid.UUID) {
		for i, qi := range state.draining {
			if qi.inputID == inputID {
				state.draining = append(state.draining[:i], state.draining[i+1:]...)
				return
			}
		}
	}

	// cancelQueued resolves a CancelQueuedInput against the actor-owned inbox. If
	// the InputID is still queued it is removed and resolved via returnEntry
	// (event.InputCancelled{CancelClientRetracted}, Header.TurnID zero — a pure
	// retract outside a turn), replying Cancelled. If not found it has already
	// started or folded, so the actor replies AlreadyCommitted with the active turn id.
	cancelQueued := func(c command.CancelQueuedInput) {
		for i, qi := range state.inbox {
			if qi.inputID != c.InputID {
				continue
			}
			state.inbox = append(state.inbox[:i], state.inbox[i+1:]...)
			// Removed from the inbox by a retract: resolve it via returnEntry (the one
			// return-emit point). TurnID is zero — a pure retract outside any turn.
			returnEntry(qi, event.CancelClientRetracted, uuid.UUID{})
			tryAck[command.CancelResult](c.Ack, command.Cancelled{})
			return
		}
		tryAck[command.CancelResult](c.Ack, command.AlreadyCommitted{TurnID: state.turnID})
	}

	for {
		select {
		case cmd, ok := <-commands:
			if !ok {
				return
			}
			switch c := cmd.(type) {

			case command.UserInput:
				// Interactive (AllowFold) input may queue behind a running turn;
				// StartOnly (Invoke/Stream) must start or be rejected. The actor
				// decides on its own live state — race-free — and replies a
				// Disposition through the buffered(1) Ack via tryAck.
				qi := queuedInput{inputID: c.CommandHeader().ID, msg: userMessageFromBlocks(c.Blocks)}
				decideSubmit(qi, c.Mode == command.AllowFold, c.Events, c.Abandoned, c.Ack)

			case command.SubagentResult:
				// A hand-back from a finished subagent loop. Same decision path as an
				// AllowFold UserInput (always queueable, no per-turn stream); triggeredBy
				// is the producing subagent loop id, stamped on the resulting events.
				qi := queuedInput{
					inputID:     c.CommandHeader().ID,
					triggeredBy: c.FromLoopID,
					msg:         userMessageFromBlocks(c.Blocks),
				}
				decideSubmit(qi, true, nil, nil, c.Ack)

			case command.CancelQueuedInput:
				// Retract a still-queued submit. Resolved by the actor against its own
				// inbox: Cancelled (+ InputCancelled) if still queued, else
				// AlreadyCommitted. No Validate/no fire-and-route — it carries an Ack.
				cancelQueued(c)

			case command.Interrupt:
				if err := c.Validate(); err != nil {
					slog.Warn("invalid Interrupt command", "error", err)
					continue
				}
				if state.cancelTurn != nil {
					state.cancelTurn()
					state.cancelTurn = nil
					c.Ack <- true
				} else {
					c.Ack <- false
				}

			case command.Shutdown:
				if err := c.Validate(); err != nil {
					slog.Warn("invalid Shutdown command", "error", err)
				} else {
					state.shutdownAcks = append(state.shutdownAcks, c.Ack)
				}
				if state.status == loopShuttingDown {
					continue
				}
				wasRunning := state.status == loopRunning
				state.status = loopShuttingDown
				if state.cancelTurn != nil {
					state.cancelTurn()
					state.cancelTurn = nil
				}
				if !wasRunning {
					// Idle shutdown: no turn is running. Return any still-queued input
					// (it will never start) before stopping; in practice the inbox is
					// empty when idle, but this guarantees nothing is silently dropped.
					returnQueuedInbox(event.CancelTurnInterrupted, uuid.UUID{})
					ackShutdowns(nil)
					return
				}
			// Turn goroutine is winding down; wait for internal below.

			// Control commands are fire-and-route: no Validate, no Ack. routeControl
			// delivers to the parked runner blocked on this CallID iff a gate is open
			// AND its kind accepts this command; any miss (unknown/stale CallID, kind
			// mismatch, duplicate after delivery) is silently dropped (fail-safe).
			case command.ApproveToolCall:
				routeControl(c, c.GateCallID())

			case command.DenyToolCall:
				routeControl(c, c.GateCallID())

			case command.ProvideUserInput:
				routeControl(c, c.GateCallID())
			}

		case reg := <-gateReg:
			// Install-before-emit: record the gate under its CallID, then close ack
			// so the parked runner may emit its request event knowing a routed reply
			// can no longer be dropped on a race. Only the actor touches pendingGates.
			state.pendingGates[reg.callID] = gate{reply: reg.reply, kind: reg.kind}
			close(reg.ack)

		case req := <-drains:
			// Tool-continuation drain: pop + clear the inbox into draining and reply the
			// foldedMsgs. The actor is the sole owner of inbox/draining, and the turn
			// goroutine is parked in cfg.drainPending while this runs, so there is no
			// concurrent access. The reply is buffered(1); the send never blocks. The
			// drained entries are now in draining and are resolved either by their
			// TurnFoldedInto commit (below) or by returnQueuedInbox on an abnormal
			// terminal — never silently lost.
			req.reply <- drainInbox()

		case req := <-commits:
			// Loop-owned incremental commit: the actor is the SOLE mutator of
			// loopState.msgs. It appends the completed step group AND emits the
			// Enduring StepDone (or TurnFoldedInto, for a fold) at the SAME point, so
			// the event is never a lie (it always reflects already-committed history).
			// The turn goroutine is parked in cfg.commit while this runs, so emitTurn
			// here cannot race the turn goroutine's TokenDelta emits. Ack last so the
			// runner only resumes after the event is on the stream.
			state.msgs = append(state.msgs, req.commit.Messages...)
			emitTurn(req.commit.Event)
			// A folded user message is now committed: resolve its draining entry (its
			// TurnFoldedInto was just emitted), so the abnormal-terminal return path
			// does not also return it. StepDone commits carry no inbox entry.
			if fi, ok := req.commit.Event.(event.TurnFoldedInto); ok {
				removeDraining(fi.InputID)
			}
			req.ack <- struct{}{}

		case result := <-internal:
			// loopState.msgs is committed incrementally via the commit handshake, not
			// from the turn result. A failed/interrupted turn discards only the
			// in-flight incomplete step (which never committed); committed steps stay.
			state.cancelTurn = nil
			shuttingDown := state.status == loopShuttingDown
			if !shuttingDown {
				state.status = loopIdle
			}
			endedTurnID := state.turnID
			// deliverAndClose publishes the terminal envelope, which must still
			// carry this turn's correlation IDs, so clear them only afterward.
			deliverAndClose(result.terminal)
			state.turnID = uuid.UUID{}
			state.causationID = uuid.UUID{}
			// A finished turn must not leave stale gates: the parked runners have
			// already unblocked via the cancelled turn ctx, and a late control
			// command for this dead turn must not match a leftover gate.
			clearGates()
			if shuttingDown {
				// Shutting down: return any still-queued input (it will never start)
				// and stop. Reason follows the terminal: a TurnDone shutdown still
				// returns queued input as interrupted (the loop is going away).
				returnQueuedInbox(cancelReasonFor(result.terminal), endedTurnID)
				ackShutdowns(nil)
				return
			}
			// Resolve any queued input now that the turn ended:
			//   - normal terminal (TurnDone): start a later turn from the FIRST queued
			//     entry (no input stranded); the rest stay queued for subsequent turns.
			//   - abnormal terminal (TurnFailed/TurnInterrupted): return EVERY queued
			//     entry via InputCancelled and auto-start nothing — the client decides
			//     whether to resend.
			if _, normal := result.terminal.(event.TurnDone); normal {
				if next, ok := popFront(); ok {
					// Inbox-exit invariant: next is now REMOVED from the inbox, so it
					// MUST reach either startTurn-success or returnEntry below — never
					// neither (that would silently strand it).
					//
					// A later turn started from the queue is fan-in-only (nil stream):
					// the original submit's per-turn stream, if any, belonged to a
					// StartOnly caller, which is never queued.
					if _, err := startTurn(next, nil, nil); err != nil {
						// Could not mint a TurnID for the popped entry: resolve THAT
						// entry as returned (returnQueuedInbox would not — next is no
						// longer in the inbox), then return any remaining entries too.
						slog.Error("turn id generation failed starting queued input; returning it", "error", err)
						returnEntry(next, event.CancelTurnFailed, endedTurnID)
						returnQueuedInbox(event.CancelTurnFailed, endedTurnID)
					}
				}
			} else {
				returnQueuedInbox(cancelReasonFor(result.terminal), endedTurnID)
			}

		case <-ctx.Done():
			if state.cancelTurn != nil {
				state.cancelTurn()
				state.cancelTurn = nil
			}
			if state.status == loopRunning || state.status == loopShuttingDown {
				// Hard loop kill. Wait for the cancelled turn goroutine to drain
				// and deliver its terminal, but bound the wait: a provider that
				// ignores ctx must not hold the actor (and Loop.Done) hostage.
				// forceAbandon lets deliverAndClose skip a caller that is already
				// gone; the timeout detaches a goroutine still blocked in the
				// provider. We do NOT close turnEvents on the timeout path — the
				// detached goroutine may still hold it and would panic on a send
				// to a closed channel; it is wedged in the provider and would
				// never have produced a terminal anyway.
				forceAbandon()
				select {
				case result := <-internal:
					deliverAndClose(result.terminal)
				case <-time.After(cfg.DrainTimeout):
					slog.Error("turn goroutine did not drain after ctx cancel; detaching",
						"timeout", cfg.DrainTimeout)
					state.turnEvents = nil
					state.turnAbandoned = nil
					state.turnDone = nil
				}
			}
			// Defensive: the loop is exiting, but drop any gates so no detached path
			// could ever match a stale entry. Parked runners already unblock via the
			// cancelled turn ctx.
			clearGates()
			// Return any still-queued input so a hard kill never silently drops it.
			// best-effort: the loop ctx is already cancelled, so publish is the only
			// observable channel.
			returnQueuedInbox(event.CancelTurnInterrupted, state.turnID)
			ackShutdowns(&command.LoopTerminatedError{Cause: ctx.Err()})
			return
		}
	}
}
