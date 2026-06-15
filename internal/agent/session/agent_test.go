package session

import (
	"context"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/inventivepotter/urvi/internal/agent/loop"
	"github.com/inventivepotter/urvi/internal/agent/loop/command"
	"github.com/inventivepotter/urvi/internal/agent/loop/event"
	"github.com/inventivepotter/urvi/internal/content"
	"github.com/inventivepotter/urvi/internal/llm"
	"github.com/inventivepotter/urvi/internal/tool"
	"github.com/inventivepotter/urvi/internal/uuid"
)

// stubLLM is a controllable llm.LLM for session tests.
type stubLLM struct {
	chunks           []content.Chunk
	blockUntilCancel bool
	ignoreCtx        bool // with blockUntilCancel: block forever (provider ignores ctx)
}

func textChunk(s string) content.Chunk {
	return &content.TextChunk{Text: s}
}

func (s *stubLLM) Invoke(ctx context.Context, req llm.Request) (*llm.Response, error) {
	return nil, errors.New("stubLLM.Invoke not used")
}
func (s *stubLLM) Stream(ctx context.Context, req llm.Request) (*llm.StreamReader[content.Chunk], error) {
	i := 0
	next := func() (content.Chunk, error) {
		if i < len(s.chunks) {
			c := s.chunks[i]
			i++
			return c, nil
		}
		if s.blockUntilCancel {
			if s.ignoreCtx {
				select {} // provider ignores cancellation; only safe under a bounded test
			}
			<-ctx.Done()
			return nil, ctx.Err()
		}
		return nil, io.EOF
	}
	return llm.NewStreamReader(next, nil), nil
}

// recordingSink captures every event envelope for assertions. It is non-blocking
// and safe for concurrent calls, as the EventSink contract requires.
type recordingSink struct {
	mu   sync.Mutex
	envs []event.EventEnvelope
}

func (r *recordingSink) OnEvent(_ context.Context, env event.EventEnvelope) {
	r.mu.Lock()
	r.envs = append(r.envs, env)
	r.mu.Unlock()
}

func (r *recordingSink) sawTerminal() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, env := range r.envs {
		switch env.Event.(type) {
		case event.TurnInterrupted:
			return true
		}
	}
	return false
}

// turnCausationID returns the CausationID stamped on the first turn-level
// envelope (skipping the session-level SessionStarted, which has none). The
// loop sets envelope CausationID to the issuing StartTurn's Header.ID, so a
// non-zero value here proves the session stamped a fresh Header.ID on the command.
func (r *recordingSink) turnCausationID() (uuid.UUID, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, env := range r.envs {
		if _, ok := env.Event.(event.SessionStarted); ok {
			continue
		}
		return env.CausationID, true
	}
	return uuid.UUID{}, false
}

func cfg(client llm.LLM) loop.Config {
	return loop.Config{Client: client, Model: llm.ModelSpec{Model: "m"}, DrainTimeout: 100 * time.Millisecond}
}

func cfgWithSink(client llm.LLM, sink event.EventSink) loop.Config {
	c := cfg(client)
	c.Sinks = []event.EventSink{sink}
	return c
}

func TestNewAgent(t *testing.T) {
	t.Parallel()
	t.Run("non-zero SessionID", func(t *testing.T) {
		t.Parallel()
		s, err := NewAgent(context.Background(), cfg(&stubLLM{chunks: []content.Chunk{textChunk("x")}}))
		if err != nil {
			t.Fatalf("NewAgent: %v", err)
		}
		t.Cleanup(func() { _ = s.Shutdown(context.Background()) })
		var zero [16]byte
		if s.SessionID == zero {
			t.Error("SessionID is zero")
		}
	})
	t.Run("ctx cancelled", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, err := NewAgent(ctx, cfg(&stubLLM{}))
		var se *SessionError
		if !errors.As(err, &se) || se.Kind != SessionContextDone {
			t.Fatalf("err = %v, want *SessionError{SessionContextDone}", err)
		}
	})
}

// capturingIDGen records every ID it mints so a test can assert the session
// stamped a non-zero Header.ID onto the command it sent (even for commands —
// Interrupt, Shutdown — whose ID has no observable runtime surface).
type capturingIDGen struct {
	mu  sync.Mutex
	ids []uuid.UUID
}

func (g *capturingIDGen) gen() (uuid.UUID, error) {
	id, err := uuid.New()
	if err != nil {
		return uuid.UUID{}, err
	}
	g.mu.Lock()
	g.ids = append(g.ids, id)
	g.mu.Unlock()
	return id, nil
}

func (g *capturingIDGen) last() (uuid.UUID, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.ids) == 0 {
		return uuid.UUID{}, false
	}
	return g.ids[len(g.ids)-1], true
}

// TestStampsCommandHeaderID asserts every command-sending method stamps a
// fresh, non-zero Header.ID on the command it sends. Each method mints the ID
// through the session's idGenerator seam, so a non-zero captured value proves
// the stamp. For Invoke and Stream the loop also copies the command's Header.ID
// onto each turn envelope's CausationID, so the sink-observed CausationID must
// equal the captured ID — an end-to-end check that the stamp reaches the loop.
func TestStampsCommandHeaderID(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		call        func(t *testing.T, s *AgentSession)
		observeable bool // true when the stamped ID surfaces via sink CausationID
	}{
		{
			name: "Invoke",
			call: func(t *testing.T, s *AgentSession) {
				if _, err := s.Invoke(context.Background(), nil); err != nil {
					t.Fatalf("Invoke: %v", err)
				}
			},
			observeable: true,
		},
		{
			name: "Stream",
			call: func(t *testing.T, s *AgentSession) {
				sr, err := s.Stream(context.Background(), nil)
				if err != nil {
					t.Fatalf("Stream: %v", err)
				}
				for {
					if _, err := sr.Next(); err == io.EOF {
						break
					} else if err != nil {
						t.Fatalf("Next: %v", err)
					}
				}
				_ = sr.Close()
			},
			observeable: true,
		},
		{
			name: "Interrupt",
			call: func(t *testing.T, s *AgentSession) {
				if _, err := s.Interrupt(context.Background()); err != nil {
					t.Fatalf("Interrupt: %v", err)
				}
			},
		},
		{
			name: "Shutdown",
			call: func(t *testing.T, s *AgentSession) {
				if err := s.Shutdown(context.Background()); err != nil {
					t.Fatalf("Shutdown: %v", err)
				}
			},
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			sink := &recordingSink{}
			s, err := NewAgent(context.Background(), cfgWithSink(&stubLLM{chunks: []content.Chunk{textChunk("hi")}}, sink))
			if err != nil {
				t.Fatalf("NewAgent: %v", err)
			}
			gen := &capturingIDGen{}
			s.newID = gen.gen
			t.Cleanup(func() { _ = s.Shutdown(context.Background()) })

			tt.call(t, s)

			minted, ok := gen.last()
			if !ok {
				t.Fatal("session minted no command-Header ID")
			}
			if minted.IsZero() {
				t.Fatal("session stamped a zero Header.ID on the command")
			}
			if tt.observeable {
				cid, ok := sink.turnCausationID()
				if !ok {
					t.Fatal("no turn-level envelope captured")
				}
				if cid != minted {
					t.Fatalf("envelope CausationID = %v, want stamped Header.ID %v", cid, minted)
				}
			}
		})
	}
}

// TestNewCommandIDGenerationFailure covers the crypto/rand failure branch: when
// the session's idGenerator fails, every command-sending method must fail secure
// with *SessionError{SessionIDGenerationFailed} and send no command (no
// unidentifiable, zero-ID command ever leaves the session).
func TestNewCommandIDGenerationFailure(t *testing.T) {
	t.Parallel()
	genErr := errors.New("rand source exhausted")
	failingGen := func() (uuid.UUID, error) { return uuid.UUID{}, genErr }

	tests := []struct {
		name string
		call func(s *AgentSession) error
	}{
		{name: "Invoke", call: func(s *AgentSession) error { _, err := s.Invoke(context.Background(), nil); return err }},
		{name: "Stream", call: func(s *AgentSession) error { _, err := s.Stream(context.Background(), nil); return err }},
		{name: "Interrupt", call: func(s *AgentSession) error { _, err := s.Interrupt(context.Background()); return err }},
		{name: "Shutdown", call: func(s *AgentSession) error { return s.Shutdown(context.Background()) }},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			s, err := NewAgent(context.Background(), cfg(&stubLLM{chunks: []content.Chunk{textChunk("x")}}))
			if err != nil {
				t.Fatalf("NewAgent: %v", err)
			}
			// Restore a working generator before cleanup so the cleanup Shutdown can
			// mint its own command ID and actually stop the actor (no leaked loop).
			t.Cleanup(func() { s.newID = uuid.New; _ = s.Shutdown(context.Background()) })
			s.newID = failingGen

			err = tt.call(s)
			var se *SessionError
			if !errors.As(err, &se) || se.Kind != SessionIDGenerationFailed {
				t.Fatalf("err = %v, want *SessionError{SessionIDGenerationFailed}", err)
			}
			if !errors.Is(err, genErr) {
				t.Fatalf("err = %v, want it to wrap the generator error", err)
			}
		})
	}
}

func TestInvokeReturnsTurnDone(t *testing.T) {
	t.Parallel()
	s, err := NewAgent(context.Background(), cfg(&stubLLM{chunks: []content.Chunk{textChunk("hello")}}))
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}
	t.Cleanup(func() { _ = s.Shutdown(context.Background()) })
	ev, err := s.Invoke(context.Background(), nil)
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if _, ok := ev.(event.TurnDone); !ok {
		t.Fatalf("event = %T, want event.TurnDone", ev)
	}
}

func TestInvokeCtxCancelReturnsInterrupted(t *testing.T) {
	t.Parallel()
	s, err := NewAgent(context.Background(), cfg(&stubLLM{blockUntilCancel: true}))
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}
	t.Cleanup(func() { _ = s.Shutdown(context.Background()) })
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(20 * time.Millisecond); cancel() }()
	ev, err := s.Invoke(ctx, nil)
	if err != nil {
		t.Fatalf("Invoke returned Go error %v, want TurnInterrupted event", err)
	}
	if _, ok := ev.(event.TurnInterrupted); !ok {
		t.Fatalf("event = %T, want event.TurnInterrupted", ev)
	}
}

func TestStreamYieldsOrderedEvents(t *testing.T) {
	t.Parallel()
	s, err := NewAgent(context.Background(), cfg(&stubLLM{chunks: []content.Chunk{textChunk("a"), textChunk("b")}}))
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}
	t.Cleanup(func() { _ = s.Shutdown(context.Background()) })
	sr, err := s.Stream(context.Background(), nil)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	var got []event.Event
	for {
		ev, err := sr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		got = append(got, ev)
	}
	if len(got) < 3 {
		t.Fatalf("got %d events, want >=3 (TurnStarted, deltas, terminal)", len(got))
	}
	if _, ok := got[0].(event.TurnStarted); !ok {
		t.Errorf("first = %T, want TurnStarted", got[0])
	}
	if _, ok := got[len(got)-1].(event.TurnDone); !ok {
		t.Errorf("last = %T, want TurnDone", got[len(got)-1])
	}
}

func TestConcurrentInvokeIsRejected(t *testing.T) {
	t.Parallel()
	s, err := NewAgent(context.Background(), cfg(&stubLLM{blockUntilCancel: true}))
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}
	t.Cleanup(func() { _ = s.Shutdown(context.Background()) })

	// first Invoke blocks (provider blocks); run it in the background
	ctx1, cancel1 := context.WithCancel(context.Background())
	defer cancel1()
	started := make(chan struct{})
	go func() { close(started); _, _ = s.Invoke(ctx1, nil) }()
	<-started
	time.Sleep(30 * time.Millisecond) // let the first turn occupy the loop

	_, err = s.Invoke(context.Background(), nil)
	var be *command.TurnBusyError
	if !errors.As(err, &be) {
		t.Fatalf("second Invoke err = %v, want *command.TurnBusyError", err)
	}
}

func TestShutdownThenMethodsExit(t *testing.T) {
	t.Parallel()
	s, err := NewAgent(context.Background(), cfg(&stubLLM{chunks: []content.Chunk{textChunk("x")}}))
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}
	if err := s.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	// second shutdown is a no-op
	if err := s.Shutdown(context.Background()); err != nil {
		t.Fatalf("second Shutdown: %v", err)
	}
	// methods after shutdown return SessionLoopExited, no deadlock
	_, err = s.Invoke(context.Background(), nil)
	var se *SessionError
	if !errors.As(err, &se) || se.Kind != SessionLoopExited {
		t.Fatalf("Invoke after shutdown err = %v, want *SessionError{SessionLoopExited}", err)
	}
}

// REGRESSION GUARD (review fix #4): a Stream reader parked in Next() unblocks
// when the loop is hard-killed (root ctx cancelled), instead of hanging forever.
func TestStreamReaderUnblocksOnLoopDeath(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	s, err := NewAgent(ctx, cfg(&stubLLM{blockUntilCancel: true}))
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}
	sr, err := s.Stream(context.Background(), nil)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	// drain the initial TurnStarted so we're parked waiting for more
	_, _ = sr.Next()

	cancel() // hard-kill the loop via its root ctx

	done := make(chan error, 1)
	go func() {
		for {
			_, err := sr.Next()
			if err != nil {
				done <- err
				return
			}
		}
	}()
	select {
	case err := <-done:
		if err != io.EOF {
			t.Fatalf("Next after loop death = %v, want io.EOF", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Stream reader hung after loop death (fix #4 missing)")
	}
}

// TestInvokeUnblocksOnLoopDeath guards the Invoke counterpart of fix #4: a caller
// parked draining events must not hang forever when the loop is hard-killed and
// the (ctx-ignoring) provider keeps the detached turn goroutine alive, so the
// actor returns without closing the events channel.
func TestInvokeUnblocksOnLoopDeath(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	s, err := NewAgent(ctx, cfg(&stubLLM{blockUntilCancel: true, ignoreCtx: true}))
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}
	errCh := make(chan error, 1)
	go func() {
		_, err := s.Invoke(context.Background(), nil)
		errCh <- err
	}()
	time.Sleep(40 * time.Millisecond) // ensure Invoke is parked draining events

	cancel() // hard-kill the loop

	select {
	case err := <-errCh:
		var se *SessionError
		if !errors.As(err, &se) || se.Kind != SessionLoopExited {
			t.Fatalf("Invoke after loop death = %v, want *SessionError{SessionLoopExited}", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Invoke hung after loop death (missing loop.Done escape)")
	}
}

// TestStreamCloseCancelsTurn: closing the stream reader abandons the event
// stream and cancels the running turn; the session is usable again afterward,
// and a sink observes the TurnInterrupted terminal event.
func TestStreamCloseCancelsTurn(t *testing.T) {
	t.Parallel()
	sink := &recordingSink{}
	s, err := NewAgent(context.Background(), cfgWithSink(&stubLLM{blockUntilCancel: true}, sink))
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}
	t.Cleanup(func() { _ = s.Shutdown(context.Background()) })

	sr, err := s.Stream(context.Background(), nil)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	// drain TurnStarted so the turn is actively running and parked in the provider
	if _, err := sr.Next(); err != nil {
		t.Fatalf("first Next: %v", err)
	}

	if err := sr.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// give the actor a moment to process the abandon + cancel and publish the
	// interrupted terminal to the sink.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) && !sink.sawTerminal() {
		time.Sleep(5 * time.Millisecond)
	}
	if !sink.sawTerminal() {
		t.Error("sink did not observe TurnInterrupted after Close")
	}

	// session must be usable again: a subsequent Invoke is accepted by the loop
	// (not rejected with TurnBusyError). Because the loop's client blocks until
	// ctx cancel, drive the new turn with a short-timeout ctx; acceptance + a
	// TurnInterrupted terminal proves the session was released by Close.
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	ev, err := s.Invoke(ctx, nil)
	if err != nil {
		t.Fatalf("Invoke after Close: %v (session not released)", err)
	}
	if _, ok := ev.(event.TurnInterrupted); !ok {
		t.Fatalf("Invoke after Close returned %T, want event.TurnInterrupted", ev)
	}
}

// TestStreamDrainReleasesSession: reading a stream until EOF releases the
// session so a later Invoke succeeds; closing early also releases it.
func TestStreamDrainReleasesSession(t *testing.T) {
	t.Parallel()
	t.Run("drain to EOF releases", func(t *testing.T) {
		t.Parallel()
		s, err := NewAgent(context.Background(), cfg(&stubLLM{chunks: []content.Chunk{textChunk("a")}}))
		if err != nil {
			t.Fatalf("NewAgent: %v", err)
		}
		t.Cleanup(func() { _ = s.Shutdown(context.Background()) })

		sr, err := s.Stream(context.Background(), nil)
		if err != nil {
			t.Fatalf("Stream: %v", err)
		}
		for {
			_, err := sr.Next()
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Fatalf("Next: %v", err)
			}
		}
		_ = sr.Close()

		ev, err := s.Invoke(context.Background(), nil)
		if err != nil {
			t.Fatalf("Invoke after drain: %v", err)
		}
		if _, ok := ev.(event.TurnDone); !ok {
			t.Fatalf("event = %T, want event.TurnDone", ev)
		}
	})
	t.Run("close early releases", func(t *testing.T) {
		t.Parallel()
		s, err := NewAgent(context.Background(), cfg(&stubLLM{chunks: []content.Chunk{textChunk("a")}}))
		if err != nil {
			t.Fatalf("NewAgent: %v", err)
		}
		t.Cleanup(func() { _ = s.Shutdown(context.Background()) })

		sr, err := s.Stream(context.Background(), nil)
		if err != nil {
			t.Fatalf("Stream: %v", err)
		}
		// read just the first event then close without draining
		if _, err := sr.Next(); err != nil {
			t.Fatalf("first Next: %v", err)
		}
		if err := sr.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}

		// Close() only signals; the actor may still be draining the cancelled turn.
		// Poll the re-invoke until the session is released (or fail on deadline).
		deadline := time.After(2 * time.Second)
		for {
			ev, err := s.Invoke(context.Background(), nil)
			if err == nil {
				if _, ok := ev.(event.TurnDone); !ok {
					t.Fatalf("event = %T, want event.TurnDone", ev)
				}
				break
			}
			var be *command.TurnBusyError
			if !errors.As(err, &be) {
				t.Fatalf("Invoke after early close = %v, want nil or TurnBusyError", err)
			}
			select {
			case <-deadline:
				t.Fatal("session never released after early Close")
			case <-time.After(5 * time.Millisecond):
			}
		}
	})
}

// TestInterruptDuringInvoke: while Invoke blocks, Interrupt returns (true, nil)
// and the Invoke returns a TurnInterrupted event.
func TestInterruptDuringInvoke(t *testing.T) {
	t.Parallel()
	s, err := NewAgent(context.Background(), cfg(&stubLLM{blockUntilCancel: true}))
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}
	t.Cleanup(func() { _ = s.Shutdown(context.Background()) })

	evCh := make(chan event.Event, 1)
	errCh := make(chan error, 1)
	go func() {
		ev, err := s.Invoke(context.Background(), nil)
		evCh <- ev
		errCh <- err
	}()
	time.Sleep(30 * time.Millisecond) // let the turn occupy the loop

	cancelled, err := s.Interrupt(context.Background())
	if err != nil {
		t.Fatalf("Interrupt: %v", err)
	}
	if !cancelled {
		t.Fatal("Interrupt returned false, want true")
	}

	select {
	case ev := <-evCh:
		if err := <-errCh; err != nil {
			t.Fatalf("Invoke returned Go error %v, want TurnInterrupted event", err)
		}
		if _, ok := ev.(event.TurnInterrupted); !ok {
			t.Fatalf("Invoke event = %T, want event.TurnInterrupted", ev)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Invoke did not return after Interrupt")
	}
}

// TestInterruptCtxCancelledBeforeSend: a cancelled ctx makes Interrupt return
// (false, *SessionError{SessionContextDone}) before any command is sent.
func TestInterruptCtxCancelledBeforeSend(t *testing.T) {
	t.Parallel()
	s, err := NewAgent(context.Background(), cfg(&stubLLM{blockUntilCancel: true}))
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}
	t.Cleanup(func() { _ = s.Shutdown(context.Background()) })

	// occupy the loop so the unbuffered Commands send would block, forcing the
	// ctx.Done() branch to win deterministically.
	go func() { _, _ = s.Invoke(context.Background(), nil) }()
	time.Sleep(30 * time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	cancelled, err := s.Interrupt(ctx)
	if cancelled {
		t.Error("Interrupt returned true, want false")
	}
	var se *SessionError
	if !errors.As(err, &se) || se.Kind != SessionContextDone {
		t.Fatalf("err = %v, want *SessionError{SessionContextDone}", err)
	}
}

// TestShutdownCtxCancelledBeforeSend: a cancelled ctx makes Shutdown return
// *SessionError{SessionContextDone} before any command is sent.
func TestShutdownCtxCancelledBeforeSend(t *testing.T) {
	t.Parallel()
	s, err := NewAgent(context.Background(), cfg(&stubLLM{blockUntilCancel: true}))
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}
	t.Cleanup(func() { _ = s.Shutdown(context.Background()) })

	// occupy the loop so the Commands send blocks, forcing the ctx.Done() branch.
	go func() { _, _ = s.Invoke(context.Background(), nil) }()
	time.Sleep(30 * time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err = s.Shutdown(ctx)
	var se *SessionError
	if !errors.As(err, &se) || se.Kind != SessionContextDone {
		t.Fatalf("err = %v, want *SessionError{SessionContextDone}", err)
	}
}

// TestShutdownSurfacesLoopTerminatedError covers the spec-table case "Shutdown
// loop root ctx cancelled during shutdown → ack receives *LoopTerminatedError;
// session wraps to *SessionError". This IS deterministic through the session API:
// AgentSession.Shutdown parks in its final select before the kill, and the actor
// sends the LoopTerminatedError ack BEFORE closing Done, so the parked select
// wakes on the ack case while Done is still open — ack wins, not a race. (A
// ctx-ignoring provider is required so the turn never completes on cancelTurn,
// forcing the root-ctx-kill + DrainTimeout path that produces the typed error.)
func TestShutdownSurfacesLoopTerminatedError(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	s, err := NewAgent(ctx, cfg(&stubLLM{blockUntilCancel: true, ignoreCtx: true}))
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}
	// Occupy the loop with a turn that never completes (provider ignores ctx).
	go func() { _, _ = s.Invoke(context.Background(), nil) }()
	time.Sleep(40 * time.Millisecond)

	// Shutdown parks waiting for the (never-arriving) internal result.
	errCh := make(chan error, 1)
	go func() { errCh <- s.Shutdown(context.Background()) }()
	time.Sleep(40 * time.Millisecond) // ensure the Shutdown command reached the actor

	cancel() // root-ctx kill: after DrainTimeout the actor acks LoopTerminatedError then closes Done

	select {
	case err := <-errCh:
		var se *SessionError
		if !errors.As(err, &se) {
			t.Fatalf("Shutdown err = %v (%T), want *SessionError", err, err)
		}
		var lte *command.LoopTerminatedError
		if !errors.As(err, &lte) {
			t.Fatalf("Shutdown err = %v, want it to wrap *command.LoopTerminatedError", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Shutdown never returned after root-ctx kill")
	}
}

// sessionWithFakeLoop builds an AgentSession wired to a fake loop whose Commands
// channel the test reads from and whose Done channel the test controls. This is
// the seam for the fire-and-route gate commands (Approve/Deny/ProvideUserInput),
// which carry no Ack and so have no sink-observable effect through the real loop:
// reading the unbuffered Commands channel directly captures the exact command the
// session sent. cmds is unbuffered to mirror the real loop.Commands, so a send is
// observable only when the test (or a closed Done) is ready.
func sessionWithFakeLoop() (s *AgentSession, cmds chan command.Command, done chan struct{}) {
	cmds = make(chan command.Command)
	done = make(chan struct{})
	s = &AgentSession{
		SessionID: mustUUID(),
		loop:      &loop.Loop{Commands: cmds, Done: done},
		newID:     uuid.New,
	}
	return s, cmds, done
}

func mustUUID() uuid.UUID {
	id, err := uuid.New()
	if err != nil {
		panic(err)
	}
	return id
}

// TestGateCommandsSendCorrectCommand asserts each gate-answer method sends the
// correct command type to loop.Commands, stamped with a fresh non-zero Header.ID
// and the right CallID/Scope/Answer, and returns nil. The fake loop's Commands
// channel captures the exact command sent (these are fire-and-route — no Ack — so
// this is the only observable effect).
func TestGateCommandsSendCorrectCommand(t *testing.T) {
	t.Parallel()
	callID := mustUUID()
	tests := []struct {
		name   string
		call   func(s *AgentSession) error
		verify func(t *testing.T, cmd command.Command)
	}{
		{
			name: "Approve",
			call: func(s *AgentSession) error { return s.Approve(context.Background(), callID, tool.ScopeSession) },
			verify: func(t *testing.T, cmd command.Command) {
				c, ok := cmd.(command.ApproveToolCall)
				if !ok {
					t.Fatalf("sent %T, want command.ApproveToolCall", cmd)
				}
				if c.CallID != callID {
					t.Errorf("CallID = %v, want %v", c.CallID, callID)
				}
				if c.Scope != tool.ScopeSession {
					t.Errorf("Scope = %v, want %v", c.Scope, tool.ScopeSession)
				}
				if c.CommandHeader().ID.IsZero() {
					t.Error("Header.ID is zero, want a fresh non-zero id")
				}
			},
		},
		{
			name: "Deny",
			call: func(s *AgentSession) error { return s.Deny(context.Background(), callID) },
			verify: func(t *testing.T, cmd command.Command) {
				c, ok := cmd.(command.DenyToolCall)
				if !ok {
					t.Fatalf("sent %T, want command.DenyToolCall", cmd)
				}
				if c.CallID != callID {
					t.Errorf("CallID = %v, want %v", c.CallID, callID)
				}
				if c.CommandHeader().ID.IsZero() {
					t.Error("Header.ID is zero, want a fresh non-zero id")
				}
			},
		},
		{
			name: "ProvideUserInput",
			call: func(s *AgentSession) error { return s.ProvideUserInput(context.Background(), callID, "the answer") },
			verify: func(t *testing.T, cmd command.Command) {
				c, ok := cmd.(command.ProvideUserInput)
				if !ok {
					t.Fatalf("sent %T, want command.ProvideUserInput", cmd)
				}
				if c.CallID != callID {
					t.Errorf("CallID = %v, want %v", c.CallID, callID)
				}
				if c.Answer != "the answer" {
					t.Errorf("Answer = %q, want %q", c.Answer, "the answer")
				}
				if c.CommandHeader().ID.IsZero() {
					t.Error("Header.ID is zero, want a fresh non-zero id")
				}
			},
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			s, cmds, _ := sessionWithFakeLoop()

			errCh := make(chan error, 1)
			go func() { errCh <- tt.call(s) }()

			select {
			case cmd := <-cmds:
				tt.verify(t, cmd)
			case <-time.After(2 * time.Second):
				t.Fatal("method never sent a command")
			}

			select {
			case err := <-errCh:
				if err != nil {
					t.Fatalf("method returned %v, want nil", err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("method never returned after send")
			}
		})
	}
}

// TestGateCommandsFreshHeaderIDPerCall asserts each method mints a distinct
// Header.ID on every invocation (fresh per command, not reused).
func TestGateCommandsFreshHeaderIDPerCall(t *testing.T) {
	t.Parallel()
	callID := mustUUID()
	tests := []struct {
		name string
		call func(s *AgentSession) error
	}{
		{name: "Approve", call: func(s *AgentSession) error { return s.Approve(context.Background(), callID, tool.ScopeOnce) }},
		{name: "Deny", call: func(s *AgentSession) error { return s.Deny(context.Background(), callID) }},
		{name: "ProvideUserInput", call: func(s *AgentSession) error { return s.ProvideUserInput(context.Background(), callID, "x") }},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			s, cmds, _ := sessionWithFakeLoop()

			ids := make([]uuid.UUID, 0, 2)
			for i := 0; i < 2; i++ {
				errCh := make(chan error, 1)
				go func() { errCh <- tt.call(s) }()
				select {
				case cmd := <-cmds:
					ids = append(ids, cmd.CommandHeader().ID)
				case <-time.After(2 * time.Second):
					t.Fatal("method never sent a command")
				}
				if err := <-errCh; err != nil {
					t.Fatalf("method returned %v, want nil", err)
				}
			}
			if ids[0] == ids[1] {
				t.Fatalf("two calls reused Header.ID %v, want fresh ids", ids[0])
			}
		})
	}
}

// TestGateCommandsCtxCancelled: a cancelled ctx makes each method return
// *SessionError{SessionContextDone} without blocking and without sending a
// command (the fake loop's Commands channel is never read).
func TestGateCommandsCtxCancelled(t *testing.T) {
	t.Parallel()
	callID := mustUUID()
	tests := []struct {
		name string
		call func(s *AgentSession, ctx context.Context) error
	}{
		{name: "Approve", call: func(s *AgentSession, ctx context.Context) error { return s.Approve(ctx, callID, tool.ScopeOnce) }},
		{name: "Deny", call: func(s *AgentSession, ctx context.Context) error { return s.Deny(ctx, callID) }},
		{name: "ProvideUserInput", call: func(s *AgentSession, ctx context.Context) error { return s.ProvideUserInput(ctx, callID, "x") }},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			s, _, _ := sessionWithFakeLoop() // Commands never read: a send would block

			ctx, cancel := context.WithCancel(context.Background())
			cancel()

			errCh := make(chan error, 1)
			go func() { errCh <- tt.call(s, ctx) }()

			select {
			case err := <-errCh:
				var se *SessionError
				if !errors.As(err, &se) || se.Kind != SessionContextDone {
					t.Fatalf("err = %v, want *SessionError{SessionContextDone}", err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("method blocked on a cancelled ctx (no ctx.Done() escape)")
			}
		})
	}
}

// TestGateCommandsLoopExited: after the loop's Done channel is closed, each method
// returns *SessionError{SessionLoopExited} without blocking and without sending.
func TestGateCommandsLoopExited(t *testing.T) {
	t.Parallel()
	callID := mustUUID()
	tests := []struct {
		name string
		call func(s *AgentSession) error
	}{
		{name: "Approve", call: func(s *AgentSession) error { return s.Approve(context.Background(), callID, tool.ScopeOnce) }},
		{name: "Deny", call: func(s *AgentSession) error { return s.Deny(context.Background(), callID) }},
		{name: "ProvideUserInput", call: func(s *AgentSession) error { return s.ProvideUserInput(context.Background(), callID, "x") }},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			s, _, done := sessionWithFakeLoop() // Commands never read
			close(done)                         // loop has exited

			errCh := make(chan error, 1)
			go func() { errCh <- tt.call(s) }()

			select {
			case err := <-errCh:
				var se *SessionError
				if !errors.As(err, &se) || se.Kind != SessionLoopExited {
					t.Fatalf("err = %v, want *SessionError{SessionLoopExited}", err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("method blocked after loop exited (no loop.Done escape)")
			}
		})
	}
}

// TestGateCommandsIDGenerationFailure: when the idGenerator fails, each method
// fails secure with *SessionError{SessionIDGenerationFailed} and sends no command.
func TestGateCommandsIDGenerationFailure(t *testing.T) {
	t.Parallel()
	genErr := errors.New("rand source exhausted")
	callID := mustUUID()
	tests := []struct {
		name string
		call func(s *AgentSession) error
	}{
		{name: "Approve", call: func(s *AgentSession) error { return s.Approve(context.Background(), callID, tool.ScopeOnce) }},
		{name: "Deny", call: func(s *AgentSession) error { return s.Deny(context.Background(), callID) }},
		{name: "ProvideUserInput", call: func(s *AgentSession) error { return s.ProvideUserInput(context.Background(), callID, "x") }},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			s, _, _ := sessionWithFakeLoop() // Commands never read: a send would block
			s.newID = func() (uuid.UUID, error) { return uuid.UUID{}, genErr }

			errCh := make(chan error, 1)
			go func() { errCh <- tt.call(s) }()

			select {
			case err := <-errCh:
				var se *SessionError
				if !errors.As(err, &se) || se.Kind != SessionIDGenerationFailed {
					t.Fatalf("err = %v, want *SessionError{SessionIDGenerationFailed}", err)
				}
				if !errors.Is(err, genErr) {
					t.Fatalf("err = %v, want it to wrap the generator error", err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("method blocked on id-generation failure (should fail before send)")
			}
		})
	}
}
