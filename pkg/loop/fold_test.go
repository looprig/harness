package loop

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/looprig/harness/pkg/command"
	"github.com/looprig/core/content"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/identity"
	"github.com/looprig/harness/pkg/llm"
	"github.com/looprig/harness/pkg/tool"
	"github.com/looprig/core/uuid"
)

// newFoldLoop builds a loop with the given scripted client + blocking tool, a
// recordingPublisher to observe the full-fidelity events the production hub sees,
// and a generous DrainTimeout. It returns the loop and the recorder.
func newFoldLoop(t *testing.T, client llm.LLM, ts ToolSet) (*Loop, *recordingPublisher) {
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
	rec := &recordingPublisher{}
	l, err := New(ctx, sessionID, loopID, Provenance{}, rec,
		Config{Client: client, Model: testModel(), Tools: ts, DrainTimeout: 500 * time.Millisecond})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return l, rec
}

// newFoldLoopWithAfterDrain builds a fold loop exactly like newFoldLoop but installs
// the test-only afterDrain seam and returns the loop's root cancel. foldPending invokes
// afterDrain (in the turn goroutine) AFTER the inbox has been moved into the actor's
// draining buffer and BEFORE the first TurnFoldedInto commit. A test uses it to cancel
// the loop in the post-drain/pre-commit window deterministically. The seam is
// unexported and never set in production.
func newFoldLoopWithAfterDrain(t *testing.T, client llm.LLM, ts ToolSet, afterDrain func()) (*Loop, *recordingPublisher, context.CancelFunc) {
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
	rec := &recordingPublisher{}
	l, err := New(ctx, sessionID, loopID, Provenance{}, rec,
		Config{Client: client, Model: testModel(), Tools: ts, DrainTimeout: 500 * time.Millisecond, afterDrain: afterDrain})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return l, rec, cancel
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
// queued message (Cause.CommandID == InputID, no Cause.LoopID for a UserInput),
// the folded message appears in the next LLM request AFTER the tool result and
// BEFORE the next assistant message, and NO second TurnStarted is emitted (the
// input rode along; it did not start a new turn).
func TestFoldAtToolContinuation(t *testing.T) {
	t.Parallel()
	bt := newBlockingTool()
	ts := agenticToolSet([]tool.InvokableTool{bt}, 25, 100)
	client := &scriptedLLM{scripts: [][]content.Chunk{
		{toolUseChunk(0, "id-1", "Block", `{}`)}, // step 0: blocking tool (turn parks here)
		{textChunk("final")},                     // step 1: text -> TurnDone (after the fold)
	}}
	l, rec := newFoldLoop(t, client, ts)

	// Turn 1 starts and parks in the blocking tool.
	startTurn(t, l, rec, textBlocks("turn1"))
	<-bt.started // step 0's tool is blocked; the inbox can be filled before the drain

	// Queue an input while the tool step is in flight.
	foldedID := mustID(t)
	d := submitUserInputBlocks(t, l, rec, foldedID, textBlocks("folded text"))
	if _, ok := d.(event.InputQueued); !ok {
		t.Fatalf("queued submit outcome = %T, want event.InputQueued", d)
	}

	// Release the tool: step 0 commits, then runTurn drains the inbox and folds the
	// queued input into the mandatory continuation request.
	close(bt.release)

	// event.TurnFoldedInto must be emitted for the queued input, with Cause.CommandID ==
	// InputID and a zero Cause.LoopID (a UserInput, not a hand-back).
	blockUntilEvents(t, rec, func(evs []event.Event) bool {
		for _, e := range evs {
			if fi, ok := e.(event.TurnFoldedInto); ok {
				return fi.Cause.CommandID == foldedID &&
					fi.Cause.LoopID.IsZero() &&
					fi.Message != nil
			}
		}
		return false
	})

	// The folded input did NOT start a new turn: there is exactly ONE TurnStarted and
	// it is NOT for foldedID.
	for _, e := range rec.events() {
		if tsv, ok := e.(event.TurnStarted); ok && tsv.Cause.CommandID == foldedID {
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
// folds at a tool-continuation boundary stamps TurnFoldedInto.Cause.LoopID with
// the producing CHILD subagent's loop id (Header.Cause.LoopID) — NOT the PARENT
// delivery target carried by the embedded Coordinates. That CHILD id is what releases
// the parent's {wake, childLoopID} quiescence token on the publish path, so it MUST
// survive the drain handshake onto the folded event.
func TestSubagentResultFoldStampsTriggeredBy(t *testing.T) {
	t.Parallel()
	bt := newBlockingTool()
	ts := agenticToolSet([]tool.InvokableTool{bt}, 25, 100)
	client := &scriptedLLM{scripts: [][]content.Chunk{
		{toolUseChunk(0, "id-1", "Block", `{}`)}, // step 0: blocking tool
		{textChunk("final")},                     // step 1: text -> TurnDone
	}}
	l, rec := newFoldLoop(t, client, ts)

	startTurn(t, l, rec, textBlocks("turn1"))
	<-bt.started

	// Hand back a SubagentResult while the tool step is in flight: it queues, then
	// folds at the tool-continuation boundary.
	childLoopID := mustID(t)
	resultID := mustID(t)
	l.Commands <- command.SubagentResult{
		Coordinates: identity.Coordinates{LoopID: mustID(t)}, // PARENT delivery target
		Header:      command.Header{CommandID: resultID, Cause: identity.Cause{Coordinates: identity.Coordinates{LoopID: childLoopID}}},
		Blocks:      textBlocks("subagent says hi"),
	}
	if _, ok := awaitReply(t, rec, resultID).(event.InputQueued); !ok {
		t.Fatal("SubagentResult not queued")
	}

	close(bt.release)

	// TurnFoldedInto must carry Cause.LoopID == fromLoopID and Cause.CommandID ==
	// resultID (the submit command id).
	blockUntilEvents(t, rec, func(evs []event.Event) bool {
		for _, e := range evs {
			if fi, ok := e.(event.TurnFoldedInto); ok && fi.Cause.CommandID == resultID {
				if fi.Cause.LoopID != childLoopID {
					t.Errorf("TurnFoldedInto.Cause.LoopID = %v, want %v (the CHILD)", fi.Cause.LoopID, childLoopID)
				}
				if fi.Cause.CommandID != resultID {
					t.Errorf("TurnFoldedInto.Cause.CommandID = %v, want %v (submit id)", fi.Cause.CommandID, resultID)
				}
				return true
			}
		}
		return false
	})
}

// submitUserInputBlocks is submitUserInput with explicit blocks, so a fold test can
// assert the folded message's content lands in the continuation request. It observes
// the published outcome event (InputQueued/TurnStarted/TurnRejected) via the recorder.
func submitUserInputBlocks(t *testing.T, l *Loop, rec *recordingPublisher, id uuid.UUID, blocks []content.Block) event.Event {
	t.Helper()
	l.Commands <- command.UserInput{Header: command.Header{CommandID: id}, Blocks: blocks}
	return awaitReply(t, rec, id)
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
	l, rec := (*Loop)(nil), (*recordingPublisher)(nil)
	client := &scriptedLLM{
		scripts: [][]content.Chunk{
			{textChunk("only step, final answer")}, // turn 1: text-only -> TurnDone
			{textChunk("turn 2 answer")},           // turn 2 (from the queued input)
		},
	}
	ts := agenticToolSet(nil, 25, 100)
	l, rec = newFoldLoop(t, client, ts)
	// Queue the input at the START of turn 1's only (text-only) step, before it
	// completes. A no-tool step performs NO drain, so this must start a later turn.
	client.mu.Lock()
	client.onStreamN = map[int]func(){
		0: func() {
			// Submit while the turn is running -> InputQueued (observed on the recorder
			// fan-in).
			l.Commands <- command.UserInput{Header: command.Header{CommandID: queuedID}, Blocks: textBlocks("later")}
			if _, ok := awaitReply(t, rec, queuedID).(event.InputQueued); !ok {
				t.Errorf("queued submit during final step not InputQueued")
			}
		},
	}
	client.mu.Unlock()

	startTurn(t, l, rec, textBlocks("turn1"))

	// The queued input must NOT fold (no TurnFoldedInto for it) and must instead
	// start a LATER turn (a TurnStarted carrying its InputID).
	blockUntilEvents(t, rec, func(evs []event.Event) bool {
		var started2 bool
		for _, e := range evs {
			if fi, ok := e.(event.TurnFoldedInto); ok && fi.Cause.CommandID == queuedID {
				t.Errorf("queued input folded into a no-tool final answer turn, want it to start a later turn")
			}
			if tsv, ok := e.(event.TurnStarted); ok && tsv.Cause.CommandID == queuedID {
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
	l, rec := newFoldLoop(t, client, ts)

	startTurn(t, l, rec, textBlocks("turn1"))
	<-bt.started

	// Queue an input so the drain has something to pull (so runTurn enters the
	// handshake meaningfully).
	queuedID := mustID(t)
	d := submitUserInputBlocks(t, l, rec, queuedID, textBlocks("queued"))
	if _, ok := d.(event.InputQueued); !ok {
		t.Fatalf("queued submit outcome = %T, want event.InputQueued", d)
	}

	// Interrupt and release the tool concurrently: the turn ctx is cancelled around
	// the drain boundary. The drain handshake's turnCtx.Done escape must free runTurn.
	iack := make(chan bool, 1)
	l.Commands <- command.Interrupt{Ack: iack}
	<-iack
	close(bt.release)

	// The turn must terminate (interrupt frees it); the loop must not wedge. The
	// terminal is observed on the recorder fan-in.
	if term := drainToTerminal(t, rec); !term.EndsTurn() {
		t.Fatalf("terminal does not end the turn: %T", term)
	}

	// Strand-safety: the queued entry must reach exactly ONE terminal outcome —
	// TurnFoldedInto (rode along) or InputCancelled (returned) — never neither.
	blockUntilEvents(t, rec, func(evs []event.Event) bool {
		for _, e := range evs {
			switch ev := e.(type) {
			case event.TurnFoldedInto:
				if ev.Cause.CommandID == queuedID {
					return true
				}
			case event.InputCancelled:
				if ev.Cause.CommandID == queuedID {
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

// TestDrainingSweepReturnsEntryOnInterrupt is the DETERMINISTIC complement to
// TestInterruptDuringDrainFreesTurn. It pins the draining-buffer abnormal-return
// sweep in returnQueuedInbox (loop.go) — the path that returns an entry the actor
// already moved from inbox into draining (for a fold) but whose TurnFoldedInto never
// committed because the turn ended abnormally first.
//
// Determinism comes from the afterDrain seam combined with a ROOT-ctx cancel.
// foldPending invokes afterDrain AFTER drainPending has moved the queued entry into the
// actor's draining buffer and BEFORE the first TurnFoldedInto commit. The hook cancels
// the loop's root ctx (which cancels the turn ctx, a child) and returns. Two strands
// then resolve without a race:
//   - The actor, parked in its main select, sees ctx.Done() ready (commits is NOT ready
//     yet — the turn goroutine has not sent), so it deterministically takes the
//     hard-kill arm and LEAVES the select; it stops reading the commits channel.
//   - The turn goroutine returns from the hook and calls cfg.commit for the fold. Since
//     the actor no longer reads commits, the unbuffered `commits <-` send blocks and the
//     commit's select deterministically takes <-cctx.Done() (turn ctx cancelled). So the
//     fold NEVER commits: foldPending returns an error and runTurn returns
//     TurnInterrupted.
//
// Both happen-before edges hold because cancel() and the commits-send run sequentially
// in the turn goroutine (cancel first), and the actor cannot observe a commits-send that
// has not happened yet. The entry is therefore in draining (not inbox, not folded) when
// the hard-kill arm calls returnQueuedInbox, so ONLY the draining sweep can return it.
//
// Using a real Interrupt COMMAND here would NOT be deterministic: after the actor acks
// the Interrupt it loops back to its select still reading commits, so the fold's
// `commits <-` send and the commit's <-cctx.Done() are BOTH ready and Go picks at random
// — the entry would fold ~half the time. The root-ctx cancel avoids that because the
// actor LEAVES the select. (TestInterruptDuringDrainFreesTurn exercises that race
// non-deterministically; this test is the deterministic complement.)
//
// Why onStreamN can't express this: onStreamN fires at the START of a Stream() call —
// i.e. at the start of the NEXT step, which only runs AFTER the folds already committed.
// By then the entry has left draining via its TurnFoldedInto, so the draining sweep is
// never the path under test.
//
// Asserts: exactly ONE event.InputCancelled{InputID==queued, Reason==CancelTurnInterrupted}
// for the drained entry, NO TurnFoldedInto for it, and the turn ends TurnInterrupted.
// The single InputCancelled IS the draining sweep: the entry was moved out of inbox by
// drainInbox, so no other path can return it — exactly-once proves draining is empty
// afterward (a stranded entry would yield zero returns; a double-return would yield two).
// Removing the `for _, qi := range state.draining` loop from returnQueuedInbox makes this
// FAIL (the entry is stranded: no TurnFoldedInto, no InputCancelled), proving it is
// load-bearing.
func TestDrainingSweepReturnsEntryOnInterrupt(t *testing.T) {
	t.Parallel()
	bt := newBlockingTool()
	ts := agenticToolSet([]tool.InvokableTool{bt}, 25, 100)
	client := &scriptedLLM{scripts: [][]content.Chunk{
		{toolUseChunk(0, "id-1", "Block", `{}`)}, // step 0: tool, then the drain happens
		{textChunk("never reached: interrupted in the fold window")},
	}}

	// The afterDrain seam cancels the loop in the post-drain/pre-commit window. It must
	// run EXACTLY once (one drain boundary with a non-empty batch).
	var rootCancel context.CancelFunc
	cancelled := make(chan struct{})
	var afterDrainOnce sync.Once
	afterDrain := func() {
		afterDrainOnce.Do(func() {
			rootCancel()
			close(cancelled)
		})
	}
	l, rec, rc := newFoldLoopWithAfterDrain(t, client, ts, afterDrain)
	rootCancel = rc

	startTurn(t, l, rec, textBlocks("turn1"))
	// The terminal is asserted via the RECORDER below: every loop event (including the
	// terminal on the hard-kill path) reaches consumers through the session fan-in.
	<-bt.started // step 0's tool is blocked; queue an input before the drain runs

	// Queue exactly one input so the drain moves exactly one entry into draining.
	queuedID := mustID(t)
	d := submitUserInputBlocks(t, l, rec, queuedID, textBlocks("queued"))
	if _, ok := d.(event.InputQueued); !ok {
		t.Fatalf("queued submit outcome = %T, want event.InputQueued", d)
	}

	// Release the tool: step 0 commits, runTurn drains (moving the entry into draining),
	// then foldPending invokes afterDrain, which cancels the loop before the fold
	// commits. The cancelled commit makes runTurn return TurnInterrupted.
	close(bt.release)
	<-cancelled // the cancel has landed in the post-drain/pre-commit window

	// The turn must terminate as TurnInterrupted (the cancelled fold commit), observed via
	// the recorder (always published, even on the hard-kill abandoned-delivery path).
	blockUntilEvents(t, rec, func(evs []event.Event) bool {
		for _, e := range evs {
			if _, ok := e.(event.TurnInterrupted); ok {
				return true
			}
		}
		return false
	})
	for _, e := range rec.events() {
		switch e.(type) {
		case event.TurnDone, event.TurnFailed:
			t.Fatalf("turn terminal = %T, want event.TurnInterrupted", e)
		}
	}

	// The entry must come back via EXACTLY ONE InputCancelled{CancelTurnInterrupted}
	// from the draining sweep, and NO TurnFoldedInto must be emitted for it.
	blockUntilEvents(t, rec, func(evs []event.Event) bool {
		for _, e := range evs {
			if ic, ok := e.(event.InputCancelled); ok && ic.Cause.CommandID == queuedID {
				return ic.Reason == event.CancelTurnInterrupted && ic.Message != nil
			}
		}
		return false
	})
	var cancels, folds int
	for _, e := range rec.events() {
		switch ev := e.(type) {
		case event.InputCancelled:
			if ev.Cause.CommandID == queuedID {
				cancels++
			}
		case event.TurnFoldedInto:
			if ev.Cause.CommandID == queuedID {
				folds++
			}
		}
	}
	if cancels != 1 {
		t.Fatalf("InputCancelled for the drained entry = %d, want exactly 1 (the draining sweep)", cancels)
	}
	if folds != 0 {
		t.Fatalf("TurnFoldedInto for the drained entry = %d, want 0 (it was interrupted before the fold committed)", folds)
	}

	// The root-ctx cancel drives the actor through its hard-kill arm to exit; Loop.Done
	// closing proves the actor returned cleanly (draining swept, nothing left pinned).
	select {
	case <-l.Done:
	case <-time.After(2 * time.Second):
		t.Fatal("Loop.Done did not close after the draining-sweep cancel; actor wedged")
	}
}
