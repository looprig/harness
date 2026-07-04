package loop

import (
	"testing"

	"github.com/looprig/core/content"
	"github.com/looprig/harness/pkg/event"
)

// TestChunkProcessorEmitThenAccumulate proves the chunk layer's ordering
// contract: process(chunk) emits the live TokenDelta for the chunk BEFORE
// folding it into the blockState. The emit callback inspects the blockState at
// emit time; if accumulation already happened the block would be visible, which
// would violate the "emit then accumulate" order.
func TestChunkProcessorEmitThenAccumulate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		chunk     content.Chunk
		wantText  string
		wantThink string
		wantTool  bool
	}{
		{
			name:     "text chunk emits TokenDelta then folds into text",
			chunk:    &content.TextChunk{Text: "hello"},
			wantText: "hello",
		},
		{
			name:      "thinking chunk emits TokenDelta then folds into thinking",
			chunk:     &content.ThinkingChunk{Thinking: "reasoning"},
			wantThink: "reasoning",
		},
		{
			name:     "tool-use chunk emits TokenDelta then folds into tool uses",
			chunk:    &content.ToolUseChunk{Index: 0, ID: "id-1", Name: "Echo", InputJSON: `{}`},
			wantTool: true,
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var st blockState

			// emitted records whether emit ran, and emptyAtEmit captures whether the
			// blockState was still empty when emit fired (proving emit precedes fold).
			var emitted bool
			var emptyAtEmit bool
			const turnIndex event.TurnIndex = 7
			emit := func(ev event.Event) {
				td, ok := ev.(event.TokenDelta)
				if !ok {
					t.Errorf("emit got %T, want event.TokenDelta", ev)
					return
				}
				if td.Chunk != tt.chunk {
					t.Errorf("TokenDelta.Chunk = %#v, want %#v", td.Chunk, tt.chunk)
				}
				if td.TurnIndex != turnIndex {
					t.Errorf("TokenDelta.TurnIndex = %d, want %d", td.TurnIndex, turnIndex)
				}
				emitted = true
				msg := st.AIMessage()
				emptyAtEmit = len(msg.Blocks) == 0
			}

			p := newChunkProcessor(emit, chunkState{blocks: &st})
			p.process(tt.chunk, turnIndex)

			if !emitted {
				t.Fatal("process did not emit a TokenDelta")
			}
			if !emptyAtEmit {
				t.Error("blockState was already updated at emit time; want emit BEFORE accumulate")
			}

			// After process, the chunk must be folded into the blockState.
			msg := st.AIMessage()
			var gotText, gotThink string
			var gotTool bool
			for _, b := range msg.Blocks {
				switch v := b.(type) {
				case *content.TextBlock:
					gotText = v.Text
				case *content.ThinkingBlock:
					gotThink = v.Thinking
				case *content.ToolUseBlock:
					gotTool = true
				}
			}
			if gotText != tt.wantText {
				t.Errorf("folded text = %q, want %q", gotText, tt.wantText)
			}
			if gotThink != tt.wantThink {
				t.Errorf("folded thinking = %q, want %q", gotThink, tt.wantThink)
			}
			if gotTool != tt.wantTool {
				t.Errorf("folded tool-use present = %v, want %v", gotTool, tt.wantTool)
			}
		})
	}
}

// TestChunkProcessorFoldsWithNoOpEmit proves accumulation is independent of
// emission: a chunk is still folded into the blockState even when the emit
// callback does nothing with it.
func TestChunkProcessorFoldsWithNoOpEmit(t *testing.T) {
	t.Parallel()

	var st blockState
	noop := func(event.Event) {}
	p := newChunkProcessor(noop, chunkState{blocks: &st})

	p.process(&content.TextChunk{Text: "a"}, 0)
	p.process(&content.TextChunk{Text: "b"}, 0)
	p.process(&content.ToolUseChunk{Index: 0, ID: "id", Name: "Echo", InputJSON: `{}`}, 0)

	msg := st.AIMessage()
	if len(msg.Blocks) != 2 {
		t.Fatalf("AIMessage blocks = %d, want 2 (text + tool_use)", len(msg.Blocks))
	}
	tb, ok := msg.Blocks[0].(*content.TextBlock)
	if !ok {
		t.Fatalf("block[0] = %T, want *content.TextBlock", msg.Blocks[0])
	}
	if tb.Text != "ab" {
		t.Errorf("folded text = %q, want %q (folds even with no-op emit)", tb.Text, "ab")
	}
	if len(st.ToolUses()) != 1 {
		t.Errorf("ToolUses() len = %d, want 1 (folds even with no-op emit)", len(st.ToolUses()))
	}
}
