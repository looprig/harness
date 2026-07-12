package loopruntime

import (
	"testing"

	"github.com/looprig/core/content"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/command"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/tool"
)

// submitInput submits a UserInput carrying the given NoFold flag and returns the queued
// outcome event, so a test can assert the queue happened before releasing the tool.
func submitInput(t *testing.T, l *Loop, rec *recordingPublisher, id uuid.UUID, blocks []content.Block, noFold bool) event.Event {
	t.Helper()
	l.Commands <- command.UserInput{Header: command.Header{CommandID: id}, Blocks: blocks, NoFold: noFold}
	return awaitReply(t, rec, id)
}

// TestNonFoldingInputStartsOwnTurn proves the non-folding enqueue primitive: an input
// queued behind a running tool-using turn with NoFold=true NEVER folds at the
// tool-continuation boundary — it starts its OWN distinct turn (a TurnStarted whose
// Cause.CommandID is its id) when the running turn finishes. The folding case
// (NoFold=false) still folds into the running turn (TurnFoldedInto, no new turn), so the
// flag is the only difference. This is the request-correlation guarantee delegate `send`
// depends on: a distinct turn per request id.
func TestNonFoldingInputStartsOwnTurn(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		noFold      bool
		wantOwnTurn bool // expect a distinct TurnStarted for the queued input
	}{
		{name: "folding input folds into running turn", noFold: false, wantOwnTurn: false},
		{name: "non-folding input starts its own turn", noFold: true, wantOwnTurn: true},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			bt := newBlockingTool()
			ts := agenticToolSet([]tool.InvokableTool{bt}, 25, 100)
			client := &scriptedLLM{scripts: [][]content.Chunk{
				{toolUseChunk(0, "id-1", "Block", `{}`)}, // step 0: blocking tool (turn 1 parks)
				{textChunk("final")},                     // step 1+: text -> TurnDone
			}}
			l, rec := newFoldLoop(t, client, ts)

			// Turn 1 starts and parks in the blocking tool.
			startTurn(t, l, rec, textBlocks("turn1"))
			<-bt.started

			// Queue an input while the tool step is in flight.
			queuedID := mustID(t)
			d := submitInput(t, l, rec, queuedID, textBlocks("queued text"), tt.noFold)
			if _, ok := d.(event.InputQueued); !ok {
				t.Fatalf("queued submit outcome = %T, want event.InputQueued", d)
			}

			// Release the tool so turn 1 reaches its tool-continuation drain, then TurnDone.
			close(bt.release)

			if tt.wantOwnTurn {
				blockUntilEvents(t, rec, func(evs []event.Event) bool {
					for _, e := range evs {
						if ts, ok := e.(event.TurnStarted); ok && ts.Cause.CommandID == queuedID {
							return true
						}
					}
					return false
				})
				for _, e := range rec.events() {
					if fi, ok := e.(event.TurnFoldedInto); ok && fi.Cause.CommandID == queuedID {
						t.Fatal("non-folding input was folded into the running turn")
					}
				}
			} else {
				blockUntilEvents(t, rec, func(evs []event.Event) bool {
					for _, e := range evs {
						if fi, ok := e.(event.TurnFoldedInto); ok && fi.Cause.CommandID == queuedID {
							return true
						}
					}
					return false
				})
				for _, e := range rec.events() {
					if ts, ok := e.(event.TurnStarted); ok && ts.Cause.CommandID == queuedID {
						t.Fatal("folding input started its own turn instead of folding")
					}
				}
			}
		})
	}
}
