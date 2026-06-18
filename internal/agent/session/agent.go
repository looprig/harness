package session

import (
	"context"
	"io"
	"sync"

	"github.com/inventivepotter/urvi/internal/agent/loop"
	"github.com/inventivepotter/urvi/internal/agent/loop/command"
	"github.com/inventivepotter/urvi/internal/agent/loop/event"
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

// idGenerator mints a fresh UUID. It defaults to uuid.New; tests inject a
// failing generator to exercise the crypto/rand failure branch.
type idGenerator func() (uuid.UUID, error)

type AgentSession struct {
	// SessionID is shared by every loop participating in this session.
	SessionID uuid.UUID

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

// PublishEvent is the session's eventPublisher implementation passed to
// loop.New. Phase 3 is a no-op STUB: the loop stores the publisher but does not
// yet call it (event-publication wiring, the real session fan-in / hub, is
// Phase 4). The method exists so *AgentSession satisfies loop's eventPublisher
// interface and the loop.New(..., s, ...) wiring stays stable.
func (s *AgentSession) PublishEvent(_ context.Context, _ event.Event) error { return nil }

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

// NewAgent constructs an AgentSession and starts its actor goroutine.
// The actor publishes SessionStarted to sinks before entering its command loop.
// Because Commands is an unbuffered channel, the first call to Invoke, Stream,
// Interrupt, or Shutdown is guaranteed to observe SessionStarted in sinks — the
// unbuffered send cannot complete until the actor is in its select loop, which
// is entered only after SessionStarted is published.
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
		sessionCtx:    sessionCtx,
		sessionCancel: sessionCancel,
		loops:         make(map[uuid.UUID]*loopHandle),
		newID:         uuid.New,
	}

	primaryLoopID, err := s.NewLoop(loop.Provenance{}, cfg)
	if err != nil {
		sessionCancel()
		return nil, err
	}
	s.primaryLoopID = primaryLoopID
	return s, nil
}

// Invoke sends input and blocks until a terminal event.
// Cancelling ctx cancels the running turn; Invoke returns the event.TurnInterrupted event.
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
	ack := make(chan error, 1)
	abandoned := make(chan struct{})
	defer close(abandoned) // ensures deliverAndClose always has an escape if Invoke exits early

	select {
	// User-initiated turn: CausationID is zero (root).
	case l.Commands <- command.StartTurn{Header: command.Header{ID: id}, Ctx: ctx, Input: input, Events: events, Abandoned: abandoned, Ack: ack}:
	case <-ctx.Done():
		return nil, &SessionError{Kind: SessionContextDone, Cause: ctx.Err()}
	case <-l.Done:
		return nil, &SessionError{Kind: SessionLoopExited}
	}

	if err := <-ack; err != nil {
		return nil, err
	}

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
		case <-l.Done:
			// Hard loop kill: on a DrainTimeout detach the actor never closes
			// `events`, so without this escape Invoke would block forever. The
			// loop is gone, so no terminal can arrive.
			return nil, &SessionError{Kind: SessionLoopExited}
		}
	}
}

// Stream sends input and returns a StreamReader[event.Event] that yields
// TurnStarted, TokenDelta×N, then one terminal event, then EOF while the caller
// keeps reading. Calling sr.Close() abandons the event stream and cancels the turn.
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
	streamCtx, streamCancel := context.WithCancel(ctx)
	abandoned := make(chan struct{})
	var abandonOnce sync.Once
	events := make(chan event.Event, 64)
	ack := make(chan error, 1)

	select {
	// User-initiated turn: CausationID is zero (root).
	case l.Commands <- command.StartTurn{
		Header:    command.Header{ID: id},
		Ctx:       streamCtx,
		Input:     input,
		Events:    events,
		Abandoned: abandoned,
		Ack:       ack,
	}:
	case <-ctx.Done():
		streamCancel()
		abandonOnce.Do(func() { close(abandoned) })
		return nil, &SessionError{Kind: SessionContextDone, Cause: ctx.Err()}
	case <-l.Done:
		streamCancel()
		abandonOnce.Do(func() { close(abandoned) })
		return nil, &SessionError{Kind: SessionLoopExited}
	}

	if err := <-ack; err != nil {
		streamCancel()
		abandonOnce.Do(func() { close(abandoned) })
		return nil, err
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
			streamCancel()
			abandonOnce.Do(func() { close(abandoned) })
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

// Shutdown cancels any running turn and blocks until the actor exits.
// Calling Shutdown after the actor has exited is a no-op.
func (s *AgentSession) Shutdown(ctx context.Context) error {
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
