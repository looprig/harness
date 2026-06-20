package loop

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/inventivepotter/urvi/internal/agent/loop/command"
	"github.com/inventivepotter/urvi/internal/agent/loop/event"
	"github.com/inventivepotter/urvi/internal/content"
	"github.com/inventivepotter/urvi/internal/llm"
	"github.com/inventivepotter/urvi/internal/tool"
	"github.com/inventivepotter/urvi/internal/uuid"
)

// mustID returns a fresh UUID or fails the test.
func mustID(t *testing.T) uuid.UUID {
	t.Helper()
	u, err := uuid.New()
	if err != nil {
		t.Fatalf("uuid.New: %v", err)
	}
	return u
}

// noopPublisher is a no-op eventPublisher for loop tests that do not assert on
// the session fan-in. New stores it in loopConfig.events; PublishEvent simply
// drops the event, which is sufficient to satisfy the New signature.
type noopPublisher struct{}

func (noopPublisher) PublishEvent(context.Context, event.Event) error { return nil }

// recordingPublisher is an eventPublisher that records every published full-
// fidelity event.Event (no envelope, no redaction — exactly what the production
// hub sees). It receives events from the actor goroutine, so it is guarded by a
// mutex. events() returns a copy so a reader never aliases the live slice the
// actor is appending to.
type recordingPublisher struct {
	mu  sync.Mutex
	got []event.Event
}

func (r *recordingPublisher) PublishEvent(_ context.Context, ev event.Event) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.got = append(r.got, ev)
	return nil
}

func (r *recordingPublisher) events() []event.Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]event.Event(nil), r.got...)
}

// blockUntilEvents polls the recording publisher's events until pred is
// satisfied, or fails after ~2s.
func blockUntilEvents(t *testing.T, rec *recordingPublisher, pred func([]event.Event) bool) {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		if pred(rec.events()) {
			return
		}
		select {
		case <-deadline:
			t.Fatal("recorded-events condition not met within deadline")
		case <-time.After(2 * time.Millisecond):
		}
	}
}

// newLoopRec is like newLoop but injects a recordingPublisher (instead of
// noopPublisher{}) as the event publisher and returns it, so a test can observe
// the full-fidelity events the production hub would see. It uses a 200ms
// DrainTimeout; the root cancel is registered with t.Cleanup and also returned
// for callers that cancel explicitly.
func newLoopRec(t *testing.T, client llm.LLM) (*Loop, *recordingPublisher, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	rec := &recordingPublisher{}
	l, err := New(ctx, mustID(t), mustID(t), Provenance{}, rec, Config{
		Client:       client,
		Model:        llm.ModelSpec{Model: "m"},
		DrainTimeout: 200 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(cancel)
	return l, rec, cancel
}

// newLoop starts a loop with a 200ms DrainTimeout and returns it plus the root cancel.
func newLoop(t *testing.T, client llm.LLM) (*Loop, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	sessionID, err := uuid.New()
	if err != nil {
		t.Fatalf("uuid.New: %v", err)
	}
	loopID, err := uuid.New()
	if err != nil {
		t.Fatalf("uuid.New: %v", err)
	}
	l, err := New(ctx, sessionID, loopID, Provenance{}, noopPublisher{}, Config{Client: client, Model: llm.ModelSpec{Model: "m"}, DrainTimeout: 200 * time.Millisecond})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(cancel)
	return l, cancel
}

// startTurn sends a StartOnly UserInput and returns the events channel + abandoned
// closer. It asserts the loop STARTED the turn by reading the first event off the
// per-turn stream and requiring it to be event.TurnStarted (the start-or-reject path
// now delivers its outcome on the stream — TurnStarted on success, TurnRejected on a
// refusal — instead of a command.Disposition reply). The peeked TurnStarted is NOT
// re-injected, mirroring production Stream's re-yield only at the session boundary;
// loop tests that need the TurnStarted from the stream read it before calling this or
// observe it via a recordingPublisher. The ctx parameter is retained for source compatibility with
// callers but is unused: submit commands carry no context, and the turn ctx derives
// from loopCtx.
func startTurn(t *testing.T, l *Loop, _ context.Context, input []content.Block) (<-chan event.Event, func()) {
	t.Helper()
	ev := make(chan event.Event, 64)
	ab := make(chan struct{})
	var once sync.Once
	closeAb := func() { once.Do(func() { close(ab) }) }
	l.Commands <- command.UserInput{Mode: command.StartOnly, Blocks: input, Events: ev, Abandoned: ab}
	select {
	case first, ok := <-ev:
		if !ok {
			closeAb()
			t.Fatal("UserInput(StartOnly) per-turn stream closed without TurnStarted (rejected)")
		}
		if _, ok := first.(event.TurnStarted); !ok {
			closeAb()
			t.Fatalf("UserInput(StartOnly) first event = %T, want event.TurnStarted", first)
		}
	case <-time.After(2 * time.Second):
		closeAb()
		t.Fatal("UserInput(StartOnly): no event on per-turn stream within deadline")
	}
	return ev, closeAb
}

// sendCmd sends cmd to the loop with the same Done escape every production sender
// (Session.routeGate) uses: once the actor has shut down it
// stops reading Commands, so a raw unbuffered send to a stopped actor wedges
// forever. Tests that send a command which MAY race the actor's exit (a second
// Shutdown after the first) must use this escape; a raw send is correct only when
// the actor is guaranteed to still be reading. It reports whether the send landed.
func sendCmd(t *testing.T, l *Loop, cmd command.Command) bool {
	t.Helper()
	select {
	case l.Commands <- cmd:
		return true
	case <-l.Done:
		return false
	}
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

// TestRecordingPublisherObservesTurnStarted proves the recordingPublisher
// observes a real loop event: a turn started via the fakeLLM client publishes an
// event.TurnStarted onto the event publisher (the production fan-in path), which
// the recorder captures. This is the migration target for tests that previously
// observed loop events through a sink.
func TestRecordingPublisherObservesTurnStarted(t *testing.T) {
	t.Parallel()
	l, rec, _ := newLoopRec(t, &fakeLLM{chunks: []content.Chunk{textChunk("hi")}})
	ev, _ := startTurn(t, l, context.Background(), nil)
	t.Cleanup(func() {
		for range ev { // drain so the actor's emit/publish path never wedges
		}
	})

	blockUntilEvents(t, rec, func(evs []event.Event) bool {
		for _, e := range evs {
			if _, ok := e.(event.TurnStarted); ok {
				return true
			}
		}
		return false
	})
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
	sessionID, _ := uuid.New()
	loopID, _ := uuid.New()

	t.Run("missing client", func(t *testing.T) {
		t.Parallel()
		_, err := New(ctx, sessionID, loopID, Provenance{}, noopPublisher{}, Config{Model: llm.ModelSpec{Model: "m"}})
		var ce *ConfigError
		if !errors.As(err, &ce) || ce.Kind != ConfigMissingClient {
			t.Fatalf("err = %v, want *ConfigError{ConfigMissingClient}", err)
		}
	})
	t.Run("invalid model unwraps to ValidationError", func(t *testing.T) {
		t.Parallel()
		bad := llm.ModelSpec{Model: "m", ThinkingBudget: 1, Temperature: func() *float64 { f := 0.5; return &f }()}
		_, err := New(ctx, sessionID, loopID, Provenance{}, noopPublisher{}, Config{Client: &fakeLLM{}, Model: bad})
		var ce *ConfigError
		if !errors.As(err, &ce) || ce.Kind != ConfigInvalidModel {
			t.Fatalf("err = %v, want *ConfigError{ConfigInvalidModel}", err)
		}
		var ve *llm.ValidationError
		if !errors.As(err, &ve) {
			t.Fatalf("err does not unwrap to *llm.ValidationError")
		}
	})
	t.Run("nil publisher", func(t *testing.T) {
		t.Parallel()
		_, err := New(ctx, sessionID, loopID, Provenance{}, nil, Config{Client: &fakeLLM{}, Model: llm.ModelSpec{Model: "m"}})
		var ce *ConfigError
		if !errors.As(err, &ce) || ce.Kind != ConfigMissingPublisher {
			t.Fatalf("err = %v, want *ConfigError{ConfigMissingPublisher}", err)
		}
	})
}

// TestNewLoopState asserts the constructor carries the loop identity (sessionID,
// loopID, parent provenance) and the event publisher onto loopState, and always
// initializes pendingGates so the actor never panics on a nil map.
func TestNewLoopState(t *testing.T) {
	t.Parallel()
	sessionID, _ := uuid.New()
	loopID, _ := uuid.New()
	parentLoop, _ := uuid.New()
	parentTurn, _ := uuid.New()
	parentStep, _ := uuid.New()

	tests := []struct {
		name      string
		sessionID uuid.UUID
		loopID    uuid.UUID
		parent    Provenance
	}{
		{
			name:      "primary loop (zero parent)",
			sessionID: sessionID,
			loopID:    loopID,
			parent:    Provenance{},
		},
		{
			name:      "subagent loop (non-zero parent)",
			sessionID: sessionID,
			loopID:    loopID,
			parent:    Provenance{LoopID: parentLoop, TurnID: parentTurn, StepID: parentStep},
		},
		{
			name:      "zero session and loop ids",
			sessionID: uuid.UUID{},
			loopID:    uuid.UUID{},
			parent:    Provenance{},
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			// The session event publisher is a dependency (loopConfig.events), no longer
			// loopState — newLoopState carries identity only.
			st := newLoopState(tt.sessionID, tt.loopID, tt.parent)
			if st.sessionID != tt.sessionID {
				t.Errorf("sessionID = %v, want %v", st.sessionID, tt.sessionID)
			}
			if st.id != tt.loopID {
				t.Errorf("id = %v, want %v", st.id, tt.loopID)
			}
			if st.parent != tt.parent {
				t.Errorf("parent = %+v, want %+v", st.parent, tt.parent)
			}
			if st.pendingGates == nil {
				t.Error("pendingGates is nil, want an initialized map")
			}
		})
	}
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

// TestTurnEventCorrelationStamped drives one full turn and asserts that the
// full-fidelity events the session fan-in sees carry correlation identity in their
// own Header: every turn event shares the same non-zero TurnID, the Reply event
// (TurnStarted) carries Cause.CommandID == the issuing UserInput's Header.ID, and no
// event carries a tool-call id (ToolExecutionID is zero — there is no tool call in this
// turn). These are the producer-stamped Header fields the recordingPublisher
// observes via cfg.events.PublishEvent (the production hub path).
func TestTurnEventCorrelationStamped(t *testing.T) {
	t.Parallel()
	l, rec, _ := newLoopRec(t, &fakeLLM{chunks: []content.Chunk{textChunk("hi")}})

	cmdID, err := uuid.New()
	if err != nil {
		t.Fatalf("uuid.New: %v", err)
	}
	ev := make(chan event.Event, 64)
	ab := make(chan struct{})
	defer close(ab)
	l.Commands <- command.UserInput{
		Header:    command.Header{CommandID: cmdID},
		Mode:      command.StartOnly,
		Blocks:    nil,
		Events:    ev,
		Abandoned: ab,
	}
	// The start-or-reject outcome is the first per-turn event: TurnStarted on success.
	if first, ok := <-ev; !ok {
		t.Fatal("per-turn stream closed without TurnStarted (rejected)")
	} else if _, ok := first.(event.TurnStarted); !ok {
		t.Fatalf("first per-turn event = %T, want event.TurnStarted", first)
	}
	if _, ok := drainToTerminal(t, ev).(event.TurnDone); !ok {
		t.Fatal("terminal != TurnDone")
	}

	// Collect only the turn's events. Skip the loop-scoped LoopIdle (the
	// running->idle announcement): it has no active turn, so it carries a zero
	// TurnID by design and is not a turn event. The loop never emits SessionStarted
	// (the session owns it), so there is nothing to skip there.
	var turnEvents []event.Event
	for _, e := range rec.events() {
		if _, ok := e.(event.LoopIdle); ok {
			continue
		}
		turnEvents = append(turnEvents, e)
	}
	if len(turnEvents) == 0 {
		t.Fatal("no turn events captured")
	}

	// Each correlation property is a named sub-assertion over the same captured
	// turn events; running them as a table keeps the single-turn flow intact
	// while satisfying the table-driven convention.
	checks := []struct {
		name   string
		assert func(t *testing.T, evs []event.Event)
	}{
		{
			name: "TurnID shared and non-zero",
			assert: func(t *testing.T, evs []event.Event) {
				turnID := evs[0].EventHeader().TurnID
				if turnID.IsZero() {
					t.Fatal("event 0: TurnID is zero")
				}
				for i, e := range evs {
					if e.EventHeader().TurnID != turnID {
						t.Errorf("event %d (%T): TurnID = %v, want shared %v", i, e, e.EventHeader().TurnID, turnID)
					}
				}
			},
		},
		{
			// Cause.CommandID is a Reply-event property (ReplyTo() == Cause.CommandID). Of this
			// turn's events only the Reply (TurnStarted) carries it; the other turn
			// events (TokenDelta/StepDone/TurnDone) leave it zero on their own Header.
			// At least one Reply must be present and every Reply must point back to the
			// issuing UserInput.
			name: "Reply Cause.CommandID equals UserInput.Header.ID",
			assert: func(t *testing.T, evs []event.Event) {
				var sawReply bool
				for i, e := range evs {
					r, ok := e.(event.Reply)
					if !ok {
						continue
					}
					sawReply = true
					if r.ReplyTo() != cmdID {
						t.Errorf("event %d (%T): ReplyTo/Cause.CommandID = %v, want UserInput.ID %v", i, e, r.ReplyTo(), cmdID)
					}
				}
				if !sawReply {
					t.Fatal("no Reply event captured; expected at least TurnStarted")
				}
			},
		},
	}
	for _, c := range checks {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			c.assert(t, turnEvents)
		})
	}
}

// countedIDGen returns a real UUID for its first okCount calls, then fails every
// subsequent call. It is safe for concurrent use (the actor mints the TurnID and
// the turn goroutine mints step/tool-call ids). okCount = 0 fails immediately.
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
func newLoopWithIDGen(t *testing.T, client llm.LLM, gen idGenerator) *Loop {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	sessionID, err := uuid.New()
	if err != nil {
		t.Fatalf("uuid.New: %v", err)
	}
	loopID, err := uuid.New()
	if err != nil {
		t.Fatalf("uuid.New: %v", err)
	}
	l, err := New(ctx, sessionID, loopID, Provenance{}, noopPublisher{}, Config{
		Client:       client,
		Model:        llm.ModelSpec{Model: "m"},
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
// minting the per-turn TurnID for a StartOnly submit, the turn does not start, the
// actor replies TurnRejected{RejectInternal} (fail-secure: it cannot mint a TurnID,
// so it declines the work — but the loop is healthy and the caller MAY retry, so
// the reason is the transient RejectInternal, NOT RejectShuttingDown), the Events
// channel is closed, and the actor stays usable.
func TestTurnIDGenerationFailure(t *testing.T) {
	t.Parallel()
	genErr := errors.New("rand source exhausted")
	tests := []struct {
		name    string
		okCount int // TurnID is the first id minted for the submit; okCount 0 fails it
	}{
		{name: "turn id mint fails", okCount: 0},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			gen := &countedIDGen{okCount: tt.okCount, err: genErr}
			l := newLoopWithIDGen(t, &fakeLLM{chunks: []content.Chunk{textChunk("hi")}}, gen.gen)

			ev := make(chan event.Event, 64)
			ab := make(chan struct{})
			defer close(ab)
			l.Commands <- command.UserInput{Mode: command.StartOnly, Blocks: nil, Events: ev, Abandoned: ab}

			// The id-gen failure is surfaced on the per-turn stream as TurnRejected{Internal},
			// then the stream is closed.
			first, ok := <-ev
			if !ok {
				t.Fatal("per-turn stream closed without a TurnRejected event")
			}
			rej, ok := first.(event.TurnRejected)
			if !ok {
				t.Fatalf("first per-turn event = %T, want event.TurnRejected (id-gen failure)", first)
			}
			if rej.Reason != event.RejectInternal {
				t.Fatalf("reject reason = %d, want RejectInternal (transient id-gen failure, not ShuttingDown)", rej.Reason)
			}
			if _, open := <-ev; open {
				t.Error("rejected turn's Events channel should be closed")
			}
		})
	}
}

func TestStartWhileRunning(t *testing.T) {
	t.Parallel()
	// provider blocks until ctx cancel, so the first turn stays running
	l, _ := newLoop(t, &fakeLLM{blockUntilCancel: true})
	ev1, ab1 := startTurn(t, l, context.Background(), nil)
	defer ab1()

	// second StartOnly submit must be rejected with TurnRejected{RejectBusy} on its
	// per-turn stream, which is then closed.
	ev2 := make(chan event.Event, 1)
	ab2 := make(chan struct{})
	defer close(ab2)
	l.Commands <- command.UserInput{Mode: command.StartOnly, Blocks: nil, Events: ev2, Abandoned: ab2}
	first, ok := <-ev2
	if !ok {
		t.Fatal("rejected turn's per-turn stream closed without a TurnRejected event")
	}
	rej, ok := first.(event.TurnRejected)
	if !ok || rej.Reason != event.RejectBusy {
		t.Fatalf("per-turn outcome = %+v, want event.TurnRejected{RejectBusy}", first)
	}
	if _, open := <-ev2; open {
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
	// First Shutdown during the running turn: the actor is guaranteed to be reading
	// Commands, so a raw send is safe and the ack always receives nil.
	l.Commands <- command.Shutdown{Ack: ack1}
	// Second Shutdown RACES the actor's exit: once the turn terminal arrives and the
	// actor sees it is shutting down, it acks the shutdowns it has and returns,
	// stopping its Commands read. A raw blocking send here would wedge forever if the
	// actor exits first (the pre-existing flake). Escape on Done exactly as the
	// production senders do; if the send lands the actor is still draining and ack2
	// receives nil, otherwise the actor already exited (and ack1 covered the stop).
	landed := sendCmd(t, l, command.Shutdown{Ack: ack2})
	if _, ok := drainToTerminal(t, ev).(event.TurnInterrupted); !ok {
		t.Fatal("terminal != TurnInterrupted")
	}
	if err := <-ack1; err != nil {
		t.Fatalf("Shutdown ack1 = %v, want nil", err)
	}
	if landed {
		if err := <-ack2; err != nil {
			t.Fatalf("Shutdown ack2 = %v, want nil", err)
		}
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

// awaitReply polls the recorded full-fidelity events for the published
// event.Reply whose ReplyTo() == inputID (the submit's outcome: TurnStarted,
// InputQueued, or TurnRejected) and returns it, failing if none lands within the
// deadline.
func awaitReply(t *testing.T, rec *recordingPublisher, inputID uuid.UUID) event.Event {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		for _, e := range rec.events() {
			if r, ok := e.(event.Reply); ok && r.ReplyTo() == inputID {
				return e
			}
		}
		select {
		case <-deadline:
			t.Fatalf("no Reply for input %v within deadline", inputID)
			return nil
		case <-time.After(2 * time.Millisecond):
		}
	}
}

// TestFanInOnlyUserInputStartsTurn proves a fan-in-only AllowFold UserInput (nil
// Events/Abandoned) starts a turn and runs it to completion through the fan-in
// only: emit and deliverAndClose are nil-safe (no send-on-nil, no close-nil). The
// published TurnStarted and the fan-in-observed TurnDone are the only evidence the
// turn ran, since there is no per-turn stream.
func TestFanInOnlyUserInputStartsTurn(t *testing.T) {
	t.Parallel()
	l, rec, _ := newLoopRec(t, &fakeLLM{chunks: []content.Chunk{textChunk("hi")}})

	id := mustID(t)
	l.Commands <- command.UserInput{Header: command.Header{CommandID: id}, Mode: command.AllowFold, Blocks: nil} // nil Events/Abandoned
	if _, ok := awaitReply(t, rec, id).(event.TurnStarted); !ok {
		t.Fatal("fan-in-only AllowFold submit did not publish TurnStarted")
	}
	// The turn runs to a TurnDone observed on the fan-in only (no per-turn channel).
	blockUntilEvents(t, rec, hasTerminal)
	// Actor is usable afterward.
	id2 := mustID(t)
	l.Commands <- command.UserInput{Header: command.Header{CommandID: id2}, Mode: command.AllowFold, Blocks: nil}
	if _, ok := awaitReply(t, rec, id2).(event.TurnStarted); !ok {
		t.Fatal("actor not usable after a fan-in-only turn")
	}
}

// TestInterruptCancelsTurn replaces the old per-turn-ctx cancellation test: submit
// commands no longer carry a context, so a running turn is cancelled via
// command.Interrupt. The actor cancels the turn ctx and the turn ends
// TurnInterrupted; the actor stays usable for a second turn.
func TestInterruptCancelsTurn(t *testing.T) {
	t.Parallel()
	l, _ := newLoop(t, &fakeLLM{blockUntilCancel: true})
	ev, _ := startTurn(t, l, context.Background(), nil)
	iack := make(chan bool, 1)
	l.Commands <- command.Interrupt{Ack: iack}
	if !<-iack {
		t.Fatal("Interrupt ack = false, want true (turn running)")
	}
	if _, ok := drainToTerminal(t, ev).(event.TurnInterrupted); !ok {
		t.Fatal("terminal != TurnInterrupted")
	}
	// actor idle after: a fresh turn is accepted, then interrupted to finish.
	ev2, _ := startTurn(t, l, context.Background(), nil)
	iack2 := make(chan bool, 1)
	l.Commands <- command.Interrupt{Ack: iack2}
	<-iack2
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
// non-terminal events (TurnStarted + 1 TokenDelta + 1 StepDone for the single
// no-tool step) so the turn goroutine completes and the actor enters
// deliverAndClose, where the terminal send blocks on the full unread buffer. Only
// the ctx.Done() escape can free it.
func TestLeakedReaderDoesNotWedgeActor(t *testing.T) {
	t.Parallel()
	rec := &recordingPublisher{}
	ctx, cancel := context.WithCancel(context.Background())
	sessionID, _ := uuid.New()
	loopID, _ := uuid.New()
	l, err := New(ctx, sessionID, loopID, Provenance{}, rec, Config{
		Client:       &fakeLLM{chunks: []content.Chunk{textChunk("a")}},
		Model:        llm.ModelSpec{Model: "m"},
		DrainTimeout: 100 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ev := make(chan event.Event, 3) // exactly TurnStarted + 1 TokenDelta + 1 StepDone; terminal cannot fit
	ab := make(chan struct{})       // never closed -> leaked reader
	// Leaked reader: never read from ev (so the buffer fills) and never close ab. The
	// turn's TurnStarted is the first event on ev; we do NOT consume it (consuming
	// would free a buffer slot and let the terminal fit, defeating the test).
	l.Commands <- command.UserInput{Mode: command.StartOnly, Blocks: nil, Events: ev, Abandoned: ab}

	// Wait until the actor has published the terminal (it does so inside
	// deliverAndClose, immediately before blocking on the full Events buffer).
	blockUntilEvents(t, rec, hasTerminal)

	cancel() // only the deliverAndClose ctx.Done() escape can free the parked actor
	select {
	case <-l.Done:
	case <-time.After(2 * time.Second):
		t.Fatal("actor wedged in deliverAndClose: Loop.Done never closed after root-ctx cancel")
	}
}

func hasTerminal(evs []event.Event) bool {
	for _, ev := range evs {
		if ev.EndsTurn() {
			return true
		}
	}
	return false
}

// REGRESSION GUARD (review fix #4): a provider that ignores ctx must not pin the actor on hard kill.
func TestCtxIgnoringProviderDoesNotPinActor(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	sessionID, _ := uuid.New()
	loopID, _ := uuid.New()
	l, err := New(ctx, sessionID, loopID, Provenance{}, noopPublisher{}, Config{
		Client:       &fakeLLM{blockUntilCancel: true, ignoreCtx: true},
		Model:        llm.ModelSpec{Model: "m"},
		DrainTimeout: 100 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ev := make(chan event.Event, 64)
	ab := make(chan struct{})
	defer close(ab)
	l.Commands <- command.UserInput{Mode: command.StartOnly, Blocks: nil, Events: ev, Abandoned: ab}
	<-ev // wait for TurnStarted (the turn is running) before cancelling
	cancel()
	select {
	case <-l.Done:
	case <-time.After(2 * time.Second):
		t.Fatal("actor pinned by ctx-ignoring provider; Done never closed")
	}
}

// TestStepGranularityRollback replaces the old whole-turn-rollback guard. With
// loop-owned incremental commit, a TurnFailed/TurnInterrupted discards ONLY the
// in-flight incomplete step; the initial UserMessage (committed at TurnStarted) and
// every completed step (committed with its StepDone) STAY in loopState.msgs. A
// terminal means "the turn stopped here," not "the turn never happened."
func TestStepGranularityRollback(t *testing.T) {
	t.Parallel()

	t.Run("fail at step 0 keeps the committed initial UserMessage (no whole-turn rollback)", func(t *testing.T) {
		t.Parallel()
		rec := &recordingLLM{} // empty chunks -> EmptyResponseError on the first step
		l, _ := newLoop(t, rec)

		// turn 1 fails on an empty response: step 0 never completes, so its (absent)
		// AIMessage is discarded — but the initial UserMessage was committed at
		// TurnStarted and stays.
		ev1, _ := startTurn(t, l, context.Background(), []content.Block{&content.TextBlock{Text: "first"}})
		if _, ok := drainToTerminal(t, ev1).(event.TurnFailed); !ok {
			t.Fatal("turn 1 terminal != TurnFailed")
		}
		// turn 2's request must contain turn 1's committed user message THEN turn 2's
		// user message = 2 messages. The committed history grows; it is not rolled back.
		rec.chunks = []content.Chunk{textChunk("ok")}
		ev2, _ := startTurn(t, l, context.Background(), []content.Block{&content.TextBlock{Text: "second"}})
		drainToTerminal(t, ev2)
		req := rec.lastReq()
		if len(req.Messages) != 2 {
			t.Fatalf("turn 2 request had %d messages, want 2 (committed user msg from turn 1 + turn 2 user msg)", len(req.Messages))
		}
		// Both are user messages (no assistant reply was ever committed for turn 1).
		for i, m := range req.Messages {
			if _, ok := m.(*content.UserMessage); !ok {
				t.Errorf("req.Messages[%d] = %T, want *UserMessage", i, m)
			}
		}
	})

	t.Run("fail mid-turn keeps completed steps committed, discards only the in-flight step (no unpaired tool_use)", func(t *testing.T) {
		t.Parallel()
		echo := &echoTool{name: "Echo", output: "ran"}
		// maxIters=2: steps 0 and 1 are completed tool steps (committed); the cap fires
		// on the uncompleted 3rd tool step, which is discarded.
		ts := agenticToolSet([]tool.InvokableTool{echo}, 2, 100)
		client := &scriptedLLM{scripts: [][]content.Chunk{
			{toolUseChunk(0, "id-1", "Echo", `{}`)}, // step 0 (committed)
			{toolUseChunk(0, "id-2", "Echo", `{}`)}, // step 1 (committed)
			{toolUseChunk(0, "id-3", "Echo", `{}`)}, // step 2 (uncompleted; cap fires)
		}}
		ctx, cancel := context.WithCancel(context.Background())
		t.Cleanup(cancel)
		sessionID, _ := uuid.New()
		loopID, _ := uuid.New()
		l, err := New(ctx, sessionID, loopID, Provenance{}, noopPublisher{},
			Config{Client: client, Model: llm.ModelSpec{Model: "m"}, Tools: ts, DrainTimeout: 200 * time.Millisecond})
		if err != nil {
			t.Fatalf("New: %v", err)
		}

		ev, _ := startTurn(t, l, context.Background(), []content.Block{&content.TextBlock{Text: "go"}})
		// Collect the per-turn stream: count StepDones and assert the terminal.
		var sds int
		var terminal event.Event
		for e := range ev {
			switch e.(type) {
			case event.StepDone:
				sds++
			case event.TurnDone, event.TurnFailed, event.TurnInterrupted:
				terminal = e
			}
			if terminal != nil {
				break
			}
		}
		if _, ok := terminal.(event.TurnFailed); !ok {
			t.Fatalf("terminal = %T, want TurnFailed", terminal)
		}
		if sds != 2 {
			t.Errorf("StepDone count = %d, want 2 (steps 0 and 1 committed before the cap)", sds)
		}

		// A follow-up turn's request reveals the committed history: user + step0(tool_use,
		// tool) + step1(tool_use, tool) + the new user message = 6 messages, with NO
		// unpaired tool_use (the in-flight step 2 tool_use was discarded).
		reqs := client.requests()
		last := reqs[len(reqs)-1]
		// Last request = user + step0(tool_use + tool) + step1(tool_use + tool) = 5.
		if len(last.Messages) != 5 {
			t.Fatalf("final request had %d messages, want 5 (user + 2 committed tool steps)", len(last.Messages))
		}
		var tu, tm int
		for _, m := range last.Messages {
			switch v := m.(type) {
			case *content.AIMessage:
				for _, b := range v.Blocks {
					if _, ok := b.(*content.ToolUseBlock); ok {
						tu++
					}
				}
			case *content.ToolResultMessage:
				tm++
			}
		}
		if tu != tm || tu != 2 {
			t.Errorf("committed pairs in final request: %d tool_use vs %d tool messages, want 2/2", tu, tm)
		}
	})
}

// TestActorCommitsInitialUserMessageAndTurnStarted asserts the loop-owned commit
// of the initial UserMessage: the actor emits TurnStarted carrying the exact
// UserMessage and Cause.CommandID = the triggering UserInput's id, BEFORE runTurn
// produces any step, and commits that UserMessage into loopState.msgs (proven by
// the first request the provider receives carrying exactly that user message).
func TestActorCommitsInitialUserMessageAndTurnStarted(t *testing.T) {
	t.Parallel()
	rec := &recordingLLM{chunks: []content.Chunk{textChunk("hi")}}
	l, _ := newLoop(t, rec)

	cmdID, err := uuid.New()
	if err != nil {
		t.Fatalf("uuid.New: %v", err)
	}
	ev := make(chan event.Event, 64)
	ab := make(chan struct{})
	defer close(ab)
	input := []content.Block{&content.TextBlock{Text: "hello there"}}
	l.Commands <- command.UserInput{
		Header:    command.Header{CommandID: cmdID},
		Mode:      command.StartOnly,
		Blocks:    input,
		Events:    ev,
		Abandoned: ab,
	}

	// The first per-turn event is TurnStarted, carrying the initial UserMessage and
	// Cause.CommandID == the submit id. It is emitted by the actor at the commit point
	// BEFORE any TokenDelta/StepDone.
	first := <-ev
	started, ok := first.(event.TurnStarted)
	if !ok {
		t.Fatalf("first event = %T, want TurnStarted", first)
	}
	if started.Cause.CommandID != cmdID {
		t.Errorf("TurnStarted.Cause.CommandID = %v, want %v (the UserInput id)", started.Cause.CommandID, cmdID)
	}
	if started.Message == nil {
		t.Fatal("TurnStarted.Message is nil, want the committed initial UserMessage")
	}
	if started.Message.Role != content.RoleUser {
		t.Errorf("TurnStarted.Message.Role = %q, want %q", started.Message.Role, content.RoleUser)
	}
	if got := flattenToText(started.Message.Blocks); got != "hello there" {
		t.Errorf("TurnStarted.Message text = %q, want %q", got, "hello there")
	}
	// InputID now carries the submit command id (== Cause.CommandID), so a consumer can
	// correlate the event.TurnStarted back to the originating UserInput.
	if started.Cause.CommandID != cmdID {
		t.Errorf("TurnStarted.Cause.CommandID = %v, want the submit id %v", started.Cause.CommandID, cmdID)
	}
	if _, ok := drainToTerminal(t, ev).(event.TurnDone); !ok {
		t.Fatal("terminal != TurnDone")
	}

	// The committed UserMessage drove the very first provider request.
	reqs := rec.reqs
	if len(reqs) == 0 {
		t.Fatal("no provider request recorded")
	}
	if len(reqs[0].Messages) != 1 {
		t.Fatalf("first request had %d messages, want 1 (the committed initial UserMessage)", len(reqs[0].Messages))
	}
	if _, ok := reqs[0].Messages[0].(*content.UserMessage); !ok {
		t.Errorf("first request msg = %T, want *UserMessage", reqs[0].Messages[0])
	}
}

// TestActorPerTurnStreamOrdering asserts the actor-owned serialization invariant:
// across the full actor path, every step's TokenDeltas precede that step's
// StepDone (emitted by the actor at the commit point while runTurn is parked in
// the handshake), and the single terminal is last. The blocking commit handshake
// guarantees there are no concurrent writers to the per-turn stream.
func TestActorPerTurnStreamOrdering(t *testing.T) {
	t.Parallel()
	echo := &echoTool{name: "Echo", output: "ran"}
	ts := agenticToolSet([]tool.InvokableTool{echo}, 25, 100)
	client := &scriptedLLM{scripts: [][]content.Chunk{
		{textChunk("step0a"), textChunk("step0b"), toolUseChunk(0, "id-1", "Echo", `{}`)}, // step 0: 2 deltas + tool
		{textChunk("final")}, // step 1: text-only → TurnDone
	}}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	sessionID, _ := uuid.New()
	loopID, _ := uuid.New()
	l, err := New(ctx, sessionID, loopID, Provenance{}, noopPublisher{},
		Config{Client: client, Model: llm.ModelSpec{Model: "m"}, Tools: ts, DrainTimeout: 200 * time.Millisecond})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// Raw StartOnly submit (not the startTurn helper) so the leading TurnStarted stays
	// on the stream to be asserted: the start-or-reject outcome IS the first per-turn
	// event now, and this test verifies the full started/delta/stepdone/done ordering.
	ev := make(chan event.Event, 64)
	ab := make(chan struct{})
	defer close(ab)
	l.Commands <- command.UserInput{Mode: command.StartOnly, Blocks: []content.Block{&content.TextBlock{Text: "go"}}, Events: ev, Abandoned: ab}

	var kinds []string
	for e := range ev {
		switch e.(type) {
		case event.TurnStarted:
			kinds = append(kinds, "started")
		case event.TokenDelta:
			kinds = append(kinds, "delta")
		case event.StepDone:
			kinds = append(kinds, "stepdone")
		case event.TurnDone:
			kinds = append(kinds, "done")
		}
		if len(kinds) > 0 && kinds[len(kinds)-1] == "done" {
			break
		}
	}
	// started, then step 0's deltas all precede step 0's stepdone, then step 1's
	// delta precedes step 1's stepdone, then the terminal last. Step 0 streams THREE
	// chunks (two text + one tool-use), each emitting a TokenDelta; step 1 streams one
	// text chunk. (Tool lifecycle events for the auto-approved Echo call also appear
	// within step 0 but are not in this projection; we assert only the
	// started/delta/stepdone/done ordering.)
	want := []string{"started", "delta", "delta", "delta", "stepdone", "delta", "stepdone", "done"}
	if !equalStringSlices(kinds, want) {
		t.Errorf("event order = %v, want %v", kinds, want)
	}
}

func equalStringSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestActorToolTurnOrderedHistory asserts that a tool-using turn produces, in
// loopState.msgs (observed via the provider's final request), one UserMessage,
// multiple AIMessages, and matching ToolResultMessages in order.
func TestActorToolTurnOrderedHistory(t *testing.T) {
	t.Parallel()
	echo := &echoTool{name: "Echo", output: "ran"}
	ts := agenticToolSet([]tool.InvokableTool{echo}, 25, 100)
	client := &scriptedLLM{scripts: [][]content.Chunk{
		{toolUseChunk(0, "id-1", "Echo", `{}`)}, // step 0: tool
		{toolUseChunk(0, "id-2", "Echo", `{}`)}, // step 1: tool
		{textChunk("done")},                     // step 2: text → TurnDone
	}}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	sessionID, _ := uuid.New()
	loopID, _ := uuid.New()
	l, err := New(ctx, sessionID, loopID, Provenance{}, noopPublisher{},
		Config{Client: client, Model: llm.ModelSpec{Model: "m"}, Tools: ts, DrainTimeout: 200 * time.Millisecond})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ev, _ := startTurn(t, l, context.Background(), []content.Block{&content.TextBlock{Text: "go"}})
	if _, ok := drainToTerminal(t, ev).(event.TurnDone); !ok {
		t.Fatal("terminal != TurnDone")
	}

	// The final (3rd) request reflects committed history: user, AI(tool_use), tool,
	// AI(tool_use), tool — in that order.
	reqs := client.requests()
	if len(reqs) != 3 {
		t.Fatalf("recorded %d requests, want 3", len(reqs))
	}
	final := reqs[2].Messages
	wantKinds := []string{"user", "ai", "tool", "ai", "tool"}
	if len(final) != len(wantKinds) {
		t.Fatalf("final request had %d messages, want %d", len(final), len(wantKinds))
	}
	for i, m := range final {
		var kind string
		switch m.(type) {
		case *content.UserMessage:
			kind = "user"
		case *content.AIMessage:
			kind = "ai"
		case *content.ToolResultMessage:
			kind = "tool"
		default:
			kind = "?"
		}
		if kind != wantKinds[i] {
			t.Errorf("final history[%d] = %s, want %s", i, kind, wantKinds[i])
		}
	}
	// Tool results pair with their calls in order.
	if tm, ok := final[2].(*content.ToolResultMessage); !ok || tm.ToolUseID != "id-1" {
		t.Errorf("final history[2] tool result ToolUseID mismatch")
	}
	if tm, ok := final[4].(*content.ToolResultMessage); !ok || tm.ToolUseID != "id-2" {
		t.Errorf("final history[4] tool result ToolUseID mismatch")
	}
}

// blockingTool blocks in InvokableRun until released, signalling started first. It
// lets a test park the turn goroutine deterministically inside a tool batch (and,
// by extension, drive the actor into the commit/interrupt path).
type blockingTool struct {
	started  chan struct{}
	release  chan struct{}
	onceStop sync.Once
}

func newBlockingTool() *blockingTool {
	return &blockingTool{started: make(chan struct{}), release: make(chan struct{})}
}

func (b *blockingTool) Info(ctx context.Context) (*tool.ToolInfo, error) {
	return &tool.ToolInfo{Name: "Block", Desc: "blocks", Schema: json.RawMessage(`{"type":"object"}`)}, nil
}

func (b *blockingTool) InvokableRun(ctx context.Context, argsJSON string) (*tool.ToolResult, error) {
	b.onceStop.Do(func() { close(b.started) })
	select {
	case <-b.release:
		return tool.TextResult("released"), nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// TestCommitParkDoesNotWedgeOnRootCancel proves the ctx-cancellable commit
// handshake never wedges the loop: a per-turn consumer that stops reading parks the
// actor blocked in the commit-point StepDone emission while runTurn waits for the
// ack; a root-ctx cancel (the universal escape) must free both so Loop.Done closes.
func TestCommitParkDoesNotWedgeOnRootCancel(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	sessionID, _ := uuid.New()
	loopID, _ := uuid.New()
	// A single no-tool step: runTurn emits one TokenDelta, then commits step 0; the
	// actor emits StepDone at the commit point.
	l, err := New(ctx, sessionID, loopID, Provenance{}, noopPublisher{}, Config{
		Client:       &fakeLLM{chunks: []content.Chunk{textChunk("a")}},
		Model:        llm.ModelSpec{Model: "m"},
		DrainTimeout: 100 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// Events buffered to EXACTLY TurnStarted + 1 TokenDelta; the StepDone send from
	// the actor's commit point blocks on the full, unread buffer, parking the actor
	// (and runTurn waiting for the ack). Only the emitTurn ctx.Done() escape frees it.
	ev := make(chan event.Event, 2)
	ab := make(chan struct{}) // never closed -> leaked reader
	// Leaked reader: never read from ev (TurnStarted fills slot 1, the TokenDelta fills
	// slot 2; the StepDone then blocks the actor). Reading would defeat the test.
	l.Commands <- command.UserInput{Mode: command.StartOnly, Blocks: nil, Events: ev, Abandoned: ab}
	// Give the turn time to fill the buffer and park the actor in the commit point.
	deadline := time.After(2 * time.Second)
	for len(ev) < 2 {
		select {
		case <-deadline:
			t.Fatal("buffer never filled; actor did not reach the commit point")
		case <-time.After(2 * time.Millisecond):
		}
	}
	cancel() // root-ctx cancel: the universal escape for the parked commit point
	select {
	case <-l.Done:
	case <-time.After(2 * time.Second):
		t.Fatal("actor wedged at the commit point: Loop.Done never closed after root-ctx cancel")
	}
}

// TestInterruptDuringToolBatchFreesTurn proves an Interrupt delivered while the
// turn goroutine is parked deep in a tool batch (just before the next commit/LLM
// request) frees the turn: the actor processes Interrupt, cancels turnCtx, the
// blocked tool returns ctx.Err(), and runTurn returns TurnInterrupted without
// wedging the loop.
func TestInterruptDuringToolBatchFreesTurn(t *testing.T) {
	t.Parallel()
	bt := newBlockingTool()
	ts := agenticToolSet([]tool.InvokableTool{bt}, 25, 100)
	client := &scriptedLLM{scripts: [][]content.Chunk{
		{toolUseChunk(0, "id-1", "Block", `{}`)},
	}}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	sessionID, _ := uuid.New()
	loopID, _ := uuid.New()
	l, err := New(ctx, sessionID, loopID, Provenance{}, noopPublisher{},
		Config{Client: client, Model: llm.ModelSpec{Model: "m"}, Tools: ts, DrainTimeout: 200 * time.Millisecond})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ev, _ := startTurn(t, l, context.Background(), []content.Block{&content.TextBlock{Text: "go"}})
	// Drain the per-turn stream in the background so emit never blocks the turn.
	terminalCh := make(chan event.Event, 1)
	go func() {
		for e := range ev {
			if e.EndsTurn() {
				terminalCh <- e
				return
			}
		}
	}()
	<-bt.started // the tool is now blocked; the turn goroutine is parked in the batch
	iack := make(chan bool, 1)
	l.Commands <- command.Interrupt{Ack: iack}
	if !<-iack {
		t.Fatal("Interrupt ack = false, want true (turn running)")
	}
	select {
	case term := <-terminalCh:
		if _, ok := term.(event.TurnInterrupted); !ok {
			t.Fatalf("terminal = %T, want TurnInterrupted", term)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("turn did not terminate after Interrupt")
	}
}
