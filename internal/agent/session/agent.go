package session

import (
	"context"
	"io"
	"sync"

	"github.com/inventivepotter/urvi/internal/agent/loop"
	"github.com/inventivepotter/urvi/internal/agent/loop/command"
	"github.com/inventivepotter/urvi/internal/agent/loop/event"
	"github.com/inventivepotter/urvi/internal/agent/session/hub"
	"github.com/inventivepotter/urvi/internal/content"
	"github.com/inventivepotter/urvi/internal/llm"
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

// TurnRejectedError is returned by Invoke/Stream when the loop refuses to start a
// turn for a StartOnly submit. Invoke/Stream are the programmatic single-shot
// (start-or-reject) callers, so a non-Started disposition is surfaced as this
// typed error. Reason carries the loop's RejectReason (Busy/QueueFull/
// ShuttingDown/Internal) so callers can errors.As and branch (e.g. retry-on-busy,
// or retry a transient RejectInternal).
type TurnRejectedError struct {
	Reason command.RejectReason
}

func (e *TurnRejectedError) Error() string {
	switch e.Reason {
	case command.RejectBusy:
		return "session: turn rejected: loop busy"
	case command.RejectQueueFull:
		return "session: turn rejected: queue full"
	case command.RejectShuttingDown:
		return "session: turn rejected: loop shutting down"
	case command.RejectInternal:
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

type AgentSession struct {
	// SessionID is shared by every loop participating in this session.
	SessionID uuid.UUID

	// hub is the session-level event fan-in. Loops publish through it (via the
	// session's PublishEvent, which delegates here); consumers subscribe via
	// SubscribeEvents. The hub also owns the federated-quiescence model that
	// WaitIdle reads. It is constructed in NewAgent before any loop, so a loop
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

	// primaryLoopID is the default target for Invoke/Stream/Interrupt/Shutdown
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
// where it is consumed (the session), per Dependency Inversion. *AgentSession
// satisfies it by delegating to the hub.
type eventSubscriber interface {
	SubscribeEvents(event.EventFilter) (*hub.EventSubscription, error)
}

// Compile-time proof that *AgentSession is the consumer-facing eventSubscriber.
// Its publisher half (PublishEvent) is asserted by loop.New accepting s as its
// eventPublisher at the NewLoop call site.
var _ eventSubscriber = (*AgentSession)(nil)

// PublishEvent is the session's eventPublisher implementation passed to loop.New.
// It delegates to the hub, which fans the event out to matching subscribers and
// applies any quiescence transition the event implies. The loop depends only on
// the narrow eventPublisher interface; it never sees the hub, its subscriber set,
// or its shutdown state (Interface Segregation / least privilege).
func (s *AgentSession) PublishEvent(ctx context.Context, ev event.Event) error {
	return s.hub.PublishEvent(ctx, ev)
}

// SubscribeEvents attaches a consumer to the session fan-in with the given filter.
// The returned subscription's Events() channel yields the filtered stream; the
// caller must Close it when done. It delegates to the hub.
func (s *AgentSession) SubscribeEvents(filter event.EventFilter) (*hub.EventSubscription, error) {
	return s.hub.SubscribeEvents(filter)
}

// WaitIdle blocks until the session is quiescent, ctx is done, or the session has
// stopped (hub.ErrSessionStopped). It is the headless caller's "is the whole
// interaction at rest?" primitive; it delegates to the hub's quiescence model.
func (s *AgentSession) WaitIdle(ctx context.Context) error {
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
func (s *AgentSession) expectTurn(ctx context.Context, subagentLoopID uuid.UUID) {
	s.hub.ExpectTurn(ctx, subagentLoopID)
}

// cancelExpectTurn releases a subagent's wake token when its hand-back is rejected or
// discarded. Session-internal (loops never call it). It is the ONLY hand-back wake
// release that is NOT on the publish path: a TurnRejected SubagentResult produces no
// event (nothing happened in the session), so deliverSubagentResult releases the token
// here after reading the Disposition. The event-path releases
// (TurnStarted/TurnFoldedInto/InputCancelled carrying TriggeredByLoopID) happen
// automatically in the hub now that loop events are published.
func (s *AgentSession) cancelExpectTurn(ctx context.Context, subagentLoopID uuid.UUID) {
	s.hub.CancelExpectTurn(ctx, subagentLoopID)
}

// deliverSubagentResult is the session-owned SubagentResult hand-back round trip. It
// routes a finished subagent's output (blocks) to its parent loop as a
// command.SubagentResult, reads the loop's Disposition through a buffered(1) Ack, and
// returns it. The parent loop decides start/queue/reject on its own state (race-free);
// the session does NOT decide. On a TurnRejected disposition the session releases the
// subagent's {wake, fromLoopID} quiescence token via cancelExpectTurn — the rejected
// hand-back produced no event, so this is the one release off the publish path; a
// Started/InputQueued outcome instead releases on the publish path when the resulting
// TurnStarted/TurnFoldedInto/InputCancelled carries TriggeredByLoopID == fromLoopID.
//
// parentLoopID selects the parent loop's command channel; fromLoopID is the producing
// subagent (stamped as SubagentResult.FromLoopID -> TriggeredByLoopID on the events the
// hand-back causes). The submit carries no per-turn stream — the parent's events flow
// to the session fan-in. ctx governs the send + ack wait only (the loop derives the
// turn ctx from its own loopCtx).
func (s *AgentSession) deliverSubagentResult(ctx context.Context, parentLoopID, fromLoopID uuid.UUID, blocks []content.Block) (command.Disposition, error) {
	l, ok := s.loopFor(parentLoopID)
	if !ok {
		return nil, &SessionError{Kind: SessionLoopNotFound}
	}
	id, err := s.newCommandID()
	if err != nil {
		return nil, err
	}
	ack := make(chan command.Disposition, 1) // buffered(1): the loop replies via tryAck
	select {
	case l.Commands <- command.SubagentResult{Header: command.Header{ID: id}, FromLoopID: fromLoopID, Blocks: blocks, Ack: ack}:
	case <-l.Done:
		return nil, &SessionError{Kind: SessionLoopExited}
	case <-ctx.Done():
		return nil, &SessionError{Kind: SessionContextDone, Cause: ctx.Err()}
	}

	select {
	case d := <-ack:
		// A rejected hand-back produced no event, so its {wake} token will never be
		// released on the publish path. Release it here (the only off-publish-path
		// release) so a rejected SubagentResult cannot leak the token.
		if _, rejected := d.(command.TurnRejected); rejected {
			s.cancelExpectTurn(ctx, fromLoopID)
		}
		return d, nil
	case <-l.Done:
		return nil, &SessionError{Kind: SessionLoopExited}
	case <-ctx.Done():
		return nil, &SessionError{Kind: SessionContextDone, Cause: ctx.Err()}
	}
}

// NewLoop creates another loop inside this session. The new loop shares
// SessionID but receives its own loop id and loop goroutine. parent is the
// provenance of the spawning turn/step (zero for the primary loop); the session
// records it in the registry and passes it to loop.New. The session stores the
// loop handle and returns only the loop id, because callers route through
// session methods rather than writing to a loop command channel directly.
func (s *AgentSession) NewLoop(parent loop.Provenance, cfg loop.Config) (uuid.UUID, error) {
	loopID, err := s.newID()
	if err != nil {
		return uuid.UUID{}, &SessionError{Kind: SessionLoopIDGenerationFailed, Cause: err}
	}

	loopCtx, cancel := context.WithCancel(s.sessionCtx)
	l, err := loop.New(loopCtx, s.SessionID, loopID, parent, s, cfg)
	if err != nil {
		cancel()
		return uuid.UUID{}, err
	}

	s.loopsMu.Lock()
	defer s.loopsMu.Unlock()
	s.loops[loopID] = &loopHandle{loop: l, parent: parent, cancel: cancel}
	return loopID, nil
}

// loopFor returns the loop's channel handle for command routing. The registry
// stores *loopHandle; this derefs to the handle's loop. The parent provenance is
// read only by future tree walks, which read s.loops directly.
func (s *AgentSession) loopFor(loopID uuid.UUID) (*loop.Loop, bool) {
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
func (s *AgentSession) newCommandID() (uuid.UUID, error) {
	id, err := s.newID()
	if err != nil {
		return uuid.UUID{}, &SessionError{Kind: SessionIDGenerationFailed, Cause: err}
	}
	return id, nil
}

// NewAgent constructs an AgentSession and starts its primary loop's actor
// goroutine. It owns the session fan-in hub and emits the session-scoped
// SessionStarted through it.
//
// Two distinct consumer paths carry SessionStarted, by design, to two distinct
// audiences: the loop's actor publishes a SessionStarted to its observability
// SINKS (cfg.Sinks) on startup, while the session here publishes the session-scoped
// SessionStarted through the HUB to its SUBSCRIBERS (TUI/CLI fan-in). These never
// double-deliver to the same consumer — sinks and subscribers are separate sets —
// so the two emissions are complementary, not redundant.
func NewAgent(ctx context.Context, cfg loop.Config) (*AgentSession, error) {
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
	s := &AgentSession{
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
	if err := s.hub.PublishEvent(sessionCtx, event.SessionStarted{Header: event.Header{SessionID: id}}); err != nil {
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
// It is the programmatic single-shot caller (start-or-reject): a Started
// disposition proceeds; a TurnRejected disposition returns a typed
// *TurnRejectedError. The submit carries no context (the loop derives the turn
// ctx from its loopCtx), so cancelling ctx no longer cancels the turn through the
// command — instead the session translates the boundary cancel into an Interrupt
// and returns the resulting event.TurnInterrupted.
func (s *AgentSession) Invoke(ctx context.Context, input []content.Block) (event.Event, error) {
	l, ok := s.loopFor(s.primaryLoopID)
	if !ok {
		return nil, &SessionError{Kind: SessionLoopNotFound}
	}
	id, err := s.newCommandID()
	if err != nil {
		return nil, err
	}
	events := make(chan event.Event, 64)
	ack := make(chan command.Disposition, 1)
	abandoned := make(chan struct{})
	defer close(abandoned) // ensures deliverAndClose always has an escape if Invoke exits early

	select {
	// User-initiated turn: CausationID is zero (root).
	case l.Commands <- command.UserInput{Header: command.Header{ID: id}, Mode: command.StartOnly, Blocks: input, Events: events, Abandoned: abandoned, Ack: ack}:
	case <-ctx.Done():
		return nil, &SessionError{Kind: SessionContextDone, Cause: ctx.Err()}
	case <-l.Done:
		return nil, &SessionError{Kind: SessionLoopExited}
	}

	select {
	case d := <-ack:
		if rej, ok := d.(command.TurnRejected); ok {
			return nil, &TurnRejectedError{Reason: rej.Reason}
		}
		// Started: a turn exists; proceed to drain its events.
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
// used to translate an Invoke/Stream boundary-ctx cancel (the submit carries no
// ctx) into a turn cancellation. The ack is buffered(1) and unread here: the
// caller observes the cancellation through the resulting TurnInterrupted terminal,
// not this command's reply. An id-gen failure is swallowed (best-effort): the worst
// case is the turn runs to its natural terminal instead of being interrupted.
func (s *AgentSession) interruptLoop(l *loop.Loop) {
	id, err := s.newID()
	if err != nil {
		return
	}
	ack := make(chan bool, 1)
	select {
	case l.Commands <- command.Interrupt{Header: command.Header{ID: id}, Ack: ack}:
	case <-l.Done:
	}
}

// Stream sends input as a StartOnly UserInput and returns a
// StreamReader[event.Event] that yields TurnStarted, TokenDelta×N, then one
// terminal event, then EOF while the caller keeps reading. A Started disposition
// returns the reader; a TurnRejected disposition returns a typed
// *TurnRejectedError. Calling sr.Close() abandons the event stream AND interrupts
// the turn (the submit carries no ctx, so Close translates into an Interrupt).
// Callers must either read until EOF or call Close.
func (s *AgentSession) Stream(ctx context.Context, input []content.Block) (*llm.StreamReader[event.Event], error) {
	l, ok := s.loopFor(s.primaryLoopID)
	if !ok {
		return nil, &SessionError{Kind: SessionLoopNotFound}
	}
	id, err := s.newCommandID()
	if err != nil {
		return nil, err
	}
	abandoned := make(chan struct{})
	var abandonOnce sync.Once
	events := make(chan event.Event, 64)
	ack := make(chan command.Disposition, 1)

	select {
	// User-initiated turn: CausationID is zero (root).
	case l.Commands <- command.UserInput{
		Header:    command.Header{ID: id},
		Mode:      command.StartOnly,
		Blocks:    input,
		Events:    events,
		Abandoned: abandoned,
		Ack:       ack,
	}:
	case <-ctx.Done():
		abandonOnce.Do(func() { close(abandoned) })
		return nil, &SessionError{Kind: SessionContextDone, Cause: ctx.Err()}
	case <-l.Done:
		abandonOnce.Do(func() { close(abandoned) })
		return nil, &SessionError{Kind: SessionLoopExited}
	}

	select {
	case d := <-ack:
		if rej, ok := d.(command.TurnRejected); ok {
			abandonOnce.Do(func() { close(abandoned) })
			return nil, &TurnRejectedError{Reason: rej.Reason}
		}
		// Started: a turn exists; return the per-turn reader.
	case <-l.Done:
		abandonOnce.Do(func() { close(abandoned) })
		return nil, &SessionError{Kind: SessionLoopExited}
	}

	return llm.NewStreamReader(
		func() (event.Event, error) {
			// The loop.Done case rescues a reader parked here if the loop is
			// hard-killed mid-turn: on a DrainTimeout detach the actor never
			// closes `events`, so without this escape a consumer would block
			// until the hung provider returned (or forever). When the loop
			// exits, no further events can arrive, so EOF is the correct signal.
			select {
			case ev, ok := <-events:
				if !ok {
					return nil, io.EOF
				}
				return ev, nil
			case <-l.Done:
				return nil, io.EOF
			}
		},
		func() error {
			// Close abandons the stream (so the actor's terminal delivery never
			// blocks on this reader) and interrupts the turn (the submit carried no
			// ctx). The Interrupt is best-effort and escapes on l.Done.
			abandonOnce.Do(func() { close(abandoned) })
			s.interruptLoop(l)
			return nil
		},
	), nil
}

// Interrupt cancels the running turn. Returns true if a turn was cancelled.
// ctx allows the caller to time out the cancel attempt if the actor is slow.
func (s *AgentSession) Interrupt(ctx context.Context) (bool, error) {
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
	case l.Commands <- command.Interrupt{Header: command.Header{ID: id}, Ack: ack}:
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
func (s *AgentSession) Shutdown(ctx context.Context) error {
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
	case l.Commands <- command.Shutdown{Header: command.Header{ID: id}, Ack: ack}:
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

// Approve approves the pending tool call identified by callID, granting it at the
// given persistence scope. It is fire-and-route: the command carries no Ack, so
// Approve returns as soon as the actor accepts it (the gate unblocking and the
// subsequent ToolCallStarted event are the observable effect, not a reply). The
// select covers ctx.Done() and the loop's Done channel so the unbuffered send can
// never block forever if the actor is busy or has exited.
func (s *AgentSession) Approve(ctx context.Context, callID uuid.UUID, scope tool.ApprovalScope) error {
	id, err := s.newCommandID()
	if err != nil {
		return err
	}
	return s.routeCommand(ctx, command.ApproveToolCall{Header: command.Header{ID: id}, CallID: callID, Scope: scope})
}

// Deny denies the pending tool call identified by callID, failing it closed
// (fail-secure). Like Approve it is fire-and-route with no Ack and no scope —
// nothing is ever persisted on a deny. The select covers ctx.Done() and the
// loop's Done channel so the unbuffered send can never block forever.
func (s *AgentSession) Deny(ctx context.Context, callID uuid.UUID) error {
	id, err := s.newCommandID()
	if err != nil {
		return err
	}
	return s.routeCommand(ctx, command.DenyToolCall{Header: command.Header{ID: id}, CallID: callID})
}

// ProvideUserInput supplies the user's answer to the pending AskUser request
// identified by callID. Like the approve/deny pair it is fire-and-route with no
// Ack: the actor routes it to the parked user-input gate, which delivers answer
// to the waiting tool. The select covers ctx.Done() and the loop's Done channel
// so the unbuffered send can never block forever.
func (s *AgentSession) ProvideUserInput(ctx context.Context, callID uuid.UUID, answer string) error {
	id, err := s.newCommandID()
	if err != nil {
		return err
	}
	return s.routeCommand(ctx, command.ProvideUserInput{Header: command.Header{ID: id}, CallID: callID, Answer: answer})
}

// routeCommand sends a fire-and-route gate command to the actor. These commands
// carry no Ack, so routeCommand returns nil as soon as the send completes and
// never waits for a reply. It selects on ctx.Done() and the loop's Done channel
// alongside the unbuffered send so the call can never block forever when the
// actor is busy (ctx times out) or has already exited (Done is closed).
func (s *AgentSession) routeCommand(ctx context.Context, cmd command.Command) error {
	l, ok := s.loopFor(s.primaryLoopID)
	if !ok {
		return &SessionError{Kind: SessionLoopNotFound}
	}
	select {
	case l.Commands <- cmd:
		return nil
	case <-l.Done:
		return &SessionError{Kind: SessionLoopExited}
	case <-ctx.Done():
		return &SessionError{Kind: SessionContextDone, Cause: ctx.Err()}
	}
}
