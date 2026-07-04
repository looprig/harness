package loop

import (
	"context"
	"testing"
	"time"

	"github.com/looprig/harness/pkg/command"
	"github.com/looprig/harness/pkg/content"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/tool"
	"github.com/looprig/core/uuid"
)

// cancelQueuedInput sends a fire-and-forget CancelQueuedInput for inputID. The
// retract carries no Ack: its outcome is the published event.InputCancelled
// {CancelClientRetracted} (keyed by InputID) when the input was still queued, and
// nothing at all when it had already started/folded or was never queued (the
// issuer infers "already committed" from the prior TurnStarted/TurnFoldedInto it
// already saw for that InputID).
func cancelQueuedInput(t *testing.T, l *Loop, inputID uuid.UUID) {
	t.Helper()
	l.Commands <- command.CancelQueuedInput{Header: command.Header{CommandID: mustID(t)}, TargetCommandID: inputID}
}

// hasInputCancelled reports whether evs contains an event.InputCancelled for
// inputID with the given reason (and a non-nil echoed Message).
func hasInputCancelled(evs []event.Event, inputID uuid.UUID, reason event.CancelReason) bool {
	for _, e := range evs {
		if ic, ok := e.(event.InputCancelled); ok &&
			ic.Cause.CommandID == inputID && ic.Reason == reason && ic.Message != nil {
			return true
		}
	}
	return false
}

// TestCancelWhileQueuedPublishesInputCancelled: a CancelQueuedInput for a
// still-queued submit publishes event.InputCancelled{CancelClientRetracted}
// carrying the InputID and original Message, and removes the entry from the inbox
// (a second retract finds it gone and is a pure no-op — no second InputCancelled).
func TestCancelWhileQueuedPublishesInputCancelled(t *testing.T) {
	t.Parallel()
	l, rec, _ := newLoop(t, &fakeLLM{blockUntilCancel: true})
	startTurn(t, l, rec, nil) // occupy the loop

	// Queue an input behind the running turn.
	queuedID := mustID(t)
	l.Commands <- command.UserInput{Header: command.Header{CommandID: queuedID}, Blocks: textBlocks("retract me")}
	if _, ok := awaitReply(t, rec, queuedID).(event.InputQueued); !ok {
		t.Fatal("submit not queued")
	}

	// Retract it while still queued: the observable outcome is the published
	// event.InputCancelled{CancelClientRetracted} for that InputID.
	cancelQueuedInput(t, l, queuedID)
	blockUntilEvents(t, rec, func(evs []event.Event) bool {
		return hasInputCancelled(evs, queuedID, event.CancelClientRetracted)
	})

	// The entry left the inbox: a second retract is a no-op. Wait long enough that a
	// (buggy) duplicate emit would have landed, then assert exactly one
	// InputCancelled for queuedID ever appeared.
	cancelQueuedInput(t, l, queuedID)
	if !waitNoExtraInputCancelled(t, rec, queuedID, event.CancelClientRetracted, 1) {
		t.Fatal("second retract published an extra InputCancelled for an already-removed input")
	}
}

// TestCancelUnknownInputIsNoop: a CancelQueuedInput for an InputID not in the inbox
// (never queued, or already started/committed) is a pure no-op — NO
// event.InputCancelled is published for it. A real submit is queued first and its
// InputQueued observed, proving the loop is alive and processing commands so the
// absence of an InputCancelled for the unknown id is meaningful, not just latency.
func TestCancelUnknownInputIsNoop(t *testing.T) {
	t.Parallel()
	l, rec, _ := newLoop(t, &fakeLLM{blockUntilCancel: true})
	startTurn(t, l, rec, nil)

	// Prove the loop is alive: queue a real input and observe its InputQueued.
	queuedID := mustID(t)
	l.Commands <- command.UserInput{Header: command.Header{CommandID: queuedID}, Blocks: textBlocks("alive")}
	if _, ok := awaitReply(t, rec, queuedID).(event.InputQueued); !ok {
		t.Fatal("submit not queued")
	}

	// Retract an InputID that was NEVER queued: no-op.
	unknownID := mustID(t)
	cancelQueuedInput(t, l, unknownID)

	// Round-trip a SECOND retract for the real queued id to flush the command
	// channel past the unknown retract: once its InputCancelled lands we know the
	// unknown retract was fully processed, so any InputCancelled for unknownID would
	// already be recorded.
	cancelQueuedInput(t, l, queuedID)
	blockUntilEvents(t, rec, func(evs []event.Event) bool {
		return hasInputCancelled(evs, queuedID, event.CancelClientRetracted)
	})
	for _, e := range rec.events() {
		if ic, ok := e.(event.InputCancelled); ok && ic.Cause.CommandID == unknownID {
			t.Fatalf("unknown-input retract published InputCancelled %+v, want no-op", ic)
		}
	}
}

// waitNoExtraInputCancelled waits a short settling window and reports whether the
// count of event.InputCancelled for (inputID, reason) stays at want. It returns
// true if the count never exceeds want during the window.
func waitNoExtraInputCancelled(t *testing.T, rec *recordingPublisher, inputID uuid.UUID, reason event.CancelReason, want int) bool {
	t.Helper()
	deadline := time.After(200 * time.Millisecond)
	for {
		n := 0
		for _, e := range rec.events() {
			if ic, ok := e.(event.InputCancelled); ok && ic.Cause.CommandID == inputID && ic.Reason == reason {
				n++
			}
		}
		if n > want {
			return false
		}
		select {
		case <-deadline:
			return true
		case <-time.After(2 * time.Millisecond):
		}
	}
}

// TestAbnormalTerminalReturnsQueuedInput: when a turn interrupts/fails with a
// queued inbox entry, the actor returns it via event.InputCancelled{reason,Message}
// and starts NO new turn from the returned entry.
func TestAbnormalTerminalReturnsQueuedInput(t *testing.T) {
	t.Parallel()

	t.Run("interrupt returns queued input as CancelTurnInterrupted", func(t *testing.T) {
		t.Parallel()
		l, rec, _ := newLoop(t, &fakeLLM{blockUntilCancel: true})
		startTurn(t, l, rec, nil)

		// Queue an input while the turn runs.
		queuedID := mustID(t)
		l.Commands <- command.UserInput{Header: command.Header{CommandID: queuedID}, Blocks: textBlocks("queued")}
		if _, ok := awaitReply(t, rec, queuedID).(event.InputQueued); !ok {
			t.Fatal("submit not queued")
		}

		// Interrupt: the running turn ends TurnInterrupted; the queued entry is
		// returned via InputCancelled{CancelTurnInterrupted}, NOT auto-started.
		iack := make(chan bool, 1)
		l.Commands <- command.Interrupt{Ack: iack}
		<-iack

		blockUntilEvents(t, rec, func(evs []event.Event) bool {
			for _, e := range evs {
				if ic, ok := e.(event.InputCancelled); ok {
					return ic.Cause.CommandID == queuedID && ic.Reason == event.CancelTurnInterrupted && ic.Message != nil
				}
			}
			return false
		})

		// No new turn was started from the returned entry: there is exactly ONE
		// TurnStarted (the original turn), never a second one for queuedID.
		for _, e := range rec.events() {
			if ts, ok := e.(event.TurnStarted); ok && ts.Cause.CommandID == queuedID {
				t.Fatal("returned input was auto-started, want no new turn")
			}
		}
	})

	t.Run("fail returns queued input as CancelTurnFailed", func(t *testing.T) {
		t.Parallel()
		bt := newBlockingTool()
		tools := agenticToolSet([]tool.InvokableTool{bt}, 25, 100)
		// Scripted client: turn 1 step 0 calls the blocking tool, holding the turn
		// running; on release the next stream (step 1) returns an empty response, which
		// fails the turn (EmptyResponseError -> TurnFailed). The input is queued at the
		// START of the empty step 1 — AFTER step 0's tool-continuation drain, so it is
		// NOT folded — so the failure returns it via InputCancelled{CancelTurnFailed}.
		queuedID := mustID(t)
		var l *Loop
		client := &scriptedLLM{scripts: [][]content.Chunk{
			{toolUseChunk(0, "id-1", "Block", `{}`)}, // step 0: hold the turn running
			{},                                       // step 1: empty -> EmptyResponseError -> TurnFailed
		}}
		ctx, cancel := context.WithCancel(context.Background())
		t.Cleanup(cancel)
		sessionID, _ := uuid.New()
		loopID, _ := uuid.New()
		rec := &recordingPublisher{}
		client.onStreamN = map[int]func(){
			1: func() {
				l.Commands <- command.UserInput{Header: command.Header{CommandID: queuedID}, Blocks: textBlocks("queued")}
				if _, ok := awaitReply(t, rec, queuedID).(event.InputQueued); !ok {
					t.Errorf("submit at step 1 not queued")
				}
			},
		}
		var err error
		l, err = New(ctx, sessionID, loopID, Provenance{}, rec,
			Config{Client: client, Model: testModel(), Tools: tools, DrainTimeout: 500 * time.Millisecond})
		if err != nil {
			t.Fatalf("New: %v", err)
		}

		startTurn(t, l, rec, nil)
		<-bt.started

		close(bt.release) // tool returns; step 0 drains (inbox empty), then step 1 queues + fails

		blockUntilEvents(t, rec, func(evs []event.Event) bool {
			for _, e := range evs {
				if ic, ok := e.(event.InputCancelled); ok {
					return ic.Cause.CommandID == queuedID && ic.Reason == event.CancelTurnFailed
				}
			}
			return false
		})
		for _, e := range rec.events() {
			if ts, ok := e.(event.TurnStarted); ok && ts.Cause.CommandID == queuedID {
				t.Fatal("returned input was auto-started after failure, want no new turn")
			}
		}
	})
}
