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

// recordingSub drains a hub Subscription — the same consumer API the TUI/CLI use
// — into a mutex-guarded slice so a test can assert on the full-fidelity events
// the session fan-in delivered. A goroutine owns the receive; record() and the
// accessors are safe for concurrent use. The drain loop exits when Events() is
// closed (the subscription's Close, a hub-forced loss, or session teardown).
type recordingSub struct {
	mu     sync.Mutex
	events []event.Event
}

// observe subscribes to s for the loop(s) under test and starts draining. The
// filter mirrors the real single-loop consumer (tui.DefaultEventFilter): live
// Ephemeral events from the primary loop, and Enduring events (StepDone, gates,
// terminals — including TurnStarted/TurnInterrupted) from every loop. The
// returned Subscription must be Closed by the caller (t.Cleanup). The
// subscription is created AFTER NewAgent, so it never sees the construction-time
// SessionStarted (the hub has no replay) — tests must not assert on it.
func observe(t *testing.T, s *Sesssion) (*recordingSub, event.Subscription) {
	t.Helper()
	sub, err := s.SubscribeEvents(event.EventFilter{
		Ephemeral: event.LoopScope{Loops: map[uuid.UUID]struct{}{s.primaryLoopID: {}}},
		Enduring:  event.LoopScope{All: true},
	})
	if err != nil {
		t.Fatalf("SubscribeEvents: %v", err)
	}
	rec := &recordingSub{}
	go func() {
		for ev := range sub.Events() {
			rec.record(ev)
		}
	}()
	return rec, sub
}

func (r *recordingSub) record(ev event.Event) {
	r.mu.Lock()
	r.events = append(r.events, ev)
	r.mu.Unlock()
}

func (r *recordingSub) sawTerminal() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, ev := range r.events {
		if _, ok := ev.(event.TurnInterrupted); ok {
			return true
		}
	}
	return false
}

// turnCausationID returns the CausationID stamped on the first turn-level event
// (a loop-scoped event; session-scoped events carry none). The loop stamps a
// turn event's CausationID with the issuing UserInput's Header.ID, so a non-zero
// value here proves the session stamped a fresh Header.ID on the command.
func (r *recordingSub) turnCausationID() (uuid.UUID, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, ev := range r.events {
		if ev.Scope() == event.ScopeSession {
			continue
		}
		return ev.EventHeader().CausationID, true
	}
	return uuid.UUID{}, false
}

// waitTurnCausationID polls turnCausationID until a turn-level event has been
// drained or the deadline elapses. The drain runs in a goroutine, so an event
// published by the time a call returns may not yet be in the slice; this bridges
// that gap deterministically without sleeping a fixed duration.
func (r *recordingSub) waitTurnCausationID(d time.Duration) (uuid.UUID, bool) {
	deadline := time.Now().Add(d)
	for {
		if cid, ok := r.turnCausationID(); ok {
			return cid, true
		}
		if time.Now().After(deadline) {
			return uuid.UUID{}, false
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// waitTerminal polls sawTerminal until a TurnInterrupted has been drained or the
// deadline elapses.
func (r *recordingSub) waitTerminal(d time.Duration) bool {
	deadline := time.Now().Add(d)
	for {
		if r.sawTerminal() {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func cfg(client llm.LLM) loop.Config {
	return loop.Config{Client: client, Model: llm.ModelSpec{Model: "m"}, DrainTimeout: 100 * time.Millisecond}
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
	t.Run("exactly one loop indexed by primaryLoopID", func(t *testing.T) {
		t.Parallel()
		s, err := NewAgent(context.Background(), cfg(&stubLLM{chunks: []content.Chunk{textChunk("x")}}))
		if err != nil {
			t.Fatalf("NewAgent: %v", err)
		}
		t.Cleanup(func() { _ = s.Shutdown(context.Background()) })

		s.loopsMu.RLock()
		n := len(s.loops)
		h, ok := s.loops[s.primaryLoopID]
		s.loopsMu.RUnlock()

		if n != 1 {
			t.Fatalf("len(loops) = %d, want 1", n)
		}
		if !ok {
			t.Fatal("loops has no entry for primaryLoopID")
		}
		if s.primaryLoopID.IsZero() {
			t.Error("primaryLoopID is zero")
		}
		if h.loop == nil {
			t.Error("primary loopHandle.loop is nil")
		}
		// The primary loop has no parent (zero provenance).
		if h.parent != (loop.Provenance{}) {
			t.Errorf("primary loopHandle.parent = %+v, want zero Provenance", h.parent)
		}
		if h.cancel == nil {
			t.Error("primary loopHandle.cancel is nil")
		}
	})
}

// TestNewLoop covers NewLoop: it mints a fresh loop id via the session's idGen,
// derives the loopCtx from sessionCtx, and stores a loopHandle with the given
// parent provenance and a non-nil cancel.
func TestNewLoop(t *testing.T) {
	t.Parallel()
	parentLoop := mustUUID()
	parentTurn := mustUUID()
	tests := []struct {
		name   string
		parent loop.Provenance
	}{
		{name: "zero parent (primary-style)", parent: loop.Provenance{}},
		{name: "non-zero parent (subagent-style)", parent: loop.Provenance{LoopID: parentLoop, TurnID: parentTurn}},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			s, err := NewAgent(context.Background(), cfg(&stubLLM{chunks: []content.Chunk{textChunk("x")}}))
			if err != nil {
				t.Fatalf("NewAgent: %v", err)
			}
			t.Cleanup(func() { _ = s.Shutdown(context.Background()) })

			// Record which ids the session mints from here on, so we can assert the
			// returned loop id came from idGen.
			gen := &capturingIDGen{}
			s.newID = gen.gen

			loopID, err := s.NewLoop(tt.parent, cfg(&stubLLM{chunks: []content.Chunk{textChunk("y")}}))
			if err != nil {
				t.Fatalf("NewLoop: %v", err)
			}
			if loopID.IsZero() {
				t.Fatal("NewLoop returned a zero loop id")
			}
			minted, ok := gen.last()
			if !ok || minted != loopID {
				t.Fatalf("returned loop id %v was not the freshly minted id %v", loopID, minted)
			}
			if loopID == s.primaryLoopID {
				t.Fatal("NewLoop reused the primary loop id, want a distinct id")
			}

			s.loopsMu.RLock()
			h, ok := s.loops[loopID]
			s.loopsMu.RUnlock()
			if !ok {
				t.Fatal("NewLoop did not store the loop in the registry")
			}
			if h.loop == nil {
				t.Error("stored loopHandle.loop is nil")
			}
			if h.parent != tt.parent {
				t.Errorf("stored loopHandle.parent = %+v, want %+v", h.parent, tt.parent)
			}
			if h.cancel == nil {
				t.Fatal("stored loopHandle.cancel is nil")
			}

			// The loopCtx must be derived from sessionCtx: cancelling sessionCtx
			// (via sessionCancel) must hard-kill the new loop, closing its Done.
			s.sessionCancel()
			select {
			case <-h.loop.Done:
			case <-time.After(2 * time.Second):
				t.Fatal("new loop's Done did not close after sessionCancel; loopCtx not derived from sessionCtx")
			}
		})
	}
}

// TestNewLoopIDGenerationFailure: when idGen fails, NewLoop returns
// *SessionError{SessionLoopIDGenerationFailed} wrapping the generator error and
// registers no loop.
func TestNewLoopIDGenerationFailure(t *testing.T) {
	t.Parallel()
	genErr := errors.New("rand source exhausted")
	s, err := NewAgent(context.Background(), cfg(&stubLLM{chunks: []content.Chunk{textChunk("x")}}))
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}
	t.Cleanup(func() { s.newID = uuid.New; _ = s.Shutdown(context.Background()) })

	s.loopsMu.RLock()
	before := len(s.loops)
	s.loopsMu.RUnlock()

	s.newID = func() (uuid.UUID, error) { return uuid.UUID{}, genErr }

	_, err = s.NewLoop(loop.Provenance{}, cfg(&stubLLM{}))
	var se *SessionError
	if !errors.As(err, &se) || se.Kind != SessionLoopIDGenerationFailed {
		t.Fatalf("err = %v, want *SessionError{SessionLoopIDGenerationFailed}", err)
	}
	if !errors.Is(err, genErr) {
		t.Fatalf("err = %v, want it to wrap the generator error", err)
	}

	s.loopsMu.RLock()
	after := len(s.loops)
	s.loopsMu.RUnlock()
	if after != before {
		t.Fatalf("registry grew from %d to %d on idGen failure, want no new loop", before, after)
	}
}

// TestLoopFor: loopFor(primaryLoopID) resolves the primary loop; a random id
// misses.
func TestLoopFor(t *testing.T) {
	t.Parallel()
	s, err := NewAgent(context.Background(), cfg(&stubLLM{chunks: []content.Chunk{textChunk("x")}}))
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}
	t.Cleanup(func() { _ = s.Shutdown(context.Background()) })

	if l, ok := s.loopFor(s.primaryLoopID); !ok || l == nil {
		t.Fatalf("loopFor(primaryLoopID) = (%v, %v), want (non-nil, true)", l, ok)
	}
	if l, ok := s.loopFor(mustUUID()); ok || l != nil {
		t.Fatalf("loopFor(random) = (%v, %v), want (nil, false)", l, ok)
	}
	if l, ok := s.loopFor(uuid.UUID{}); ok || l != nil {
		t.Fatalf("loopFor(zero) = (%v, %v), want (nil, false)", l, ok)
	}
}

// TestRoutingMethodsLoopNotFound covers the SessionLoopNotFound branch shared by
// every routing method: when loopFor(primaryLoopID) misses (the registry has no
// entry for the primary id), the method must fail secure with
// *SessionError{SessionLoopNotFound} and send no command. The miss is forced by
// deleting the primary registry entry under loopsMu after construction, so the
// id stays set but resolves to nothing — the exact state the branch guards.
func TestRoutingMethodsLoopNotFound(t *testing.T) {
	t.Parallel()
	callID := mustUUID()
	tests := []struct {
		name string
		call func(s *Sesssion) error
	}{
		{name: "Invoke", call: func(s *Sesssion) error { _, err := s.Invoke(context.Background(), nil); return err }},
		{name: "Stream", call: func(s *Sesssion) error { _, err := s.Stream(context.Background(), nil); return err }},
		{name: "Interrupt", call: func(s *Sesssion) error { _, err := s.Interrupt(context.Background()); return err }},
		// Approve/Deny/ProvideUserInput route through routeCommand, which performs
		// the same loopFor(primaryLoopID) lookup behind the gate-answer methods.
		{name: "Approve", call: func(s *Sesssion) error { return s.Approve(context.Background(), callID, tool.ScopeOnce) }},
		{name: "Deny", call: func(s *Sesssion) error { return s.Deny(context.Background(), callID) }},
		{name: "ProvideUserInput", call: func(s *Sesssion) error { return s.ProvideUserInput(context.Background(), callID, "x") }},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			s, cmds, _ := sessionWithFakeLoop() // Commands never read: a send would block forever

			// Force loopFor(primaryLoopID) to miss by deleting the primary entry
			// while leaving primaryLoopID set. The routing method must short-circuit
			// before ever touching the (unread) Commands channel.
			s.loopsMu.Lock()
			delete(s.loops, s.primaryLoopID)
			s.loopsMu.Unlock()

			errCh := make(chan error, 1)
			go func() { errCh <- tt.call(s) }()

			select {
			case err := <-errCh:
				var se *SessionError
				if !errors.As(err, &se) || se.Kind != SessionLoopNotFound {
					t.Fatalf("err = %v, want *SessionError{SessionLoopNotFound}", err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("method blocked on a missing loop (no SessionLoopNotFound short-circuit)")
			}

			// No command may have been sent: the fake loop's Commands channel is
			// unbuffered and never read, so any send would have blocked the goroutine
			// above (which instead returned an error). A non-blocking receive must miss.
			select {
			case cmd := <-cmds:
				t.Fatalf("method sent %T on a missing-loop path, want no command", cmd)
			default:
			}
		})
	}
}

// TestNewLoopReturnsLoopNewError covers NewLoop's loop.New error path: when
// loop.New fails its own Config validation, NewLoop must (a) return that error
// unwrapped (a *loop.ConfigError, NOT a *SessionError — the id generation
// already succeeded), (b) leave the registry unmutated (no handle stored), and
// (c) cancel the derived loopCtx so the session leaks no context.
//
// The cheapest loop.New validation failure is a nil Client, which short-circuits
// to *loop.ConfigError{ConfigMissingClient} synchronously, before any goroutine
// or LLM is involved. Cancellation of the derived loopCtx is asserted
// structurally: NewLoop derives loopCtx from s.sessionCtx and, on the loop.New
// error, sessionCtx must still be live (NewLoop must cancel only the child, never
// the session). The child loopCtx is local to NewLoop and cannot be captured
// without changing production code, so the cancel-observation here is
// structural-only (the cancel() call sits on the asserted error path); the
// positive guard is that sessionCtx itself was NOT cancelled.
func TestNewLoopReturnsLoopNewError(t *testing.T) {
	t.Parallel()
	sessionCtx, sessionCancel := context.WithCancel(context.Background())
	t.Cleanup(sessionCancel)
	primaryLoopID := mustUUID()
	s := &Sesssion{
		SessionID:     mustUUID(),
		sessionCtx:    sessionCtx,
		sessionCancel: sessionCancel,
		loops:         map[uuid.UUID]*loopHandle{primaryLoopID: {}},
		primaryLoopID: primaryLoopID,
		newID:         uuid.New, // id mint succeeds; only loop.New must fail
	}

	s.loopsMu.RLock()
	before := len(s.loops)
	s.loopsMu.RUnlock()

	// loop.New rejects a nil Client with *ConfigError{ConfigMissingClient} before
	// starting any goroutine — the cheapest validation failure to inject.
	badCfg := loop.Config{Model: llm.ModelSpec{Model: "m"}}
	loopID, err := s.NewLoop(loop.Provenance{}, badCfg)

	// (a) the loop.New error is returned, unwrapped, not remapped to *SessionError.
	if err == nil {
		t.Fatal("NewLoop returned nil error, want loop.New's ConfigError")
	}
	if !loopID.IsZero() {
		t.Errorf("NewLoop returned loop id %v on error, want zero", loopID)
	}
	var ce *loop.ConfigError
	if !errors.As(err, &ce) || ce.Kind != loop.ConfigMissingClient {
		t.Fatalf("err = %v, want *loop.ConfigError{ConfigMissingClient}", err)
	}
	var se *SessionError
	if errors.As(err, &se) {
		t.Fatalf("err = %v, want the raw loop.New error, not a *SessionError", err)
	}

	// (b) the registry must be unchanged: no handle stored for the failed loop.
	s.loopsMu.RLock()
	after := len(s.loops)
	s.loopsMu.RUnlock()
	if after != before {
		t.Fatalf("registry size changed from %d to %d on loop.New failure, want unchanged", before, after)
	}

	// (c) structural cancel guard: NewLoop must cancel ONLY the derived loopCtx,
	// never the session backstop. sessionCtx must still be live after the error.
	select {
	case <-sessionCtx.Done():
		t.Fatal("NewLoop cancelled sessionCtx on the loop.New error path, want only the derived loopCtx cancelled")
	default:
	}
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

// first returns the earliest minted id — the turn-initiating command's id (the
// UserInput for Invoke/Stream). A later Stream.Close mints a second id for its
// best-effort Interrupt, so the observable turn CausationID is the FIRST mint.
func (g *capturingIDGen) first() (uuid.UUID, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.ids) == 0 {
		return uuid.UUID{}, false
	}
	return g.ids[0], true
}

// TestStampsCommandHeaderID asserts every command-sending method stamps a
// fresh, non-zero Header.ID on the command it sends. Each method mints the ID
// through the session's idGenerator seam, so a non-zero captured value proves
// the stamp. For Invoke and Stream the loop also copies the command's Header.ID
// onto each turn event's CausationID, so the CausationID observed through a hub
// Subscription must equal the captured ID — an end-to-end check that the stamp
// reaches the loop.
func TestStampsCommandHeaderID(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		call        func(t *testing.T, s *Sesssion)
		observeable bool // true when the stamped ID surfaces via a turn event's CausationID
	}{
		{
			name: "Invoke",
			call: func(t *testing.T, s *Sesssion) {
				if _, err := s.Invoke(context.Background(), nil); err != nil {
					t.Fatalf("Invoke: %v", err)
				}
			},
			observeable: true,
		},
		{
			name: "Stream",
			call: func(t *testing.T, s *Sesssion) {
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
			call: func(t *testing.T, s *Sesssion) {
				if _, err := s.Interrupt(context.Background()); err != nil {
					t.Fatalf("Interrupt: %v", err)
				}
			},
		},
		{
			name: "Shutdown",
			call: func(t *testing.T, s *Sesssion) {
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
			s, err := NewAgent(context.Background(), cfg(&stubLLM{chunks: []content.Chunk{textChunk("hi")}}))
			if err != nil {
				t.Fatalf("NewAgent: %v", err)
			}
			// Subscribe BEFORE the call so the turn events it triggers are observed
			// (the hub has no replay; a late subscriber would miss them).
			rec, sub := observe(t, s)
			t.Cleanup(func() { _ = sub.Close() })
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
				// The turn-initiating command (the UserInput) is the FIRST mint; a
				// Stream.Close fires a best-effort Interrupt that mints a later id, so
				// compare the observable CausationID against the first.
				turnID, ok := gen.first()
				if !ok {
					t.Fatal("session minted no command-Header ID")
				}
				cid, ok := rec.waitTurnCausationID(2 * time.Second)
				if !ok {
					t.Fatal("no turn-level event observed via the subscription")
				}
				if cid != turnID {
					t.Fatalf("event CausationID = %v, want stamped Header.ID %v", cid, turnID)
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
		call func(s *Sesssion) error
	}{
		{name: "Invoke", call: func(s *Sesssion) error { _, err := s.Invoke(context.Background(), nil); return err }},
		{name: "Stream", call: func(s *Sesssion) error { _, err := s.Stream(context.Background(), nil); return err }},
		{name: "Interrupt", call: func(s *Sesssion) error { _, err := s.Interrupt(context.Background()); return err }},
		{name: "Shutdown", call: func(s *Sesssion) error { return s.Shutdown(context.Background()) }},
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
	var rej *TurnRejectedError
	if !errors.As(err, &rej) || rej.Reason != event.RejectBusy {
		t.Fatalf("second Invoke err = %v, want *TurnRejectedError{RejectBusy}", err)
	}
}

// TestStreamBusyRejected asserts Stream is also start-or-reject: a second Stream
// while a turn occupies the loop returns *TurnRejectedError{RejectBusy} (the
// published event.TurnRejected mapped to a typed error), never a reader.
func TestStreamBusyRejected(t *testing.T) {
	t.Parallel()
	s, err := NewAgent(context.Background(), cfg(&stubLLM{blockUntilCancel: true}))
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}
	t.Cleanup(func() { _ = s.Shutdown(context.Background()) })

	ctx1, cancel1 := context.WithCancel(context.Background())
	defer cancel1()
	started := make(chan struct{})
	go func() { close(started); _, _ = s.Invoke(ctx1, nil) }()
	<-started
	time.Sleep(30 * time.Millisecond) // let the first turn occupy the loop

	sr, err := s.Stream(context.Background(), nil)
	if sr != nil {
		_ = sr.Close()
		t.Fatal("Stream returned a reader while the loop was busy, want nil + TurnRejectedError")
	}
	var rej *TurnRejectedError
	if !errors.As(err, &rej) || rej.Reason != event.RejectBusy {
		t.Fatalf("Stream while busy err = %v, want *TurnRejectedError{RejectBusy}", err)
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
// and a hub Subscription observes the TurnInterrupted terminal event.
func TestStreamCloseCancelsTurn(t *testing.T) {
	t.Parallel()
	s, err := NewAgent(context.Background(), cfg(&stubLLM{blockUntilCancel: true}))
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}
	t.Cleanup(func() { _ = s.Shutdown(context.Background()) })
	// Subscribe BEFORE the turn so the terminal it triggers on Close is observed.
	rec, sub := observe(t, s)
	t.Cleanup(func() { _ = sub.Close() })

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

	// the actor processes the abandon + cancel and publishes the interrupted
	// terminal to the hub; the recorder's drain goroutine records it. Poll until
	// observed (or the deadline elapses).
	if !rec.waitTerminal(time.Second) {
		t.Error("subscription did not observe TurnInterrupted after Close")
	}

	// session must be usable again: a subsequent Invoke is accepted by the loop
	// (not rejected with TurnRejectedError). Because the loop's client blocks until
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
			var rej *TurnRejectedError
			if !errors.As(err, &rej) {
				t.Fatalf("Invoke after early close = %v, want nil or *TurnRejectedError", err)
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
func sessionWithFakeLoop() (s *Sesssion, cmds chan command.Command, done chan struct{}) {
	cmds = make(chan command.Command)
	done = make(chan struct{})
	sessionCtx, sessionCancel := context.WithCancel(context.Background())
	primaryLoopID := mustUUID()
	s = &Sesssion{
		SessionID:     mustUUID(),
		sessionCtx:    sessionCtx,
		sessionCancel: sessionCancel,
		loops: map[uuid.UUID]*loopHandle{
			primaryLoopID: {loop: &loop.Loop{Commands: cmds, Done: done}},
		},
		primaryLoopID: primaryLoopID,
		newID:         uuid.New,
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
		call   func(s *Sesssion) error
		verify func(t *testing.T, cmd command.Command)
	}{
		{
			name: "Approve",
			call: func(s *Sesssion) error { return s.Approve(context.Background(), callID, tool.ScopeSession) },
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
			call: func(s *Sesssion) error { return s.Deny(context.Background(), callID) },
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
			call: func(s *Sesssion) error { return s.ProvideUserInput(context.Background(), callID, "the answer") },
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
		call func(s *Sesssion) error
	}{
		{name: "Approve", call: func(s *Sesssion) error { return s.Approve(context.Background(), callID, tool.ScopeOnce) }},
		{name: "Deny", call: func(s *Sesssion) error { return s.Deny(context.Background(), callID) }},
		{name: "ProvideUserInput", call: func(s *Sesssion) error { return s.ProvideUserInput(context.Background(), callID, "x") }},
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
		call func(s *Sesssion, ctx context.Context) error
	}{
		{name: "Approve", call: func(s *Sesssion, ctx context.Context) error { return s.Approve(ctx, callID, tool.ScopeOnce) }},
		{name: "Deny", call: func(s *Sesssion, ctx context.Context) error { return s.Deny(ctx, callID) }},
		{name: "ProvideUserInput", call: func(s *Sesssion, ctx context.Context) error { return s.ProvideUserInput(ctx, callID, "x") }},
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
		call func(s *Sesssion) error
	}{
		{name: "Approve", call: func(s *Sesssion) error { return s.Approve(context.Background(), callID, tool.ScopeOnce) }},
		{name: "Deny", call: func(s *Sesssion) error { return s.Deny(context.Background(), callID) }},
		{name: "ProvideUserInput", call: func(s *Sesssion) error { return s.ProvideUserInput(context.Background(), callID, "x") }},
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
		call func(s *Sesssion) error
	}{
		{name: "Approve", call: func(s *Sesssion) error { return s.Approve(context.Background(), callID, tool.ScopeOnce) }},
		{name: "Deny", call: func(s *Sesssion) error { return s.Deny(context.Background(), callID) }},
		{name: "ProvideUserInput", call: func(s *Sesssion) error { return s.ProvideUserInput(context.Background(), callID, "x") }},
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
