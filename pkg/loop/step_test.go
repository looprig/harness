package loop

import (
	"context"
	"errors"
	"testing"

	"github.com/looprig/harness/pkg/content"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/llm"
	"github.com/looprig/harness/pkg/uuid"
)

// mustUUID mints a UUID for tests or fails the test (crypto/rand should never
// fail in a test environment).
func mustUUID(t *testing.T) uuid.UUID {
	t.Helper()
	id, err := uuid.New()
	if err != nil {
		t.Fatalf("uuid.New: %v", err)
	}
	return id
}

// newTestStep builds a fresh stepState with the given identity ids and index,
// the way runTurn does before each LLM cycle.
func newTestStep(t *testing.T, index StepIndex) stepState {
	t.Helper()
	return newStepState(mustUUID(t), mustUUID(t), mustUUID(t), mustUUID(t), index)
}

func TestNewStepState(t *testing.T) {
	t.Parallel()

	sessionID := mustUUID(t)
	loopID := mustUUID(t)
	turnID := mustUUID(t)
	stepID := mustUUID(t)

	st := newStepState(sessionID, loopID, turnID, stepID, 7)

	if st.sessionID != sessionID {
		t.Errorf("sessionID = %v, want %v", st.sessionID, sessionID)
	}
	if st.loopID != loopID {
		t.Errorf("loopID = %v, want %v", st.loopID, loopID)
	}
	if st.turnID != turnID {
		t.Errorf("turnID = %v, want %v", st.turnID, turnID)
	}
	if st.id != stepID {
		t.Errorf("id = %v, want %v", st.id, stepID)
	}
	if st.index != 7 {
		t.Errorf("index = %d, want 7", st.index)
	}
	if len(st.msgs) != 0 {
		t.Errorf("fresh stepState.msgs len = %d, want 0", len(st.msgs))
	}
	if st.status != stepStreaming {
		t.Errorf("fresh stepState.status = %v, want stepStreaming", st.status)
	}
}

func TestRunStep(t *testing.T) {
	t.Parallel()

	t.Run("text-only stream finalizes one AIMessage into msgs[0]", func(t *testing.T) {
		t.Parallel()
		client := &fakeLLM{chunks: []content.Chunk{textChunk("hel"), textChunk("lo")}}
		var emitted []event.Event
		cfg := stepConfig{req: llm.Request{Model: testModel()}, client: client, emit: drainEmit(&emitted)}

		res := runStep(context.Background(), cfg, 5, newTestStep(t, 0))

		if res.terminal != nil {
			t.Fatalf("terminal = %v, want nil (success)", res.terminal)
		}
		if len(res.state.msgs) != 1 {
			t.Fatalf("msgs len = %d, want 1 (exactly one AIMessage)", len(res.state.msgs))
		}
		ai, ok := res.state.msgs[0].(*content.AIMessage)
		if !ok {
			t.Fatalf("msgs[0] = %T, want *AIMessage", res.state.msgs[0])
		}
		if len(ai.Blocks) != 1 {
			t.Fatalf("AIMessage blocks = %d, want 1", len(ai.Blocks))
		}
		tb, ok := ai.Blocks[0].(*content.TextBlock)
		if !ok {
			t.Fatalf("block[0] = %T, want *TextBlock", ai.Blocks[0])
		}
		if tb.Text != "hello" {
			t.Errorf("assembled text = %q, want %q", tb.Text, "hello")
		}
		if res.state.status != stepDone {
			t.Errorf("status = %v, want stepDone", res.state.status)
		}
		// One TokenDelta per chunk, each stamped with the PASSED turn index (5), not
		// the step index (0) — they are independent.
		var deltas int
		for _, e := range emitted {
			if td, ok := e.(event.TokenDelta); ok {
				deltas++
				if td.TurnIndex != 5 {
					t.Errorf("TokenDelta.TurnIndex = %d, want 5 (the passed turn index, not step index)", td.TurnIndex)
				}
			}
		}
		if deltas != 2 {
			t.Errorf("TokenDelta count = %d, want 2", deltas)
		}
	})

	t.Run("text + tool use stores one AIMessage with a TextBlock and a ToolUseBlock", func(t *testing.T) {
		t.Parallel()
		client := &scriptedLLM{scripts: [][]content.Chunk{{
			textChunk("calling"),
			toolUseChunk(0, "id-1", "Echo", `{"x":1}`),
		}}}
		var emitted []event.Event
		cfg := stepConfig{req: llm.Request{Model: testModel()}, client: client, emit: drainEmit(&emitted)}

		res := runStep(context.Background(), cfg, 5, newTestStep(t, 0))

		if res.terminal != nil {
			t.Fatalf("terminal = %v, want nil", res.terminal)
		}
		if len(res.state.msgs) != 1 {
			t.Fatalf("msgs len = %d, want 1", len(res.state.msgs))
		}
		ai := res.state.msgs[0].(*content.AIMessage)
		var hasText, hasTool bool
		for _, b := range ai.Blocks {
			switch b.(type) {
			case *content.TextBlock:
				hasText = true
			case *content.ToolUseBlock:
				hasTool = true
			}
		}
		if !hasText || !hasTool {
			t.Errorf("AIMessage blocks: hasText=%v hasTool=%v, want both true", hasText, hasTool)
		}
		// The executable tool-use view is exposed on the step's blockState.
		if got := res.state.blocks.ToolUses(); len(got) != 1 {
			t.Errorf("ToolUses len = %d, want 1", len(got))
		}
	})

	t.Run("empty response returns terminal carrying *EmptyResponseError, no message stored", func(t *testing.T) {
		t.Parallel()
		client := &fakeLLM{chunks: nil}
		var emitted []event.Event
		cfg := stepConfig{req: llm.Request{Model: testModel()}, client: client, emit: drainEmit(&emitted)}

		res := runStep(context.Background(), cfg, 5, newTestStep(t, 0))

		if res.terminal == nil {
			t.Fatal("terminal = nil, want non-nil for empty response")
		}
		failed, ok := res.terminal.(event.TurnFailed)
		if !ok {
			t.Fatalf("terminal = %T, want event.TurnFailed", res.terminal)
		}
		var ere *event.EmptyResponseError
		if !errors.As(failed.Err, &ere) {
			t.Fatalf("terminal.Err = %T, want *EmptyResponseError via errors.As", failed.Err)
		}
		if len(res.state.msgs) != 0 {
			t.Errorf("msgs len = %d, want 0 (no message stored on empty response)", len(res.state.msgs))
		}
	})

	t.Run("empty-string-only chunks are empty (zero-length blocks, not zero blocks)", func(t *testing.T) {
		t.Parallel()
		chunks := []content.Chunk{textChunk(""), textChunk("")}
		client := &fakeLLM{chunks: chunks}
		var emitted []event.Event
		cfg := stepConfig{req: llm.Request{Model: testModel()}, client: client, emit: drainEmit(&emitted)}

		res := runStep(context.Background(), cfg, 5, newTestStep(t, 0))

		var ere *event.EmptyResponseError
		failed, ok := res.terminal.(event.TurnFailed)
		if !ok || !errors.As(failed.Err, &ere) {
			t.Fatalf("terminal = %v, want TurnFailed{*EmptyResponseError}", res.terminal)
		}
		if len(res.state.msgs) != 0 {
			t.Errorf("msgs len = %d, want 0", len(res.state.msgs))
		}
		// A TokenDelta is still emitted per chunk even though materialized text is empty.
		var deltas int
		for _, e := range emitted {
			if _, ok := e.(event.TokenDelta); ok {
				deltas++
			}
		}
		if deltas != len(chunks) {
			t.Errorf("TokenDelta count = %d, want %d", deltas, len(chunks))
		}
	})

	t.Run("Stream() error returns terminal carrying the typed cause", func(t *testing.T) {
		t.Parallel()
		boom := &llm.ValidationError{Field: "x", Reason: "boom"}
		client := &fakeLLM{streamErr: boom}
		cfg := stepConfig{req: llm.Request{Model: testModel()}, client: client, emit: func(event.Event) {}}

		res := runStep(context.Background(), cfg, 5, newTestStep(t, 0))

		failed, ok := res.terminal.(event.TurnFailed)
		if !ok {
			t.Fatalf("terminal = %T, want event.TurnFailed", res.terminal)
		}
		var ve *llm.ValidationError
		if !errors.As(failed.Err, &ve) {
			t.Fatalf("terminal.Err = %T, want *llm.ValidationError", failed.Err)
		}
		if len(res.state.msgs) != 0 {
			t.Errorf("msgs len = %d, want 0", len(res.state.msgs))
		}
	})

	t.Run("mid-stream Next error returns terminal carrying the typed cause", func(t *testing.T) {
		t.Parallel()
		boom := &llm.ValidationError{Field: "y", Reason: "midstream"}
		client := &fakeLLM{chunks: []content.Chunk{textChunk("partial")}, nextErr: boom}
		cfg := stepConfig{req: llm.Request{Model: testModel()}, client: client, emit: func(event.Event) {}}

		res := runStep(context.Background(), cfg, 5, newTestStep(t, 0))

		failed, ok := res.terminal.(event.TurnFailed)
		if !ok {
			t.Fatalf("terminal = %T, want event.TurnFailed", res.terminal)
		}
		var ve *llm.ValidationError
		if !errors.As(failed.Err, &ve) {
			t.Fatalf("terminal.Err = %T, want *llm.ValidationError", failed.Err)
		}
	})

	t.Run("cancelled context returns a TurnInterrupted terminal", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		client := &fakeLLM{streamErr: context.Canceled}
		cfg := stepConfig{req: llm.Request{Model: testModel()}, client: client, emit: func(event.Event) {}}

		res := runStep(ctx, cfg, 5, newTestStep(t, 0))

		if _, ok := res.terminal.(event.TurnInterrupted); !ok {
			t.Fatalf("terminal = %T, want event.TurnInterrupted", res.terminal)
		}
		if len(res.state.msgs) != 0 {
			t.Errorf("msgs len = %d, want 0", len(res.state.msgs))
		}
	})

	t.Run("malformed tool args: stored AIMessage Input sanitized to {}, raw executable view kept", func(t *testing.T) {
		t.Parallel()
		client := &scriptedLLM{scripts: [][]content.Chunk{{
			toolUseChunk(0, "id-bad", "Echo", `{not valid json`),
		}}}
		cfg := stepConfig{req: llm.Request{Model: testModel()}, client: client, emit: func(event.Event) {}}

		res := runStep(context.Background(), cfg, 5, newTestStep(t, 0))

		if res.terminal != nil {
			t.Fatalf("terminal = %v, want nil", res.terminal)
		}
		ai := res.state.msgs[0].(*content.AIMessage)
		var stored *content.ToolUseBlock
		for _, b := range ai.Blocks {
			if x, ok := b.(*content.ToolUseBlock); ok {
				stored = x
			}
		}
		if stored == nil {
			t.Fatal("no ToolUseBlock in stored AIMessage")
		}
		if string(stored.Input) != "{}" {
			t.Errorf("stored tool_use Input = %q, want %q (sanitized)", string(stored.Input), "{}")
		}
		// The raw executable view keeps the malformed Input so RunBatch reports it.
		raw := res.state.blocks.ToolUses()
		if len(raw) != 1 || string(raw[0].Input) != `{not valid json` {
			t.Errorf("raw executable Input = %q, want %q (unsanitized)", string(raw[0].Input), `{not valid json`)
		}
	})
}
