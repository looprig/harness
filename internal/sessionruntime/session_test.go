package sessionruntime

import (
	"context"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/looprig/core/content"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/command"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/gate"
	"github.com/looprig/harness/pkg/hub"
	"github.com/looprig/harness/pkg/identity"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/tool"
	"github.com/looprig/inference"
)

// stubLLM is a controllable inference.Client for session tests.
type stubLLM struct {
	chunks           []content.Chunk
	blockUntilCancel bool
	ignoreCtx        bool // with blockUntilCancel: block forever (provider ignores ctx)
}

type channelBackend struct {
	Commands chan command.Command
	Done     chan struct{}
}

func (b *channelBackend) CommandSink() chan<- command.Command { return b.Commands }
func (b *channelBackend) DoneChan() <-chan struct{}           { return b.Done }
func (b *channelBackend) Snapshot(context.Context) (content.AgenticMessages, event.TurnIndex, error) {
	return nil, 0, nil
}

func textChunk(s string) content.Chunk {
	return &content.TextChunk{Text: s}
}

func (s *stubLLM) Invoke(ctx context.Context, req inference.Request) (*inference.Response, error) {
	return nil, errors.New("stubLLM.Invoke not used")
}
func (s *stubLLM) Stream(ctx context.Context, req inference.Request) (*inference.StreamReader[content.Chunk], error) {
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
	return inference.NewStreamReader(next, nil), nil
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
// subscription is created AFTER New, so it never sees the construction-time
// SessionStarted (the hub has no replay) — tests must not assert on it.
func observe(t *testing.T, s *Session) (*recordingSub, event.Subscription) {
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
		for d := range sub.Events() {
			rec.record(d.Event)
		}
	}()
	return rec, sub
}

func (r *recordingSub) record(ev event.Event) {
	r.mu.Lock()
	r.events = append(r.events, ev)
	r.mu.Unlock()
}

// turnCausationID returns the Cause.CommandID stamped on the first turn-level event
// (a loop-scoped event; session-scoped events carry none). The loop stamps a
// turn event's Cause.CommandID with the issuing UserInput's Header.ID, so a non-zero
// value here proves the session stamped a fresh Header.ID on the command.
func (r *recordingSub) turnCausationID() (uuid.UUID, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, ev := range r.events {
		if ev.Scope() == event.ScopeSession {
			continue
		}
		return ev.EventHeader().Cause.CommandID, true
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

// validModel returns a minimal but VALID inference.Model (passes inference.Model.Validate): a
// known provider speaking a supported dialect at a loopback endpoint. It replaces the
// retired ModelSpec in session tests that construct a loop.Config.
func validModel(name string) inference.Model {
	return inference.Model{
		Provider:  inference.ProviderName("lmstudio"),
		APIFormat: inference.APIFormatOpenAI,
		BaseURL:   "http://localhost:1234",
		Name:      name,
	}
}

func mustDefine(opts ...loop.Option) loop.Definition {
	d, err := loop.Define(opts...)
	if err != nil {
		panic(err)
	}
	return d
}

func cfg(client inference.Client) loop.Definition {
	return mustDefine(loop.WithName("agent"), loop.WithInference(client, validModel("m")), loop.WithDrainTimeout(100*time.Millisecond))
}

func engineCfg(client inference.Client, engine loop.Engine, system string) loop.Definition {
	return mustDefine(loop.WithName("agent"), loop.WithInference(client, validModel("m")), loop.WithSystem(system), loop.WithEngine(engine), loop.WithDrainTimeout(100*time.Millisecond))
}

func bindCfg(d loop.Definition, sessionID, loopID uuid.UUID) loop.BoundDefinition {
	bound, err := d.Bind(context.Background(), tool.Bindings{SessionID: sessionID, LoopID: loopID})
	if err != nil {
		panic(err)
	}
	return bound
}

func TestNew(t *testing.T) {
	t.Parallel()
	t.Run("non-zero SessionID", func(t *testing.T) {
		t.Parallel()
		s, err := New(context.Background(), cfg(&stubLLM{chunks: []content.Chunk{textChunk("x")}}))
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		t.Cleanup(func() { _ = s.Shutdown(context.Background()) })
		var zero [16]byte
		if s.SessionID() == zero {
			t.Error("SessionID is zero")
		}
	})
	t.Run("ctx cancelled", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, err := New(ctx, cfg(&stubLLM{}))
		var se *SessionError
		if !errors.As(err, &se) || se.Kind != SessionContextDone {
			t.Fatalf("err = %v, want *SessionError{SessionContextDone}", err)
		}
	})
	t.Run("exactly one loop indexed by primaryLoopID", func(t *testing.T) {
		t.Parallel()
		s, err := New(context.Background(), cfg(&stubLLM{chunks: []content.Chunk{textChunk("x")}}))
		if err != nil {
			t.Fatalf("New: %v", err)
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
		if h.backend == nil {
			t.Error("primary loopHandle.backend is nil")
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
			s, err := New(context.Background(), cfg(&stubLLM{chunks: []content.Chunk{textChunk("x")}}))
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			t.Cleanup(func() { _ = s.Shutdown(context.Background()) })

			// Record which ids the session mints from here on, so we can assert the
			// returned loop id came from idGen. NewLoop mints the loop id FIRST, then
			// the LoopStarted EventID — so the loop id is the first captured id.
			gen := &capturingIDGen{}
			s.newID = gen.gen

			loopID, err := s.NewLoop(tt.parent, cfg(&stubLLM{chunks: []content.Chunk{textChunk("y")}}))
			if err != nil {
				t.Fatalf("NewLoop: %v", err)
			}
			if loopID.IsZero() {
				t.Fatal("NewLoop returned a zero loop id")
			}
			// NewLoop mints twice (loop id, then the LoopStarted EventID); the
			// FIRST mint is the loop id, so first() here means the loop id.
			minted, ok := gen.first()
			if !ok || minted != loopID {
				t.Fatalf("returned loop id %v was not the first freshly minted id %v", loopID, minted)
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
			if h.backend == nil {
				t.Error("stored loopHandle.backend is nil")
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
			case <-h.backend.DoneChan():
			case <-time.After(2 * time.Second):
				t.Fatal("new loop's Done did not close after sessionCancel; loopCtx not derived from sessionCtx")
			}
		})
	}
}

// cfgWithAgent is cfg with an AgentName set, so a test can assert the loop's
// configured attribution name is stamped onto its published LoopStarted.
func cfgWithAgent(client inference.Client, name identity.AgentName) loop.Definition {
	return mustDefine(loop.WithName(name), loop.WithInference(client, validModel("m")), loop.WithDrainTimeout(100*time.Millisecond))
}

// TestNewLoopStampsAgentName proves NewLoop stamps the loop's configured AgentName
// onto the published LoopStarted's Header — the immutable attribution record. An empty
// (unset) configured name leaves the stamped name empty. We subscribe BEFORE the call
// (the hub has no replay) and read the LoopStarted off the fan-in.
func TestNewLoopStampsAgentName(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		agent identity.AgentName
	}{
		{name: "named loop stamps its agent name", agent: "operator"},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			s, err := New(context.Background(), cfg(&stubLLM{chunks: []content.Chunk{textChunk("x")}}))
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			t.Cleanup(func() { _ = s.Shutdown(context.Background()) })

			sub, err := s.SubscribeEvents(allFilter())
			if err != nil {
				t.Fatalf("SubscribeEvents: %v", err)
			}
			t.Cleanup(func() { _ = sub.Close() })

			loopID, err := s.NewLoop(loop.Provenance{}, cfgWithAgent(&stubLLM{chunks: []content.Chunk{textChunk("y")}}, tt.agent))
			if err != nil {
				t.Fatalf("NewLoop: %v", err)
			}

			ls, ok := firstMatching[event.LoopStarted](t, sub)
			if !ok {
				t.Fatal("did not observe a LoopStarted on the fan-in")
			}
			// Read the LoopStarted for the loop we just created (not the primary's, which
			// was published before we subscribed — but guard regardless).
			if ls.LoopID != loopID {
				t.Fatalf("observed LoopStarted for loop %v, want the just-created %v", ls.LoopID, loopID)
			}
			if ls.Header.AgentName != tt.agent {
				t.Errorf("LoopStarted.Header.AgentName = %q, want %q", ls.Header.AgentName, tt.agent)
			}
		})
	}
}

// TestNewLoopIDGenerationFailure: when idGen fails, NewLoop returns a typed
// *SessionError wrapping the generator error and registers no loop. NewLoop mints
// two ids — the loop id (1st) then the LoopStarted EventID (2nd). A failure on the
// 1st call is SessionLoopIDGenerationFailed (no loop id); a failure on the 2nd is
// SessionIDGenerationFailed (loop id minted, but the announcement id failed). Both
// must fail BEFORE any loop is built or registered, so the registry never grows.
func TestNewLoopIDGenerationFailure(t *testing.T) {
	t.Parallel()
	genErr := errors.New("rand source exhausted")
	tests := []struct {
		name      string
		failOnNth int // 1 = loop id mint fails; 2 = EventID mint fails
		wantKind  SessionErrorKind
	}{
		{name: "loop id mint fails", failOnNth: 1, wantKind: SessionLoopIDGenerationFailed},
		{name: "event id mint fails", failOnNth: 2, wantKind: SessionIDGenerationFailed},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			s, err := New(context.Background(), cfg(&stubLLM{chunks: []content.Chunk{textChunk("x")}}))
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			t.Cleanup(func() { s.newID = uuid.New; _ = s.Shutdown(context.Background()) })

			s.loopsMu.RLock()
			before := len(s.loops)
			s.loopsMu.RUnlock()

			// Fail the Nth idGen call; earlier calls return real ids.
			var n int
			s.newID = func() (uuid.UUID, error) {
				n++
				if n == tt.failOnNth {
					return uuid.UUID{}, genErr
				}
				return uuid.New()
			}

			_, err = s.NewLoop(loop.Provenance{}, cfg(&stubLLM{}))
			var se *SessionError
			if !errors.As(err, &se) || se.Kind != tt.wantKind {
				t.Fatalf("err = %v, want *SessionError{%v}", err, tt.wantKind)
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
		})
	}
}

// TestNewLoopClosingRejects: once the session is closing (the flag Shutdown
// sets in the next task, set here directly under loopsMu as a white-box seam),
// NewLoop must fail secure — return *SessionError{SessionClosing}, register no
// loop, and publish no LoopStarted. The not-closing positive case proves the
// guard does not reject a healthy session.
func TestNewLoopClosingRejects(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		closing  bool
		wantErr  bool
		wantKind SessionErrorKind
	}{
		{name: "closing rejects", closing: true, wantErr: true, wantKind: SessionClosing},
		{name: "not closing succeeds", closing: false, wantErr: false},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			s, err := New(context.Background(), cfg(&stubLLM{chunks: []content.Chunk{textChunk("x")}}))
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			t.Cleanup(func() { _ = s.Shutdown(context.Background()) })

			s.loopsMu.Lock()
			s.closing = tt.closing
			before := len(s.loops)
			s.loopsMu.Unlock()

			loopID, err := s.NewLoop(loop.Provenance{}, cfg(&stubLLM{chunks: []content.Chunk{textChunk("y")}}))

			var se *SessionError
			if tt.wantErr {
				if !errors.As(err, &se) || se.Kind != tt.wantKind {
					t.Fatalf("err = %v, want *SessionError{%v}", err, tt.wantKind)
				}
				if !loopID.IsZero() {
					t.Fatalf("NewLoop returned loop id %v on closing, want zero", loopID)
				}
			} else {
				if err != nil {
					t.Fatalf("NewLoop: %v", err)
				}
				if loopID.IsZero() {
					t.Fatal("NewLoop returned a zero loop id on a healthy session")
				}
			}

			s.loopsMu.RLock()
			after := len(s.loops)
			s.loopsMu.RUnlock()
			if tt.wantErr {
				if after != before {
					t.Fatalf("registry grew from %d to %d while closing, want no new loop", before, after)
				}
			} else {
				if after != before+1 {
					t.Fatalf("registry = %d, want %d (one new loop) on a healthy session", after, before+1)
				}
			}
		})
	}
}

// TestLoopFor: loopFor(primaryLoopID) resolves the primary loop; a random id
// misses.
func TestLoopFor(t *testing.T) {
	t.Parallel()
	s, err := New(context.Background(), cfg(&stubLLM{chunks: []content.Chunk{textChunk("x")}}))
	if err != nil {
		t.Fatalf("New: %v", err)
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
// the SINGLE-TARGET routing methods: when loopFor misses (the registry has no
// entry for the addressed id), the method must fail secure with
// *SessionError{SessionLoopNotFound} and send no command. The miss is forced by
// deleting the primary registry entry under loopsMu after construction, so the
// id stays set but resolves to nothing — the exact state the branch guards.
//
// Interrupt is deliberately NOT in this table. Task 8 made it distributed: it
// iterates ALL loops rather than resolving one by id, so an empty registry is the
// no-op case (returns (false, nil)), NOT a SessionLoopNotFound error — there is no
// longer a single-target miss to guard. Its every-loop fan-out is covered by
// TestInterruptReachesEveryLoop.
//
// The Approve/Deny/ProvideUserInput trio was removed: those resolved the target
// loop by id via resolveGate (also removed). Their replacement, RespondGate,
// addresses a gate by GateID and — being durable-first — treats a post-append
// dispatch-loop miss as a nil return (the response is already durably accepted),
// not a SessionLoopNotFound; that path is covered by
// TestRespondGateReturnsNilAfterDurableAppendWhenDispatchFails.
func TestRoutingMethodsLoopNotFound(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		call func(s *Session) error
	}{
		{name: "Submit", call: func(s *Session) error { _, err := s.Submit(context.Background(), nil); return err }},
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
	s := &Session{
		sessionID:     mustUUID(),
		sessionCtx:    sessionCtx,
		sessionCancel: sessionCancel,
		loops:         map[uuid.UUID]*loopHandle{primaryLoopID: {}},
		primaryLoopID: primaryLoopID,
		newID:         uuid.New, // id mint succeeds; only loop.New must fail
		now:           time.Now,
	}
	// NewLoop stamps the LoopStarted header via the Factory before loop.New, so a
	// manually-built Session must wire one (New does this; this test bypasses New).
	s.factory = event.NewFactory(func() (uuid.UUID, error) { return s.newID() }, func() time.Time { return s.now() })

	s.loopsMu.RLock()
	before := len(s.loops)
	s.loopsMu.RUnlock()

	// loop.New rejects a nil Client with *ConfigError{ConfigMissingClient} before
	// starting any goroutine — the cheapest validation failure to inject.
	badCfg := loop.Definition{}
	loopID, err := s.NewLoop(loop.Provenance{}, badCfg)

	// (a) the loop.New error is returned, unwrapped, not remapped to *SessionError.
	if err == nil {
		t.Fatal("NewLoop returned nil error, want loop.New's ConfigError")
	}
	if !loopID.IsZero() {
		t.Errorf("NewLoop returned loop id %v on error, want zero", loopID)
	}
	var be *loop.BindError
	if !errors.As(err, &be) || be.Kind != loop.BindInvalidDefinition {
		t.Fatalf("err = %v, want *loop.BindError{BindInvalidDefinition}", err)
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

// first returns the earliest minted id. What that id means is call-site specific
// (e.g. the turn-initiating UserInput's id for Submit, or NewLoop's loop id, the
// first of its two mints); each call site documents which id it expects.
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
// the stamp. For Submit the loop also copies the command's Header.ID
// onto each turn event's Cause.CommandID, so the Cause.CommandID observed through a hub
// Subscription must equal the captured ID — an end-to-end check that the stamp
// reaches the loop.
func TestStampsCommandHeaderID(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		call        func(t *testing.T, s *Session)
		observeable bool // true when the stamped ID surfaces via a turn event's Cause.CommandID
	}{
		{
			name: "Submit",
			call: func(t *testing.T, s *Session) {
				if _, err := s.Submit(context.Background(), nil); err != nil {
					t.Fatalf("Submit: %v", err)
				}
			},
			observeable: true,
		},
		{
			name: "Interrupt",
			call: func(t *testing.T, s *Session) {
				if _, err := s.Interrupt(context.Background()); err != nil {
					t.Fatalf("Interrupt: %v", err)
				}
			},
		},
		{
			name: "Shutdown",
			call: func(t *testing.T, s *Session) {
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
			s, err := New(context.Background(), cfg(&stubLLM{chunks: []content.Chunk{textChunk("hi")}}))
			if err != nil {
				t.Fatalf("New: %v", err)
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
				// The turn-initiating command (the UserInput) is the FIRST mint, so
				// compare the observable Cause.CommandID against the first.
				turnID, ok := gen.first()
				if !ok {
					t.Fatal("session minted no command-Header ID")
				}
				cid, ok := rec.waitTurnCausationID(2 * time.Second)
				if !ok {
					t.Fatal("no turn-level event observed via the subscription")
				}
				if cid != turnID {
					t.Fatalf("event Cause.CommandID = %v, want stamped Header.ID %v", cid, turnID)
				}
			}
		})
	}
}

// TestNewCommandIDGenerationFailure covers the crypto/rand failure branch for the
// SINGLE-TARGET command methods: when the session's idGenerator fails, the method
// must fail secure with *SessionError{SessionIDGenerationFailed} and send no
// command (no unidentifiable, zero-ID command ever leaves the session).
//
// Shutdown is deliberately NOT in this table: per Task 7 / design §9, an id-gen
// failure for one loop must NOT abort the whole Shutdown — Shutdown's job is to
// tear down EVERY loop, so it skips that loop's graceful command and falls back to
// the deferred sessionCancel backstop, returning nil. That distinct contract is
// covered by TestShutdownIDGenFailureStillTearsDownAllLoops.
//
// Interrupt is also NOT in this table for the same reason (Task 8): it is the
// distributed human "stop everything" and skips a loop it cannot mint an id for
// (best-effort, consistent with Shutdown) rather than aborting the whole Interrupt,
// so an id-gen failure yields (false, nil), not SessionIDGenerationFailed. Its
// fan-out is covered by TestInterruptReachesEveryLoop.
func TestNewCommandIDGenerationFailure(t *testing.T) {
	t.Parallel()
	genErr := errors.New("rand source exhausted")
	failingGen := func() (uuid.UUID, error) { return uuid.UUID{}, genErr }

	tests := []struct {
		name string
		call func(s *Session) error
	}{
		{name: "Submit", call: func(s *Session) error { _, err := s.Submit(context.Background(), nil); return err }},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			s, err := New(context.Background(), cfg(&stubLLM{chunks: []content.Chunk{textChunk("x")}}))
			if err != nil {
				t.Fatalf("New: %v", err)
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

// TestShutdownIDGenFailureStillTearsDownAllLoops covers Shutdown's distinct
// id-gen-failure contract (Task 7 / design §9): a crypto/rand failure must NOT
// abort the whole Shutdown. Shutdown skips the graceful command.Shutdown for the
// loops it cannot mint an id for and falls back to the deferred sessionCancel
// backstop, which hard-cancels every loopCtx — so EVERY loop's actor still exits
// (Done closes) and Shutdown returns nil. A leaked, still-running loop on a
// shutdown is the failure this guards against.
func TestShutdownIDGenFailureStillTearsDownAllLoops(t *testing.T) {
	t.Parallel()
	s, err := New(context.Background(), cfg(&stubLLM{chunks: []content.Chunk{textChunk("x")}}))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	subID, err := s.NewLoop(loop.Provenance{}, cfg(&stubLLM{chunks: []content.Chunk{textChunk("y")}}))
	if err != nil {
		t.Fatalf("NewLoop: %v", err)
	}

	s.loopsMu.RLock()
	primaryDone := s.loops[s.primaryLoopID].backend.DoneChan()
	subDone := s.loops[subID].backend.DoneChan()
	s.loopsMu.RUnlock()

	// Fail every command-id mint during Shutdown.
	s.newID = func() (uuid.UUID, error) { return uuid.UUID{}, errors.New("rand source exhausted") }

	if err := s.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown with failing id-gen = %v, want nil (backstop tears loops down)", err)
	}

	// Despite the id-gen failure, the sessionCancel backstop must hard-cancel every
	// loopCtx, so both actors exit.
	select {
	case <-primaryDone:
	case <-time.After(2 * time.Second):
		t.Fatal("primary loop Done not closed after Shutdown with failing id-gen (backstop did not fire)")
	}
	select {
	case <-subDone:
	case <-time.After(2 * time.Second):
		t.Fatal("sub-loop Done not closed after Shutdown with failing id-gen (backstop did not fire)")
	}
}

func TestShutdownThenMethodsExit(t *testing.T) {
	t.Parallel()
	s, err := New(context.Background(), cfg(&stubLLM{chunks: []content.Chunk{textChunk("x")}}))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := s.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	// second shutdown is a no-op
	if err := s.Shutdown(context.Background()); err != nil {
		t.Fatalf("second Shutdown: %v", err)
	}
	// methods after shutdown return SessionLoopExited, no deadlock
	_, err = s.Submit(context.Background(), nil)
	var se *SessionError
	if !errors.As(err, &se) || se.Kind != SessionLoopExited {
		t.Fatalf("Submit after shutdown err = %v, want *SessionError{SessionLoopExited}", err)
	}
}

// TestInterruptDuringRunningTurn is the session-level integration proof that a human
// Interrupt against a REAL running primary turn returns (true, nil) AND ends that turn
// on a TurnInterrupted terminal. The turn is started fire-and-forget via Submit (the
// provider blocks so the turn stays running); both the running state and the terminal
// are observed on the event fan-in (the same surface a TUI/CLI consumes), not a
// blocking call's return value.
func TestInterruptDuringRunningTurn(t *testing.T) {
	t.Parallel()
	s, err := New(context.Background(), cfg(&stubLLM{blockUntilCancel: true}))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = s.Shutdown(context.Background()) })

	// Subscribe BEFORE submitting (the hub has no replay) so the TurnStarted that
	// proves the turn is running, and the TurnInterrupted terminal, are both observed.
	sub, err := s.SubscribeEvents(allFilter())
	if err != nil {
		t.Fatalf("SubscribeEvents: %v", err)
	}
	t.Cleanup(func() { _ = sub.Close() })

	if _, err := s.Submit(context.Background(), nil); err != nil {
		t.Fatalf("Submit: %v", err)
	}

	// Wait until the turn is actually running (TurnStarted on the fan-in) so the
	// Interrupt lands on a live turn deterministically.
	if !drainFor[event.TurnStarted](t, sub) {
		t.Fatal("turn never started")
	}

	cancelled, err := s.Interrupt(context.Background())
	if err != nil {
		t.Fatalf("Interrupt: %v", err)
	}
	if !cancelled {
		t.Fatal("Interrupt returned false, want true (a running turn was cancelled)")
	}

	// The cancelled turn ends on a TurnInterrupted terminal, observed on the fan-in.
	if !drainFor[event.TurnInterrupted](t, sub) {
		t.Fatal("running turn did not end on a TurnInterrupted terminal after Interrupt")
	}
}

// TestInterruptCtxCancelledBeforeSend: a cancelled ctx makes Interrupt return
// (false, *SessionError{SessionContextDone}) before any command is sent.
//
// Determinism comes from a FAKE loop whose unbuffered Commands channel NOTHING
// receives from and whose Done is never closed: the per-loop send select in
// Interrupt has only its ctx.Done() arm ready (the Commands send blocks forever
// with no reader; Done blocks because it is open), so a pre-cancelled ctx wins
// every time. A REAL running loop would keep reading Commands, leaving both the
// send and ctx.Done() ready and letting Go's select pick at random — the source
// of the prior flake. No time.Sleep is needed (or correct) here.
func TestInterruptCtxCancelledBeforeSend(t *testing.T) {
	t.Parallel()
	s, _, _ := sessionWithFakeLoop() // Commands never read + Done never closed: the send blocks

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	resCh := make(chan struct {
		cancelled bool
		err       error
	}, 1)
	go func() {
		cancelled, err := s.Interrupt(ctx)
		resCh <- struct {
			cancelled bool
			err       error
		}{cancelled, err}
	}()

	select {
	case res := <-resCh:
		if res.cancelled {
			t.Error("Interrupt returned true, want false")
		}
		var se *SessionError
		if !errors.As(res.err, &se) || se.Kind != SessionContextDone {
			t.Fatalf("err = %v, want *SessionError{SessionContextDone}", res.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Interrupt blocked on a cancelled ctx (no ctx.Done() escape in the send select)")
	}
}

// TestShutdownCtxCancelledBeforeSend: a cancelled ctx makes Shutdown return
// *SessionError{SessionContextDone} before any command is sent.
//
// As with Interrupt, determinism comes from a FAKE loop whose unbuffered Commands
// channel NOTHING receives from and whose Done is never closed: Shutdown's
// per-loop send select has only its ctx.Done() arm ready, so a pre-cancelled ctx
// wins on the very first per-loop send. Shutdown first latches closing, snapshots
// the loops, and calls hub.StopSession before the sends — all harmless here; the
// send still blocks and the cancelled ctx still wins. sessionWithTwoFakeLoopsAndDone
// (not sessionWithFakeLoop) is used because Shutdown dereferences s.hub, which that
// helper populates. No time.Sleep is needed (or correct) here.
func TestShutdownCtxCancelledBeforeSend(t *testing.T) {
	t.Parallel()
	s, _, _, _, _ := sessionWithTwoFakeLoopsAndDone() // Commands never read + Done never closed

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	errCh := make(chan error, 1)
	go func() { errCh <- s.Shutdown(ctx) }()

	select {
	case err := <-errCh:
		var se *SessionError
		if !errors.As(err, &se) || se.Kind != SessionContextDone {
			t.Fatalf("err = %v, want *SessionError{SessionContextDone}", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Shutdown blocked on a cancelled ctx (no ctx.Done() escape in the send select)")
	}
}

// TestShutdownSurfacesLoopTerminatedError covers the spec-table case "Shutdown
// loop root ctx cancelled during shutdown → ack receives *LoopTerminatedError;
// session wraps to *SessionError". This IS deterministic through the session API:
// Session.Shutdown parks in its final select before the kill, and the actor
// sends the LoopTerminatedError ack BEFORE closing Done, so the parked select
// wakes on the ack case while Done is still open — ack wins, not a race. (A
// ctx-ignoring provider is required so the turn never completes on cancelTurn,
// forcing the root-ctx-kill + DrainTimeout path that produces the typed error.)
func TestShutdownSurfacesLoopTerminatedError(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	s, err := New(ctx, cfg(&stubLLM{blockUntilCancel: true, ignoreCtx: true}))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// Occupy the loop with a turn that never completes (provider ignores ctx).
	if _, err := s.Submit(context.Background(), nil); err != nil {
		t.Fatalf("Submit: %v", err)
	}
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

// sessionWithFakeLoop builds a Session wired to a fake loop whose Commands
// channel the test reads from and whose Done channel the test controls. This is
// the seam for the fire-and-route gate commands (Approve/Deny/ProvideUserInput),
// which carry no Ack and so have no sink-observable effect through the real loop:
// reading the unbuffered Commands channel directly captures the exact command the
// session sent. cmds is unbuffered to mirror the real loop.Commands, so a send is
// observable only when the test (or a closed Done) is ready.
func sessionWithFakeLoop() (s *Session, cmds chan command.Command, done chan struct{}) {
	cmds = make(chan command.Command)
	done = make(chan struct{})
	sessionCtx, sessionCancel := context.WithCancel(context.Background())
	primaryLoopID := mustUUID()
	s = &Session{
		sessionID:     mustUUID(),
		sessionCtx:    sessionCtx,
		sessionCancel: sessionCancel,
		loops: map[uuid.UUID]*loopHandle{
			primaryLoopID: {backend: &channelBackend{Commands: cmds, Done: done}},
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

// TestGateCommandsSendCorrectCommand asserts RespondGate dispatches the correct
// command type to the owning loop's Commands channel, stamped with a fresh
// non-zero Header.ID and the right ToolExecutionID (from the gate's route) plus
// Scope/Answer, and returns nil. The loop's Commands channel captures the exact
// command dispatched. (This replaces the removed Approve/Deny/ProvideUserInput
// trio, which sent these same commands directly.)
func TestGateCommandsSendCorrectCommand(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		gate    gate.Gate
		payload gate.Payload
		respond func(gateID gate.ID) gate.GateResponse
		verify  func(t *testing.T, cmd command.Command, callID uuid.UUID)
	}{
		{
			name:    "approve",
			gate:    permissionGate(),
			payload: bashPayload(),
			respond: func(gateID gate.ID) gate.GateResponse { return userApprove(gateID, "session") },
			verify: func(t *testing.T, cmd command.Command, callID uuid.UUID) {
				c, ok := cmd.(command.ApproveToolCall)
				if !ok {
					t.Fatalf("dispatched %T, want command.ApproveToolCall", cmd)
				}
				if c.ToolExecutionID != callID {
					t.Errorf("ToolExecutionID = %v, want %v", c.ToolExecutionID, callID)
				}
				if c.Scope != tool.ScopeSession {
					t.Errorf("Scope = %v, want %v", c.Scope, tool.ScopeSession)
				}
				if c.CommandHeader().CommandID.IsZero() {
					t.Error("Header.ID is zero, want a fresh non-zero id")
				}
			},
		},
		{
			name:    "deny",
			gate:    permissionGate(),
			payload: bashPayload(),
			respond: userDeny,
			verify: func(t *testing.T, cmd command.Command, callID uuid.UUID) {
				c, ok := cmd.(command.DenyToolCall)
				if !ok {
					t.Fatalf("dispatched %T, want command.DenyToolCall", cmd)
				}
				if c.ToolExecutionID != callID {
					t.Errorf("ToolExecutionID = %v, want %v", c.ToolExecutionID, callID)
				}
				if c.CommandHeader().CommandID.IsZero() {
					t.Error("Header.ID is zero, want a fresh non-zero id")
				}
			},
		},
		{
			name:    "answer",
			gate:    askUserGate(),
			payload: askUserPayload(),
			respond: func(gateID gate.ID) gate.GateResponse { return userAnswer(gateID, "the answer") },
			verify: func(t *testing.T, cmd command.Command, callID uuid.UUID) {
				c, ok := cmd.(command.ProvideUserInput)
				if !ok {
					t.Fatalf("dispatched %T, want command.ProvideUserInput", cmd)
				}
				if c.ToolExecutionID != callID {
					t.Errorf("ToolExecutionID = %v, want %v", c.ToolExecutionID, callID)
				}
				if c.Answer != "the answer" {
					t.Errorf("Answer = %q, want %q", c.Answer, "the answer")
				}
				if c.CommandHeader().CommandID.IsZero() {
					t.Error("Header.ID is zero, want a fresh non-zero id")
				}
			},
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			s, _, loopID, cmds := gateSession(t)
			callID := mustUUID()
			gateID := activateOn(t, s, loopID, callID, tt.gate, tt.payload)

			errCh := make(chan error, 1)
			go func() { errCh <- s.RespondGate(context.Background(), tt.respond(gateID)) }()

			select {
			case cmd := <-cmds:
				tt.verify(t, cmd, callID)
			case <-time.After(2 * time.Second):
				t.Fatal("RespondGate never dispatched a command")
			}

			select {
			case err := <-errCh:
				if err != nil {
					t.Fatalf("RespondGate returned %v, want nil", err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("RespondGate never returned after dispatch")
			}
		})
	}
}

// TestGateCommandsFreshHeaderIDPerCall asserts RespondGate mints a distinct
// Header.ID on every dispatched command (fresh per command, not reused). Since a
// gate answers only once, each of the two responses answers its own freshly
// prepared+activated gate. (This replaces the removed trio.)
func TestGateCommandsFreshHeaderIDPerCall(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		gate    gate.Gate
		payload gate.Payload
		respond func(gateID gate.ID) gate.GateResponse
	}{
		{name: "approve", gate: permissionGate(), payload: bashPayload(), respond: func(gateID gate.ID) gate.GateResponse { return userApprove(gateID, "once") }},
		{name: "deny", gate: permissionGate(), payload: bashPayload(), respond: userDeny},
		{name: "answer", gate: askUserGate(), payload: askUserPayload(), respond: func(gateID gate.ID) gate.GateResponse { return userAnswer(gateID, "x") }},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			s, _, loopID, cmds := gateSession(t)

			ids := make([]uuid.UUID, 0, 2)
			for i := 0; i < 2; i++ {
				gateID := activateOn(t, s, loopID, mustUUID(), tt.gate, tt.payload)
				errCh := make(chan error, 1)
				go func() { errCh <- s.RespondGate(context.Background(), tt.respond(gateID)) }()
				select {
				case cmd := <-cmds:
					ids = append(ids, cmd.CommandHeader().CommandID)
				case <-time.After(2 * time.Second):
					t.Fatal("RespondGate never dispatched a command")
				}
				if err := <-errCh; err != nil {
					t.Fatalf("RespondGate returned %v, want nil", err)
				}
			}
			if ids[0] == ids[1] {
				t.Fatalf("two responses reused Header.ID %v, want fresh ids", ids[0])
			}
		})
	}
}

// gateCommands returns representative gate commands, one per dispatched shape, so
// the routeGate escape-hatch tests can prove the select behaves uniformly for
// each command type the gate machinery routes.
func gateCommands() []struct {
	name string
	cmd  command.Command
} {
	return []struct {
		name string
		cmd  command.Command
	}{
		{name: "ApproveToolCall", cmd: command.ApproveToolCall{Header: command.Header{CommandID: mustUUID()}}},
		{name: "DenyToolCall", cmd: command.DenyToolCall{Header: command.Header{CommandID: mustUUID()}}},
		{name: "ProvideUserInput", cmd: command.ProvideUserInput{Header: command.Header{CommandID: mustUUID()}}},
	}
}

// TestRouteGateCtxCancelled: a cancelled ctx makes routeGate return
// *SessionError{SessionContextDone} without blocking and without sending (the
// loop's Commands channel is never read). This exercises routeGate directly
// because the Approve/Deny/ProvideUserInput trio that used to wrap it was removed;
// routeGate is the still-live fire-and-route seam (now driven by RespondGate),
// whose ctx.Done() escape is not observable through RespondGate (which dispatches
// on the session context and swallows the routing error after the durable commit).
func TestRouteGateCtxCancelled(t *testing.T) {
	t.Parallel()
	for _, tt := range gateCommands() {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			s, _, _ := sessionWithFakeLoop() // Commands never read: a send would block
			l, ok := s.loopFor(s.primaryLoopID)
			if !ok {
				t.Fatal("primary loop missing from registry")
			}

			ctx, cancel := context.WithCancel(context.Background())
			cancel()

			errCh := make(chan error, 1)
			go func() { errCh <- s.routeGate(ctx, s.primaryLoopID, l, tt.cmd) }()

			select {
			case err := <-errCh:
				var se *SessionError
				if !errors.As(err, &se) || se.Kind != SessionContextDone {
					t.Fatalf("err = %v, want *SessionError{SessionContextDone}", err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("routeGate blocked on a cancelled ctx (no ctx.Done() escape)")
			}
		})
	}
}

// TestRouteGateLoopExited: after the loop's Done channel is closed, routeGate
// returns *SessionError{SessionLoopExited} without blocking and without sending.
// Like TestRouteGateCtxCancelled it drives routeGate directly (the trio that used
// to wrap it was removed); this loop.Done escape is a routeGate property that
// RespondGate does not surface (it swallows the routing error post-commit).
func TestRouteGateLoopExited(t *testing.T) {
	t.Parallel()
	for _, tt := range gateCommands() {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			s, _, done := sessionWithFakeLoop() // Commands never read
			l, ok := s.loopFor(s.primaryLoopID)
			if !ok {
				t.Fatal("primary loop missing from registry")
			}
			close(done) // loop has exited

			errCh := make(chan error, 1)
			go func() { errCh <- s.routeGate(context.Background(), s.primaryLoopID, l, tt.cmd) }()

			select {
			case err := <-errCh:
				var se *SessionError
				if !errors.As(err, &se) || se.Kind != SessionLoopExited {
					t.Fatalf("err = %v, want *SessionError{SessionLoopExited}", err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("routeGate blocked after loop exited (no loop.Done escape)")
			}
		})
	}
}

// TestGateCommandsIDGenerationFailure: when the id generator fails while minting
// the command id, RespondGate fails secure with *GateError{GateAppendFailed}
// (wrapping the *SessionError{SessionIDGenerationFailed} that carries the
// generator error) BEFORE claiming the gate or dispatching a command. The failure
// surfaces as the gate machinery's typed error rather than the removed trio's
// bare *SessionError; the underlying generator error is still recoverable via
// errors.Is. No command is dispatched.
func TestGateCommandsIDGenerationFailure(t *testing.T) {
	t.Parallel()
	genErr := errors.New("rand source exhausted")
	tests := []struct {
		name    string
		gate    gate.Gate
		payload gate.Payload
		respond func(gateID gate.ID) gate.GateResponse
	}{
		{name: "approve", gate: permissionGate(), payload: bashPayload(), respond: func(gateID gate.ID) gate.GateResponse { return userApprove(gateID, "once") }},
		{name: "deny", gate: permissionGate(), payload: bashPayload(), respond: userDeny},
		{name: "answer", gate: askUserGate(), payload: askUserPayload(), respond: func(gateID gate.ID) gate.GateResponse { return userAnswer(gateID, "x") }},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			s, _, loopID, cmds := gateSession(t)
			gateID := activateOn(t, s, loopID, mustUUID(), tt.gate, tt.payload)
			// Fail id generation only after the gate is open, so the failure lands on
			// the response's command-id mint (not on gate prepare/activate).
			s.newID = func() (uuid.UUID, error) { return uuid.UUID{}, genErr }

			err := s.RespondGate(context.Background(), tt.respond(gateID))
			var ge *GateError
			if !errors.As(err, &ge) || ge.Kind != GateAppendFailed {
				t.Fatalf("err = %v, want *GateError{GateAppendFailed}", err)
			}
			if !errors.Is(err, genErr) {
				t.Fatalf("err = %v, want it to wrap the generator error", err)
			}

			select {
			case c := <-cmds:
				t.Fatalf("id-generation failure dispatched a command %T, want none", c)
			default:
			}
		})
	}
}

// sessionWithTwoFakeLoops builds a Session wired to TWO fake loops (A and B), each
// keyed by its own loop id in the registry. A is the primary loop. The test reads
// each loop's unbuffered Commands channel directly to observe exactly which loop a
// gate reply was dispatched to — the seam for the multi-loop routing guarantee.
func sessionWithTwoFakeLoops() (s *Session, loopA, loopB uuid.UUID, cmdsA, cmdsB chan command.Command) {
	cmdsA = make(chan command.Command)
	cmdsB = make(chan command.Command)
	doneA := make(chan struct{})
	doneB := make(chan struct{})
	sessionCtx, sessionCancel := context.WithCancel(context.Background())
	loopA = mustUUID()
	loopB = mustUUID()
	s = &Session{
		sessionID:     mustUUID(),
		sessionCtx:    sessionCtx,
		sessionCancel: sessionCancel,
		loops: map[uuid.UUID]*loopHandle{
			loopA: {backend: &channelBackend{Commands: cmdsA, Done: doneA}},
			loopB: {backend: &channelBackend{Commands: cmdsB, Done: doneB}},
		},
		primaryLoopID: loopA,
		newID:         uuid.New,
	}
	return s, loopA, loopB, cmdsA, cmdsB
}

// TestGateReplyRoutesToTargetLoopNeverSibling is the point of Task 13: a gate reply
// for a gate opened on loop A is dispatched to loop A's command channel and NEVER
// reaches loop B. RespondGate dispatches by the gate's stored route (LoopID +
// ToolExecutionID, both set at ActivateGate); the command carries both
// Coordinates.LoopID (the dispatch target) and ToolExecutionID (the uuid match
// key). Matching is by ToolExecutionID — a uuid, never the provider's ToolUseID
// (a string), which is structurally impossible to confuse here because the field
// is typed uuid.UUID. (This replaces the removed trio, which took loopID directly.)
func TestGateReplyRoutesToTargetLoopNeverSibling(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		gate    gate.Gate
		payload gate.Payload
		respond func(gateID gate.ID) gate.GateResponse
		verify  func(t *testing.T, cmd command.Command, wantLoop, callID uuid.UUID)
	}{
		{
			name:    "approve",
			gate:    permissionGate(),
			payload: bashPayload(),
			respond: func(gateID gate.ID) gate.GateResponse { return userApprove(gateID, "session") },
			verify: func(t *testing.T, cmd command.Command, wantLoop, callID uuid.UUID) {
				c, ok := cmd.(command.ApproveToolCall)
				if !ok {
					t.Fatalf("dispatched %T, want command.ApproveToolCall", cmd)
				}
				if c.GateRoute.LoopID != wantLoop {
					t.Errorf("GateRoute.LoopID = %v, want %v", c.GateRoute.LoopID, wantLoop)
				}
				if c.GateRoute.ToolExecutionID != callID {
					t.Errorf("GateRoute.ToolExecutionID = %v, want %v", c.GateRoute.ToolExecutionID, callID)
				}
				if c.Scope != tool.ScopeSession {
					t.Errorf("Scope = %v, want %v", c.Scope, tool.ScopeSession)
				}
			},
		},
		{
			name:    "deny",
			gate:    permissionGate(),
			payload: bashPayload(),
			respond: userDeny,
			verify: func(t *testing.T, cmd command.Command, wantLoop, callID uuid.UUID) {
				c, ok := cmd.(command.DenyToolCall)
				if !ok {
					t.Fatalf("dispatched %T, want command.DenyToolCall", cmd)
				}
				if c.GateRoute.LoopID != wantLoop {
					t.Errorf("GateRoute.LoopID = %v, want %v", c.GateRoute.LoopID, wantLoop)
				}
				if c.GateRoute.ToolExecutionID != callID {
					t.Errorf("GateRoute.ToolExecutionID = %v, want %v", c.GateRoute.ToolExecutionID, callID)
				}
			},
		},
		{
			name:    "answer",
			gate:    askUserGate(),
			payload: askUserPayload(),
			respond: func(gateID gate.ID) gate.GateResponse { return userAnswer(gateID, "the answer") },
			verify: func(t *testing.T, cmd command.Command, wantLoop, callID uuid.UUID) {
				c, ok := cmd.(command.ProvideUserInput)
				if !ok {
					t.Fatalf("dispatched %T, want command.ProvideUserInput", cmd)
				}
				if c.GateRoute.LoopID != wantLoop {
					t.Errorf("GateRoute.LoopID = %v, want %v", c.GateRoute.LoopID, wantLoop)
				}
				if c.GateRoute.ToolExecutionID != callID {
					t.Errorf("GateRoute.ToolExecutionID = %v, want %v", c.GateRoute.ToolExecutionID, callID)
				}
				if c.Answer != "the answer" {
					t.Errorf("Answer = %q, want %q", c.Answer, "the answer")
				}
			},
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			s, loopA, _, cmdsA, cmdsB := gateSessionTwoLoops(t)
			callID := mustUUID()
			// Open the gate on loop A and assert B never sees the reply; the
			// sibling-isolation guarantee is symmetric. Targeting A proves dispatch
			// keys on the gate's own route, not "primary by default".
			gateID := activateOn(t, s, loopA, callID, tt.gate, tt.payload)

			if err := s.RespondGate(context.Background(), tt.respond(gateID)); err != nil {
				t.Fatalf("RespondGate returned %v, want nil", err)
			}

			// The reply must have arrived on loop A's channel and never on B's.
			select {
			case cmd := <-cmdsA:
				tt.verify(t, cmd, loopA, callID)
			case cmd := <-cmdsB:
				t.Fatalf("gate reply for loop A was delivered to sibling loop B: %T", cmd)
			default:
				t.Fatal("gate reply never reached the target loop")
			}

			// Loop B must never receive anything.
			select {
			case cmd := <-cmdsB:
				t.Fatalf("sibling loop B received a stray command: %T", cmd)
			default:
			}
		})
	}
}

// TestGateReplyToNonPrimaryLoop proves dispatch follows the gate's own route even
// when the owning loop is NOT the primary: a reply to a gate opened on loop B
// reaches B (not the primary A). This is the latent multi-loop bug Task 13 fixed —
// a gate reply must route to the loop that opened the gate, not the primary.
// (This replaces the removed trio, which was addressed by loopID directly.)
func TestGateReplyToNonPrimaryLoop(t *testing.T) {
	t.Parallel()
	s, _, loopB, cmdsA, cmdsB := gateSessionTwoLoops(t)
	callID := mustUUID()
	gateID := activateOn(t, s, loopB, callID, permissionGate(), bashPayload())

	if err := s.RespondGate(context.Background(), userApprove(gateID, "once")); err != nil {
		t.Fatalf("RespondGate returned %v, want nil", err)
	}

	select {
	case cmd := <-cmdsB:
		c, ok := cmd.(command.ApproveToolCall)
		if !ok {
			t.Fatalf("dispatched %T, want command.ApproveToolCall", cmd)
		}
		if c.GateRoute.LoopID != loopB {
			t.Errorf("GateRoute.LoopID = %v, want %v (loop B)", c.GateRoute.LoopID, loopB)
		}
	case cmd := <-cmdsA:
		t.Fatalf("gate reply for loop B was misrouted to the primary loop A: %T", cmd)
	default:
		t.Fatal("gate reply never reached loop B")
	}
}

// twoFakeLoopHandles exposes both fake loops' command AND done channels so a test
// can observe the per-loop Shutdown send and drive each actor's exit. Unlike
// sessionWithTwoFakeLoops it returns the done channels so the test owns the
// actor-exit signal. cmds are unbuffered to mirror the real loop.Commands.
func sessionWithTwoFakeLoopsAndDone() (s *Session, cmdsA, cmdsB chan command.Command, doneA, doneB chan struct{}) {
	cmdsA = make(chan command.Command)
	cmdsB = make(chan command.Command)
	doneA = make(chan struct{})
	doneB = make(chan struct{})
	sessionCtx, sessionCancel := context.WithCancel(context.Background())
	loopA := mustUUID()
	loopB := mustUUID()
	s = &Session{
		sessionID:     mustUUID(),
		hub:           hub.New(mustUUID()),
		sessionCtx:    sessionCtx,
		sessionCancel: sessionCancel,
		loops: map[uuid.UUID]*loopHandle{
			loopA: {backend: &channelBackend{Commands: cmdsA, Done: doneA}, cancel: func() {}},
			loopB: {backend: &channelBackend{Commands: cmdsB, Done: doneB}, cancel: func() {}},
		},
		primaryLoopID: loopA,
		newID:         uuid.New,
	}
	return s, cmdsA, cmdsB, doneA, doneB
}

// TestShutdownReachesEveryLoop is the point of Task 7: Shutdown must send a
// graceful command.Shutdown to EVERY registered loop (the primary AND every
// sub-loop), not just the primary. With two fake loops whose Done never closes on
// their own, the only way both actors learn to stop is the per-loop Shutdown
// command — the current primary-only Shutdown sends it to loop A alone and never
// reaches loop B, so the "sub-loop B received a Shutdown" requirement is the red.
//
// The fake loops never close Done themselves, so the test acts as both actors:
// it receives the Shutdown on each command channel and replies nil on each Ack,
// exactly as a real actor would, letting Shutdown complete and return nil.
func TestShutdownReachesEveryLoop(t *testing.T) {
	t.Parallel()
	s, cmdsA, cmdsB, _, _ := sessionWithTwoFakeLoopsAndDone()

	errCh := make(chan error, 1)
	go func() { errCh <- s.Shutdown(context.Background()) }()

	// Both loops MUST be reached. The sends may arrive in either order, so accept
	// whichever channel is ready first across two iterations, then require both
	// were a real command.Shutdown carrying a fresh id and a non-nil Ack.
	var ackA, ackB chan<- error
	for i := 0; i < 2; i++ {
		select {
		case cmd := <-cmdsA:
			sd, ok := cmd.(command.Shutdown)
			if !ok {
				t.Fatalf("loop A received %T, want command.Shutdown", cmd)
			}
			if sd.Ack == nil || sd.Header.CommandID.IsZero() {
				t.Fatalf("loop A Shutdown malformed: ack=%v id=%v", sd.Ack, sd.Header.CommandID)
			}
			ackA = sd.Ack
		case cmd := <-cmdsB:
			sd, ok := cmd.(command.Shutdown)
			if !ok {
				t.Fatalf("loop B received %T, want command.Shutdown", cmd)
			}
			if sd.Ack == nil || sd.Header.CommandID.IsZero() {
				t.Fatalf("loop B Shutdown malformed: ack=%v id=%v", sd.Ack, sd.Header.CommandID)
			}
			ackB = sd.Ack
		case <-time.After(2 * time.Second):
			t.Fatal("a loop never received its Shutdown command (Shutdown did not reach every loop)")
		}
	}
	if ackA == nil {
		t.Fatal("primary loop A never received a Shutdown command")
	}
	if ackB == nil {
		t.Fatal("sub-loop B never received a Shutdown command (current primary-only Shutdown leaves it running)")
	}

	// Both actors ack a clean exit.
	ackA <- nil
	ackB <- nil

	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("Shutdown returned %v, want nil after both loops acked clean", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Shutdown never returned after both loops acked")
	}
}

// TestInterruptReachesEveryLoop is the point of Task 8: Interrupt is the human
// "stop everything" — it must send a command.Interrupt to EVERY registered loop
// (the primary AND every sub-loop), not just the primary. With two fake loops
// whose Done never closes on their own, the only way both actors learn to cancel
// is the per-loop Interrupt command — the prior primary-only Interrupt reaches
// loop A alone and never touches loop B, so the "sub-loop B received an Interrupt"
// requirement is the red.
//
// Every Interrupt must carry Agency=AgencyUser (a human pressed interrupt). The
// fake loops never close Done themselves, so the test acts as both actors: it
// receives the Interrupt on each command channel and replies on each Ack with the
// table's per-loop value, then asserts Interrupt aggregates to true iff any loop
// reported it cancelled a turn (idle loops ack false and must not break the fan-in
// or panic).
func TestInterruptReachesEveryLoop(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		ackA    bool
		ackB    bool
		wantAny bool
	}{
		{name: "both cancelled a turn", ackA: true, ackB: true, wantAny: true},
		{name: "primary cancelled, sub idle", ackA: true, ackB: false, wantAny: true},
		{name: "sub cancelled, primary idle", ackA: false, ackB: true, wantAny: true},
		{name: "both idle ack false", ackA: false, ackB: false, wantAny: false},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			s, cmdsA, cmdsB, _, _ := sessionWithTwoFakeLoopsAndDone()

			resCh := make(chan bool, 1)
			errCh := make(chan error, 1)
			go func() {
				any, err := s.Interrupt(context.Background())
				errCh <- err
				resCh <- any
			}()

			// Both loops MUST be reached. The sends may arrive in either order, so
			// accept whichever channel is ready first across two iterations, then
			// require both were a real command.Interrupt carrying a fresh id, a
			// non-nil Ack, and Agency=AgencyUser. Each loop's Ack is replied with the
			// table's per-loop value, exactly as a real actor would.
			var ackA, ackB chan<- bool
			for i := 0; i < 2; i++ {
				select {
				case cmd := <-cmdsA:
					ic, ok := cmd.(command.Interrupt)
					if !ok {
						t.Fatalf("loop A received %T, want command.Interrupt", cmd)
					}
					if ic.Ack == nil || ic.Header.CommandID.IsZero() {
						t.Fatalf("loop A Interrupt malformed: ack=%v id=%v", ic.Ack, ic.Header.CommandID)
					}
					if ic.Header.Agency != identity.AgencyUser {
						t.Fatalf("loop A Interrupt Agency = %v, want AgencyUser", ic.Header.Agency)
					}
					ackA = ic.Ack
				case cmd := <-cmdsB:
					ic, ok := cmd.(command.Interrupt)
					if !ok {
						t.Fatalf("loop B received %T, want command.Interrupt", cmd)
					}
					if ic.Ack == nil || ic.Header.CommandID.IsZero() {
						t.Fatalf("loop B Interrupt malformed: ack=%v id=%v", ic.Ack, ic.Header.CommandID)
					}
					if ic.Header.Agency != identity.AgencyUser {
						t.Fatalf("loop B Interrupt Agency = %v, want AgencyUser", ic.Header.Agency)
					}
					ackB = ic.Ack
				case <-time.After(2 * time.Second):
					t.Fatal("a loop never received its Interrupt command (Interrupt did not reach every loop)")
				}
			}
			if ackA == nil {
				t.Fatal("primary loop A never received an Interrupt command")
			}
			if ackB == nil {
				t.Fatal("sub-loop B never received an Interrupt command (prior primary-only Interrupt leaves it running)")
			}

			// Both actors ack their table value.
			ackA <- tt.ackA
			ackB <- tt.ackB

			select {
			case err := <-errCh:
				if err != nil {
					t.Fatalf("Interrupt returned %v, want nil", err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("Interrupt never returned after both loops acked")
			}
			any := <-resCh
			if any != tt.wantAny {
				t.Fatalf("Interrupt returned %v, want %v (ackA=%v ackB=%v)", any, tt.wantAny, tt.ackA, tt.ackB)
			}
		})
	}
}

// TestInterruptLoopIDTargetsRightLoop is the point of Task 10: interruptLoopID is
// the per-loop, machine-originated interrupt the subagent drain uses as a fail-safe.
// It must resolve the addressed loop id and send a command.Interrupt to THAT loop —
// the SUB-loop B here, never the primary A — carrying Agency=AgencyMachine (a
// programmatic action, not a human pressing interrupt). With two fake loops, the
// only way to prove correct routing is that B's command channel receives the
// Interrupt while A's does not, and that the Header.Agency is the machine zero value.
func TestInterruptLoopIDTargetsRightLoop(t *testing.T) {
	t.Parallel()
	s, _, subLoopID, cmdsA, cmdsB := sessionWithTwoFakeLoops()

	errCh := make(chan error, 1)
	go func() { errCh <- s.interruptLoopID(subLoopID) }()

	select {
	case cmd := <-cmdsB:
		ic, ok := cmd.(command.Interrupt)
		if !ok {
			t.Fatalf("sub-loop B received %T, want command.Interrupt", cmd)
		}
		if ic.Header.CommandID.IsZero() {
			t.Fatalf("Interrupt CommandID is zero, want a fresh id")
		}
		if ic.Header.Agency != identity.AgencyMachine {
			t.Errorf("interruptLoopID Header.Agency = %v, want AgencyMachine (programmatic per-loop interrupt)", ic.Header.Agency)
		}
	case cmd := <-cmdsA:
		t.Fatalf("primary loop A received %T, but interruptLoopID(subLoopID) must target the SUB-loop, never the primary", cmd)
	case <-time.After(2 * time.Second):
		t.Fatal("interruptLoopID never sent an Interrupt to the addressed sub-loop")
	}

	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("interruptLoopID returned %v, want nil", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("interruptLoopID never returned after the Interrupt was delivered")
	}
}

// TestInterruptLoopIDUnknownID covers the fail-secure miss: interruptLoopID against
// an id with no registry entry must return *SessionError{SessionLoopNotFound} and
// send no command (the registered loops' channels stay empty).
func TestInterruptLoopIDUnknownID(t *testing.T) {
	t.Parallel()
	s, _, _, cmdsA, cmdsB := sessionWithTwoFakeLoops()

	err := s.interruptLoopID(mustUUID()) // an id no loop is registered under
	var se *SessionError
	if !errors.As(err, &se) || se.Kind != SessionLoopNotFound {
		t.Fatalf("interruptLoopID(unknown) err = %v, want *SessionError{SessionLoopNotFound}", err)
	}

	// No command may have been sent to either registered loop: the channels are
	// unbuffered and never read, so a send would have blocked. A non-blocking
	// receive must miss on both.
	select {
	case cmd := <-cmdsA:
		t.Fatalf("interruptLoopID(unknown) sent %T to loop A, want no command", cmd)
	case cmd := <-cmdsB:
		t.Fatalf("interruptLoopID(unknown) sent %T to loop B, want no command", cmd)
	default:
	}
}

// TestShutdownClosesAllRealLoopsAndLatchesClosing drives a real two-loop session
// to shutdown and asserts the whole-session teardown contract:
//
//	(a) BOTH the primary loop's AND the sub-loop's actor exit (Done closes) — every
//	    loop is reached, none is left running.
//	(b) WaitIdle returns hub.ErrSessionStopped — the session phase is stopped.
//	(c) a NewLoop after Shutdown fails secure with *SessionError{SessionClosing} —
//	    Shutdown latched the closing flag so no loop can register post-shutdown.
func TestShutdownClosesAllRealLoopsAndLatchesClosing(t *testing.T) {
	t.Parallel()
	s, err := New(context.Background(), cfg(&stubLLM{chunks: []content.Chunk{textChunk("x")}}))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	subID, err := s.NewLoop(loop.Provenance{}, cfg(&stubLLM{chunks: []content.Chunk{textChunk("y")}}))
	if err != nil {
		t.Fatalf("NewLoop: %v", err)
	}

	// Capture both actors' Done channels before shutdown.
	s.loopsMu.RLock()
	primaryDone := s.loops[s.primaryLoopID].backend.DoneChan()
	subDone := s.loops[subID].backend.DoneChan()
	s.loopsMu.RUnlock()

	if err := s.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}

	// (a) both actors have exited.
	select {
	case <-primaryDone:
	case <-time.After(2 * time.Second):
		t.Fatal("primary loop Done not closed after Shutdown")
	}
	select {
	case <-subDone:
	case <-time.After(2 * time.Second):
		t.Fatal("sub-loop Done not closed after Shutdown (Shutdown did not reach it)")
	}

	// (b) the session is stopped.
	if err := s.WaitIdle(context.Background()); !errors.Is(err, hub.ErrSessionStopped) {
		t.Fatalf("WaitIdle after Shutdown = %v, want hub.ErrSessionStopped", err)
	}

	// (c) no loop can register once closing is latched.
	_, err = s.NewLoop(loop.Provenance{}, cfg(&stubLLM{chunks: []content.Chunk{textChunk("z")}}))
	var se *SessionError
	if !errors.As(err, &se) || se.Kind != SessionClosing {
		t.Fatalf("NewLoop after Shutdown = %v, want *SessionError{SessionClosing}", err)
	}
}

// TestSubscribeEventsReturnsInterface is a compile-time assertion that
// *Session.SubscribeEvents returns the narrow event.Subscription interface (not
// the concrete *hub.EventSubscription). This lets consumers (e.g. pkg/serve)
// depend on pkg/event without importing pkg/hub.
func TestSubscribeEventsReturnsInterface(t *testing.T) {
	t.Parallel()
	var s *Session
	var _ func(event.EventFilter) (event.Subscription, error) = s.SubscribeEvents
}
