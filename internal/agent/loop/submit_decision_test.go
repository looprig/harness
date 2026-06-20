package loop

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/inventivepotter/urvi/internal/agent/loop/command"
	"github.com/inventivepotter/urvi/internal/agent/loop/event"
	"github.com/inventivepotter/urvi/internal/agent/loop/identity"
	"github.com/inventivepotter/urvi/internal/content"
	"github.com/inventivepotter/urvi/internal/llm"
	"github.com/inventivepotter/urvi/internal/uuid"
)

// submitUserInput sends an AllowFold/StartOnly UserInput (fan-in only) with the
// given id and returns the published outcome event (TurnStarted/InputQueued/
// TurnRejected) observed via the recording publisher. It is the interactive submit
// path (no per-turn stream).
func submitUserInput(t *testing.T, l *Loop, rec *recordingPublisher, id uuid.UUID, mode command.InputMode) event.Event {
	t.Helper()
	l.Commands <- command.UserInput{Header: command.Header{CommandID: id}, Mode: mode}
	return awaitReply(t, rec, id)
}

// textBlocks wraps a string in a one-element block slice.
func textBlocks(s string) []content.Block {
	return []content.Block{&content.TextBlock{Text: s}}
}

// TestSubmitToIdleStartsTurn: an AllowFold UserInput to an idle loop publishes
// event.TurnStarted (the Started outcome) carrying InputID, Message, and
// Cause.CommandID == InputID. There is no separate Started reply: the TurnStarted IS the
// outcome.
func TestSubmitToIdleStartsTurn(t *testing.T) {
	t.Parallel()
	l, rec, _ := newLoopRec(t, &fakeLLM{chunks: []content.Chunk{textChunk("hi")}})

	inputID := mustID(t)
	l.Commands <- command.UserInput{Header: command.Header{CommandID: inputID}, Mode: command.AllowFold, Blocks: textBlocks("hello")}

	ev := awaitReply(t, rec, inputID)
	started, ok := ev.(event.TurnStarted)
	if !ok {
		t.Fatalf("outcome = %T, want event.TurnStarted", ev)
	}
	if started.Cause.CommandID != inputID {
		t.Errorf("TurnStarted Cause.CommandID = %v, want %v", started.Cause.CommandID, inputID)
	}
	if started.TurnID.IsZero() {
		t.Error("TurnStarted.TurnID is zero, want a minted turn id")
	}
	if started.Message == nil {
		t.Error("TurnStarted.Message is nil, want the committed user message")
	}
}

// TestSubagentResultToIdleStartsTurn: a SubagentResult to an idle loop starts a turn
// and stamps the producing CHILD subagent's loop id (Cause.LoopID) as Cause.LoopID on
// the published event.TurnStarted — NOT the PARENT loop id carried by the embedded
// Coordinates (the delivery target). This is the end-to-end proof that the wake token
// rides the CHILD, which is the behavior the old FromLoopID provided.
func TestSubagentResultToIdleStartsTurn(t *testing.T) {
	t.Parallel()
	l, rec, _ := newLoopRec(t, &fakeLLM{chunks: []content.Chunk{textChunk("hi")}})

	inputID := mustID(t)
	childLoop := mustID(t)  // the producing subagent (wake token)
	parentLoop := mustID(t) // the delivery target — must NOT appear as Cause.LoopID
	l.Commands <- command.SubagentResult{
		Coordinates: identity.Coordinates{LoopID: parentLoop},
		Header:      command.Header{CommandID: inputID, Cause: identity.Cause{Coordinates: identity.Coordinates{LoopID: childLoop}}},
		Blocks:      textBlocks("subagent output"),
	}

	ev := awaitReply(t, rec, inputID)
	started, ok := ev.(event.TurnStarted)
	if !ok || started.Cause.CommandID != inputID {
		t.Fatalf("outcome = %+v, want event.TurnStarted{Cause.CommandID:%v}", ev, inputID)
	}
	if started.Cause.LoopID != childLoop {
		t.Errorf("TurnStarted.Cause.LoopID = %v, want %v (the CHILD = wake token)", started.Cause.LoopID, childLoop)
	}
	if started.Cause.LoopID == parentLoop {
		t.Errorf("TurnStarted.Cause.LoopID = %v leaked the PARENT delivery target, want the CHILD", started.Cause.LoopID)
	}
}

// TestSubmitToRunningQueueableQueues: an AllowFold UserInput to a running loop
// publishes event.InputQueued{InputID} (Cause.CommandID == InputID, no TurnID) and is
// held in the inbox in order.
func TestSubmitToRunningQueueableQueues(t *testing.T) {
	t.Parallel()
	// blockUntilCancel keeps the first turn running so the second submit queues.
	l, rec, _ := newLoopRec(t, &fakeLLM{blockUntilCancel: true})
	ev1, _ := startTurn(t, l, context.Background(), nil) // StartOnly: occupies the loop
	_ = ev1

	idA := mustID(t)
	idB := mustID(t)
	qA := submitUserInput(t, l, rec, idA, command.AllowFold)
	qB := submitUserInput(t, l, rec, idB, command.AllowFold)

	for _, tc := range []struct {
		name string
		ev   event.Event
		want uuid.UUID
	}{
		{"first queued", qA, idA},
		{"second queued", qB, idB},
	} {
		q, ok := tc.ev.(event.InputQueued)
		if !ok {
			t.Fatalf("%s: outcome = %T, want event.InputQueued", tc.name, tc.ev)
		}
		if q.Cause.CommandID != tc.want {
			t.Errorf("%s: InputQueued Cause.CommandID = %v, want %v", tc.name, q.Cause.CommandID, tc.want)
		}
		if !q.TurnID.IsZero() {
			t.Errorf("%s: InputQueued.TurnID = %v, want zero (no turn yet)", tc.name, q.TurnID)
		}
	}
}

// TestSubagentResultToFullInboxQueues: a SubagentResult is NEVER rejected — even
// when the inbox is at inboxCap it is QUEUED (appended), publishing
// event.InputQueued (NOT event.TurnRejected). This bypasses the queue-full reject so
// the subagent's quiescence {wake} token is always released by a resulting Enduring
// event.
func TestSubagentResultToFullInboxQueues(t *testing.T) {
	t.Parallel()
	l, rec, _ := newLoopRec(t, &fakeLLM{blockUntilCancel: true})
	ev1, _ := startTurn(t, l, context.Background(), nil) // occupy the loop
	_ = ev1

	// Fill the inbox to capacity with AllowFold UserInputs.
	for i := 0; i < inboxCap; i++ {
		id := mustID(t)
		if _, ok := submitUserInput(t, l, rec, id, command.AllowFold).(event.InputQueued); !ok {
			t.Fatalf("submit %d: want event.InputQueued", i)
		}
	}

	// A SubagentResult to the full inbox must still QUEUE (never reject).
	childLoop := mustID(t)
	srID := mustID(t)
	l.Commands <- command.SubagentResult{
		Coordinates: identity.Coordinates{LoopID: mustID(t)}, // PARENT delivery target
		Header:      command.Header{CommandID: srID, Cause: identity.Cause{Coordinates: identity.Coordinates{LoopID: childLoop}}},
		Blocks:      textBlocks("subagent output"),
	}
	ev := awaitReply(t, rec, srID)
	q, ok := ev.(event.InputQueued)
	if !ok {
		t.Fatalf("SubagentResult to full inbox outcome = %T, want event.InputQueued (never rejected)", ev)
	}
	if q.Cause.CommandID != srID || q.Cause.LoopID != childLoop {
		t.Errorf("InputQueued InputID/Cause.LoopID = %v/%v, want %v/%v", q.Cause.CommandID, q.Cause.LoopID, srID, childLoop)
	}
}

// TestSubagentResultIDGenerationFailureCancels: a SubagentResult delivered to an
// IDLE loop whose TurnID id-gen fails is NEVER rejected — it surfaces as
// event.InputCancelled{CancelTurnFailed} (NOT event.TurnRejected), because a
// SubagentResult's {wake} quiescence token releases only via an Enduring event
// carrying Cause.LoopID (InputCancelled does; TurnRejected does NOT). This is
// the SubagentResult half of the idle id-gen-failure branch (loop.go decideSubmit's
// bypassReject path); the plain-UserInput half is covered by
// TestTurnIDGenerationFailure. The cancellation must carry Cause.CommandID == the
// SubagentResult command id (== InputID) and Cause.LoopID == the CHILD loop
// (Header.Cause.LoopID).
func TestSubagentResultIDGenerationFailureCancels(t *testing.T) {
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
			rec := &recordingPublisher{}
			gen := &countedIDGen{okCount: tt.okCount, err: genErr}
			ctx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)
			l, err := New(ctx, mustID(t), mustID(t), Provenance{}, rec, Config{
				Client:       &fakeLLM{chunks: []content.Chunk{textChunk("hi")}},
				Model:        llm.ModelSpec{Model: "m"},
				DrainTimeout: 200 * time.Millisecond,
				idGen:        gen.gen,
			})
			if err != nil {
				t.Fatalf("New: %v", err)
			}

			inputID := mustID(t)
			childLoop := mustID(t)
			l.Commands <- command.SubagentResult{
				Coordinates: identity.Coordinates{LoopID: mustID(t)}, // PARENT delivery target
				Header:      command.Header{CommandID: inputID, Cause: identity.Cause{Coordinates: identity.Coordinates{LoopID: childLoop}}},
				Blocks:      textBlocks("subagent output"),
			}

			// A never-rejected SubagentResult that cannot mint its TurnID resolves as
			// InputCancelled{CancelTurnFailed} (the {wake} release rides this Enduring event),
			// recognised on the fan-in via ReplyTo() == its command id.
			ev := awaitReply(t, rec, inputID)
			ic, ok := ev.(event.InputCancelled)
			if !ok {
				t.Fatalf("outcome = %T, want event.InputCancelled (SubagentResult is never rejected)", ev)
			}
			if ic.Reason != event.CancelTurnFailed {
				t.Errorf("InputCancelled.Reason = %d, want CancelTurnFailed", ic.Reason)
			}
			if ic.Cause.CommandID != inputID {
				t.Errorf("InputCancelled Cause.CommandID = %v, want %v", ic.Cause.CommandID, inputID)
			}
			if ic.Cause.LoopID != childLoop {
				t.Errorf("InputCancelled.Cause.LoopID = %v, want %v (the CHILD releases the {wake} token)", ic.Cause.LoopID, childLoop)
			}

			// It must NOT be rejected: a TurnRejected would not carry/release the {wake} token.
			for _, e := range rec.events() {
				if rej, ok := e.(event.TurnRejected); ok && rej.Cause.CommandID == inputID {
					t.Fatalf("SubagentResult was rejected (%+v); a SubagentResult is never rejected", rej)
				}
			}
		})
	}
}

// TestStartOnlyBusyRejected: a StartOnly UserInput to a running loop publishes
// event.TurnRejected{RejectBusy} (it must start or be rejected, never queue) and the
// same reason is delivered on the caller's per-turn stream, which is then closed.
func TestStartOnlyBusyRejected(t *testing.T) {
	t.Parallel()
	l, rec, _ := newLoopRec(t, &fakeLLM{blockUntilCancel: true})
	ev1, _ := startTurn(t, l, context.Background(), nil)
	_ = ev1

	id := mustID(t)
	ev := make(chan event.Event, 1)
	ab := make(chan struct{})
	defer close(ab)
	l.Commands <- command.UserInput{Header: command.Header{CommandID: id}, Mode: command.StartOnly, Events: ev, Abandoned: ab}

	// The per-turn stream carries the reason as its first event, then closes.
	first, ok := <-ev
	if !ok {
		t.Fatal("per-turn stream closed without a TurnRejected event")
	}
	rej, ok := first.(event.TurnRejected)
	if !ok || rej.Reason != event.RejectBusy {
		t.Fatalf("per-turn outcome = %+v, want event.TurnRejected{RejectBusy}", first)
	}
	if _, open := <-ev; open {
		t.Error("rejected turn's per-turn stream should be closed after the reason")
	}

	// And the published fan-in event mirrors it (Cause.CommandID == InputID).
	pub, ok := awaitReply(t, rec, id).(event.TurnRejected)
	if !ok || pub.Reason != event.RejectBusy || pub.Cause.CommandID != id {
		t.Fatalf("published outcome = %+v, want event.TurnRejected{RejectBusy, Cause.CommandID:%v}", pub, id)
	}
}

// TestInboxFullRejected: when the inbox is at inboxCap, a further AllowFold submit
// is rejected with event.TurnRejected{RejectQueueFull} (a length check; never
// blocks).
func TestInboxFullRejected(t *testing.T) {
	t.Parallel()
	l, rec, _ := newLoopRec(t, &fakeLLM{blockUntilCancel: true})
	ev1, _ := startTurn(t, l, context.Background(), nil) // occupy the loop
	_ = ev1

	// Fill the inbox to capacity.
	for i := 0; i < inboxCap; i++ {
		id := mustID(t)
		if _, ok := submitUserInput(t, l, rec, id, command.AllowFold).(event.InputQueued); !ok {
			t.Fatalf("submit %d: want event.InputQueued", i)
		}
	}
	// One more: the queue is full.
	id := mustID(t)
	ev := submitUserInput(t, l, rec, id, command.AllowFold)
	rej, ok := ev.(event.TurnRejected)
	if !ok || rej.Reason != event.RejectQueueFull {
		t.Fatalf("outcome = %+v, want event.TurnRejected{RejectQueueFull}", ev)
	}
}

// TestShuttingDownRejected: an AllowFold submit after the loop is shutting down is
// rejected with event.TurnRejected{RejectShuttingDown}.
func TestShuttingDownRejected(t *testing.T) {
	t.Parallel()
	l, rec, _ := newLoopRec(t, &fakeLLM{blockUntilCancel: true})
	ev1, _ := startTurn(t, l, context.Background(), nil)
	_ = ev1

	// Shutdown flips status to loopShuttingDown; the running turn winds down.
	sack := make(chan error, 1)
	l.Commands <- command.Shutdown{Ack: sack}

	// A submit during shutdown is rejected. The loop may still be running the
	// winding-down turn, so poll until it reports ShuttingDown (or the loop exits).
	deadline := time.After(2 * time.Second)
	for {
		id := mustID(t)
		select {
		case l.Commands <- command.UserInput{Header: command.Header{CommandID: id}, Mode: command.AllowFold}:
		case <-l.Done:
			// Loop exited before we observed the rejection — acceptable: a stopped
			// loop also refuses input. End the test.
			<-sack
			return
		}
		if rej, ok := awaitReplyOrNil(rec, id).(event.TurnRejected); ok && rej.Reason == event.RejectShuttingDown {
			<-sack
			return
		}
		select {
		case <-deadline:
			t.Fatal("never observed RejectShuttingDown")
		case <-time.After(5 * time.Millisecond):
		}
	}
}

// awaitReplyOrNil returns the published Reply for inputID if one is already in the
// recording publisher, else nil (non-blocking single sweep). TestShuttingDownRejected
// uses it to poll without failing on a not-yet-rejected submit.
func awaitReplyOrNil(rec *recordingPublisher, inputID uuid.UUID) event.Event {
	for _, e := range rec.events() {
		if r, ok := e.(event.Reply); ok && r.ReplyTo() == inputID {
			return e
		}
	}
	return nil
}

// TestNormalCompletionPopsInbox: a turn that completes normally with a non-empty
// inbox makes the actor pop the FIRST queued entry and start a later turn from it.
// Turn 1 is a single no-tool text step; the input is queued at the START of that step
// (after any drain, so it is NOT folded — folding happens only at a tool-continuation
// boundary), so on the normal terminal the actor pops it and starts turn 2, which
// drives a second TurnStarted carrying its InputID.
func TestNormalCompletionPopsInbox(t *testing.T) {
	t.Parallel()
	ts := agenticToolSet(nil, 25, 100)
	queuedID := mustID(t)
	client := &scriptedLLM{scripts: [][]content.Chunk{
		{textChunk("done turn 1")}, // turn 1: single text step -> TurnDone (no fold)
		{textChunk("done turn 2")}, // turn 2 (from queued input) -> TurnDone
	}}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	sessionID, _ := uuid.New()
	loopID, _ := uuid.New()
	rec := &recordingPublisher{}
	l, err := New(ctx, sessionID, loopID, Provenance{}, rec,
		Config{Client: client, Model: llm.ModelSpec{Model: "m"}, Tools: ts, DrainTimeout: 500 * time.Millisecond})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// Queue a UserInput at the START of turn 1's only (no-tool) step. The turn is
	// running, so it is accepted into the inbox; the no-tool step performs no drain, so
	// it is not folded and waits for the on-idle pop.
	client.mu.Lock()
	client.onStreamN = map[int]func(){
		0: func() {
			l.Commands <- command.UserInput{Header: command.Header{CommandID: queuedID}, Mode: command.AllowFold}
		},
	}
	client.mu.Unlock()

	// Turn 1 starts (StartOnly).
	ev1, _ := startTurn(t, l, context.Background(), textBlocks("turn1"))
	go func() { // drain turn 1's per-turn stream so emit never blocks
		for range ev1 {
		}
	}()

	// Turn 1 completes normally (TurnDone); the actor pops the queued entry and starts
	// turn 2 from it. A second event.TurnStarted must appear carrying the queued InputID.
	blockUntilEvents(t, rec, func(evs []event.Event) bool {
		for _, e := range evs {
			if ts, ok := e.(event.TurnStarted); ok && ts.Cause.CommandID == queuedID {
				return true
			}
		}
		return false
	})
}
