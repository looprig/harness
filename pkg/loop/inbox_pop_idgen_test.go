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
	"github.com/looprig/core/uuid"
)

// errIDGenForTest is the injected id-gen failure used by the inbox-pop regression.
var errIDGenForTest = errors.New("rand source exhausted")

// flagIDGen mints real UUIDs until fail() is flipped, then fails every subsequent
// call. It lets a test drive the loop through a clean turn-1 lifecycle, then force
// the id-gen to fail exactly when the actor reaches the on-idle inbox pop (the
// TurnID mint for the queued entry's turn). It is safe for concurrent use (the
// actor and the turn goroutine both mint).
type flagIDGen struct {
	mu      sync.Mutex
	failing bool
	err     error
}

func (g *flagIDGen) gen() (uuid.UUID, error) {
	g.mu.Lock()
	failing := g.failing
	g.mu.Unlock()
	if failing {
		return uuid.UUID{}, g.err
	}
	return uuid.New()
}

func (g *flagIDGen) fail() {
	g.mu.Lock()
	g.failing = true
	g.mu.Unlock()
}

// TestInboxPopIDGenFailureReturnsEntry is the regression test for the stranded
// queued-entry defect. On normal completion the actor pops the FIRST queued entry
// (removing it from the inbox) before calling startTurn for it. If that startTurn
// fails to mint the TurnID, the popped entry must NOT be silently dropped: it must
// be resolved with event.InputCancelled{CancelTurnFailed}.
//
// Setup: turn 1 is a single no-tool text step so the queued entry is never folded
// (folding happens only at a tool-continuation boundary). We queue the entry at the
// START of that final step — after any drain, so it stays in the inbox to be popped
// on idle — and flip the id-gen to fail in the same hook. Turn 1 completes normally
// (TurnDone — its terminal EventIDs mint zero best-effort), and the on-idle pop's
// TurnID mint fails. The popped entry must surface as InputCancelled{CancelTurnFailed}.
func TestInboxPopIDGenFailureReturnsEntry(t *testing.T) {
	t.Parallel()
	gen := &flagIDGen{err: errIDGenForTest}
	ts := agenticToolSet(nil, 25, 100)
	queuedID := mustID(t)
	client := &scriptedLLM{scripts: [][]content.Chunk{
		{textChunk("done turn 1")}, // turn 1: single text step -> TurnDone (no fold)
		{textChunk("done turn 2")}, // turn 2 (should never start)
	}}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	sessionID, _ := uuid.New()
	loopID, _ := uuid.New()
	rec := &recordingPublisher{}
	l, err := New(ctx, sessionID, loopID, Provenance{}, rec, Config{
		Client:       client,
		Model:        testModel(),
		Tools:        ts,
		DrainTimeout: 500 * time.Millisecond,
		idGen:        gen.gen,
		// The injected gen fails the CORRELATION-id mint (the branch under test); give
		// the loop a working EventID factory so the Enduring InputCancelled it publishes
		// in response is still stamped and observable on the fan-in.
		eventFactory: workingFactory(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// Queue the entry at the START of turn 1's only (no-tool) step, then flip the
	// id-gen to fail. The submit lands in the inbox (the turn is running) and, because
	// the step requests no tools, no drain consumes it: it waits for the on-idle pop.
	client.mu.Lock()
	client.onStreamN = map[int]func(){
		0: func() {
			l.Commands <- command.UserInput{Header: command.Header{CommandID: queuedID}}
			if _, ok := awaitReply(t, rec, queuedID).(event.InputQueued); !ok {
				t.Errorf("queued submit during final step not InputQueued")
			}
			gen.fail()
		},
	}
	client.mu.Unlock()

	// Turn 1 starts.
	startTurn(t, l, rec, textBlocks("turn1"))

	// The popped entry must NOT be stranded: it must surface as
	// InputCancelled{CancelTurnFailed}. (Before the fix it is silently dropped.)
	blockUntilEvents(t, rec, func(evs []event.Event) bool {
		for _, e := range evs {
			if ic, ok := e.(event.InputCancelled); ok &&
				ic.Cause.CommandID == queuedID && ic.Reason == event.CancelTurnFailed {
				return true
			}
		}
		return false
	})

	// And no second turn ever started from the popped entry.
	for _, e := range rec.events() {
		if ts, ok := e.(event.TurnStarted); ok && ts.Cause.CommandID == queuedID {
			t.Fatal("popped entry was auto-started despite id-gen failure, want InputCancelled")
		}
	}
}
