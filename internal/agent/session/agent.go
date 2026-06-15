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
	SessionIDGenerationFailed SessionErrorKind = "id_generation_failed"
	SessionLoopExited         SessionErrorKind = "loop_exited"
	SessionEventChannelClosed SessionErrorKind = "event_channel_closed"
	SessionContextDone        SessionErrorKind = "context_done"
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
	case SessionLoopExited:
		msg = "session: loop exited"
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
	SessionID uuid.UUID
	loop      *loop.Loop
	newID     idGenerator // mints command-Header IDs; defaults to uuid.New
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
	l, err := loop.New(ctx, id, cfg)
	if err != nil {
		return nil, err
	}
	return &AgentSession{SessionID: id, loop: l, newID: uuid.New}, nil
}

// Invoke sends input and blocks until a terminal event.
// Cancelling ctx cancels the running turn; Invoke returns the event.TurnInterrupted event.
func (s *AgentSession) Invoke(ctx context.Context, input []content.Block) (event.Event, error) {
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
	case s.loop.Commands <- command.StartTurn{Header: command.Header{ID: id}, Ctx: ctx, Input: input, Events: events, Abandoned: abandoned, Ack: ack}:
	case <-ctx.Done():
		return nil, &SessionError{Kind: SessionContextDone, Cause: ctx.Err()}
	case <-s.loop.Done:
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
		case <-s.loop.Done:
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
	case s.loop.Commands <- command.StartTurn{
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
	case <-s.loop.Done:
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
			case <-s.loop.Done:
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
	id, err := s.newCommandID()
	if err != nil {
		return false, err
	}
	ack := make(chan bool, 1)
	select {
	case s.loop.Commands <- command.Interrupt{Header: command.Header{ID: id}, Ack: ack}:
	case <-s.loop.Done:
		return false, &SessionError{Kind: SessionLoopExited}
	case <-ctx.Done():
		return false, &SessionError{Kind: SessionContextDone, Cause: ctx.Err()}
	}

	select {
	case cancelled := <-ack:
		return cancelled, nil
	case <-s.loop.Done:
		return false, &SessionError{Kind: SessionLoopExited}
	case <-ctx.Done():
		return false, &SessionError{Kind: SessionContextDone, Cause: ctx.Err()}
	}
}

// Shutdown cancels any running turn and blocks until the actor exits.
// Calling Shutdown after the actor has exited is a no-op.
func (s *AgentSession) Shutdown(ctx context.Context) error {
	id, err := s.newCommandID()
	if err != nil {
		return err
	}
	ack := make(chan error, 1)
	select {
	case s.loop.Commands <- command.Shutdown{Header: command.Header{ID: id}, Ack: ack}:
	case <-s.loop.Done:
		return nil
	case <-ctx.Done():
		return &SessionError{Kind: SessionContextDone, Cause: ctx.Err()}
	}

	select {
	case err := <-ack:
		// err is non-nil when the loop's root context was cancelled before
		// the actor finished cleanup. Wrap it so callers always receive a
		// typed *SessionError rather than a raw context error.
		if err != nil {
			return &SessionError{Kind: SessionContextDone, Cause: err}
		}
		return nil
	case <-s.loop.Done:
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
	select {
	case s.loop.Commands <- cmd:
		return nil
	case <-s.loop.Done:
		return &SessionError{Kind: SessionLoopExited}
	case <-ctx.Done():
		return &SessionError{Kind: SessionContextDone, Cause: ctx.Err()}
	}
}
