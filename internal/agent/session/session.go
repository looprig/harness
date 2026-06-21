package session

import (
	"context"
	"sync"

	"github.com/inventivepotter/urvi/internal/agent/loop"
	"github.com/inventivepotter/urvi/internal/agent/loop/command"
	"github.com/inventivepotter/urvi/internal/agent/loop/event"
	"github.com/inventivepotter/urvi/internal/agent/loop/identity"
	"github.com/inventivepotter/urvi/internal/agent/session/hub"
	"github.com/inventivepotter/urvi/internal/content"
	"github.com/inventivepotter/urvi/internal/tool"
	"github.com/inventivepotter/urvi/internal/uuid"
)

type SessionErrorKind string

const (
	SessionIDGenerationFailed     SessionErrorKind = "id_generation_failed"
	SessionLoopIDGenerationFailed SessionErrorKind = "loop_id_generation_failed"
	SessionLoopExited             SessionErrorKind = "loop_exited"
	SessionLoopNotFound           SessionErrorKind = "loop_not_found"
	SessionEventChannelClosed     SessionErrorKind = "event_channel_closed"
	SessionContextDone            SessionErrorKind = "context_done"
)

// SessionError is returned when a session method cannot complete.
// Cause is non-nil when there is an underlying error to chain.
type SessionError struct {
	Kind  SessionErrorKind
	Cause error
}

func (e *SessionError) Error() string {
	var msg string
	switch e.Kind {
	case SessionIDGenerationFailed:
		msg = "session: id generation failed"
	case SessionLoopIDGenerationFailed:
		msg = "session: loop id generation failed"
	case SessionLoopExited:
		msg = "session: loop exited"
	case SessionLoopNotFound:
		msg = "session: loop not found"
	case SessionEventChannelClosed:
		msg = "session: event channel closed without terminal event"
	case SessionContextDone:
		msg = "session: context done"
	default:
		msg = "session: error"
	}
	if e.Cause == nil {
		return msg
	}
	return msg + ": " + e.Cause.Error()
}
func (e *SessionError) Unwrap() error { return e.Cause }

// TurnRejectedError is returned by Invoke when the loop refuses to start a
// turn for a StartOnly submit. Invoke is the programmatic single-shot
// (start-or-reject) callers, so a published event.TurnRejected on the per-turn
// stream is surfaced as this typed error. Reason carries the event RejectReason
// (Busy/QueueFull/ShuttingDown/Internal) so callers can errors.As and branch (e.g.
// retry-on-busy, or retry a transient RejectInternal).
type TurnRejectedError struct {
	Reason event.RejectReason
}

func (e *TurnRejectedError) Error() string {
	switch e.Reason {
	case event.RejectBusy:
		return "session: turn rejected: loop busy"
	case event.RejectQueueFull:
		return "session: turn rejected: queue full"
	case event.RejectShuttingDown:
		return "session: turn rejected: loop shutting down"
	case event.RejectInternal:
		// Transient internal failure (e.g. id generation); the loop is healthy and
		// the caller MAY retry. Distinct from RejectShuttingDown.
		return "session: turn rejected: transient internal failure"
	default:
		return "session: turn rejected"
	}
}

// idGenerator mints a fresh UUID. It defaults to uuid.New; tests inject a
// failing generator to exercise the crypto/rand failure branch.
type idGenerator func() (uuid.UUID, error)

type Session struct {
	// SessionID is shared by every loop participating in this session.
	SessionID uuid.UUID

	// hub is the session-level event fan-in. Loops publish through it (via the
	// session's PublishEvent, which delegates here); consumers subscribe via
	// SubscribeEvents. The hub also owns the federated-quiescence model that
	// WaitIdle reads. It is constructed in New before any loop, so a loop
	// never publishes into a nil hub.
	hub *hub.Hub

	// sessionCtx is the shared lifetime root for the session; every loop gets a
	// loopCtx derived from it. sessionCancel is the final backstop, cancelled by
	// the construction context (today) or future explicit teardown.
	sessionCtx    context.Context
	sessionCancel context.CancelFunc

	// loopsMu protects loops and primaryLoopID. There is no session goroutine, so
	// session methods serialize registry access with a normal RWMutex.
	loopsMu sync.RWMutex

	// loops are the loop handles in this session, keyed by loop id. Each entry
	// pairs the loop handle with the provenance of whatever spawned it (zero for
	// the primary loop). Today this map holds one entry; multi-agent
	// orchestration adds subagent loops with a non-zero parent.
	loops map[uuid.UUID]*loopHandle

	// primaryLoopID is the default target for Invoke/Interrupt/Shutdown
	// and the gate-answer methods.
	primaryLoopID uuid.UUID

	// newID mints command-Header IDs and loop ids. It defaults to uuid.New; kept
	// as a field only so tests can inject failure and prove the session never
	// sends zero-id commands and never registers a zero-id loop.
	newID idGenerator
}

// loopHandle is the session's registry entry: the loop's channel handle, the
// provenance of the turn/step that spawned it (zero for the primary loop), and
// the cancel for this loop's loopCtx (a session-owned backstop).
type loopHandle struct {
	loop   *loop.Loop
	parent loop.Provenance
	cancel context.CancelFunc
}

// eventSubscriber is the consumer-facing half of the session fan-in: a TUI/CLI (or
// later a durable journal) attaches here to receive filtered events. It is defined
// where it is consumed (the session), per Dependency Inversion. *Session
// satisfies it by delegating to the hub.
type eventSubscriber interface {
	SubscribeEvents(event.EventFilter) (*hub.EventSubscription, error)
}

// Compile-time proof that *Session is the consumer-facing eventSubscriber.
// Its publisher half (PublishEvent) is asserted by loop.New accepting s as its
// eventPublisher at the NewLoop call site.
var _ eventSubscriber = (*Session)(nil)

// PublishEvent is the session's eventPublisher implementation passed to loop.New.
// It delegates to the hub, which fans the event out to matching subscribers and
// applies any quiescence transition the event implies. The loop depends only on
// the narrow eventPublisher interface; it never sees the hub, its subscriber set,
// or its shutdown state (Interface Segregation / least privilege).
func (s *Session) PublishEvent(ctx context.Context, ev event.Event) error {
	return s.hub.PublishEvent(ctx, ev)
}

// SubscribeEvents attaches a consumer to the session fan-in with the given filter.
// The returned subscription's Events() channel yields the filtered stream; the
// caller must Close it when done. It delegates to the hub.
func (s *Session) SubscribeEvents(filter event.EventFilter) (*hub.EventSubscription, error) {
	return s.hub.SubscribeEvents(filter)
}

// PrimaryLoopID returns the session's primary loop id — the default target for
// Invoke and the loop whose live Ephemeral tokens a single-loop TUI streams.
// A whole-session subscriber builds its EventFilter from it (primary-only Ephemeral
// + all-loop Enduring). It is read-only identity, safe to call concurrently.
func (s *Session) PrimaryLoopID() uuid.UUID {
	s.loopsMu.RLock()
	defer s.loopsMu.RUnlock()
	return s.primaryLoopID
}

// WaitIdle blocks until the session is quiescent, ctx is done, or the session has
// stopped (hub.ErrSessionStopped). It is the headless caller's "is the whole
// interaction at rest?" primitive; it delegates to the hub's quiescence model.
func (s *Session) WaitIdle(ctx context.Context) error {
	return s.hub.WaitIdle(ctx)
}

// expectTurn takes a hand-back wake token for a subagent loop at spawn so its
// in-flight result cannot empty the quiescence set and fire a false idle. It is
// session-internal — loops never call it (they hold only the narrow eventPublisher);
// only the session's subagent orchestration does.
//
// TODO(Open Items A): async subagent spawn must call expectTurn(subagentLoopID)
// before the child can complete its first turn, so the {wake} token guards the
// quiescence set across the hand-back. That async-spawn orchestration is deferred;
// when it lands, NewLoop's async-spawn path is where this call wires in. Today no loop
// spawns an async subagent, so this method has no production caller yet — it is
// exercised by the round-trip and the session+hub quiescence tests.
func (s *Session) expectTurn(ctx context.Context, subagentLoopID uuid.UUID) {
	s.hub.ExpectTurn(ctx, subagentLoopID)
}

// cancelExpectTurn releases a subagent's wake token off the publish path. It is
// session-internal (loops never call it) and is NO LONGER on the SubagentResult
// hand-back path: a SubagentResult is never rejected, so its {wake} token always
// releases on the publish path via the resulting TurnStarted/TurnFoldedInto/
// InputCancelled carrying Cause.LoopID. cancelExpectTurn remains for the future
// async-spawn DISCARD path (a child spawned but abandoned before it ever hands back,
// so no event ever carries its Cause.LoopID). Today it has no production caller;
// it is exercised by the session+hub quiescence tests.
func (s *Session) cancelExpectTurn(ctx context.Context, subagentLoopID uuid.UUID) {
	s.hub.CancelExpectTurn(ctx, subagentLoopID)
}

// deliverSubagentResult is the session-owned SubagentResult hand-back: it routes a
// finished subagent's output (blocks) to its parent loop as a command.SubagentResult
// and returns only a transport error (the loop is gone, or ctx is done). It is
// FIRE-AND-FORGET: a SubagentResult is NEVER rejected, so there is no outcome to wait
// for off the publish path. The parent loop always starts (idle) or queues
// (running/shutting-down) the hand-back, and its quiescence {wake, fromLoopID} token
// is ALWAYS released on the publish path by the resulting Enduring event — a
// TurnStarted/TurnFoldedInto carrying Cause.LoopID == fromLoopID, or an
// InputCancelled (also carrying it) if the loop ends before the hand-back commits (the
// shutdown terminal's returnQueuedInbox, or an idle-time id-gen failure to start). The
// session no longer reads a disposition and no longer releases the token off the
// publish path.
//
// parentLoopID selects the parent loop's command channel — it rides as the command's
// embedded Coordinates.LoopID (the delivery target). fromLoopID is the producing
// subagent (the CHILD); it rides as Header.Cause.LoopID and is stamped onto the events
// the hand-back causes. The submit carries no per-turn stream — the parent's events flow
// to the session fan-in. ctx governs the send only (the loop derives the turn ctx from
// its own loopCtx).
func (s *Session) deliverSubagentResult(ctx context.Context, parentLoopID, fromLoopID uuid.UUID, blocks []content.Block) error {
	l, ok := s.loopFor(parentLoopID)
	if !ok {
		return &SessionError{Kind: SessionLoopNotFound}
	}
	id, err := s.newCommandID()
	if err != nil {
		return err
	}
	select {
	case l.Commands <- command.SubagentResult{
		Coordinates: identity.Coordinates{LoopID: parentLoopID}, // delivery target (PARENT)
		Header: command.Header{
			CommandID: id,
			Cause:     identity.Cause{Coordinates: identity.Coordinates{LoopID: fromLoopID}}, // CHILD (wake token)
		},
		Blocks: blocks,
	}: // Agency left default AgencyMachine — a hand-back is machine-originated
		return nil
	case <-l.Done:
		return &SessionError{Kind: SessionLoopExited}
	case <-ctx.Done():
		return &SessionError{Kind: SessionContextDone, Cause: ctx.Err()}
	}
}

// NewLoop creates another loop inside this session. The new loop shares
// SessionID but receives its own loop id and loop goroutine. parent is the
// provenance of the spawning turn/step (zero for the primary loop); the session
// records it in the registry and passes it to loop.New. The session stores the
// loop handle and returns only the loop id, because callers route through
// session methods rather than writing to a loop command channel directly.
func (s *Session) NewLoop(parent loop.Provenance, cfg loop.Config) (uuid.UUID, error) {
	loopID, err := s.newID()
	if err != nil {
		return uuid.UUID{}, &SessionError{Kind: SessionLoopIDGenerationFailed, Cause: err}
	}

	// Mint the LoopStarted EventID BEFORE building or registering the loop, so an
	// id-gen failure fails NewLoop cleanly (typed error) before any loop exists —
	// we never leave a registered loop behind a returned error.
	eventID, err := s.newID()
	if err != nil {
		return uuid.UUID{}, &SessionError{Kind: SessionIDGenerationFailed, Cause: err}
	}

	loopCtx, cancel := context.WithCancel(s.sessionCtx)
	l, err := loop.New(loopCtx, s.SessionID, loopID, parent, s, cfg)
	if err != nil {
		cancel()
		return uuid.UUID{}, err
	}

	s.loopsMu.Lock()
	s.loops[loopID] = &loopHandle{loop: l, parent: parent, cancel: cancel}
	s.loopsMu.Unlock()

	// Announce the loop tree to subscribers active now. Published AFTER releasing
	// loopsMu — never under the registry lock — because a hub publish fans out and
	// must not hold the registry lock. LoopStarted is a pure announcement: it is not
	// one of the active-mutating events (TurnStarted/LoopIdle/TurnFoldedInto/
	// InputCancelled), so it never perturbs session quiescence. Header.Coordinates is
	// the NEW loop (SessionID+LoopID; Turn/Step zero); Header.Cause is the spawning
	// loop/turn/step (zero for the primary = root), machine-originated. There is no
	// ctx param, so it publishes on the session lifetime (s.sessionCtx).
	ev := event.LoopStarted{
		Header: event.Header{
			Coordinates: identity.Coordinates{SessionID: s.SessionID, LoopID: loopID},
			EventID:     eventID,
			Cause: identity.Cause{
				Coordinates: identity.Coordinates{LoopID: parent.LoopID, TurnID: parent.TurnID, StepID: parent.StepID},
				Agency:      identity.AgencyMachine,
			},
		},
	}
	if err := s.PublishEvent(s.sessionCtx, ev); err != nil {
		return uuid.UUID{}, err
	}
	return loopID, nil
}

// loopFor returns the loop's channel handle for command routing. The registry
// stores *loopHandle; this derefs to the handle's loop. The parent provenance is
// read only by future tree walks, which read s.loops directly.
func (s *Session) loopFor(loopID uuid.UUID) (*loop.Loop, bool) {
	s.loopsMu.RLock()
	defer s.loopsMu.RUnlock()
	h, ok := s.loops[loopID]
	if !ok {
		return nil, false
	}
	return h.loop, true
}

// newCommandID mints a fresh correlation ID for a command Header. Any
// crypto/rand failure is mapped onto the session's typed error path rather than
// swallowed, so callers never send an unidentifiable (zero-ID) command.
func (s *Session) newCommandID() (uuid.UUID, error) {
	id, err := s.newID()
	if err != nil {
		return uuid.UUID{}, &SessionError{Kind: SessionIDGenerationFailed, Cause: err}
	}
	return id, nil
}

// New constructs a Session and starts its primary loop's actor
// goroutine. It owns the session fan-in hub and emits the session-scoped
// SessionStarted through it.
//
// This SessionStarted (the s.hub.PublishEvent below) is the SOLE SessionStarted:
// the session publishes it through the HUB to its SUBSCRIBERS (TUI/CLI fan-in),
// and the loop never emits one. It is published before any subscriber attaches,
// so a subscriber that connects later does not observe it; reliable delivery of
// the session start to late subscribers is a separate future follow-on.
func New(ctx context.Context, cfg loop.Config) (*Session, error) {
	select {
	case <-ctx.Done():
		return nil, &SessionError{Kind: SessionContextDone, Cause: ctx.Err()}
	default:
	}

	id, err := uuid.New()
	if err != nil {
		return nil, &SessionError{Kind: SessionIDGenerationFailed, Cause: err}
	}

	sessionCtx, sessionCancel := context.WithCancel(ctx)
	s := &Session{
		SessionID:     id,
		hub:           hub.New(id),
		sessionCtx:    sessionCtx,
		sessionCancel: sessionCancel,
		loops:         make(map[uuid.UUID]*loopHandle),
		newID:         uuid.New,
	}

	// The hub is built before any loop, so a loop publishing through the session's
	// PublishEvent never sees a nil hub. With no subscribers yet, this delivers to
	// nobody (a no-op), but it is the session's authoritative session-scoped start.
	if err := s.hub.PublishEvent(sessionCtx, event.SessionStarted{Header: event.Header{Coordinates: identity.Coordinates{SessionID: id}}}); err != nil {
		sessionCancel()
		return nil, &SessionError{Kind: SessionContextDone, Cause: err}
	}

	primaryLoopID, err := s.NewLoop(loop.Provenance{}, cfg)
	if err != nil {
		sessionCancel()
		return nil, err
	}
	s.primaryLoopID = primaryLoopID
	return s, nil
}

// Invoke sends input as a StartOnly UserInput and blocks until a terminal event.
// It is the programmatic single-shot caller (start-or-reject), reading the outcome
// from the per-turn stream's first event: an event.TurnStarted proceeds; an
// event.TurnRejected returns a typed *TurnRejectedError. The submit carries no context (the loop derives the turn
// ctx from its loopCtx), so cancelling ctx no longer cancels the turn through the
// command — instead the session translates the boundary cancel into an Interrupt
// and returns the resulting event.TurnInterrupted.
func (s *Session) Invoke(ctx context.Context, input []content.Block) (event.Event, error) {
	l, ok := s.loopFor(s.primaryLoopID)
	if !ok {
		return nil, &SessionError{Kind: SessionLoopNotFound}
	}
	id, err := s.newCommandID()
	if err != nil {
		return nil, err
	}
	events := make(chan event.Event, 64)
	abandoned := make(chan struct{})
	defer close(abandoned) // ensures deliverAndClose always has an escape if Invoke exits early

	select {
	// User-initiated turn: Cause.CommandID is zero (root).
	case l.Commands <- command.UserInput{Header: command.Header{CommandID: id}, Mode: command.StartOnly, Blocks: input, Events: events, Abandoned: abandoned}:
	case <-ctx.Done():
		return nil, &SessionError{Kind: SessionContextDone, Cause: ctx.Err()}
	case <-l.Done:
		return nil, &SessionError{Kind: SessionLoopExited}
	}

	// The start-or-reject outcome is the FIRST event on the per-turn stream: the loop
	// delivers event.TurnRejected (then closes the stream) on a refusal, or
	// event.TurnStarted on success. Read it before draining to the terminal.
	select {
	case first, ok := <-events:
		if !ok {
			// Stream closed with no event: a rejection whose on-stream send lost the
			// non-blocking race (defensive — the loop sends before closing). The fan-in
			// still carries the authoritative TurnRejected; surface a generic rejection.
			return nil, &SessionError{Kind: SessionEventChannelClosed}
		}
		if rej, ok := first.(event.TurnRejected); ok {
			return nil, &TurnRejectedError{Reason: rej.Reason}
		}
		// event.TurnStarted (or any non-reject first event): a turn exists; fall through
		// and keep draining for the terminal. The first event itself is not a terminal.
	case <-l.Done:
		return nil, &SessionError{Kind: SessionLoopExited}
	}

	// interrupted guards a single best-effort Interrupt: once ctx is done we cancel
	// the turn and keep draining for the resulting terminal.
	interrupted := false
	for {
		select {
		case ev, ok := <-events:
			if !ok {
				return nil, &SessionError{Kind: SessionEventChannelClosed}
			}
			switch ev.(type) {
			case event.TurnDone, event.TurnFailed, event.TurnInterrupted:
				return ev, nil
			}
		case <-ctx.Done():
			// Boundary cancel: submits carry no ctx, so translate it into an
			// Interrupt and keep draining for the TurnInterrupted terminal.
			if !interrupted {
				interrupted = true
				s.interruptLoop(l)
			}
		case <-l.Done:
			// Hard loop kill: on a DrainTimeout detach the actor never closes
			// `events`, so without this escape Invoke would block forever. The
			// loop is gone, so no terminal can arrive.
			return nil, &SessionError{Kind: SessionLoopExited}
		}
	}
}

// interruptLoop sends a best-effort Interrupt to the loop to cancel its active
// turn, escaping on the loop's Done so a stopped loop never wedges the send. It is
// used to translate an Invoke boundary-ctx cancel (the submit carries no
// ctx) into a turn cancellation. The ack is buffered(1) and unread here: the
// caller observes the cancellation through the resulting TurnInterrupted terminal,
// not this command's reply. An id-gen failure is swallowed (best-effort): the worst
// case is the turn runs to its natural terminal instead of being interrupted.
func (s *Session) interruptLoop(l *loop.Loop) {
	id, err := s.newID()
	if err != nil {
		return
	}
	ack := make(chan bool, 1)
	select {
	case l.Commands <- command.Interrupt{Header: command.Header{CommandID: id}, Ack: ack}:
	case <-l.Done:
	}
}

// Submit is the HUMAN-ONLY submit entry point: it stamps Agency=AgencyUser (a
// person authored this input). Programmatic/machine callers use Invoke
// (StartOnly), which stays Agency=AgencyMachine.
//
// Submit sends input as an AllowFold (queueable) UserInput to the primary loop,
// FIRE-AND-FORGET: it returns the InputID (the submit command's id, == the
// Cause.CommandID on the resulting Reply events) and a transport error only if the
// command could not be handed to the loop. The outcome — InputQueued /
// TurnStarted / TurnFoldedInto / TurnRejected / InputCancelled — is observed on
// the event fan-in (each Reply carries Cause.CommandID == this returned id), NOT
// returned here.
//
// AllowFold is the interactive queueable mode: a submit while a turn is running
// QUEUES rather than rejecting; a submit while idle starts a turn. Submit attaches
// no per-turn stream (Events/Abandoned nil) — unlike Invoke, it never reads
// a reply, so it returns the instant the command is accepted by the loop.
//
// The send carries the same escapes as Invoke: ctx.Done() →
// SessionContextDone, the loop's Done → SessionLoopExited, and a missing primary
// loop → SessionLoopNotFound. On any of those the returned id is the zero UUID,
// because nothing was sent and there is no correlation to hand back.
func (s *Session) Submit(ctx context.Context, input []content.Block) (uuid.UUID, error) {
	l, ok := s.loopFor(s.primaryLoopID)
	if !ok {
		return uuid.UUID{}, &SessionError{Kind: SessionLoopNotFound}
	}
	id, err := s.newCommandID()
	if err != nil {
		return uuid.UUID{}, err
	}
	select {
	// User-initiated queueable turn: Cause.CommandID is zero (root); Events/Abandoned
	// nil because the outcome is observed on the session fan-in, not a per-turn
	// stream. Submit is the interactive (AllowFold) submit — the human-typed input
	// path — so it stamps Agency=AgencyUser (a human authored this). The programmatic
	// submit path is the SEPARATE StartOnly Invoke method, which stays machine.
	case l.Commands <- command.UserInput{Header: command.Header{CommandID: id, Agency: identity.AgencyUser}, Mode: command.AllowFold, Blocks: input}:
		return id, nil
	case <-ctx.Done():
		return uuid.UUID{}, &SessionError{Kind: SessionContextDone, Cause: ctx.Err()}
	case <-l.Done:
		return uuid.UUID{}, &SessionError{Kind: SessionLoopExited}
	}
}

// Interrupt cancels the running turn. Returns true if a turn was cancelled.
// ctx allows the caller to time out the cancel attempt if the actor is slow.
func (s *Session) Interrupt(ctx context.Context) (bool, error) {
	l, ok := s.loopFor(s.primaryLoopID)
	if !ok {
		return false, &SessionError{Kind: SessionLoopNotFound}
	}
	id, err := s.newCommandID()
	if err != nil {
		return false, err
	}
	ack := make(chan bool, 1)
	select {
	// A manual Interrupt is a human-origination point (the human pressed interrupt),
	// so it stamps Agency=AgencyUser. The programmatic boundary-cancel translation
	// (interruptLoop, fired by an Invoke ctx cancel) is a SEPARATE method and
	// stays machine — we never falsely attribute that machine action to a user.
	case l.Commands <- command.Interrupt{Header: command.Header{CommandID: id, Agency: identity.AgencyUser}, Ack: ack}:
	case <-l.Done:
		return false, &SessionError{Kind: SessionLoopExited}
	case <-ctx.Done():
		return false, &SessionError{Kind: SessionContextDone, Cause: ctx.Err()}
	}

	select {
	case cancelled := <-ack:
		return cancelled, nil
	case <-l.Done:
		return false, &SessionError{Kind: SessionLoopExited}
	case <-ctx.Done():
		return false, &SessionError{Kind: SessionContextDone, Cause: ctx.Err()}
	}
}

// Shutdown drives the session to its stopped phase and blocks until the loop
// actor exits. The order is deliberate:
//
//  1. hub.StopSession FIRST — flip the session phase to SessionStopped, wake every
//     WaitIdle waiter with ErrSessionStopped, and deliver SessionStopped to
//     subscribers. Doing this before the loop teardown means a headless WaitIdle
//     unblocks immediately and any shutdown-induced loop terminals that arrive
//     later are published but no longer mutate quiescence (post-stop publishes
//     never derive SessionIdle).
//  2. THEN snapshot the loops and send command.Shutdown to each, keeping the
//     Phase-3 Done/ctx send escapes so the unbuffered send can never wedge.
//  3. THEN sessionCancel as the final backstop so every loopCtx derived from
//     sessionCtx is released.
//
// Calling Shutdown after the actor has exited is a no-op (StopSession is
// idempotent; the loop's Done short-circuits the rest).
func (s *Session) Shutdown(ctx context.Context) error {
	s.hub.StopSession(ctx)

	l, ok := s.loopFor(s.primaryLoopID)
	if !ok {
		// No primary loop to stop; still cancel the session lifetime backstop so
		// any loopCtx derived from sessionCtx is released.
		s.sessionCancel()
		return nil
	}
	id, err := s.newCommandID()
	if err != nil {
		return err
	}
	ack := make(chan error, 1)
	select {
	case l.Commands <- command.Shutdown{Header: command.Header{CommandID: id}, Ack: ack}:
	case <-l.Done:
		// Loop already exited; cancel the session lifetime backstop and return.
		s.sessionCancel()
		return nil
	case <-ctx.Done():
		return &SessionError{Kind: SessionContextDone, Cause: ctx.Err()}
	}

	select {
	case err := <-ack:
		// The actor has stopped; cancel the session lifetime backstop as the
		// final step so every loopCtx derived from sessionCtx is released.
		s.sessionCancel()
		// err is non-nil when the loop's root context was cancelled before
		// the actor finished cleanup. Wrap it so callers always receive a
		// typed *SessionError rather than a raw context error.
		if err != nil {
			return &SessionError{Kind: SessionContextDone, Cause: err}
		}
		return nil
	case <-l.Done:
		s.sessionCancel()
		return nil
	case <-ctx.Done():
		return &SessionError{Kind: SessionContextDone, Cause: ctx.Err()}
	}
}

// Approve approves the pending tool call identified by toolExecutionID, granting
// it at the given persistence scope. The reply is dispatched to loopID — the loop
// that opened the gate — so a subagent loop's gate is never answered by routing to
// the primary (the latent multi-loop misroute). loopID is resolved against the
// registry; a zero loopID falls back to the primary loop (single-loop default),
// and an unknown non-zero loopID fails secure with SessionLoopNotFound. It is
// fire-and-route: the command carries no Ack, so Approve returns as soon as the
// actor accepts it (the gate unblocking and the subsequent ToolCallStarted event
// are the observable effect, not a reply). The select covers ctx.Done() and the
// loop's Done channel so the unbuffered send can never block forever.
func (s *Session) Approve(ctx context.Context, loopID, toolExecutionID uuid.UUID, scope tool.ApprovalScope) error {
	l, route, err := s.resolveGate(loopID, toolExecutionID)
	if err != nil {
		return err
	}
	id, err := s.newCommandID()
	if err != nil {
		return err
	}
	// A human approve is a user-origination point (the gate replies): stamp AgencyUser.
	return s.routeGate(ctx, l, command.ApproveToolCall{Header: command.Header{CommandID: id, Agency: identity.AgencyUser}, GateRoute: route, Scope: scope})
}

// Deny denies the pending tool call identified by toolExecutionID, failing it
// closed (fail-secure). Like Approve it dispatches to loopID (the loop that opened
// the gate) and is fire-and-route with no Ack and no scope — nothing is ever
// persisted on a deny. A zero loopID falls back to the primary loop; an unknown
// non-zero loopID fails secure with SessionLoopNotFound.
func (s *Session) Deny(ctx context.Context, loopID, toolExecutionID uuid.UUID) error {
	l, route, err := s.resolveGate(loopID, toolExecutionID)
	if err != nil {
		return err
	}
	id, err := s.newCommandID()
	if err != nil {
		return err
	}
	// A human deny is a user-origination point (the gate replies): stamp AgencyUser.
	return s.routeGate(ctx, l, command.DenyToolCall{Header: command.Header{CommandID: id, Agency: identity.AgencyUser}, GateRoute: route})
}

// ProvideUserInput supplies the user's answer to the pending AskUser request
// identified by toolExecutionID. Like the approve/deny pair it dispatches to
// loopID (the loop that opened the gate) and is fire-and-route with no Ack: the
// actor routes it to the parked user-input gate, which delivers answer to the
// waiting tool. A zero loopID falls back to the primary loop; an unknown non-zero
// loopID fails secure with SessionLoopNotFound.
func (s *Session) ProvideUserInput(ctx context.Context, loopID, toolExecutionID uuid.UUID, answer string) error {
	l, route, err := s.resolveGate(loopID, toolExecutionID)
	if err != nil {
		return err
	}
	id, err := s.newCommandID()
	if err != nil {
		return err
	}
	// A human answer is a user-origination point (the gate replies): stamp AgencyUser.
	return s.routeGate(ctx, l, command.ProvideUserInput{Header: command.Header{CommandID: id, Agency: identity.AgencyUser}, GateRoute: route, Answer: answer})
}

// resolveGate selects the target loop for a gate reply and builds the command's
// GateRoute. A zero loopID is "unspecified at this granularity": it falls back to
// the primary loop (the single-loop default). A non-zero loopID is looked up in
// the registry as-is; an unknown one fails secure with SessionLoopNotFound rather
// than silently falling through to the primary loop — an unroutable approval must
// never approve a tool call on a loop the caller did not address. The returned
// GateRoute carries the RESOLVED loop id (the loop actually dispatched to) and the
// match key (ToolExecutionID), so the route is concrete and self-describing.
func (s *Session) resolveGate(loopID, toolExecutionID uuid.UUID) (*loop.Loop, command.GateRoute, error) {
	targetLoopID := loopID
	if targetLoopID.IsZero() {
		targetLoopID = s.PrimaryLoopID()
	}
	l, ok := s.loopFor(targetLoopID)
	if !ok {
		return nil, command.GateRoute{}, &SessionError{Kind: SessionLoopNotFound}
	}
	route := command.GateRoute{
		Coordinates:     identity.Coordinates{SessionID: s.SessionID, LoopID: targetLoopID},
		ToolExecutionID: toolExecutionID,
	}
	return l, route, nil
}

// routeGate sends a fire-and-route gate command to the resolved target loop. These
// commands carry no Ack, so routeGate returns nil as soon as the send completes
// and never waits for a reply. It selects on ctx.Done() and the loop's Done
// channel alongside the unbuffered send so the call can never block forever when
// the actor is busy (ctx times out) or has already exited (Done is closed).
func (s *Session) routeGate(ctx context.Context, l *loop.Loop, cmd command.Command) error {
	select {
	case l.Commands <- cmd:
		return nil
	case <-l.Done:
		return &SessionError{Kind: SessionLoopExited}
	case <-ctx.Done():
		return &SessionError{Kind: SessionContextDone, Cause: ctx.Err()}
	}
}
