package loop

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/inventivepotter/urvi/internal/agent/loop/command"
	"github.com/inventivepotter/urvi/internal/agent/loop/event"
	"github.com/inventivepotter/urvi/internal/content"
	"github.com/inventivepotter/urvi/internal/llm"
	"github.com/inventivepotter/urvi/internal/uuid"
)

type captureSink struct {
	mu  sync.Mutex
	got []event.EventEnvelope
}

func (s *captureSink) OnEvent(_ context.Context, e event.EventEnvelope) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.got = append(s.got, e)
}
func (s *captureSink) events() []event.EventEnvelope {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]event.EventEnvelope(nil), s.got...)
}

// panicSink panics on every OnEvent; the actor must recover and keep running.
type panicSink struct{}

func (panicSink) OnEvent(context.Context, event.EventEnvelope) { panic("boom in sink") }

// newLoop starts a loop with a 200ms DrainTimeout and returns it plus the root cancel.
func newLoop(t *testing.T, client llm.LLM, sinks ...event.EventSink) (*Loop, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	id, err := uuid.New()
	if err != nil {
		t.Fatalf("uuid.New: %v", err)
	}
	l, err := New(ctx, id, Config{Client: client, Model: llm.ModelSpec{Model: "m"}, Sinks: sinks, DrainTimeout: 200 * time.Millisecond})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(cancel)
	return l, cancel
}

// startTurn sends a StartTurn and returns the events channel + abandoned closer.
func startTurn(t *testing.T, l *Loop, ctx context.Context, input []content.Block) (<-chan event.Event, func()) {
	t.Helper()
	ev := make(chan event.Event, 64)
	ack := make(chan error, 1)
	ab := make(chan struct{})
	var once sync.Once
	closeAb := func() { once.Do(func() { close(ab) }) }
	l.Commands <- command.StartTurn{Ctx: ctx, Input: input, Events: ev, Abandoned: ab, Ack: ack}
	if err := <-ack; err != nil {
		closeAb()
		t.Fatalf("StartTurn ack = %v, want nil", err)
	}
	return ev, closeAb
}

// drainToTerminal reads until a terminal event, returns it.
func drainToTerminal(t *testing.T, ev <-chan event.Event) event.Event {
	t.Helper()
	for e := range ev {
		switch e.(type) {
		case event.TurnDone, event.TurnFailed, event.TurnInterrupted:
			return e
		}
	}
	t.Fatal("events channel closed without terminal")
	return nil
}

func TestResolveDrainTimeout(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		in   time.Duration
		want time.Duration
	}{
		{"zero defaults", 0, defaultDrainTimeout},
		{"negative defaults", -1, defaultDrainTimeout},
		{"positive preserved", 250 * time.Millisecond, 250 * time.Millisecond},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := resolveDrainTimeout(tt.in); got != tt.want {
				t.Errorf("resolveDrainTimeout(%v) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}

func TestNew_Validation(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	id, _ := uuid.New()

	t.Run("missing client", func(t *testing.T) {
		t.Parallel()
		_, err := New(ctx, id, Config{Model: llm.ModelSpec{Model: "m"}})
		var ce *ConfigError
		if !errors.As(err, &ce) || ce.Kind != ConfigMissingClient {
			t.Fatalf("err = %v, want *ConfigError{ConfigMissingClient}", err)
		}
	})
	t.Run("invalid model unwraps to ValidationError", func(t *testing.T) {
		t.Parallel()
		bad := llm.ModelSpec{Model: "m", ThinkingBudget: 1, Temperature: func() *float64 { f := 0.5; return &f }()}
		_, err := New(ctx, id, Config{Client: &fakeLLM{}, Model: bad})
		var ce *ConfigError
		if !errors.As(err, &ce) || ce.Kind != ConfigInvalidModel {
			t.Fatalf("err = %v, want *ConfigError{ConfigInvalidModel}", err)
		}
		var ve *llm.ValidationError
		if !errors.As(err, &ve) {
			t.Fatalf("err does not unwrap to *llm.ValidationError")
		}
	})
}

func TestSingleTurn(t *testing.T) {
	t.Parallel()
	l, _ := newLoop(t, &fakeLLM{chunks: []content.Chunk{textChunk("hi")}})
	ev, _ := startTurn(t, l, context.Background(), nil)
	terminal := drainToTerminal(t, ev)
	if _, ok := terminal.(event.TurnDone); !ok {
		t.Fatalf("terminal = %T, want TurnDone", terminal)
	}
	// actor is idle again: a second turn is accepted
	ev2, _ := startTurn(t, l, context.Background(), nil)
	if _, ok := drainToTerminal(t, ev2).(event.TurnDone); !ok {
		t.Fatal("second turn not accepted/idle")
	}
}

// TestEnvelopeCorrelationStamped drives one full turn and asserts that the
// sink-side envelopes carry correlation identity: every envelope for the turn
// shares the same non-zero TurnID, each EventID is distinct and non-zero, and
// CausationID equals the issuing StartTurn's Header.ID. The bare per-turn events
// are unchanged; only the envelope gains these fields.
func TestEnvelopeCorrelationStamped(t *testing.T) {
	t.Parallel()
	sink := &captureSink{}
	l, _ := newLoop(t, &fakeLLM{chunks: []content.Chunk{textChunk("hi")}}, sink)

	cmdID, err := uuid.New()
	if err != nil {
		t.Fatalf("uuid.New: %v", err)
	}
	ev := make(chan event.Event, 64)
	ack := make(chan error, 1)
	ab := make(chan struct{})
	defer close(ab)
	l.Commands <- command.StartTurn{
		Header:    command.Header{ID: cmdID},
		Ctx:       context.Background(),
		Input:     nil,
		Events:    ev,
		Abandoned: ab,
		Ack:       ack,
	}
	if err := <-ack; err != nil {
		t.Fatalf("StartTurn ack = %v, want nil", err)
	}
	if _, ok := drainToTerminal(t, ev).(event.TurnDone); !ok {
		t.Fatal("terminal != TurnDone")
	}

	// Collect only the turn's envelopes (skip the session-level SessionStarted,
	// which has no active turn so carries zero TurnID/CausationID).
	var turnEnvs []event.EventEnvelope
	for _, e := range sink.events() {
		if _, ok := e.Event.(event.SessionStarted); ok {
			continue
		}
		turnEnvs = append(turnEnvs, e)
	}
	if len(turnEnvs) == 0 {
		t.Fatal("no turn envelopes captured")
	}

	// Each correlation property is a named sub-assertion over the same captured
	// turn envelopes; running them as a table keeps the single-turn flow intact
	// while satisfying the table-driven convention.
	checks := []struct {
		name   string
		assert func(t *testing.T, envs []event.EventEnvelope)
	}{
		{
			name: "TurnID shared and non-zero",
			assert: func(t *testing.T, envs []event.EventEnvelope) {
				turnID := envs[0].TurnID
				if turnID.IsZero() {
					t.Fatal("envelope 0: TurnID is zero")
				}
				for i, e := range envs {
					if e.TurnID != turnID {
						t.Errorf("envelope %d: TurnID = %v, want shared %v", i, e.TurnID, turnID)
					}
				}
			},
		},
		{
			name: "EventID distinct and non-zero",
			assert: func(t *testing.T, envs []event.EventEnvelope) {
				seen := make(map[uuid.UUID]struct{})
				for i, e := range envs {
					if e.EventID.IsZero() {
						t.Errorf("envelope %d: EventID is zero", i)
					}
					if _, dup := seen[e.EventID]; dup {
						t.Errorf("envelope %d: EventID %v is duplicated", i, e.EventID)
					}
					seen[e.EventID] = struct{}{}
				}
			},
		},
		{
			name: "CausationID equals StartTurn.Header.ID",
			assert: func(t *testing.T, envs []event.EventEnvelope) {
				for i, e := range envs {
					if e.CausationID != cmdID {
						t.Errorf("envelope %d: CausationID = %v, want StartTurn.ID %v", i, e.CausationID, cmdID)
					}
				}
			},
		},
		{
			name: "CallID zero (no tool call in v1)",
			assert: func(t *testing.T, envs []event.EventEnvelope) {
				for i, e := range envs {
					if !e.CallID.IsZero() {
						t.Errorf("envelope %d: CallID = %v, want zero (no tool call)", i, e.CallID)
					}
				}
			},
		},
	}
	for _, c := range checks {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			c.assert(t, turnEnvs)
		})
	}
}

// countedIDGen returns a real UUID for its first okCount calls, then fails every
// subsequent call. It is safe for concurrent use (the actor and turn goroutines
// both mint via publish). okCount = 0 fails immediately.
type countedIDGen struct {
	mu      sync.Mutex
	calls   int
	okCount int
	err     error
}

func (g *countedIDGen) gen() (uuid.UUID, error) {
	g.mu.Lock()
	g.calls++
	n := g.calls
	g.mu.Unlock()
	if n <= g.okCount {
		return uuid.New()
	}
	return uuid.UUID{}, g.err
}

// newLoopWithIDGen starts a loop wired with a custom id generator, exercising
// the crypto/rand failure branches that uuid.New cannot reach in tests.
func newLoopWithIDGen(t *testing.T, client llm.LLM, gen idGenerator, sinks ...event.EventSink) *Loop {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	id, err := uuid.New()
	if err != nil {
		t.Fatalf("uuid.New: %v", err)
	}
	l, err := New(ctx, id, Config{
		Client:       client,
		Model:        llm.ModelSpec{Model: "m"},
		Sinks:        sinks,
		DrainTimeout: 200 * time.Millisecond,
		idGen:        gen,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(cancel)
	return l
}

// TestTurnIDGenerationFailure covers branch 1: when the id generator fails while
// minting the per-turn TurnID, the turn is rejected at the gate with a typed
// *IDGenerationError, its Events channel is closed, and the actor stays usable.
func TestTurnIDGenerationFailure(t *testing.T) {
	t.Parallel()
	genErr := errors.New("rand source exhausted")
	tests := []struct {
		name    string
		okCount int // succeeds for SessionStarted EventID (call #1); TurnID (call #2) fails
	}{
		{name: "turn id mint fails", okCount: 1},
		{name: "all id mints fail", okCount: 0},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			gen := &countedIDGen{okCount: tt.okCount, err: genErr}
			l := newLoopWithIDGen(t, &fakeLLM{chunks: []content.Chunk{textChunk("hi")}}, gen.gen)

			ev := make(chan event.Event, 64)
			ack := make(chan error, 1)
			ab := make(chan struct{})
			defer close(ab)
			l.Commands <- command.StartTurn{Ctx: context.Background(), Input: nil, Events: ev, Abandoned: ab, Ack: ack}

			err := <-ack
			var ide *IDGenerationError
			if !errors.As(err, &ide) {
				t.Fatalf("ack = %v, want *IDGenerationError", err)
			}
			if !errors.Is(err, genErr) {
				t.Fatalf("ack = %v, want it to wrap the generator error", err)
			}
			if _, ok := <-ev; ok {
				t.Error("rejected turn's Events channel should be closed")
			}
		})
	}
}

// TestEventIDGenerationFailureBestEffort covers branch 3: when EventID minting
// fails after the TurnID is already minted, the turn still completes to a
// terminal and the affected envelopes carry a zero EventID (best-effort sink
// contract), rather than the turn aborting.
func TestEventIDGenerationFailureBestEffort(t *testing.T) {
	t.Parallel()
	genErr := errors.New("rand source exhausted")
	// okCount = 2: SessionStarted EventID (call #1) and TurnID (call #2) succeed;
	// every per-event EventID mint after that fails.
	gen := &countedIDGen{okCount: 2, err: genErr}
	sink := &captureSink{}
	l := newLoopWithIDGen(t, &fakeLLM{chunks: []content.Chunk{textChunk("hi")}}, gen.gen, sink)

	ev := make(chan event.Event, 64)
	ack := make(chan error, 1)
	ab := make(chan struct{})
	defer close(ab)
	l.Commands <- command.StartTurn{Ctx: context.Background(), Input: nil, Events: ev, Abandoned: ab, Ack: ack}
	if err := <-ack; err != nil {
		t.Fatalf("StartTurn ack = %v, want nil (TurnID minted; only EventID fails)", err)
	}
	// Branch 3 must not abort the turn: a terminal still arrives.
	if _, ok := drainToTerminal(t, ev).(event.TurnDone); !ok {
		t.Fatal("turn did not complete despite EventID mint failure")
	}

	// At least one turn-level envelope must carry a zero EventID (the failed mints
	// were emitted best-effort with a zero EventID rather than dropped).
	var sawTurnEnv, sawZeroEventID bool
	for _, e := range sink.events() {
		if _, ok := e.Event.(event.SessionStarted); ok {
			continue
		}
		sawTurnEnv = true
		if e.EventID.IsZero() {
			sawZeroEventID = true
		}
		// TurnID was minted before the failures, so it must remain non-zero.
		if e.TurnID.IsZero() {
			t.Error("turn envelope TurnID is zero; TurnID was minted successfully")
		}
	}
	if !sawTurnEnv {
		t.Fatal("no turn-level envelope captured")
	}
	if !sawZeroEventID {
		t.Fatal("expected at least one envelope with a zero EventID after mint failure")
	}
}

func TestStartWhileRunning(t *testing.T) {
	t.Parallel()
	// provider blocks until ctx cancel, so the first turn stays running
	l, _ := newLoop(t, &fakeLLM{blockUntilCancel: true})
	ev1, ab1 := startTurn(t, l, context.Background(), nil)
	defer ab1()

	// second StartTurn must be rejected with TurnBusyError
	ev2 := make(chan event.Event, 1)
	ack2 := make(chan error, 1)
	ab2 := make(chan struct{})
	defer close(ab2)
	l.Commands <- command.StartTurn{Ctx: context.Background(), Input: nil, Events: ev2, Abandoned: ab2, Ack: ack2}
	err := <-ack2
	var be *command.TurnBusyError
	if !errors.As(err, &be) || be.Reason != command.TurnAlreadyRunning {
		t.Fatalf("ack = %v, want *TurnBusyError{TurnAlreadyRunning}", err)
	}
	if _, ok := <-ev2; ok {
		t.Error("rejected turn's Events channel should be closed")
	}
	_ = ev1
}

func TestInterruptMidTurn(t *testing.T) {
	t.Parallel()
	l, _ := newLoop(t, &fakeLLM{blockUntilCancel: true})
	ev, _ := startTurn(t, l, context.Background(), nil)

	ack := make(chan bool, 1)
	l.Commands <- command.Interrupt{Ack: ack}
	if !<-ack {
		t.Fatal("Interrupt ack = false, want true (turn was running)")
	}
	if _, ok := drainToTerminal(t, ev).(event.TurnInterrupted); !ok {
		t.Fatal("terminal != TurnInterrupted")
	}
}

func TestInterruptIdle(t *testing.T) {
	t.Parallel()
	l, _ := newLoop(t, &fakeLLM{chunks: []content.Chunk{textChunk("x")}})
	ack := make(chan bool, 1)
	l.Commands <- command.Interrupt{Ack: ack}
	if <-ack {
		t.Fatal("Interrupt ack = true, want false (no turn running)")
	}
}

func TestShutdownIdle(t *testing.T) {
	t.Parallel()
	l, _ := newLoop(t, &fakeLLM{chunks: []content.Chunk{textChunk("x")}})
	ack := make(chan error, 1)
	l.Commands <- command.Shutdown{Ack: ack}
	if err := <-ack; err != nil {
		t.Fatalf("Shutdown ack = %v, want nil", err)
	}
	select {
	case <-l.Done:
	case <-time.After(time.Second):
		t.Fatal("Loop.Done did not close")
	}
}

func TestShutdownMidTurn(t *testing.T) {
	t.Parallel()
	l, _ := newLoop(t, &fakeLLM{blockUntilCancel: true})
	ev, _ := startTurn(t, l, context.Background(), nil)
	ack := make(chan error, 1)
	l.Commands <- command.Shutdown{Ack: ack}
	// terminal still delivered to the caller
	if _, ok := drainToTerminal(t, ev).(event.TurnInterrupted); !ok {
		t.Fatal("terminal != TurnInterrupted")
	}
	if err := <-ack; err != nil {
		t.Fatalf("Shutdown ack = %v, want nil", err)
	}
	<-l.Done
}

func TestShutdownWhileShuttingDown(t *testing.T) {
	t.Parallel()
	l, _ := newLoop(t, &fakeLLM{blockUntilCancel: true})
	ev, _ := startTurn(t, l, context.Background(), nil)
	ack1 := make(chan error, 1)
	ack2 := make(chan error, 1)
	// two Shutdowns during one running turn; both acks must receive nil
	l.Commands <- command.Shutdown{Ack: ack1}
	l.Commands <- command.Shutdown{Ack: ack2}
	if _, ok := drainToTerminal(t, ev).(event.TurnInterrupted); !ok {
		t.Fatal("terminal != TurnInterrupted")
	}
	if err := <-ack1; err != nil {
		t.Fatalf("Shutdown ack1 = %v, want nil", err)
	}
	if err := <-ack2; err != nil {
		t.Fatalf("Shutdown ack2 = %v, want nil", err)
	}
	<-l.Done
}

func TestTurnPanic(t *testing.T) {
	t.Parallel()
	l, _ := newLoop(t, panicLLM{})
	ev, _ := startTurn(t, l, context.Background(), nil)
	terminal := drainToTerminal(t, ev)
	failed, ok := terminal.(event.TurnFailed)
	if !ok {
		t.Fatalf("terminal = %T, want TurnFailed", terminal)
	}
	var pe *event.TurnPanicError
	if !errors.As(failed.Err, &pe) {
		t.Fatalf("TurnFailed.Err = %T, want *TurnPanicError", failed.Err)
	}
}

func TestStartupSinkEvent(t *testing.T) {
	t.Parallel()
	sink := &captureSink{}
	l, _ := newLoop(t, &fakeLLM{chunks: []content.Chunk{textChunk("x")}}, sink)
	// unbuffered Commands guarantees SessionStarted was published before this send returns
	ack := make(chan error, 1)
	l.Commands <- command.Shutdown{Ack: ack}
	<-ack
	<-l.Done
	got := sink.events()
	if len(got) == 0 {
		t.Fatal("no sink events")
	}
	if _, ok := got[0].Event.(event.SessionStarted); !ok {
		t.Fatalf("first sink event = %T, want SessionStarted", got[0].Event)
	}
}

func TestEventSinkPanicRecovered(t *testing.T) {
	t.Parallel()
	// a sink whose OnEvent always panics must not break the turn or the actor
	l, _ := newLoop(t, &fakeLLM{chunks: []content.Chunk{textChunk("ok")}}, panicSink{})
	ev, _ := startTurn(t, l, context.Background(), nil)
	if _, ok := drainToTerminal(t, ev).(event.TurnDone); !ok {
		t.Fatal("turn did not complete despite sink panic")
	}
	ack := make(chan error, 1)
	l.Commands <- command.Shutdown{Ack: ack}
	if err := <-ack; err != nil {
		t.Fatalf("Shutdown ack = %v, want nil", err)
	}
	<-l.Done
}

func TestInvalidStartMissingAbandoned(t *testing.T) {
	t.Parallel()
	l, _ := newLoop(t, &fakeLLM{chunks: []content.Chunk{textChunk("x")}})
	ev := make(chan event.Event, 1)
	ack := make(chan error, 1)
	l.Commands <- command.StartTurn{Ctx: context.Background(), Input: nil, Events: ev, Abandoned: nil, Ack: ack}
	err := <-ack
	var ice *command.InvalidCommandError
	if !errors.As(err, &ice) || ice.Field != command.StartTurnAbandoned {
		t.Fatalf("ack = %v, want *InvalidCommandError{Field: StartTurnAbandoned}", err)
	}
	if _, ok := <-ev; ok {
		t.Error("invalid turn's Events channel should be closed")
	}
	// actor still usable: a valid turn works
	ev2, _ := startTurn(t, l, context.Background(), nil)
	if _, ok := drainToTerminal(t, ev2).(event.TurnDone); !ok {
		t.Fatal("actor not usable after invalid StartTurn")
	}
}

func TestPerTurnCtxCancelMidTurn(t *testing.T) {
	t.Parallel()
	l, _ := newLoop(t, &fakeLLM{blockUntilCancel: true})
	turnCtx, turnCancel := context.WithCancel(context.Background())
	ev, _ := startTurn(t, l, turnCtx, nil)
	turnCancel() // cancel the per-turn ctx, not the root ctx
	if _, ok := drainToTerminal(t, ev).(event.TurnInterrupted); !ok {
		t.Fatal("terminal != TurnInterrupted")
	}
	// actor idle after: a fresh turn is accepted (provider still blocks, so
	// interrupt it to let the test finish)
	turn2Ctx, turn2Cancel := context.WithCancel(context.Background())
	ev2, _ := startTurn(t, l, turn2Ctx, nil)
	turn2Cancel()
	if _, ok := drainToTerminal(t, ev2).(event.TurnInterrupted); !ok {
		t.Fatal("second turn terminal != TurnInterrupted")
	}
}

func TestTurnFailedProviderErrorTyped(t *testing.T) {
	t.Parallel()
	provErr := &llm.ValidationError{Field: "Model", Reason: "boom"}
	l, _ := newLoop(t, &fakeLLM{streamErr: provErr})
	ev, _ := startTurn(t, l, context.Background(), nil)
	terminal := drainToTerminal(t, ev)
	failed, ok := terminal.(event.TurnFailed)
	if !ok {
		t.Fatalf("terminal = %T, want TurnFailed", terminal)
	}
	var ve *llm.ValidationError
	if !errors.As(failed.Err, &ve) {
		t.Fatalf("TurnFailed.Err = %T, want *llm.ValidationError", failed.Err)
	}
}

// TestLeakedReaderDoesNotWedgeActor guards review fix #1: deliverAndClose's
// ctx.Done() escape. The caller leaks the stream (never reads Events past the
// buffer, never closes Abandoned). Events is buffered to EXACTLY the count of
// non-terminal events (TurnStarted + 1 TokenDelta) so the turn goroutine
// completes and the actor enters deliverAndClose, where the terminal send
// blocks on the full unread buffer. Only the ctx.Done() escape can free it.
func TestLeakedReaderDoesNotWedgeActor(t *testing.T) {
	t.Parallel()
	sink := &captureSink{}
	ctx, cancel := context.WithCancel(context.Background())
	id, _ := uuid.New()
	l, err := New(ctx, id, Config{
		Client:       &fakeLLM{chunks: []content.Chunk{textChunk("a")}},
		Model:        llm.ModelSpec{Model: "m"},
		Sinks:        []event.EventSink{sink},
		DrainTimeout: 100 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ev := make(chan event.Event, 2) // exactly TurnStarted + 1 TokenDelta; terminal cannot fit
	ack := make(chan error, 1)
	ab := make(chan struct{}) // never closed -> leaked reader
	l.Commands <- command.StartTurn{Ctx: context.Background(), Input: nil, Events: ev, Abandoned: ab, Ack: ack}
	if err := <-ack; err != nil {
		t.Fatalf("ack = %v", err)
	}

	// Wait until the actor has published the terminal (it does so inside
	// deliverAndClose, immediately before blocking on the full Events buffer).
	deadline := time.After(2 * time.Second)
	for {
		if hasTerminal(sink.events()) {
			break
		}
		select {
		case <-deadline:
			t.Fatal("actor never reached deliverAndClose (no terminal published)")
		case <-time.After(2 * time.Millisecond):
		}
	}

	cancel() // only the deliverAndClose ctx.Done() escape can free the parked actor
	select {
	case <-l.Done:
	case <-time.After(2 * time.Second):
		t.Fatal("actor wedged in deliverAndClose: Loop.Done never closed after root-ctx cancel")
	}
}

func hasTerminal(evs []event.EventEnvelope) bool {
	for _, e := range evs {
		switch e.Event.(type) {
		case event.TurnDone, event.TurnFailed, event.TurnInterrupted:
			return true
		}
	}
	return false
}

// REGRESSION GUARD (review fix #4): a provider that ignores ctx must not pin the actor on hard kill.
func TestCtxIgnoringProviderDoesNotPinActor(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	id, _ := uuid.New()
	l, err := New(ctx, id, Config{
		Client:       &fakeLLM{blockUntilCancel: true, ignoreCtx: true},
		Model:        llm.ModelSpec{Model: "m"},
		DrainTimeout: 100 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ev := make(chan event.Event, 64)
	ack := make(chan error, 1)
	ab := make(chan struct{})
	defer close(ab)
	l.Commands <- command.StartTurn{Ctx: context.Background(), Input: nil, Events: ev, Abandoned: ab, Ack: ack}
	<-ack
	cancel()
	select {
	case <-l.Done:
	case <-time.After(2 * time.Second):
		t.Fatal("actor pinned by ctx-ignoring provider; Done never closed")
	}
}

// REGRESSION GUARD (review fix #3): failed turn rolls back; next turn sees no doubled user message.
func TestFailedTurnRollsBackHistory(t *testing.T) {
	t.Parallel()
	rec := &recordingLLM{} // empty chunks -> EmptyResponseError on first turn
	l, _ := newLoop(t, rec)

	// turn 1 fails (empty response) and must roll back the user message
	ev1, _ := startTurn(t, l, context.Background(), []content.Block{&content.TextBlock{Text: "first"}})
	if _, ok := drainToTerminal(t, ev1).(event.TurnFailed); !ok {
		t.Fatal("turn 1 terminal != TurnFailed")
	}
	// turn 2: its request must NOT contain two consecutive user messages from turn 1
	rec.chunks = []content.Chunk{textChunk("ok")}
	ev2, _ := startTurn(t, l, context.Background(), []content.Block{&content.TextBlock{Text: "second"}})
	drainToTerminal(t, ev2)
	req := rec.lastReq()
	if len(req.Messages) != 1 {
		t.Fatalf("turn 2 request had %d messages, want 1 (rolled-back history)", len(req.Messages))
	}
}
