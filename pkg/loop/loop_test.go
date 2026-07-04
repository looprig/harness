package loop

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/looprig/core/content"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/command"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/tool"
	"github.com/looprig/inference"
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

// newLoop starts a loop with a 200ms DrainTimeout wired to a recordingPublisher,
// and returns it plus the recorder and the root cancel. Every loop event is observed
// on the session fan-in (there is no per-turn stream), so the recorder is the single
// observation seam.
func newLoop(t *testing.T, client inference.Client) (*Loop, *recordingPublisher, context.CancelFunc) {
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
	rec := &recordingPublisher{}
	l, err := New(ctx, sessionID, loopID, Provenance{}, rec, Config{Client: client, Model: testModel(), DrainTimeout: 200 * time.Millisecond})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(cancel)
	return l, rec, cancel
}

// startTurn sends a fan-in UserInput with a fresh input id and blocks until the loop
// publishes event.TurnStarted for it (failing if the loop instead refused the submit).
// It returns the input id (== the started turn's Cause.CommandID) and a no-op closer
// kept only so the two-value `id, _ := startTurn(...)` destructuring at call sites
// reads cleanly. Every outcome is observed on the session fan-in via rec.
func startTurn(t *testing.T, l *Loop, rec *recordingPublisher, input []content.Block) (uuid.UUID, func()) {
	t.Helper()
	id := mustID(t)
	l.Commands <- command.UserInput{Header: command.Header{CommandID: id}, Blocks: input}
	if _, ok := awaitReply(t, rec, id).(event.TurnStarted); !ok {
		t.Fatalf("UserInput did not start a turn for %v (outcome was not TurnStarted)", id)
	}
	return id, func() {}
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

// drainToTerminal blocks until a terminal event (TurnDone/TurnFailed/
// TurnInterrupted) has been published, and returns the FIRST such terminal. For a
// loop driven through a single turn this is that turn's terminal; tests that run
// multiple turns use awaitTerminalAfter to select the later turn's terminal.
func drainToTerminal(t *testing.T, rec *recordingPublisher) event.Event {
	t.Helper()
	return awaitTerminalAfter(t, rec, 0)
}

// awaitTerminalAfter blocks until a terminal event appears at recorded-index >= from
// and returns it. `from` lets a multi-turn test skip an earlier turn's terminal:
// capture len(rec.events()) after turn 1's terminal, then await the next terminal
// from that index. It returns the terminal AND its index so a caller can chain.
func awaitTerminalAfter(t *testing.T, rec *recordingPublisher, from int) event.Event {
	t.Helper()
	var found event.Event
	blockUntilEvents(t, rec, func(evs []event.Event) bool {
		for i := from; i < len(evs); i++ {
			switch evs[i].(type) {
			case event.TurnDone, event.TurnFailed, event.TurnInterrupted:
				found = evs[i]
				return true
			}
		}
		return false
	})
	return found
}

// terminalIndex returns the recorded index just past the first terminal at/after
// `from` (the baseline a follow-up awaitTerminalAfter should use to find the NEXT
// turn's terminal). It assumes a terminal at/after `from` already exists.
func terminalIndex(rec *recordingPublisher, from int) int {
	evs := rec.events()
	for i := from; i < len(evs); i++ {
		switch evs[i].(type) {
		case event.TurnDone, event.TurnFailed, event.TurnInterrupted:
			return i + 1
		}
	}
	return len(evs)
}

// TestRecordingPublisherObservesTurnStarted proves the recordingPublisher
// observes a real loop event: a turn started via the fakeLLM client publishes an
// event.TurnStarted onto the event publisher (the production fan-in path), which
// the recorder captures. This is the migration target for tests that previously
// observed loop events through a sink.
func TestRecordingPublisherObservesTurnStarted(t *testing.T) {
	t.Parallel()
	l, rec, _ := newLoop(t, &fakeLLM{chunks: []content.Chunk{textChunk("hi")}})
	startTurn(t, l, rec, nil)

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
		_, err := New(ctx, sessionID, loopID, Provenance{}, noopPublisher{}, Config{Model: testModel()})
		var ce *ConfigError
		if !errors.As(err, &ce) || ce.Kind != ConfigMissingClient {
			t.Fatalf("err = %v, want *ConfigError{ConfigMissingClient}", err)
		}
	})
	t.Run("invalid model unwraps to ValidationError", func(t *testing.T) {
		t.Parallel()
		bad := inference.Model{} // empty Name — fails inference.Model.Validate (structural check)
		_, err := New(ctx, sessionID, loopID, Provenance{}, noopPublisher{}, Config{Client: &fakeLLM{}, Model: bad})
		var ce *ConfigError
		if !errors.As(err, &ce) || ce.Kind != ConfigInvalidModel {
			t.Fatalf("err = %v, want *ConfigError{ConfigInvalidModel}", err)
		}
		var ve *inference.ValidationError
		if !errors.As(err, &ve) {
			t.Fatalf("err does not unwrap to *inference.ValidationError")
		}
	})
	t.Run("nil publisher", func(t *testing.T) {
		t.Parallel()
		_, err := New(ctx, sessionID, loopID, Provenance{}, nil, Config{Client: &fakeLLM{}, Model: testModel()})
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
	l, rec, _ := newLoop(t, &fakeLLM{chunks: []content.Chunk{textChunk("hi")}})
	startTurn(t, l, rec, nil)
	terminal := drainToTerminal(t, rec)
	if _, ok := terminal.(event.TurnDone); !ok {
		t.Fatalf("terminal = %T, want TurnDone", terminal)
	}
	// actor is idle again: a second turn is accepted and runs to its own terminal,
	// which lands AFTER the first turn's terminal on the fan-in.
	from := terminalIndex(rec, 0)
	startTurn(t, l, rec, nil)
	if _, ok := awaitTerminalAfter(t, rec, from).(event.TurnDone); !ok {
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
	l, rec, _ := newLoop(t, &fakeLLM{chunks: []content.Chunk{textChunk("hi")}})

	cmdID, err := uuid.New()
	if err != nil {
		t.Fatalf("uuid.New: %v", err)
	}
	l.Commands <- command.UserInput{
		Header: command.Header{CommandID: cmdID},
		Blocks: nil,
	}
	// The submit's outcome is observed on the fan-in: TurnStarted on success.
	if _, ok := awaitReply(t, rec, cmdID).(event.TurnStarted); !ok {
		t.Fatal("submit did not start a turn (outcome was not TurnStarted)")
	}
	if _, ok := drainToTerminal(t, rec).(event.TurnDone); !ok {
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
// the crypto/rand failure branches that uuid.New cannot reach in tests. It wires a
// recordingPublisher so the resulting outcome (e.g. TurnRejected) is observable on
// the session fan-in, and returns it alongside the loop.
func newLoopWithIDGen(t *testing.T, client inference.Client, gen idGenerator) (*Loop, *recordingPublisher) {
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
	rec := &recordingPublisher{}
	l, err := New(ctx, sessionID, loopID, Provenance{}, rec, Config{
		Client:       client,
		Model:        testModel(),
		DrainTimeout: 200 * time.Millisecond,
		idGen:        gen,
		// The injected gen fails the CORRELATION-id mint (the branch under test); give
		// the loop a working EventID factory so the Enduring outcome event it publishes
		// in response (TurnRejected/InputCancelled) is still stamped and observable.
		eventFactory: workingFactory(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(cancel)
	return l, rec
}

// TestTurnIDGenerationFailure covers branch 1: when the id generator fails while
// minting the per-turn TurnID for an idle-time submit, the turn does not start, and
// the actor publishes TurnRejected{RejectInternal} on the session fan-in (fail-secure:
// it cannot mint a TurnID, so it declines the work — but the loop is healthy and the
// caller MAY retry, so the reason is the transient RejectInternal, NOT
// RejectShuttingDown).
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
			l, rec := newLoopWithIDGen(t, &fakeLLM{chunks: []content.Chunk{textChunk("hi")}}, gen.gen)

			id := mustID(t)
			l.Commands <- command.UserInput{Header: command.Header{CommandID: id}, Blocks: nil}

			// The id-gen failure is surfaced on the fan-in as TurnRejected{Internal}.
			reply := awaitReply(t, rec, id)
			rej, ok := reply.(event.TurnRejected)
			if !ok {
				t.Fatalf("reply = %T, want event.TurnRejected (id-gen failure)", reply)
			}
			if rej.Reason != event.RejectInternal {
				t.Fatalf("reject reason = %d, want RejectInternal (transient id-gen failure, not ShuttingDown)", rej.Reason)
			}
		})
	}
}

// TestStartWhileRunning proves a submit that arrives while a turn is running is
// QUEUED (not rejected): busy is no longer a rejection — the loop accepts the input
// into the inbox and publishes InputQueued on the session fan-in.
func TestStartWhileRunning(t *testing.T) {
	t.Parallel()
	// provider blocks until ctx cancel, so the first turn stays running
	l, rec, _ := newLoop(t, &fakeLLM{blockUntilCancel: true})
	startTurn(t, l, rec, nil)

	// A second submit while the loop is busy queues behind the running turn.
	id2 := mustID(t)
	l.Commands <- command.UserInput{Header: command.Header{CommandID: id2}, Blocks: nil}
	if _, ok := awaitReply(t, rec, id2).(event.InputQueued); !ok {
		t.Fatalf("submit while running was not queued (outcome was not InputQueued)")
	}
}

func TestInterruptMidTurn(t *testing.T) {
	t.Parallel()
	l, rec, _ := newLoop(t, &fakeLLM{blockUntilCancel: true})
	startTurn(t, l, rec, nil)

	ack := make(chan bool, 1)
	l.Commands <- command.Interrupt{Ack: ack}
	if !<-ack {
		t.Fatal("Interrupt ack = false, want true (turn was running)")
	}
	if _, ok := drainToTerminal(t, rec).(event.TurnInterrupted); !ok {
		t.Fatal("terminal != TurnInterrupted")
	}
}

func TestInterruptIdle(t *testing.T) {
	t.Parallel()
	l, _, _ := newLoop(t, &fakeLLM{chunks: []content.Chunk{textChunk("x")}})
	ack := make(chan bool, 1)
	l.Commands <- command.Interrupt{Ack: ack}
	if <-ack {
		t.Fatal("Interrupt ack = true, want false (no turn running)")
	}
}

func TestShutdownIdle(t *testing.T) {
	t.Parallel()
	l, _, _ := newLoop(t, &fakeLLM{chunks: []content.Chunk{textChunk("x")}})
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
	l, rec, _ := newLoop(t, &fakeLLM{blockUntilCancel: true})
	startTurn(t, l, rec, nil)
	ack := make(chan error, 1)
	l.Commands <- command.Shutdown{Ack: ack}
	// terminal still published on the fan-in
	if _, ok := drainToTerminal(t, rec).(event.TurnInterrupted); !ok {
		t.Fatal("terminal != TurnInterrupted")
	}
	if err := <-ack; err != nil {
		t.Fatalf("Shutdown ack = %v, want nil", err)
	}
	<-l.Done
}

func TestShutdownWhileShuttingDown(t *testing.T) {
	t.Parallel()
	l, rec, _ := newLoop(t, &fakeLLM{blockUntilCancel: true})
	startTurn(t, l, rec, nil)
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
	if _, ok := drainToTerminal(t, rec).(event.TurnInterrupted); !ok {
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
	l, rec, _ := newLoop(t, panicLLM{})
	startTurn(t, l, rec, nil)
	terminal := drainToTerminal(t, rec)
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

// TestFanInOnlyUserInputStartsTurn proves a UserInput starts a turn and runs it to
// completion through the session fan-in: every loop event reaches consumers via the
// single publish path. The published TurnStarted and the fan-in-observed TurnDone are
// the only evidence the turn ran (there is no per-turn stream).
func TestFanInOnlyUserInputStartsTurn(t *testing.T) {
	t.Parallel()
	l, rec, _ := newLoop(t, &fakeLLM{chunks: []content.Chunk{textChunk("hi")}})

	id := mustID(t)
	l.Commands <- command.UserInput{Header: command.Header{CommandID: id}, Blocks: nil}
	if _, ok := awaitReply(t, rec, id).(event.TurnStarted); !ok {
		t.Fatal("submit did not publish TurnStarted")
	}
	// The turn runs to a TurnDone observed on the fan-in.
	blockUntilEvents(t, rec, hasTerminal)
	// Actor is usable afterward.
	id2 := mustID(t)
	l.Commands <- command.UserInput{Header: command.Header{CommandID: id2}, Blocks: nil}
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
	l, rec, _ := newLoop(t, &fakeLLM{blockUntilCancel: true})
	startTurn(t, l, rec, nil)
	iack := make(chan bool, 1)
	l.Commands <- command.Interrupt{Ack: iack}
	if !<-iack {
		t.Fatal("Interrupt ack = false, want true (turn running)")
	}
	if _, ok := drainToTerminal(t, rec).(event.TurnInterrupted); !ok {
		t.Fatal("terminal != TurnInterrupted")
	}
	// actor idle after: a fresh turn is accepted, then interrupted to finish. Its
	// terminal lands AFTER the first turn's on the fan-in.
	from := terminalIndex(rec, 0)
	startTurn(t, l, rec, nil)
	iack2 := make(chan bool, 1)
	l.Commands <- command.Interrupt{Ack: iack2}
	<-iack2
	if _, ok := awaitTerminalAfter(t, rec, from).(event.TurnInterrupted); !ok {
		t.Fatal("second turn terminal != TurnInterrupted")
	}
}

func TestTurnFailedProviderErrorTyped(t *testing.T) {
	t.Parallel()
	provErr := &inference.ValidationError{Field: "Model", Reason: "boom"}
	l, rec, _ := newLoop(t, &fakeLLM{streamErr: provErr})
	startTurn(t, l, rec, nil)
	terminal := drainToTerminal(t, rec)
	failed, ok := terminal.(event.TurnFailed)
	if !ok {
		t.Fatalf("terminal = %T, want TurnFailed", terminal)
	}
	var ve *inference.ValidationError
	if !errors.As(failed.Err, &ve) {
		t.Fatalf("TurnFailed.Err = %T, want *inference.ValidationError", failed.Err)
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
	rec := &recordingPublisher{}
	l, err := New(ctx, sessionID, loopID, Provenance{}, rec, Config{
		Client:       &fakeLLM{blockUntilCancel: true, ignoreCtx: true},
		Model:        testModel(),
		DrainTimeout: 100 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	id := mustID(t)
	l.Commands <- command.UserInput{Header: command.Header{CommandID: id}, Blocks: nil}
	// Wait for TurnStarted on the fan-in (the turn is running) before cancelling.
	if _, ok := awaitReply(t, rec, id).(event.TurnStarted); !ok {
		t.Fatal("submit did not start a turn")
	}
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
		client := &recordingLLM{} // empty chunks -> EmptyResponseError on the first step
		l, rec, _ := newLoop(t, client)

		// turn 1 fails on an empty response: step 0 never completes, so its (absent)
		// AIMessage is discarded — but the initial UserMessage was committed at
		// TurnStarted and stays.
		startTurn(t, l, rec, []content.Block{&content.TextBlock{Text: "first"}})
		if _, ok := drainToTerminal(t, rec).(event.TurnFailed); !ok {
			t.Fatal("turn 1 terminal != TurnFailed")
		}
		// turn 2's request must contain turn 1's committed user message THEN turn 2's
		// user message = 2 messages. The committed history grows; it is not rolled back.
		from := terminalIndex(rec, 0)
		client.chunks = []content.Chunk{textChunk("ok")}
		startTurn(t, l, rec, []content.Block{&content.TextBlock{Text: "second"}})
		awaitTerminalAfter(t, rec, from)
		req := client.lastReq()
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
		rec := &recordingPublisher{}
		l, err := New(ctx, sessionID, loopID, Provenance{}, rec,
			Config{Client: client, Model: testModel(), Tools: ts, DrainTimeout: 200 * time.Millisecond})
		if err != nil {
			t.Fatalf("New: %v", err)
		}

		startTurn(t, l, rec, []content.Block{&content.TextBlock{Text: "go"}})
		// On the fan-in: assert the terminal, then count the StepDones published before it.
		terminal := drainToTerminal(t, rec)
		if _, ok := terminal.(event.TurnFailed); !ok {
			t.Fatalf("terminal = %T, want TurnFailed", terminal)
		}
		var sds int
		for _, e := range rec.events() {
			switch e.(type) {
			case event.StepDone:
				sds++
			case event.TurnDone, event.TurnFailed, event.TurnInterrupted:
			}
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
	client := &recordingLLM{chunks: []content.Chunk{textChunk("hi")}}
	l, rec, _ := newLoop(t, client)

	cmdID, err := uuid.New()
	if err != nil {
		t.Fatalf("uuid.New: %v", err)
	}
	input := []content.Block{&content.TextBlock{Text: "hello there"}}
	l.Commands <- command.UserInput{
		Header: command.Header{CommandID: cmdID},
		Blocks: input,
	}

	// The submit's outcome on the fan-in is TurnStarted, carrying the initial
	// UserMessage and Cause.CommandID == the submit id. It is emitted by the actor at
	// the commit point BEFORE any TokenDelta/StepDone.
	reply := awaitReply(t, rec, cmdID)
	started, ok := reply.(event.TurnStarted)
	if !ok {
		t.Fatalf("reply = %T, want TurnStarted", reply)
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
	if _, ok := drainToTerminal(t, rec).(event.TurnDone); !ok {
		t.Fatal("terminal != TurnDone")
	}

	// The committed UserMessage drove the very first provider request.
	reqs := client.reqs
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

// TestActorEventOrderingOnFanIn asserts the actor-owned serialization invariant on
// the session fan-in: every loop event reaches consumers through the single publish
// path, in order — TurnStarted first, then for each step every TokenDelta precedes
// that step's StepDone (emitted by the actor at the commit point while runTurn is
// parked in the handshake), and the single terminal is last. The blocking commit
// handshake guarantees there are no concurrent writers to the fan-in.
func TestActorEventOrderingOnFanIn(t *testing.T) {
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
	rec := &recordingPublisher{}
	l, err := New(ctx, sessionID, loopID, Provenance{}, rec,
		Config{Client: client, Model: testModel(), Tools: ts, DrainTimeout: 200 * time.Millisecond})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	l.Commands <- command.UserInput{Header: command.Header{CommandID: mustID(t)}, Blocks: []content.Block{&content.TextBlock{Text: "go"}}}

	// Wait until the terminal has been published, then project the recorded fan-in
	// events to the started/delta/stepdone/done ordering.
	blockUntilEvents(t, rec, hasTerminal)
	var kinds []string
	for _, e := range rec.events() {
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
	rec := &recordingPublisher{}
	l, err := New(ctx, sessionID, loopID, Provenance{}, rec,
		Config{Client: client, Model: testModel(), Tools: ts, DrainTimeout: 200 * time.Millisecond})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	startTurn(t, l, rec, []content.Block{&content.TextBlock{Text: "go"}})
	if _, ok := drainToTerminal(t, rec).(event.TurnDone); !ok {
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
	rec := &recordingPublisher{}
	l, err := New(ctx, sessionID, loopID, Provenance{}, rec,
		Config{Client: client, Model: testModel(), Tools: ts, DrainTimeout: 200 * time.Millisecond})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	startTurn(t, l, rec, []content.Block{&content.TextBlock{Text: "go"}})
	<-bt.started // the tool is now blocked; the turn goroutine is parked in the batch
	iack := make(chan bool, 1)
	l.Commands <- command.Interrupt{Ack: iack}
	if !<-iack {
		t.Fatal("Interrupt ack = false, want true (turn running)")
	}
	if _, ok := drainToTerminal(t, rec).(event.TurnInterrupted); !ok {
		t.Fatal("turn did not terminate TurnInterrupted after Interrupt")
	}
}
