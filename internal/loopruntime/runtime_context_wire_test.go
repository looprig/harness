package loopruntime

import (
	"context"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/looprig/core/content"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/inference"
)

type fakeRuntimeContextProvider struct{ blocks []content.Block }

func (f fakeRuntimeContextProvider) Blocks(context.Context) []content.Block { return f.blocks }

// countingRuntimeProvider returns a single fresh <runtime_context> block whose
// text embeds a monotonically increasing counter, so each call yields DIFFERENT
// content. This is the cache-safety + freshness probe: the loop must append the
// LATEST block at the turn tail without ever mutating Model.System or accumulating
// stale blocks in committed history.
type countingRuntimeProvider struct {
	calls atomic.Int64
}

func (p *countingRuntimeProvider) Blocks(context.Context) []content.Block {
	n := p.calls.Add(1)
	return []content.Block{&content.TextBlock{Text: "<runtime_context>n=" + strconv.FormatInt(n, 10) + "</runtime_context>"}}
}

// newLoopWithRuntime starts a loop wired with the given RuntimeContextProvider and
// a recording client/publisher, mirroring newLoop but threading the provider into
// loop.runtimeConfig. A nil provider exercises the OFF (current-behavior) path.
func newLoopWithRuntime(t *testing.T, client inference.Client, rc RuntimeContextProvider) (*Loop, *recordingPublisher) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	sessionID := mustID(t)
	loopID := mustID(t)
	rec := &recordingPublisher{}
	l, err := newWithConfig(ctx, sessionID, loopID, Provenance{}, rec, runtimeConfig{
		Client:         client,
		Model:          testModel(),
		System:         "CACHED-PREFIX",
		DrainTimeout:   200 * time.Millisecond,
		RuntimeContext: rc,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return l, rec
}

// runtimeBlockText returns the flattened text of every block across all messages in
// req that look like a runtime_context block (text starting with the tag). It lets a
// test count how many runtime blocks rode in a single request (must be exactly one).
func runtimeBlockTexts(req inference.Request) []string {
	var out []string
	for _, m := range req.Messages {
		um, ok := m.(*content.UserMessage)
		if !ok {
			continue
		}
		for _, b := range um.Blocks {
			tb, ok := b.(*content.TextBlock)
			if !ok {
				continue
			}
			if len(tb.Text) >= len("<runtime_context>") && tb.Text[:len("<runtime_context>")] == "<runtime_context>" {
				out = append(out, tb.Text)
			}
		}
	}
	return out
}

// waitForScriptedRequests blocks until the scriptedLLM has recorded at least n
// requests, returning them (a defensive copy).
func waitForScriptedRequests(t *testing.T, client *scriptedLLM, n int) []inference.Request {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		reqs := client.requests()
		if len(reqs) >= n {
			return reqs
		}
		select {
		case <-deadline:
			t.Fatalf("recorded %d requests, want >= %d", len(reqs), n)
			return nil
		case <-time.After(2 * time.Millisecond):
		}
	}
}

// runTwoTurns drives two sequential text-only turns to completion and returns the
// per-request slice the scriptedLLM saw. Each turn is one request (no tools).
func runTwoTurns(t *testing.T, l *Loop, rec *recordingPublisher, client *scriptedLLM) []inference.Request {
	t.Helper()
	startTurn(t, l, rec, []content.Block{&content.TextBlock{Text: "turn one"}})
	if _, ok := drainToTerminal(t, rec).(event.TurnDone); !ok {
		t.Fatal("turn 1 terminal != TurnDone")
	}
	from := terminalIndex(rec, 0)
	startTurn(t, l, rec, []content.Block{&content.TextBlock{Text: "turn two"}})
	if _, ok := awaitTerminalAfter(t, rec, from).(event.TurnDone); !ok {
		t.Fatal("turn 2 terminal != TurnDone")
	}
	return waitForScriptedRequests(t, client, 2)
}

// TestRuntimeContextAppendedAtTurnTail proves the loop appends the provider's
// block(s) at the TAIL of the per-turn request (after the user message), that the
// Model.System cached prefix is byte-identical across turns even though the provider
// returns DIFFERENT blocks each turn, that each turn carries exactly ONE (fresh)
// runtime block (no unbounded accumulation), and that a nil provider reproduces
// today's behavior (no extra blocks).
func TestRuntimeContextAppendedAtTurnTail(t *testing.T) {
	t.Parallel()

	t.Run("appends fresh block at tail; system prompt stable across turns", func(t *testing.T) {
		t.Parallel()
		client := &scriptedLLM{scripts: [][]content.Chunk{{textChunk("ok")}}}
		rc := &countingRuntimeProvider{}
		l, rec := newLoopWithRuntime(t, client, rc)

		reqs := runTwoTurns(t, l, rec, client)

		// Turn 1 request: last message is the appended runtime block (the tail), AFTER
		// the user message. Exactly one runtime block in the request.
		r1 := reqs[0]
		last1, ok := r1.Messages[len(r1.Messages)-1].(*content.UserMessage)
		if !ok {
			t.Fatalf("turn 1 tail message = %T, want *UserMessage (runtime block)", r1.Messages[len(r1.Messages)-1])
		}
		if got := flattenToText(last1.Blocks); got != "<runtime_context>n=1</runtime_context>" {
			t.Errorf("turn 1 tail = %q, want the first runtime block", got)
		}
		// The user message ("turn one") must precede the runtime tail.
		if len(r1.Messages) < 2 {
			t.Fatalf("turn 1 request had %d messages, want >= 2 (user + runtime tail)", len(r1.Messages))
		}
		if got := flattenToText(r1.Messages[0].(*content.UserMessage).Blocks); got != "turn one" {
			t.Errorf("turn 1 first message = %q, want the user message %q", got, "turn one")
		}
		if texts := runtimeBlockTexts(r1); len(texts) != 1 {
			t.Errorf("turn 1 carried %d runtime blocks, want exactly 1: %v", len(texts), texts)
		}

		// Turn 2 request: a FRESH runtime block (n=2), still exactly one (turn 1's block
		// did not accumulate into committed history).
		r2 := reqs[1]
		if texts := runtimeBlockTexts(r2); len(texts) != 1 || texts[0] != "<runtime_context>n=2</runtime_context>" {
			t.Errorf("turn 2 runtime blocks = %v, want exactly [n=2]", texts)
		}

		// Cache-safety: the system prompt (cached prefix) is byte-identical turn 1 vs
		// turn 2, even though the appended runtime block changed.
		if r1.System != r2.System {
			t.Errorf("System changed across turns: turn1=%q turn2=%q", r1.System, r2.System)
		}
		if r1.System != "CACHED-PREFIX" {
			t.Errorf("System = %q, want the untouched cached prefix %q", r1.System, "CACHED-PREFIX")
		}
	})

	t.Run("nil provider: no extra blocks (current behavior)", func(t *testing.T) {
		t.Parallel()
		client := &scriptedLLM{scripts: [][]content.Chunk{{textChunk("ok")}}}
		l, rec := newLoopWithRuntime(t, client, nil)

		startTurn(t, l, rec, []content.Block{&content.TextBlock{Text: "solo"}})
		if _, ok := drainToTerminal(t, rec).(event.TurnDone); !ok {
			t.Fatal("terminal != TurnDone")
		}
		reqs := waitForScriptedRequests(t, client, 1)

		// Exactly one message: the committed user message, no appended runtime tail.
		if len(reqs[0].Messages) != 1 {
			t.Fatalf("nil-provider request had %d messages, want 1 (just the user message)", len(reqs[0].Messages))
		}
		if texts := runtimeBlockTexts(reqs[0]); len(texts) != 0 {
			t.Errorf("nil provider appended runtime blocks %v, want none", texts)
		}
	})

	t.Run("empty provider blocks: nothing appended", func(t *testing.T) {
		t.Parallel()
		client := &scriptedLLM{scripts: [][]content.Chunk{{textChunk("ok")}}}
		l, rec := newLoopWithRuntime(t, client, fakeRuntimeContextProvider{blocks: nil})

		startTurn(t, l, rec, []content.Block{&content.TextBlock{Text: "solo"}})
		if _, ok := drainToTerminal(t, rec).(event.TurnDone); !ok {
			t.Fatal("terminal != TurnDone")
		}
		reqs := waitForScriptedRequests(t, client, 1)
		if len(reqs[0].Messages) != 1 {
			t.Fatalf("empty-provider request had %d messages, want 1", len(reqs[0].Messages))
		}
	})
}

// TestRuntimeContextNotCommittedToHistory proves the appended runtime block is
// TRANSIENT: it rides the per-turn request tail but never enters committed history,
// so a snapshot of loopState.msgs after a turn carries only the user message and the
// assistant answer — never a runtime_context block.
func TestRuntimeContextNotCommittedToHistory(t *testing.T) {
	t.Parallel()
	client := &scriptedLLM{scripts: [][]content.Chunk{{textChunk("ok")}}}
	rc := &countingRuntimeProvider{}
	l, rec := newLoopWithRuntime(t, client, rc)

	startTurn(t, l, rec, []content.Block{&content.TextBlock{Text: "hello"}})
	if _, ok := drainToTerminal(t, rec).(event.TurnDone); !ok {
		t.Fatal("terminal != TurnDone")
	}

	msgs, _, err := l.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	for _, m := range msgs {
		um, ok := m.(*content.UserMessage)
		if !ok {
			continue
		}
		for _, b := range um.Blocks {
			if tb, ok := b.(*content.TextBlock); ok && len(tb.Text) >= len("<runtime_context>") && tb.Text[:len("<runtime_context>")] == "<runtime_context>" {
				t.Fatalf("committed history contains a runtime_context block %q; it must be transient", tb.Text)
			}
		}
	}
}
