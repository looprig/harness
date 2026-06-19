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

// submitUserInput sends an AllowFold UserInput (fan-in only) with the given id and
// returns its Disposition. It is the interactive submit path (no per-turn stream).
func submitUserInput(t *testing.T, l *Loop, id uuid.UUID, mode command.InputMode) command.Disposition {
	t.Helper()
	ack := make(chan command.Disposition, 1)
	l.Commands <- command.UserInput{Header: command.Header{ID: id}, Mode: mode, Ack: ack}
	select {
	case d := <-ack:
		return d
	case <-time.After(2 * time.Second):
		t.Fatal("UserInput disposition not received")
		return nil
	}
}

// textBlocks wraps a string in a one-element block slice.
func textBlocks(s string) []content.Block {
	return []content.Block{&content.TextBlock{Text: s}}
}

// blockUntilSink waits until pred sees the captured sink events, or fails.
func blockUntilSink(t *testing.T, sink *captureSink, pred func([]event.EventEnvelope) bool) {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		if pred(sink.events()) {
			return
		}
		select {
		case <-deadline:
			t.Fatal("sink condition not met within deadline")
		case <-time.After(2 * time.Millisecond):
		}
	}
}

// TestSubmitToIdleStartsTurn: an AllowFold UserInput to an idle loop returns
// Started{TurnID, InputID} and emits event.TurnStarted carrying InputID, Message,
// and CausationID == InputID.
func TestSubmitToIdleStartsTurn(t *testing.T) {
	t.Parallel()
	sink := &captureSink{}
	l, _ := newLoop(t, &fakeLLM{chunks: []content.Chunk{textChunk("hi")}}, sink)

	inputID := mustID(t)
	ack := make(chan command.Disposition, 1)
	l.Commands <- command.UserInput{Header: command.Header{ID: inputID}, Mode: command.AllowFold, Blocks: textBlocks("hello"), Ack: ack}
	d := <-ack

	started, ok := d.(command.Started)
	if !ok {
		t.Fatalf("disposition = %T, want Started", d)
	}
	if started.InputID != inputID {
		t.Errorf("Started.InputID = %v, want %v", started.InputID, inputID)
	}
	if started.TurnID.IsZero() {
		t.Error("Started.TurnID is zero, want a minted turn id")
	}

	// event.TurnStarted carries InputID, Message, CausationID == InputID.
	blockUntilSink(t, sink, func(evs []event.EventEnvelope) bool {
		for _, e := range evs {
			if ts, ok := e.Event.(event.TurnStarted); ok {
				return ts.InputID == inputID && ts.CausationID == inputID && ts.Message != nil
			}
		}
		return false
	})
}

// TestSubmitToRunningQueueableQueues: an AllowFold UserInput to a running loop
// returns InputQueued{InputID} (no TurnID) and is held in the inbox in order.
func TestSubmitToRunningQueueableQueues(t *testing.T) {
	t.Parallel()
	// blockUntilCancel keeps the first turn running so the second submit queues.
	l, _ := newLoop(t, &fakeLLM{blockUntilCancel: true})
	ev1, _ := startTurn(t, l, context.Background(), nil) // StartOnly: occupies the loop
	_ = ev1

	idA := mustID(t)
	idB := mustID(t)
	dA := submitUserInput(t, l, idA, command.AllowFold)
	dB := submitUserInput(t, l, idB, command.AllowFold)

	for _, tc := range []struct {
		name string
		d    command.Disposition
		want uuid.UUID
	}{
		{"first queued", dA, idA},
		{"second queued", dB, idB},
	} {
		q, ok := tc.d.(command.InputQueued)
		if !ok {
			t.Fatalf("%s: disposition = %T, want InputQueued", tc.name, tc.d)
		}
		if q.InputID != tc.want {
			t.Errorf("%s: InputQueued.InputID = %v, want %v", tc.name, q.InputID, tc.want)
		}
	}
}

// TestStartOnlyBusyRejected: a StartOnly UserInput to a running loop returns
// TurnRejected{RejectBusy} (it must start or be rejected, never queue).
func TestStartOnlyBusyRejected(t *testing.T) {
	t.Parallel()
	l, _ := newLoop(t, &fakeLLM{blockUntilCancel: true})
	ev1, _ := startTurn(t, l, context.Background(), nil)
	_ = ev1

	d := submitUserInput(t, l, mustID(t), command.StartOnly)
	rej, ok := d.(command.TurnRejected)
	if !ok || rej.Reason != command.RejectBusy {
		t.Fatalf("disposition = %+v, want TurnRejected{RejectBusy}", d)
	}
}

// TestInboxFullRejected: when the inbox is at inboxCap, a further AllowFold submit
// is rejected with TurnRejected{RejectQueueFull} (a length check; never blocks).
func TestInboxFullRejected(t *testing.T) {
	t.Parallel()
	l, _ := newLoop(t, &fakeLLM{blockUntilCancel: true})
	ev1, _ := startTurn(t, l, context.Background(), nil) // occupy the loop
	_ = ev1

	// Fill the inbox to capacity.
	for i := 0; i < inboxCap; i++ {
		d := submitUserInput(t, l, mustID(t), command.AllowFold)
		if _, ok := d.(command.InputQueued); !ok {
			t.Fatalf("submit %d: disposition = %T, want InputQueued", i, d)
		}
	}
	// One more: the queue is full.
	d := submitUserInput(t, l, mustID(t), command.AllowFold)
	rej, ok := d.(command.TurnRejected)
	if !ok || rej.Reason != command.RejectQueueFull {
		t.Fatalf("disposition = %+v, want TurnRejected{RejectQueueFull}", d)
	}
}

// TestShuttingDownRejected: a submit after the loop is shutting down returns
// TurnRejected{RejectShuttingDown}.
func TestShuttingDownRejected(t *testing.T) {
	t.Parallel()
	l, _ := newLoop(t, &fakeLLM{blockUntilCancel: true})
	ev1, _ := startTurn(t, l, context.Background(), nil)
	_ = ev1

	// Shutdown flips status to loopShuttingDown; the running turn winds down.
	sack := make(chan error, 1)
	l.Commands <- command.Shutdown{Ack: sack}

	// A submit during shutdown is rejected. The loop may still be running the
	// winding-down turn, so poll until it reports ShuttingDown (or the loop exits).
	deadline := time.After(2 * time.Second)
	for {
		ack := make(chan command.Disposition, 1)
		select {
		case l.Commands <- command.UserInput{Header: command.Header{ID: mustID(t)}, Mode: command.AllowFold, Ack: ack}:
		case <-l.Done:
			// Loop exited before we observed the rejection — acceptable: a stopped
			// loop also refuses input. End the test.
			<-sack
			return
		}
		d := <-ack
		if rej, ok := d.(command.TurnRejected); ok && rej.Reason == command.RejectShuttingDown {
			<-sack
			return
		}
		select {
		case <-deadline:
			t.Fatalf("never observed RejectShuttingDown; last disposition = %T", d)
		case <-time.After(5 * time.Millisecond):
		}
	}
}

// TestNormalCompletionPopsInbox: a turn that completes normally with a non-empty
// inbox makes the actor pop the FIRST queued entry and start a later turn from it.
// The first turn is parked in a blocking tool (so it is running while we queue),
// then released so it completes (TurnDone); the queued input then drives a second
// TurnStarted carrying its InputID.
func TestNormalCompletionPopsInbox(t *testing.T) {
	t.Parallel()
	bt := newBlockingTool()
	ts := agenticToolSet([]tool.InvokableTool{bt}, 25, 100)
	client := &scriptedLLM{scripts: [][]content.Chunk{
		{toolUseChunk(0, "id-1", "Block", `{}`)}, // turn 1 step 0: blocking tool
		{textChunk("done turn 1")},               // turn 1 step 1: text -> TurnDone
		{textChunk("done turn 2")},               // turn 2 (from queued input) -> TurnDone
	}}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	sessionID, _ := uuid.New()
	loopID, _ := uuid.New()
	sink := &captureSink{}
	l, err := New(ctx, sessionID, loopID, Provenance{}, noopPublisher{},
		Config{Client: client, Model: llm.ModelSpec{Model: "m"}, Tools: ts, Sinks: []event.EventSink{sink}, DrainTimeout: 500 * time.Millisecond})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// Turn 1 starts (StartOnly), parks in the blocking tool.
	ev1, _ := startTurn(t, l, context.Background(), textBlocks("turn1"))
	go func() { // drain turn 1's per-turn stream so emit never blocks
		for range ev1 {
		}
	}()
	<-bt.started // turn 1 is now running, parked in the tool

	// Queue a UserInput while running; it must be accepted.
	queuedID := mustID(t)
	d := submitUserInput(t, l, queuedID, command.AllowFold)
	if _, ok := d.(command.InputQueued); !ok {
		t.Fatalf("queued submit disposition = %T, want InputQueued", d)
	}

	// Release the tool: turn 1 completes normally (TurnDone), then the actor pops
	// the queued entry and starts turn 2 from it.
	close(bt.release)

	// A second event.TurnStarted must appear carrying the queued InputID.
	blockUntilSink(t, sink, func(evs []event.EventEnvelope) bool {
		for _, e := range evs {
			if ts, ok := e.Event.(event.TurnStarted); ok && ts.InputID == queuedID {
				return true
			}
		}
		return false
	})
}
