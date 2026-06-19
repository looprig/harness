package loop

import (
	"context"
	"testing"
	"time"

	"github.com/inventivepotter/urvi/internal/agent/loop/command"
	"github.com/inventivepotter/urvi/internal/agent/loop/event"
	"github.com/inventivepotter/urvi/internal/content"
	"github.com/inventivepotter/urvi/internal/llm"
	"github.com/inventivepotter/urvi/internal/tool"
	"github.com/inventivepotter/urvi/internal/uuid"
)

// cancelQueuedInput sends a CancelQueuedInput for inputID and returns the result.
func cancelQueuedInput(t *testing.T, l *Loop, inputID uuid.UUID) command.CancelResult {
	t.Helper()
	ack := make(chan command.CancelResult, 1)
	l.Commands <- command.CancelQueuedInput{Header: command.Header{ID: mustID(t)}, InputID: inputID, Ack: ack}
	select {
	case r := <-ack:
		return r
	case <-time.After(2 * time.Second):
		t.Fatal("CancelQueuedInput result not received")
		return nil
	}
}

// TestCancelWhileQueuedReturnsCancelled: a CancelQueuedInput for a still-queued
// submit returns Cancelled and emits event.InputCancelled{CancelClientRetracted}
// carrying the InputID and original Message; the entry is removed from the inbox
// (a second cancel finds it already gone -> AlreadyCommitted).
func TestCancelWhileQueuedReturnsCancelled(t *testing.T) {
	t.Parallel()
	sink := &captureSink{}
	l, _ := newLoop(t, &fakeLLM{blockUntilCancel: true}, sink)
	ev1, _ := startTurn(t, l, context.Background(), nil) // occupy the loop
	_ = ev1

	// Queue an input.
	queuedID := mustID(t)
	ack := make(chan command.Disposition, 1)
	l.Commands <- command.UserInput{Header: command.Header{ID: queuedID}, Mode: command.AllowFold, Blocks: textBlocks("retract me"), Ack: ack}
	if _, ok := (<-ack).(command.InputQueued); !ok {
		t.Fatal("submit not queued")
	}

	// Cancel it while still queued.
	r := cancelQueuedInput(t, l, queuedID)
	if _, ok := r.(command.Cancelled); !ok {
		t.Fatalf("cancel result = %T, want Cancelled", r)
	}

	// event.InputCancelled{CancelClientRetracted} must be emitted with the InputID.
	blockUntilSink(t, sink, func(evs []event.EventEnvelope) bool {
		for _, e := range evs {
			if ic, ok := e.Event.(event.InputCancelled); ok {
				return ic.InputID == queuedID && ic.Reason == event.CancelClientRetracted && ic.Message != nil
			}
		}
		return false
	})

	// A second cancel finds it gone: AlreadyCommitted (it left the queue).
	r2 := cancelQueuedInput(t, l, queuedID)
	if _, ok := r2.(command.AlreadyCommitted); !ok {
		t.Fatalf("second cancel result = %T, want AlreadyCommitted", r2)
	}
}

// TestCancelUnknownInputAlreadyCommitted: a CancelQueuedInput for an InputID not in
// the inbox (never queued, or already started/committed) returns AlreadyCommitted.
func TestCancelUnknownInputAlreadyCommitted(t *testing.T) {
	t.Parallel()
	l, _ := newLoop(t, &fakeLLM{blockUntilCancel: true})
	ev1, _ := startTurn(t, l, context.Background(), nil)
	_ = ev1

	r := cancelQueuedInput(t, l, mustID(t)) // never queued
	if _, ok := r.(command.AlreadyCommitted); !ok {
		t.Fatalf("cancel result = %T, want AlreadyCommitted", r)
	}
}

// TestAbnormalTerminalReturnsQueuedInput: when a turn interrupts/fails with a
// queued inbox entry, the actor returns it via event.InputCancelled{reason,Message}
// and starts NO new turn from the returned entry.
func TestAbnormalTerminalReturnsQueuedInput(t *testing.T) {
	t.Parallel()

	t.Run("interrupt returns queued input as CancelTurnInterrupted", func(t *testing.T) {
		t.Parallel()
		sink := &captureSink{}
		l, _ := newLoop(t, &fakeLLM{blockUntilCancel: true}, sink)
		ev1, _ := startTurn(t, l, context.Background(), nil)
		go func() {
			for range ev1 {
			}
		}()

		// Queue an input while the turn runs.
		queuedID := mustID(t)
		ack := make(chan command.Disposition, 1)
		l.Commands <- command.UserInput{Header: command.Header{ID: queuedID}, Mode: command.AllowFold, Blocks: textBlocks("queued"), Ack: ack}
		if _, ok := (<-ack).(command.InputQueued); !ok {
			t.Fatal("submit not queued")
		}

		// Interrupt: the running turn ends TurnInterrupted; the queued entry is
		// returned via InputCancelled{CancelTurnInterrupted}, NOT auto-started.
		iack := make(chan bool, 1)
		l.Commands <- command.Interrupt{Ack: iack}
		<-iack

		blockUntilSink(t, sink, func(evs []event.EventEnvelope) bool {
			for _, e := range evs {
				if ic, ok := e.Event.(event.InputCancelled); ok {
					return ic.InputID == queuedID && ic.Reason == event.CancelTurnInterrupted && ic.Message != nil
				}
			}
			return false
		})

		// No new turn was started from the returned entry: there is exactly ONE
		// TurnStarted (the original turn), never a second one for queuedID.
		for _, e := range sink.events() {
			if ts, ok := e.Event.(event.TurnStarted); ok && ts.InputID == queuedID {
				t.Fatal("returned input was auto-started, want no new turn")
			}
		}
	})

	t.Run("fail returns queued input as CancelTurnFailed", func(t *testing.T) {
		t.Parallel()
		bt := newBlockingTool()
		tools := agenticToolSet([]tool.InvokableTool{bt}, 25, 100)
		// Scripted client: turn 1 step 0 calls the blocking tool, holding the turn
		// running; on release the next stream returns an empty response, which fails
		// the turn (EmptyResponseError -> TurnFailed).
		client := &scriptedLLM{scripts: [][]content.Chunk{
			{toolUseChunk(0, "id-1", "Block", `{}`)}, // hold the turn running
			{}, // empty -> EmptyResponseError -> TurnFailed
		}}
		ctx, cancel := context.WithCancel(context.Background())
		t.Cleanup(cancel)
		sessionID, _ := uuid.New()
		loopID, _ := uuid.New()
		sink := &captureSink{}
		l, err := New(ctx, sessionID, loopID, Provenance{}, noopPublisher{},
			Config{Client: client, Model: llm.ModelSpec{Model: "m"}, Tools: tools, Sinks: []event.EventSink{sink}, DrainTimeout: 500 * time.Millisecond})
		if err != nil {
			t.Fatalf("New: %v", err)
		}

		ev1, _ := startTurn(t, l, context.Background(), nil)
		go func() {
			for range ev1 {
			}
		}()
		<-bt.started

		queuedID := mustID(t)
		ack := make(chan command.Disposition, 1)
		l.Commands <- command.UserInput{Header: command.Header{ID: queuedID}, Mode: command.AllowFold, Blocks: textBlocks("queued"), Ack: ack}
		if _, ok := (<-ack).(command.InputQueued); !ok {
			t.Fatal("submit not queued")
		}

		close(bt.release) // tool returns; next stream is empty -> TurnFailed

		blockUntilSink(t, sink, func(evs []event.EventEnvelope) bool {
			for _, e := range evs {
				if ic, ok := e.Event.(event.InputCancelled); ok {
					return ic.InputID == queuedID && ic.Reason == event.CancelTurnFailed
				}
			}
			return false
		})
		for _, e := range sink.events() {
			if ts, ok := e.Event.(event.TurnStarted); ok && ts.InputID == queuedID {
				t.Fatal("returned input was auto-started after failure, want no new turn")
			}
		}
	})
}
