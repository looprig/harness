package loop

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"sync"
	"testing"

	"github.com/inventivepotter/urvi/internal/agent/loop/event"
	"github.com/inventivepotter/urvi/internal/content"
	"github.com/inventivepotter/urvi/internal/llm"
	"github.com/inventivepotter/urvi/internal/tool"
	"github.com/inventivepotter/urvi/internal/uuid"
)

func drainEmit(events *[]event.Event) func(event.Event) {
	return func(ev event.Event) { *events = append(*events, ev) }
}

// ---------------------------------------------------------------------------
// Multi-stream fake LLM: returns a DIFFERENT scripted stream per Stream() call
// (one per agentic iteration), records every request it received, and can be told
// to cancel ctx between/within iterations.
// ---------------------------------------------------------------------------

// scriptedLLM streams scripts[i] on its i-th Stream() call. If more Stream()
// calls arrive than there are scripts, the last script is repeated (so an
// "always calls tools" model is a single tool-call script repeated forever).
// onStreamN, if set for an index, runs at the START of that Stream() call (used
// to cancel ctx mid-loop). It records every request for toolDefs assertions.
type scriptedLLM struct {
	mu        sync.Mutex
	scripts   [][]content.Chunk
	reqs      []llm.Request
	calls     int
	onStreamN map[int]func()
}

func (s *scriptedLLM) Invoke(ctx context.Context, req llm.Request) (*llm.Response, error) {
	return nil, errors.New("scriptedLLM.Invoke not used")
}

func (s *scriptedLLM) Stream(ctx context.Context, req llm.Request) (*llm.StreamReader[content.Chunk], error) {
	s.mu.Lock()
	n := s.calls
	s.calls++
	s.reqs = append(s.reqs, req)
	hook := s.onStreamN[n]
	var script []content.Chunk
	if n < len(s.scripts) {
		script = s.scripts[n]
	} else if len(s.scripts) > 0 {
		script = s.scripts[len(s.scripts)-1]
	}
	s.mu.Unlock()

	if hook != nil {
		hook()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	i := 0
	next := func() (content.Chunk, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if i < len(script) {
			c := script[i]
			i++
			return c, nil
		}
		return nil, io.EOF
	}
	return llm.NewStreamReader(next, nil), nil
}

func (s *scriptedLLM) requests() []llm.Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]llm.Request, len(s.reqs))
	copy(out, s.reqs)
	return out
}

// toolUseChunk builds a single-fragment tool-call delta.
func toolUseChunk(index int, id, name, inputJSON string) content.Chunk {
	return &content.ToolUseChunk{Index: index, ID: id, Name: name, InputJSON: inputJSON}
}

// echoTool is a registered fake tool for the agentic-loop tests: it echoes a
// fixed output and records how many times it ran. It implements only the base
// interface (autoApproveGate handles permission).
type echoTool struct {
	name   string
	output string
	mu     sync.Mutex
	runs   int
}

func (e *echoTool) Info(ctx context.Context) (*tool.ToolInfo, error) {
	return &tool.ToolInfo{Name: e.name, Desc: "echoes", Schema: json.RawMessage(`{"type":"object"}`)}, nil
}

func (e *echoTool) InvokableRun(ctx context.Context, argsJSON string) (*tool.ToolResult, error) {
	e.mu.Lock()
	e.runs++
	e.mu.Unlock()
	return tool.TextResult(e.output), nil
}

func (e *echoTool) runCount() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.runs
}

// countToolUseInHistory counts tool_use blocks (in AIMessages) and ToolResultMessages.
// A well-formed turn has equal counts; rollback to base leaves both at zero.
func countToolUseInHistory(msgs content.AgenticMessages) (toolUse, toolMsg int) {
	for _, m := range msgs {
		switch v := m.(type) {
		case *content.AIMessage:
			for _, b := range v.Blocks {
				if _, ok := b.(*content.ToolUseBlock); ok {
					toolUse++
				}
			}
		case *content.ToolResultMessage:
			toolMsg++
		}
	}
	return
}

func agenticToolSet(reg []tool.InvokableTool, maxIters, maxCalls int) ToolSet {
	return resolveToolSetCaps(ToolSet{
		Permission:           autoApproveGate{},
		Registry:             reg,
		MaxToolIterations:    maxIters,
		MaxToolCallsPerTurn:  maxCalls,
		MaxParallelToolCalls: 4,
	})
}

// agenticCfg builds a turn Config wired like loop.New does: idGen defaulted to
// the real uuid.New (RunBatch mints a CallID per call via this seam).
func agenticCfg(ts ToolSet) Config {
	return Config{Model: llm.ModelSpec{Model: "m"}, Tools: ts, idGen: uuid.New}
}

// noGateReg returns a gateReg channel that is never read (no EffectAsk in
// auto-approve scenarios).
func noGateReg() chan gateRegistration { return make(chan gateRegistration) }

func TestRunTurnAgentic(t *testing.T) {
	t.Parallel()
	input := []content.Block{&content.TextBlock{Text: "hi"}}

	t.Run("one tool round-trip then text-only completes with TurnDone", func(t *testing.T) {
		t.Parallel()
		echo := &echoTool{name: "Echo", output: "tool ran"}
		cfg := agenticCfg(agenticToolSet([]tool.InvokableTool{echo}, 25, 100))
		client := &scriptedLLM{scripts: [][]content.Chunk{
			{toolUseChunk(0, "id-1", "Echo", `{"x":1}`)}, // iter1: one tool call
			{textChunk("all done")},                      // iter2: text-only → TurnDone
		}}
		var emitted []event.Event
		msgs, terminal := runTurn(context.Background(), input, 1, nil, cfg, client, noGateReg(), drainEmit(&emitted))

		done, ok := terminal.(event.TurnDone)
		if !ok {
			t.Fatalf("terminal = %T, want TurnDone", terminal)
		}
		if echo.runCount() != 1 {
			t.Errorf("echo ran %d times, want 1", echo.runCount())
		}
		// history: user, assistant(tool_use), tool message, assistant(text)
		if len(msgs) != 4 {
			t.Fatalf("history len = %d, want 4 (user, assistant tool_use, tool, assistant text)", len(msgs))
		}
		if _, ok := msgs[0].(*content.UserMessage); !ok {
			t.Errorf("msgs[0] = %T, want *UserMessage", msgs[0])
		}
		ai1, ok := msgs[1].(*content.AIMessage)
		if !ok {
			t.Fatalf("msgs[1] = %T, want *AIMessage", msgs[1])
		}
		if _, ok := ai1.Blocks[len(ai1.Blocks)-1].(*content.ToolUseBlock); !ok {
			t.Errorf("msgs[1] last block = %T, want *ToolUseBlock", ai1.Blocks[len(ai1.Blocks)-1])
		}
		tm, ok := msgs[2].(*content.ToolResultMessage)
		if !ok {
			t.Fatalf("msgs[2] = %T, want *ToolResultMessage", msgs[2])
		}
		if tm.ToolUseID != "id-1" {
			t.Errorf("tool message ToolUseID = %q, want %q", tm.ToolUseID, "id-1")
		}
		if got := flattenToText(tm.Blocks); got != "tool ran" {
			t.Errorf("tool message text = %q, want %q", got, "tool ran")
		}
		if _, ok := msgs[3].(*content.AIMessage); !ok {
			t.Errorf("msgs[3] = %T, want *AIMessage", msgs[3])
		}
		if done.Message != msgs[3] {
			t.Errorf("TurnDone.Message must be the final assistant message")
		}
		tu, tmCount := countToolUseInHistory(msgs)
		if tu != tmCount {
			t.Errorf("unpaired tool_use: %d tool_use vs %d tool messages", tu, tmCount)
		}
	})

	t.Run("text-only on the LIMIT iteration completes with TurnDone (cap does not fire)", func(t *testing.T) {
		t.Parallel()
		echo := &echoTool{name: "Echo", output: "ran"}
		// maxIters=1: iter1 streams a tool call (iters becomes 1, == cap, OK),
		// iter2 streams text-only → TurnDone (the cap is only checked when ANOTHER
		// tool batch is requested, which never happens here).
		cfg := agenticCfg(agenticToolSet([]tool.InvokableTool{echo}, 1, 100))
		client := &scriptedLLM{scripts: [][]content.Chunk{
			{toolUseChunk(0, "id-1", "Echo", `{}`)},
			{textChunk("final answer")},
		}}
		var emitted []event.Event
		_, terminal := runTurn(context.Background(), input, 1, nil, cfg, client, noGateReg(), drainEmit(&emitted))
		if _, ok := terminal.(event.TurnDone); !ok {
			t.Fatalf("terminal = %T, want TurnDone (text-only on the limit iteration must win)", terminal)
		}
	})

	t.Run("model always calls tools → TurnFailed{ToolLimitError}, whole-turn rollback to base", func(t *testing.T) {
		t.Parallel()
		echo := &echoTool{name: "Echo", output: "ran"}
		cfg := agenticCfg(agenticToolSet([]tool.InvokableTool{echo}, 3, 100))
		// A single tool-call script repeated forever → never a text-only iteration.
		client := &scriptedLLM{scripts: [][]content.Chunk{
			{toolUseChunk(0, "id-x", "Echo", `{}`)},
		}}
		// Pre-existing history so base != 0.
		pre := content.AgenticMessages{&content.UserMessage{Message: content.Message{Role: content.RoleUser, Blocks: []content.Block{&content.TextBlock{Text: "earlier"}}}}}
		base := len(pre)
		var emitted []event.Event
		msgs, terminal := runTurn(context.Background(), input, 1, pre, cfg, client, noGateReg(), drainEmit(&emitted))

		failed, ok := terminal.(event.TurnFailed)
		if !ok {
			t.Fatalf("terminal = %T, want TurnFailed", terminal)
		}
		var tle *event.ToolLimitError
		if !errors.As(failed.Err, &tle) {
			t.Fatalf("TurnFailed.Err = %T, want *ToolLimitError via errors.As", failed.Err)
		}
		if len(msgs) != base {
			t.Fatalf("history len = %d, want %d (whole-turn rollback to base)", len(msgs), base)
		}
		// The pre-existing message must survive; the whole new turn must be gone.
		if msgs[0] != pre[0] {
			t.Errorf("rollback must preserve pre-turn history exactly")
		}
		// No unpaired tool_use survives — the rolled-back history encodes cleanly.
		tu, tmCount := countToolUseInHistory(msgs)
		if tu != 0 || tmCount != 0 {
			t.Errorf("after rollback: %d tool_use, %d tool messages, want 0/0", tu, tmCount)
		}
	})

	t.Run("malformed tool args → {} in stored assistant message + tool-result error; turn continues", func(t *testing.T) {
		t.Parallel()
		echo := &echoTool{name: "Echo", output: "ran"}
		cfg := agenticCfg(agenticToolSet([]tool.InvokableTool{echo}, 25, 100))
		client := &scriptedLLM{scripts: [][]content.Chunk{
			{toolUseChunk(0, "id-bad", "Echo", `{not valid json`)}, // malformed args
			{textChunk("recovered")},                               // model reacts → TurnDone
		}}
		var emitted []event.Event
		msgs, terminal := runTurn(context.Background(), input, 1, nil, cfg, client, noGateReg(), drainEmit(&emitted))

		if _, ok := terminal.(event.TurnDone); !ok {
			t.Fatalf("terminal = %T, want TurnDone (turn recovers from malformed args)", terminal)
		}
		// The tool never runs (invalid args is a pre-exec failure in RunBatch).
		if echo.runCount() != 0 {
			t.Errorf("echo ran %d times, want 0 (invalid args)", echo.runCount())
		}
		// Stored assistant message sanitizes the bad Input to {}.
		ai1, ok := msgs[1].(*content.AIMessage)
		if !ok {
			t.Fatalf("msgs[1] = %T, want *AIMessage", msgs[1])
		}
		var tub *content.ToolUseBlock
		for _, b := range ai1.Blocks {
			if x, ok := b.(*content.ToolUseBlock); ok {
				tub = x
			}
		}
		if tub == nil {
			t.Fatal("no ToolUseBlock in stored assistant message")
		}
		if string(tub.Input) != "{}" {
			t.Errorf("stored tool_use Input = %q, want %q (sanitized)", string(tub.Input), "{}")
		}
		// The whole stored history must re-encode cleanly (no invalid JSON).
		if !json.Valid(tub.Input) {
			t.Errorf("stored tool_use Input is not valid JSON: %q", string(tub.Input))
		}
		// A tool-result error reached the model.
		tm, ok := msgs[2].(*content.ToolResultMessage)
		if !ok {
			t.Fatalf("msgs[2] = %T, want *ToolResultMessage", msgs[2])
		}
		if got := flattenToText(tm.Blocks); got == "" {
			t.Errorf("tool message must carry a non-empty error result, got %q", got)
		}
		// Exactly one Started + one Completed{IsError} for the malformed call.
		var nStarted, nCompletedErr int
		for _, ev := range emitted {
			switch e := ev.(type) {
			case event.ToolCallStarted:
				nStarted++
			case event.ToolCallCompleted:
				if e.IsError {
					nCompletedErr++
				}
			}
		}
		if nStarted != 1 || nCompletedErr != 1 {
			t.Errorf("events: %d started / %d completed-err, want 1/1", nStarted, nCompletedErr)
		}
	})

	t.Run("ToolUseChunk fragments fold by Index (multi-fragment, multi-index, negative/huge Index no panic)", func(t *testing.T) {
		t.Parallel()
		echoA := &echoTool{name: "A", output: "ra"}
		echoB := &echoTool{name: "B", output: "rb"}
		cfg := agenticCfg(agenticToolSet([]tool.InvokableTool{echoA, echoB}, 25, 100))
		client := &scriptedLLM{scripts: [][]content.Chunk{
			{
				// Index 1 first (out of order); fragments split across deltas.
				toolUseChunk(1, "id-b", "B", `{"k"`),
				toolUseChunk(0, "id-a", "A", `{"k"`),
				toolUseChunk(1, "", "", `:2}`),
				toolUseChunk(0, "", "", `:1}`),
				// A pathological negative and huge Index must not panic.
				toolUseChunk(-5, "id-neg", "A", `{}`),
				toolUseChunk(1<<30, "id-huge", "B", `{}`),
			},
			{textChunk("done")},
		}}
		var emitted []event.Event
		msgs, terminal := runTurn(context.Background(), input, 1, nil, cfg, client, noGateReg(), drainEmit(&emitted))
		if _, ok := terminal.(event.TurnDone); !ok {
			t.Fatalf("terminal = %T, want TurnDone", terminal)
		}
		ai1 := msgs[1].(*content.AIMessage)
		var blocks []*content.ToolUseBlock
		for _, b := range ai1.Blocks {
			if x, ok := b.(*content.ToolUseBlock); ok {
				blocks = append(blocks, x)
			}
		}
		if len(blocks) != 4 {
			t.Fatalf("folded %d tool_use blocks, want 4 (indices -5,0,1,2^30)", len(blocks))
		}
		// Emitted in ASCENDING Index order: -5, 0, 1, 1<<30 → ids neg, a, b, huge.
		wantIDs := []string{"id-neg", "id-a", "id-b", "id-huge"}
		for i, b := range blocks {
			if b.ID != wantIDs[i] {
				t.Errorf("block[%d].ID = %q, want %q (ascending Index order)", i, b.ID, wantIDs[i])
			}
		}
		// Fragments concatenated per Index.
		for _, b := range blocks {
			if b.ID == "id-a" && string(b.Input) != `{"k":1}` {
				t.Errorf("index 0 folded Input = %q, want %q", string(b.Input), `{"k":1}`)
			}
			if b.ID == "id-b" && string(b.Input) != `{"k":2}` {
				t.Errorf("index 1 folded Input = %q, want %q", string(b.Input), `{"k":2}`)
			}
		}
	})

	t.Run("interrupt mid-loop → whole-turn rollback + TurnInterrupted", func(t *testing.T) {
		t.Parallel()
		echo := &echoTool{name: "Echo", output: "ran"}
		cfg := agenticCfg(agenticToolSet([]tool.InvokableTool{echo}, 25, 100))
		ctx, cancel := context.WithCancel(context.Background())
		// iter1 streams a tool call and runs it; iter2's Stream() cancels ctx
		// before producing any chunk → interrupt between iterations.
		client := &scriptedLLM{
			scripts: [][]content.Chunk{
				{toolUseChunk(0, "id-1", "Echo", `{}`)},
				{textChunk("never reached")},
			},
			onStreamN: map[int]func(){1: cancel},
		}
		pre := content.AgenticMessages{&content.UserMessage{Message: content.Message{Role: content.RoleUser, Blocks: []content.Block{&content.TextBlock{Text: "earlier"}}}}}
		base := len(pre)
		var emitted []event.Event
		msgs, terminal := runTurn(ctx, input, 1, pre, cfg, client, noGateReg(), drainEmit(&emitted))

		if _, ok := terminal.(event.TurnInterrupted); !ok {
			t.Fatalf("terminal = %T, want TurnInterrupted", terminal)
		}
		if len(msgs) != base {
			t.Errorf("history len = %d, want %d (whole-turn rollback)", len(msgs), base)
		}
		tu, tmCount := countToolUseInHistory(msgs)
		if tu != 0 || tmCount != 0 {
			t.Errorf("after interrupt rollback: %d tool_use, %d tool messages, want 0/0", tu, tmCount)
		}
	})

	t.Run("toolDefs maps registry → req.Tools (the client receives the tool defs)", func(t *testing.T) {
		t.Parallel()
		echoA := &echoTool{name: "A", output: "ra"}
		echoB := &echoTool{name: "B", output: "rb"}
		cfg := agenticCfg(agenticToolSet([]tool.InvokableTool{echoA, echoB}, 25, 100))
		client := &scriptedLLM{scripts: [][]content.Chunk{{textChunk("hi")}}}
		var emitted []event.Event
		runTurn(context.Background(), input, 1, nil, cfg, client, noGateReg(), drainEmit(&emitted))

		reqs := client.requests()
		if len(reqs) == 0 {
			t.Fatal("no request recorded")
		}
		defs := reqs[0].Tools
		if len(defs) != 2 {
			t.Fatalf("req.Tools len = %d, want 2", len(defs))
		}
		byName := map[string]llm.Tool{}
		for _, d := range defs {
			byName[d.Name] = d
		}
		if _, ok := byName["A"]; !ok {
			t.Errorf("tool A missing from req.Tools")
		}
		if got := string(byName["A"].Schema); got != `{"type":"object"}` {
			t.Errorf("tool A schema = %q, want %q", got, `{"type":"object"}`)
		}
		if byName["A"].Description != "echoes" {
			t.Errorf("tool A description = %q, want %q", byName["A"].Description, "echoes")
		}
	})

	t.Run("call-cap fires when a batch exceeds MaxToolCallsPerTurn", func(t *testing.T) {
		t.Parallel()
		echo := &echoTool{name: "Echo", output: "ran"}
		// maxCalls=1, but iter1 requests 2 calls → calls=2 > 1 → cap fires.
		cfg := agenticCfg(agenticToolSet([]tool.InvokableTool{echo}, 25, 1))
		client := &scriptedLLM{scripts: [][]content.Chunk{
			{toolUseChunk(0, "id-1", "Echo", `{}`), toolUseChunk(1, "id-2", "Echo", `{}`)},
		}}
		var emitted []event.Event
		msgs, terminal := runTurn(context.Background(), input, 1, nil, cfg, client, noGateReg(), drainEmit(&emitted))
		failed, ok := terminal.(event.TurnFailed)
		if !ok {
			t.Fatalf("terminal = %T, want TurnFailed", terminal)
		}
		var tle *event.ToolLimitError
		if !errors.As(failed.Err, &tle) {
			t.Fatalf("TurnFailed.Err = %T, want *ToolLimitError", failed.Err)
		}
		if len(msgs) != 0 {
			t.Errorf("history len = %d, want 0 (rollback to base)", len(msgs))
		}
		// Cap fires before RunBatch executes the batch.
		if echo.runCount() != 0 {
			t.Errorf("echo ran %d times, want 0 (cap fires before execution)", echo.runCount())
		}
	})
}

func TestRunTurn(t *testing.T) {
	t.Parallel()
	cfg := Config{Model: llm.ModelSpec{Model: "m"}}
	input := []content.Block{&content.TextBlock{Text: "hi"}}

	t.Run("success appends user+assistant and returns TurnDone", func(t *testing.T) {
		t.Parallel()
		client := &fakeLLM{chunks: []content.Chunk{textChunk("hel"), textChunk("lo")}}
		var emitted []event.Event
		msgs, terminal := runTurn(context.Background(), input, 1, nil, cfg, client, noGateReg(), drainEmit(&emitted))

		done, ok := terminal.(event.TurnDone)
		if !ok {
			t.Fatalf("terminal = %T, want TurnDone", terminal)
		}
		if len(msgs) != 2 {
			t.Fatalf("history len = %d, want 2 (user, assistant)", len(msgs))
		}
		if _, ok := msgs[0].(*content.UserMessage); !ok {
			t.Errorf("msgs[0] = %T, want *UserMessage", msgs[0])
		}
		last := done.Message.Blocks[len(done.Message.Blocks)-1]
		tb, ok := last.(*content.TextBlock)
		if !ok {
			t.Fatalf("last block = %T, want *content.TextBlock", last)
		}
		if tb.Text != "hello" {
			t.Errorf("assembled text = %q, want %q", tb.Text, "hello")
		}
	})

	t.Run("stream error rolls back user message, TurnFailed carries typed cause", func(t *testing.T) {
		t.Parallel()
		boom := &llm.ValidationError{Field: "x", Reason: "boom"}
		client := &fakeLLM{streamErr: boom}
		var emitted []event.Event
		msgs, terminal := runTurn(context.Background(), input, 1, nil, cfg, client, noGateReg(), drainEmit(&emitted))

		failed, ok := terminal.(event.TurnFailed)
		if !ok {
			t.Fatalf("terminal = %T, want TurnFailed", terminal)
		}
		var ve *llm.ValidationError
		if !errors.As(failed.Err, &ve) {
			t.Fatalf("TurnFailed.Err = %T, want *llm.ValidationError via errors.As", failed.Err)
		}
		if len(msgs) != 0 {
			t.Errorf("history len = %d, want 0 (user rolled back)", len(msgs))
		}
	})

	t.Run("empty response rolls back and returns EmptyResponseError", func(t *testing.T) {
		t.Parallel()
		client := &fakeLLM{chunks: nil}
		var emitted []event.Event
		msgs, terminal := runTurn(context.Background(), input, 1, nil, cfg, client, noGateReg(), drainEmit(&emitted))

		failed, ok := terminal.(event.TurnFailed)
		if !ok {
			t.Fatalf("terminal = %T, want TurnFailed", terminal)
		}
		var ere *event.EmptyResponseError
		if !errors.As(failed.Err, &ere) {
			t.Fatalf("TurnFailed.Err = %T, want *EmptyResponseError", failed.Err)
		}
		if len(msgs) != 0 {
			t.Errorf("history len = %d, want 0 (user rolled back)", len(msgs))
		}
	})

	t.Run("empty-string-only chunks roll back and return EmptyResponseError (zero-length blocks, not zero blocks)", func(t *testing.T) {
		t.Parallel()
		// Unlike the chunks:nil case above (zero blocks), this stream emits real
		// chunks whose text is empty. streamaccumulator.Text.received flips true on
		// the first Add, but the loop decides emptiness on the materialized block
		// TEXT (isEmptyAssistantMessage), so a zero-LENGTH text block must still be
		// EmptyResponseError with no assistant message stored.
		chunks := []content.Chunk{textChunk(""), textChunk("")}
		client := &fakeLLM{chunks: chunks}
		var emitted []event.Event
		msgs, terminal := runTurn(context.Background(), input, 1, nil, cfg, client, noGateReg(), drainEmit(&emitted))

		failed, ok := terminal.(event.TurnFailed)
		if !ok {
			t.Fatalf("terminal = %T, want TurnFailed", terminal)
		}
		var ere *event.EmptyResponseError
		if !errors.As(failed.Err, &ere) {
			t.Fatalf("TurnFailed.Err = %T, want *EmptyResponseError", failed.Err)
		}
		if len(msgs) != 0 {
			t.Errorf("history len = %d, want 0 (no assistant message stored; rolled back to base)", len(msgs))
		}
		// A TokenDelta is still emitted per chunk even though the materialized text is empty.
		var deltas int
		for _, e := range emitted {
			if _, ok := e.(event.TokenDelta); ok {
				deltas++
			}
		}
		if deltas != len(chunks) {
			t.Errorf("TokenDelta count = %d, want %d (one per chunk)", deltas, len(chunks))
		}
	})

	t.Run("cancelled context rolls back and returns TurnInterrupted", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		client := &fakeLLM{streamErr: context.Canceled}
		var emitted []event.Event
		msgs, terminal := runTurn(ctx, input, 1, nil, cfg, client, noGateReg(), drainEmit(&emitted))

		if _, ok := terminal.(event.TurnInterrupted); !ok {
			t.Fatalf("terminal = %T, want TurnInterrupted", terminal)
		}
		if len(msgs) != 0 {
			t.Errorf("history len = %d, want 0 (user rolled back)", len(msgs))
		}
	})

	t.Run("mid-stream Next error rolls back with typed cause", func(t *testing.T) {
		t.Parallel()
		boom := &llm.ValidationError{Field: "y", Reason: "midstream"}
		client := &fakeLLM{chunks: []content.Chunk{textChunk("partial")}, nextErr: boom}
		var emitted []event.Event
		msgs, terminal := runTurn(context.Background(), input, 1, nil, cfg, client, noGateReg(), drainEmit(&emitted))

		failed, ok := terminal.(event.TurnFailed)
		if !ok {
			t.Fatalf("terminal = %T, want TurnFailed", terminal)
		}
		var ve *llm.ValidationError
		if !errors.As(failed.Err, &ve) {
			t.Fatalf("TurnFailed.Err = %T, want *llm.ValidationError", failed.Err)
		}
		if len(msgs) != 0 {
			t.Errorf("history len = %d, want 0 (user rolled back)", len(msgs))
		}
	})

	t.Run("emits TurnStarted then TokenDelta per chunk", func(t *testing.T) {
		t.Parallel()
		client := &fakeLLM{chunks: []content.Chunk{textChunk("a"), textChunk("b")}}
		var emitted []event.Event
		runTurn(context.Background(), input, 1, nil, cfg, client, noGateReg(), drainEmit(&emitted))
		if len(emitted) < 1 {
			t.Fatal("no events emitted")
		}
		if _, ok := emitted[0].(event.TurnStarted); !ok {
			t.Errorf("first event = %T, want TurnStarted", emitted[0])
		}
		var deltas int
		for _, e := range emitted {
			if _, ok := e.(event.TokenDelta); ok {
				deltas++
			}
		}
		if deltas != 2 {
			t.Errorf("TokenDelta count = %d, want 2", deltas)
		}
	})
}
