package loop

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/looprig/harness/pkg/command"
	"github.com/looprig/core/content"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/llm"
	"github.com/looprig/core/uuid"
)

// fixedClock returns a constant instant so a test can assert CreatedAt
// deterministically.
func fixedClock(ts time.Time) event.Clock { return func() time.Time { return ts } }

// seqIDGen mints a deterministic, distinct UUID per call (1, 2, 3, ...) so a test
// can assert minted EventIDs are non-zero without coupling to crypto/rand. It is
// safe for concurrent use (the actor and the turn goroutine both mint ids).
type seqIDGen struct {
	mu sync.Mutex
	n  byte
}

func (g *seqIDGen) gen() (uuid.UUID, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.n++
	return uuid.UUID{g.n}, nil
}

// workingFactory builds an event Factory whose EventID mint ALWAYS succeeds
// (uuid.New + time.Now). Tests that inject a FAILING idGen to exercise the
// loop's correlation-id failure branch use this so the Enduring OUTCOME event the
// loop publishes (TurnRejected/InputCancelled) is still stamped and delivered —
// otherwise the loop's own factory (sharing the failing idGen) would drop that
// outcome too (its EventID mint fails), and the test could never observe the
// reply. The EventID-mint failure branch itself is covered separately by
// TestEnduringMintErrorSkipsPublish.
func workingFactory() *event.Factory {
	return event.NewFactory(uuid.New, time.Now)
}

// newLoopWithFactory starts a loop wired to a recordingPublisher AND a
// deterministic event Factory (fixed clock + sequential id-gen). The same id-gen
// feeds the loop's correlation ids (TurnID/StepID/ToolExecutionID) and the
// Factory's EventID mint, mirroring production where one generator backs both.
func newLoopWithFactory(t *testing.T, client llm.LLM, ts time.Time) (*Loop, *recordingPublisher) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	gen := &seqIDGen{}
	rec := &recordingPublisher{}
	l, err := New(ctx, mustID(t), mustID(t), Provenance{}, rec, Config{
		Client:       client,
		Model:        testModel(),
		DrainTimeout: 200 * time.Millisecond,
		idGen:        gen.gen,
		now:          fixedClock(ts),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return l, rec
}

// TestEnduringLoopEventsStamped drives a full turn through the loop and asserts
// every Enduring loop event published on the session fan-in carries a non-zero
// EventID and the factory's CreatedAt, while the Ephemeral TokenDelta is published
// WITHOUT requiring a mint (its EventID stays zero — Ephemeral events are never
// persisted, so they are not stamped, avoiding per-token crypto/rand). The single
// publish chokepoint stamps by Class(), so this covers TurnStarted/StepDone/
// TurnDone/LoopIdle alike.
func TestEnduringLoopEventsStamped(t *testing.T) {
	t.Parallel()
	ts := time.Date(2026, 6, 21, 12, 0, 0, 0, time.UTC)
	l, rec := newLoopWithFactory(t, &fakeLLM{chunks: []content.Chunk{textChunk("hi")}}, ts)

	id := mustID(t)
	l.Commands <- command.UserInput{Header: command.Header{CommandID: id}, Blocks: nil}
	if _, ok := awaitReply(t, rec, id).(event.TurnStarted); !ok {
		t.Fatal("submit did not start a turn")
	}
	// Drive to the terminal and the post-terminal LoopIdle so the full Enduring set
	// (TurnStarted, StepDone, TurnDone, LoopIdle) has been published.
	blockUntilEvents(t, rec, func(evs []event.Event) bool {
		for _, e := range evs {
			if _, ok := e.(event.LoopIdle); ok {
				return true
			}
		}
		return false
	})

	var sawEnduring, sawEphemeral bool
	for _, e := range rec.events() {
		h := e.EventHeader()
		switch e.Class() {
		case event.Enduring:
			sawEnduring = true
			if h.EventID.IsZero() {
				t.Errorf("Enduring %T published with zero EventID, want a minted id", e)
			}
			if !h.CreatedAt.Equal(ts) {
				t.Errorf("Enduring %T CreatedAt = %v, want %v (factory clock)", e, h.CreatedAt, ts)
			}
		case event.Ephemeral:
			sawEphemeral = true
			if !h.EventID.IsZero() {
				t.Errorf("Ephemeral %T published WITH EventID %v, want zero (never persisted, not stamped)", e, h.EventID)
			}
		}
	}
	if !sawEnduring {
		t.Fatal("no Enduring event captured")
	}
	if !sawEphemeral {
		t.Fatal("no Ephemeral event captured (expected at least one TokenDelta)")
	}
}

// TestEnduringMintErrorSkipsPublish asserts the loop's fail-secure mint-error
// handling: when the Factory cannot mint an EventID for an Enduring event, that
// event is NOT published (no zero-EventID Enduring event ever reaches the fan-in),
// and the loop does not wedge. The Ephemeral TokenDeltas still publish unstamped.
//
// The id-gen succeeds for the correlation ids the actor/turn need (TurnID, StepID,
// ToolExecutionID) but fails the EventID mint, so the turn runs yet its Enduring
// events are skipped. We force this by injecting a Factory whose IDGen always
// errors while the loop's own idGen succeeds — the two seams are independent.
func TestEnduringMintErrorSkipsPublish(t *testing.T) {
	t.Parallel()
	ts := time.Date(2026, 6, 21, 12, 0, 0, 0, time.UTC)
	mintErr := errors.New("event id mint failed")

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	gen := &seqIDGen{} // correlation ids succeed
	rec := &recordingPublisher{}
	// Factory IDGen always fails: every Enduring stamp errors, so every Enduring
	// event is skipped, but the turn (using gen for TurnID/StepID) still runs.
	failingFactory := event.NewFactory(func() (uuid.UUID, error) { return uuid.UUID{}, mintErr }, fixedClock(ts))
	l, err := New(ctx, mustID(t), mustID(t), Provenance{}, rec, Config{
		Client:       &fakeLLM{chunks: []content.Chunk{textChunk("hi")}},
		Model:        testModel(),
		DrainTimeout: 200 * time.Millisecond,
		idGen:        gen.gen,
		eventFactory: failingFactory,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	id := mustID(t)
	l.Commands <- command.UserInput{Header: command.Header{CommandID: id}, Blocks: nil}

	// The loop must not wedge: it keeps running and remains usable. Give it time to
	// process the submit (whose Enduring outcomes are all skipped), then prove no
	// zero-EventID Enduring event was published and the actor still accepts work.
	time.Sleep(100 * time.Millisecond)
	for _, e := range rec.events() {
		if e.Class() == event.Enduring {
			t.Fatalf("Enduring %T was published despite a mint failure; it must be skipped, never published with a zero EventID", e)
		}
	}

	// The actor is not wedged: a Shutdown still completes.
	ack := make(chan error, 1)
	if !sendCmd(t, l, command.Shutdown{Ack: ack}) {
		t.Fatal("actor wedged after mint-error skips (Shutdown send did not land)")
	}
	select {
	case <-l.Done:
	case <-time.After(2 * time.Second):
		t.Fatal("actor did not exit after Shutdown; mint-error handling wedged the loop")
	}
}
