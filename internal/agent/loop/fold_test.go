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

// newFoldLoop builds a loop with the given scripted client + blocking tool, a sink
// to observe events, and a generous DrainTimeout. It returns the loop and the sink.
func newFoldLoop(t *testing.T, client llm.LLM, ts ToolSet) (*Loop, *captureSink) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	sessionID, err := uuid.New()
	if err != nil {
		t.Fatalf("uuid.New: %v", err)
	}
	loopID, err := uuid.New()
	if err != nil {
		t.Fatalf("uuid.New: %v", err)
	}
	sink := &captureSink{}
	l, err := New(ctx, sessionID, loopID, Provenance{}, noopPublisher{},
		Config{Client: client, Model: llm.ModelSpec{Model: "m"}, Tools: ts, Sinks: []event.EventSink{sink}, DrainTimeout: 500 * time.Millisecond})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return l, sink
}

// waitForRequests polls the scripted client until it has recorded at least n
// requests, or fails. The continuation request after a fold is issued asynchronously
// (after the fold commits), so a test must wait for it rather than read once.
func waitForRequests(t *testing.T, client *scriptedLLM, n int) []llm.Request {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		reqs := client.requests()
		if len(reqs) >= n {
			return reqs
		}
		select {
		case <-deadline:
			t.Fatalf("recorded %d requests, want >= %d (initial + continuation)", len(reqs), n)
			return nil
		case <-time.After(2 * time.Millisecond):
		}
	}
}

// TestFoldAtToolContinuation proves the core fold behavior: input queued behind a
// running tool-using turn folds at the tool-continuation boundary. After the tool
// step commits, runTurn drains the inbox and emits event.TurnFoldedInto for the
// queued message (CausationID == InputID, no TriggeredByLoopID for a UserInput),
// the folded message appears in the next LLM request AFTER the tool result and
// BEFORE the next assistant message, and NO second TurnStarted is emitted (the
// input rode along; it did not start a new turn).
func TestFoldAtToolContinuation(t *testing.T) {
	t.Parallel()
	bt := newBlockingTool()
	ts := agenticToolSet([]tool.InvokableTool{bt}, 25, 100)
	client := &scriptedLLM{scripts: [][]content.Chunk{
		{toolUseChunk(0, "id-1", "Block", `{}`)}, // step 0: blocking tool (turn parks here)
		{textChunk("final")},                      // step 1: text -> TurnDone (after the fold)
	}}
	l, sink := newFoldLoop(t, client, ts)

	// Turn 1 starts and parks in the blocking tool.
	ev1, _ := startTurn(t, l, context.Background(), textBlocks("turn1"))
	go func() {
		for range ev1 {
		}
	}()
	<-bt.started // step 0's tool is blocked; the inbox can be filled before the drain

	// Queue an input while the tool step is in flight.
	foldedID := mustID(t)
	d := submitUserInputBlocks(t, l, foldedID, command.AllowFold, textBlocks("folded text"))
	if _, ok := d.(command.InputQueued); !ok {
		t.Fatalf("queued submit disposition = %T, want InputQueued", d)
	}

	// Release the tool: step 0 commits, then runTurn drains the inbox and folds the
	// queued input into the mandatory continuation request.
	close(bt.release)

	// event.TurnFoldedInto must be emitted for the queued input, with CausationID ==
	// InputID and a zero TriggeredByLoopID (a UserInput, not a hand-back).
	blockUntilSink(t, sink, func(evs []event.EventEnvelope) bool {
		for _, e := range evs {
			if fi, ok := e.Event.(event.TurnFoldedInto); ok {
				return fi.InputID == foldedID &&
					fi.CausationID == foldedID &&
					fi.TriggeredByLoopID.IsZero() &&
					fi.Message != nil
			}
		}
		return false
	})

	// The folded input did NOT start a new turn: there is exactly ONE TurnStarted and
	// it is NOT for foldedID.
	for _, e := range sink.events() {
		if tsv, ok := e.Event.(event.TurnStarted); ok && tsv.InputID == foldedID {
			t.Fatal("folded input started a new turn, want it folded into the running turn")
		}
	}

	// The continuation request (step 1) carries the folded UserMessage AFTER the tool
	// result and BEFORE the next assistant message. History at that point is:
	//   user(turn1), AI(tool_use id-1), tool(id-1), user(folded)
	// The continuation request is issued AFTER the fold commits (TurnFoldedInto is
	// emitted at the commit point, before runTurn loops to the next runStep), so wait
	// for the second request to be recorded rather than reading once.
	reqs := waitForRequests(t, client, 2)
	cont := reqs[1].Messages
	wantKinds := []string{"user", "ai", "tool", "user"}
	if len(cont) != len(wantKinds) {
		t.Fatalf("continuation request had %d messages, want %d (user, AI tool_use, tool, folded user)", len(cont), len(wantKinds))
	}
	for i, m := range cont {
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
			t.Errorf("continuation request[%d] = %s, want %s", i, kind, wantKinds[i])
		}
	}
	// The last message is the folded user message (its text), proving order.
	if um, ok := cont[3].(*content.UserMessage); !ok {
		t.Errorf("continuation request[3] = %T, want *UserMessage (the folded message)", cont[3])
	} else if got := flattenToText(um.Blocks); got != "folded text" {
		t.Errorf("folded user message text = %q, want %q", got, "folded text")
	}
}

// TestSubagentResultFoldStampsTriggeredBy proves a SubagentResult hand-back that
// folds at a tool-continuation boundary stamps TurnFoldedInto.TriggeredByLoopID with
// the producing subagent's loop id (FromLoopID). That id is what releases the parent's
// {wake, subagentLoopID} quiescence token on the publish path, so it MUST survive the
// drain handshake onto the folded event.
func TestSubagentResultFoldStampsTriggeredBy(t *testing.T) {
	t.Parallel()
	bt := newBlockingTool()
	ts := agenticToolSet([]tool.InvokableTool{bt}, 25, 100)
	client := &scriptedLLM{scripts: [][]content.Chunk{
		{toolUseChunk(0, "id-1", "Block", `{}`)}, // step 0: blocking tool
		{textChunk("final")},                      // step 1: text -> TurnDone
	}}
	l, sink := newFoldLoop(t, client, ts)

	ev1, _ := startTurn(t, l, context.Background(), textBlocks("turn1"))
	go func() {
		for range ev1 {
		}
	}()
	<-bt.started

	// Hand back a SubagentResult while the tool step is in flight: it queues, then
	// folds at the tool-continuation boundary.
	fromLoopID := mustID(t)
	resultID := mustID(t)
	ack := make(chan command.Disposition, 1)
	l.Commands <- command.SubagentResult{
		Header:     command.Header{ID: resultID},
		FromLoopID: fromLoopID,
		Blocks:     textBlocks("subagent says hi"),
		Ack:        ack,
	}
	if _, ok := (<-ack).(command.InputQueued); !ok {
		t.Fatal("SubagentResult not queued")
	}

	close(bt.release)

	// TurnFoldedInto must carry TriggeredByLoopID == fromLoopID and CausationID ==
	// resultID (the submit command id).
	blockUntilSink(t, sink, func(evs []event.EventEnvelope) bool {
		for _, e := range evs {
			if fi, ok := e.Event.(event.TurnFoldedInto); ok && fi.InputID == resultID {
				if fi.TriggeredByLoopID != fromLoopID {
					t.Errorf("TurnFoldedInto.TriggeredByLoopID = %v, want %v (FromLoopID)", fi.TriggeredByLoopID, fromLoopID)
				}
				if fi.CausationID != resultID {
					t.Errorf("TurnFoldedInto.CausationID = %v, want %v (submit id)", fi.CausationID, resultID)
				}
				return true
			}
		}
		return false
	})
}

// submitUserInputBlocks is submitUserInput with explicit blocks, so a fold test can
// assert the folded message's content lands in the continuation request.
func submitUserInputBlocks(t *testing.T, l *Loop, id uuid.UUID, mode command.InputMode, blocks []content.Block) command.Disposition {
	t.Helper()
	ack := make(chan command.Disposition, 1)
	l.Commands <- command.UserInput{Header: command.Header{ID: id}, Mode: mode, Blocks: blocks, Ack: ack}
	select {
	case d := <-ack:
		return d
	case <-time.After(2 * time.Second):
		t.Fatal("UserInput disposition not received")
		return nil
	}
}

// TestNoToolFinalAnswerDoesNotFold proves a no-tool final answer does NOT drain the
// inbox: input queued while a turn is running but completing with a text-only final
// answer is NOT pulled into that completed turn. Instead it starts a LATER turn.
// The queue is performed via an onStreamN hook at the START of the final (text-only)
// step, so it is guaranteed queued before the turn completes yet never at a
// tool-continuation boundary.
func TestNoToolFinalAnswerDoesNotFold(t *testing.T) {
	t.Parallel()
	queuedID := mustID(t)
	l, sink := (*Loop)(nil), (*captureSink)(nil)
	client := &scriptedLLM{
		scripts: [][]content.Chunk{
			{textChunk("only step, final answer")}, // turn 1: text-only -> TurnDone
			{textChunk("turn 2 answer")},            // turn 2 (from the queued input)
		},
	}
	ts := agenticToolSet(nil, 25, 100)
	l, sink = newFoldLoop(t, client, ts)
	// Queue the input at the START of turn 1's only (text-only) step, before it
	// completes. A no-tool step performs NO drain, so this must start a later turn.
	client.mu.Lock()
	client.onStreamN = map[int]func(){
		0: func() {
			ack := make(chan command.Disposition, 1)
			// AllowFold submit while the turn is running -> InputQueued.
			l.Commands <- command.UserInput{Header: command.Header{ID: queuedID}, Mode: command.AllowFold, Blocks: textBlocks("later"), Ack: ack}
			if _, ok := (<-ack).(command.InputQueued); !ok {
				t.Errorf("queued submit during final step not InputQueued")
			}
		},
	}
	client.mu.Unlock()

	ev1, _ := startTurn(t, l, context.Background(), textBlocks("turn1"))
	go func() {
		for range ev1 {
		}
	}()

	// The queued input must NOT fold (no TurnFoldedInto for it) and must instead
	// start a LATER turn (a TurnStarted carrying its InputID).
	blockUntilSink(t, sink, func(evs []event.EventEnvelope) bool {
		var started2 bool
		for _, e := range evs {
			if fi, ok := e.Event.(event.TurnFoldedInto); ok && fi.InputID == queuedID {
				t.Errorf("queued input folded into a no-tool final answer turn, want it to start a later turn")
			}
			if tsv, ok := e.Event.(event.TurnStarted); ok && tsv.InputID == queuedID {
				started2 = true
			}
		}
		return started2
	})
}

// TestInterruptDuringDrainFreesTurn proves an Interrupt delivered around the drain
// boundary frees the turn without wedging AND never silently strands the queued
// entry. The drain handshake selects on turnCtx.Done, so an Interrupt while runTurn is
// parked in cfg.drainPending cancels the turn and runTurn returns TurnInterrupted. The
// queued entry — whether still in the inbox (drain not yet reached) or already moved
// into the actor's draining buffer (drain replied, fold not yet committed) — must
// reach exactly one terminal outcome: TurnFoldedInto (it rode along) or InputCancelled
// (it was returned). The actor's returnQueuedInbox sweeps BOTH inbox and draining, so
// the draining-buffer entry is never lost.
func TestInterruptDuringDrainFreesTurn(t *testing.T) {
	t.Parallel()
	bt := newBlockingTool()
	ts := agenticToolSet([]tool.InvokableTool{bt}, 25, 100)
	client := &scriptedLLM{scripts: [][]content.Chunk{
		{toolUseChunk(0, "id-1", "Block", `{}`)}, // step 0: tool, then a drain happens
		{textChunk("never reached if interrupted")},
	}}
	l, sink := newFoldLoop(t, client, ts)

	ev1, _ := startTurn(t, l, context.Background(), textBlocks("turn1"))
	terminalCh := make(chan event.Event, 1)
	go func() {
		for e := range ev1 {
			if e.EndsTurn() {
				terminalCh <- e
				return
			}
		}
	}()
	<-bt.started

	// Queue an input so the drain has something to pull (so runTurn enters the
	// handshake meaningfully).
	queuedID := mustID(t)
	d := submitUserInputBlocks(t, l, queuedID, command.AllowFold, textBlocks("queued"))
	if _, ok := d.(command.InputQueued); !ok {
		t.Fatalf("queued submit disposition = %T, want InputQueued", d)
	}

	// Interrupt and release the tool concurrently: the turn ctx is cancelled around
	// the drain boundary. The drain handshake's turnCtx.Done escape must free runTurn.
	iack := make(chan bool, 1)
	l.Commands <- command.Interrupt{Ack: iack}
	<-iack
	close(bt.release)

	// The turn must terminate (interrupt frees it); the loop must not wedge.
	select {
	case term := <-terminalCh:
		if !term.EndsTurn() {
			t.Fatalf("terminal does not end the turn: %T", term)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("turn did not terminate after Interrupt during the drain boundary")
	}

	// Strand-safety: the queued entry must reach exactly ONE terminal outcome —
	// TurnFoldedInto (rode along) or InputCancelled (returned) — never neither.
	blockUntilSink(t, sink, func(evs []event.EventEnvelope) bool {
		for _, e := range evs {
			switch ev := e.Event.(type) {
			case event.TurnFoldedInto:
				if ev.InputID == queuedID {
					return true
				}
			case event.InputCancelled:
				if ev.InputID == queuedID {
					return true
				}
			}
		}
		return false
	})

	// A follow-up Shutdown completes, proving the actor is not wedged.
	ack := make(chan error, 1)
	if !sendCmd(t, l, command.Shutdown{Ack: ack}) {
		<-l.Done
		return
	}
	select {
	case <-ack:
	case <-time.After(2 * time.Second):
		t.Fatal("Shutdown did not complete; actor wedged after drain interrupt")
	}
	<-l.Done
}
