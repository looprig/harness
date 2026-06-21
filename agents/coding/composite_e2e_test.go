package coding

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/inventivepotter/urvi/internal/agent/loop/event"
	"github.com/inventivepotter/urvi/internal/agent/loop/identity"
	"github.com/inventivepotter/urvi/internal/content"
	"github.com/inventivepotter/urvi/internal/llm"
	"github.com/inventivepotter/urvi/internal/uuid"
)

// composite_e2e_test.go is the cross-cutting acceptance slice (T18 Part A): a REAL
// parent turn whose LLM emits a Subagent tool-call drives the REAL Subagent tool →
// codingSpawner.Spawn → session.RunSubagent → a sub-loop, and the sub-loop's final
// text returns as the tool result so the parent turn can continue. It exercises the
// whole production wiring (newWithClient + Submit + Subscribe), proving the Subagent
// tool reaches AutoApprove under the live PermissionChecker (no gate stalls the call)
// and that a sub-loop is a fresh, machine-driven loop under the session.
//
// THE DETERMINISM CHALLENGE. The parent and every sub-loop share ONE llm.LLM, so a
// by-call-index script is nondeterministic under the concurrent parent/sub Stream
// calls. The routingLLM below routes by the LATEST input message instead — pure
// content addressing, independent of call order — so the script is deterministic no
// matter how the parent's continuation interleaves with a sub-loop's turn:
//
//   - last message is a UserMessage whose text contains a delegate marker → return a
//     Subagent tool-use carrying the sub-task as {"message": ...} (the parent, or a
//     sub-loop asked to recurse);
//   - last message is a UserMessage whose text contains a final marker → return a
//     plain final assistant message (the sub-loop / grandchild's answer);
//   - last message is a ToolResultMessage → the spawner's tool result came back; return
//     a plain final assistant message that QUOTES the tool result (the parent's, or a
//     sub-loop's, continuation), proving the subagent text reached the caller.
//
// Each branch's reply is a complete one-shot stream (chunks then io.EOF), so a single
// routingLLM instance safely serves the parent AND every sub-loop concurrently.

const (
	// delegateMarker, when present in a user message, makes routingLLM emit a Subagent
	// tool-use. The parent prompt and any non-leaf recurse sub-task carry it.
	delegateMarker = "DELEGATE"
	// finalMarker, when present in a user message, makes routingLLM emit a plain final
	// answer instead of delegating — the leaf of the spawn tree.
	finalMarker = "FINAL"
)

// routingLLM is a content-addressed fake llm.LLM: it inspects req.Messages' LAST
// message and returns a deterministic reply for it, so one instance can serve the
// shared parent + sub-loop traffic without a call-order race. answer is the plain
// final text a leaf turn emits; nextTask derives, from the incoming delegate message,
// the {"message"} a delegating turn hands to its subagent — so recursion depth is
// encoded in the message CONTENT, never in a shared mutable field (which could not
// distinguish the parent's delegate from a sub-loop's delegate). Both fields are set
// once at construction and only read thereafter, so concurrent Stream calls from the
// parent and every sub-loop need no synchronization.
type routingLLM struct {
	answer   string                       // plain final text for a FINAL user message
	nextTask func(incoming string) string // the sub-task a DELEGATE message hands down
}

func (r *routingLLM) Invoke(ctx context.Context, req llm.Request) (*llm.Response, error) {
	return nil, &routingNotUsedError{}
}

// routingNotUsedError is the typed error Invoke returns; the loop only ever Streams,
// so a non-nil Invoke would be a wiring bug, surfaced with a concrete type.
type routingNotUsedError struct{}

func (*routingNotUsedError) Error() string { return "routingLLM.Invoke not used" }

func (r *routingLLM) Stream(ctx context.Context, req llm.Request) (*llm.StreamReader[content.Chunk], error) {
	chunks := r.route(req)
	i := 0
	next := func() (content.Chunk, error) {
		if i < len(chunks) {
			c := chunks[i]
			i++
			return c, nil
		}
		return nil, io.EOF
	}
	return llm.NewStreamReader(next, nil), nil
}

// route picks the reply for req by classifying its LAST message. It is the whole
// determinism mechanism: the decision depends only on message CONTENT, never on how
// many times Stream has been called.
func (r *routingLLM) route(req llm.Request) []content.Chunk {
	last := lastMessage(req.Messages)
	switch m := last.(type) {
	case *content.ToolResultMessage:
		// The Subagent tool's result came back; continue the turn with a plain final
		// answer that QUOTES the result so the test can prove the subagent text reached
		// the caller.
		return []content.Chunk{textChunk("parent saw: " + toolResultText(m))}
	case *content.UserMessage:
		text := messageText(m.Blocks)
		if strings.Contains(text, finalMarker) {
			return []content.Chunk{textChunk(r.answer)}
		}
		// A delegate user message → emit a Subagent tool-use whose sub-task is derived
		// from THIS message (so each level can hand down a different next task; recursion
		// depth lives in the content, not in shared state).
		return []content.Chunk{subagentToolUseChunk(r.nextTask(text))}
	default:
		// No recognizable trailing message: fail closed with a plain (non-delegating)
		// reply so the turn terminates rather than spinning.
		return []content.Chunk{textChunk(r.answer)}
	}
}

// subagentToolUseChunk builds a single ToolUseChunk that names the Subagent tool and
// carries {"message": task} as its args, so the folded ToolUseBlock.Input is the exact
// JSON the Subagent tool decodes.
func subagentToolUseChunk(task string) content.Chunk {
	args, err := json.Marshal(subagentArgsForTest{Message: task})
	if err != nil {
		// task is a test constant; a marshal failure is a test bug, not a runtime path.
		panic("composite_e2e_test: marshal subagent args: " + err.Error())
	}
	return &content.ToolUseChunk{Index: 0, ID: "tu-subagent", Name: "Subagent", InputJSON: string(args)}
}

// subagentArgsForTest mirrors the Subagent tool's {message string} arg contract; it is
// a local typed encode so the test never hand-builds JSON strings.
type subagentArgsForTest struct {
	Message string `json:"message"`
}

// lastMessage returns the final element of a conversation thread, or nil if empty.
func lastMessage(msgs content.AgenticMessages) content.Conversation {
	if len(msgs) == 0 {
		return nil
	}
	return msgs[len(msgs)-1]
}

// messageText flattens the TextBlocks of a block slice (ignoring non-text blocks).
func messageText(blocks []content.Block) string {
	var b strings.Builder
	for _, blk := range blocks {
		if tb, ok := blk.(*content.TextBlock); ok {
			b.WriteString(tb.Text)
		}
	}
	return b.String()
}

// toolResultText flattens a ToolResultMessage's nested text content. The loop wraps a
// tool result string as a ToolResultMessage whose Blocks are TextBlocks, so this reads
// the subagent's final text back out.
func toolResultText(m *content.ToolResultMessage) string {
	return messageText(m.Blocks)
}

// TestCompositeSubagentE2E drives the full parent→Subagent→sub-loop→back composition
// through the production wiring. The parent turn (a real Submit) emits a Subagent
// tool-call; the live PermissionChecker AutoApproves it (Subagent is in the manifest's
// HardApprove list), so the real Subagent tool runs, Spawn drives a fresh sub-loop to a
// plain final answer, and that answer returns as the tool result so the parent
// continues to a TurnDone that QUOTES the subagent's answer.
//
// Assertions (all off a whole-session {Enduring:{All}} observer attached before
// Submit, since the hub has no replay):
//   - a LoopStarted for a fresh, NON-primary loop (the sub-loop);
//   - that sub-loop's TurnStarted is machine-driven (Cause.Agency == AgencyMachine) —
//     the sub-loop submit was our code's, never a human's;
//   - the PARENT's terminal TurnDone text incorporates the subagent's answer — proving
//     the sub-loop's final text round-tripped through the tool result into the parent.
func TestCompositeSubagentE2E(t *testing.T) {
	t.Parallel()

	const subagentAnswer = "SUBAGENT_ANSWER_42"
	// One level: the parent delegates a FINAL sub-task, so the single sub-loop is the
	// leaf and emits the plain answer.
	client := &routingLLM{
		answer:   subagentAnswer,
		nextTask: func(string) string { return "go do the work " + finalMarker },
	}

	c, err := newWithClient(context.Background(), client, testSpec())
	if err != nil {
		t.Fatalf("newWithClient: %v", err)
	}
	t.Cleanup(func() { _ = c.Close(context.Background()) })

	// Whole-session observer BEFORE Submit so the sub-loop's opening LoopStarted +
	// TurnStarted (no hub replay) cannot be missed. Enduring:{All} carries StepDone,
	// terminals (TurnDone), TurnStarted and LoopStarted from every loop.
	obs, err := c.Subscribe(event.EventFilter{Enduring: event.LoopScope{All: true}})
	if err != nil {
		t.Fatalf("Subscribe(observer): %v", err)
	}
	t.Cleanup(func() { _ = obs.Close() })

	primary := c.PrimaryLoopID()

	// Parent prompt carries the delegate marker → the parent turn emits a Subagent
	// tool-use.
	if _, err := c.Submit(context.Background(), textBlocks("parent please "+delegateMarker)); err != nil {
		t.Fatalf("Submit: %v", err)
	}

	// The sub-loop announces itself (a fresh, non-primary LoopStarted) and runs a
	// machine-driven turn.
	subLoopID, ok := waitLoopStartedNonPrimary(t, obs, primary)
	if !ok {
		t.Fatal("never observed a LoopStarted for a fresh (non-primary) sub-loop")
	}
	ts, ok := waitTurnStartedOnLoop(t, obs, subLoopID)
	if !ok {
		t.Fatalf("never observed a TurnStarted attributed to sub-loop %v", subLoopID)
	}
	if ts.Cause.Agency != identity.AgencyMachine {
		t.Errorf("sub-loop TurnStarted Cause.Agency = %v, want AgencyMachine", ts.Cause.Agency)
	}

	// The PARENT's terminal TurnDone must incorporate the subagent's answer — proving
	// the sub-loop's final text round-tripped through the tool result into the parent's
	// continuation.
	done, ok := waitTurnDoneOnLoop(t, obs, primary)
	if !ok {
		t.Fatal("never observed the parent's terminal TurnDone")
	}
	got := aiMessageText(done.Message)
	if !strings.Contains(got, subagentAnswer) {
		t.Errorf("parent TurnDone text = %q, want it to incorporate subagent answer %q", got, subagentAnswer)
	}

	// The AutoApprove proof is implicit but real: had the Subagent tool not been
	// AutoApproved by the live PermissionChecker, the parent's tool batch would have
	// stalled on a permission gate and no sub-loop LoopStarted would ever appear — the
	// assertions above would all time out. Reaching here is the no-gate-stall proof.
}

// TestRecursiveSubagentSpawnE2E proves recursion through the LIVE Spawn path (not the
// inductive buildToolSet proxy): a sub-loop's OWN Subagent tool spawns a grandchild,
// and the grandchild runs under a FRESH PermissionChecker obtained from the real
// per-call buildToolSet inside codingSpawner.Spawn. The depth cap is intentionally
// removed (design §8), so this is expected to work to arbitrary depth; we assert two
// levels.
//
// Routing: the parent and the sub-loop both receive a DELEGATE user message, so both
// emit a Subagent tool-use; the grandchild receives a FINAL user message and emits the
// plain answer. The content-addressed routing means both delegating turns get the same
// (tool-use) reply with no call-order dependence.
//
// Assertions (off a whole-session {Enduring:{All}} observer): at least TWO distinct
// non-primary loops are started (the sub-loop and its grandchild), the grandchild's
// turn is machine-driven, and the parent's terminal TurnDone incorporates the
// grandchild's answer (which propagates parent←sub←grandchild through two tool results).
func TestRecursiveSubagentSpawnE2E(t *testing.T) {
	t.Parallel()

	const grandchildAnswer = "GRANDCHILD_ANSWER_7"
	const recurseMarker = "recurse"
	// nextTask is depth-aware via message CONTENT (the only state that distinguishes the
	// parent's delegate from the sub-loop's): the parent's prompt has no recurse marker,
	// so it hands the sub-loop a DELEGATE task that DOES carry it; the sub-loop, seeing
	// that recurse marker, hands the grandchild a FINAL task — making the grandchild the
	// leaf. Two delegations, one final, regardless of interleaving.
	client := &routingLLM{
		answer: grandchildAnswer,
		nextTask: func(incoming string) string {
			if strings.Contains(incoming, recurseMarker) {
				return recurseMarker + " grandchild leaf " + finalMarker
			}
			return recurseMarker + " sub-loop delegates again " + delegateMarker
		},
	}

	c, err := newWithClient(context.Background(), client, testSpec())
	if err != nil {
		t.Fatalf("newWithClient: %v", err)
	}
	t.Cleanup(func() { _ = c.Close(context.Background()) })

	obs, err := c.Subscribe(event.EventFilter{Enduring: event.LoopScope{All: true}})
	if err != nil {
		t.Fatalf("Subscribe(observer): %v", err)
	}
	t.Cleanup(func() { _ = obs.Close() })

	primary := c.PrimaryLoopID()

	if _, err := c.Submit(context.Background(), textBlocks("parent please "+delegateMarker)); err != nil {
		t.Fatalf("Submit: %v", err)
	}

	// Two distinct non-primary loops must start: the sub-loop and its grandchild.
	subLoops, lastTS := waitNNonPrimaryLoops(t, obs, primary, 2)
	if len(subLoops) < 2 {
		t.Fatalf("observed %d non-primary loops, want >= 2 (sub-loop + grandchild)", len(subLoops))
	}
	if lastTS.Cause.Agency != identity.AgencyMachine {
		t.Errorf("deepest sub-loop TurnStarted Cause.Agency = %v, want AgencyMachine", lastTS.Cause.Agency)
	}

	// The parent's terminal TurnDone must incorporate the grandchild's answer, which
	// propagated parent ← sub ← grandchild through two tool-result round-trips.
	done, ok := waitTurnDoneOnLoop(t, obs, primary)
	if !ok {
		t.Fatal("never observed the parent's terminal TurnDone")
	}
	got := aiMessageText(done.Message)
	if !strings.Contains(got, grandchildAnswer) {
		t.Errorf("parent TurnDone text = %q, want it to incorporate grandchild answer %q", got, grandchildAnswer)
	}
}

// textBlocks wraps s as a single TextBlock user message body.
func textBlocks(s string) []content.Block {
	return []content.Block{&content.TextBlock{Text: s}}
}

// waitTurnDoneOnLoop reads the observer until a TurnDone whose Coordinates.LoopID
// equals loopID arrives, returning it so the caller can inspect its terminal Message.
func waitTurnDoneOnLoop(t *testing.T, sub event.Subscription, loopID uuid.UUID) (event.TurnDone, bool) {
	t.Helper()
	deadline := time.After(3 * time.Second)
	for {
		select {
		case ev, ok := <-sub.Events():
			if !ok {
				return event.TurnDone{}, false
			}
			if td, ok := ev.(event.TurnDone); ok && td.Coordinates.LoopID == loopID {
				return td, true
			}
		case <-deadline:
			return event.TurnDone{}, false
		}
	}
}

// waitNNonPrimaryLoops reads the observer until n distinct non-primary LoopStarted ids
// have been seen, returning the set and the TurnStarted of the LAST sub-loop observed
// (so the caller can assert machine agency on the deepest turn). It correlates a
// TurnStarted to a known sub-loop as those arrive.
func waitNNonPrimaryLoops(t *testing.T, sub event.Subscription, primary uuid.UUID, n int) (map[uuid.UUID]struct{}, event.TurnStarted) {
	t.Helper()
	loops := make(map[uuid.UUID]struct{})
	var lastTS event.TurnStarted
	deadline := time.After(5 * time.Second)
	for len(loops) < n {
		select {
		case ev, ok := <-sub.Events():
			if !ok {
				return loops, lastTS
			}
			switch e := ev.(type) {
			case event.LoopStarted:
				if e.Coordinates.LoopID != primary {
					loops[e.Coordinates.LoopID] = struct{}{}
				}
			case event.TurnStarted:
				if e.Coordinates.LoopID != primary {
					lastTS = e
				}
			}
		case <-deadline:
			return loops, lastTS
		}
	}
	return loops, lastTS
}
