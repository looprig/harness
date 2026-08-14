package loopruntime

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/looprig/core/content"
)

// feedBlock folds a slice of chunks into a fresh blockState, dispatching by the
// chunk's concrete type the same way the chunk layer does. It returns the
// blockState so a test can materialize the AIMessage / ToolUses.
func feedBlock(chunks []content.Chunk) *blockState {
	st := &blockState{}
	for _, c := range chunks {
		st.msgs.add(c)
	}
	return st
}

func TestBlockStateAIMessage(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		chunks     []content.Chunk
		wantBlocks []content.Block
	}{
		{
			name:       "empty: no chunks yields an AIMessage with no blocks",
			chunks:     nil,
			wantBlocks: nil,
		},
		{
			name:   "text only yields a single TextBlock",
			chunks: []content.Chunk{&content.TextChunk{Text: "hello"}},
			wantBlocks: []content.Block{
				&content.TextBlock{Text: "hello"},
			},
		},
		{
			name:   "thinking only yields a single ThinkingBlock",
			chunks: []content.Chunk{&content.ThinkingChunk{Thinking: "reasoning"}},
			wantBlocks: []content.Block{
				&content.ThinkingBlock{Thinking: "reasoning"},
			},
		},
		{
			name: "tool-use only yields a single ToolUseBlock",
			chunks: []content.Chunk{
				&content.ToolUseChunk{Index: 0, ID: "id-1", Name: "Echo", InputJSON: `{"x":1}`},
			},
			wantBlocks: []content.Block{
				&content.ToolUseBlock{ID: "id-1", Name: "Echo", Input: []byte(`{"x":1}`)},
			},
		},
		{
			name: "thinking + text + tool_use materialize in block order (thinking, text, tool_use)",
			chunks: []content.Chunk{
				&content.ThinkingChunk{Thinking: "thinking..."},
				&content.TextChunk{Text: "the answer"},
				&content.ToolUseChunk{Index: 0, ID: "id-1", Name: "Echo", InputJSON: `{"x":1}`},
			},
			wantBlocks: []content.Block{
				&content.ThinkingBlock{Thinking: "thinking..."},
				&content.TextBlock{Text: "the answer"},
				&content.ToolUseBlock{ID: "id-1", Name: "Echo", Input: []byte(`{"x":1}`)},
			},
		},
		{
			name:   "refusal only yields a single RefusalBlock",
			chunks: []content.Chunk{&content.RefusalChunk{Text: "I can't help with that."}},
			wantBlocks: []content.Block{
				&content.RefusalBlock{Text: "I can't help with that."},
			},
		},
		{
			// The accumulator materializes on having RECEIVED a delta, not on the
			// text being non-empty: a provider may decline without explaining, and
			// the block's presence is the signal.
			name:       "an empty refusal delta still yields a RefusalBlock",
			chunks:     []content.Chunk{&content.RefusalChunk{}},
			wantBlocks: []content.Block{&content.RefusalBlock{}},
		},
		{
			name: "image deltas fold per Index and materialize in ascending Index order",
			chunks: []content.Chunk{
				&content.ImageChunk{Index: 1, MediaType: "image/jpeg", Source: content.ImageSource{Data: []byte{0xff}}},
				&content.ImageChunk{Index: 0, MediaType: "image/png", Source: content.ImageSource{Data: []byte{0x89}}},
				&content.ImageChunk{Index: 0, Source: content.ImageSource{Data: []byte{'P'}}},
			},
			wantBlocks: []content.Block{
				&content.ImageBlock{MediaType: "image/png", Source: content.ImageSource{Data: []byte{0x89, 'P'}}},
				&content.ImageBlock{MediaType: "image/jpeg", Source: content.ImageSource{Data: []byte{0xff}}},
			},
		},
		{
			name: "every variant materializes in provider emission order",
			chunks: []content.Chunk{
				&content.ToolUseChunk{Index: 0, ID: "id-1", Name: "Echo", InputJSON: `{"x":1}`},
				&content.ImageChunk{Index: 0, MediaType: "image/png", Source: content.ImageSource{Data: []byte{0x89}}},
				&content.RefusalChunk{Text: "no"},
				&content.TextChunk{Text: "the answer"},
				&content.ThinkingChunk{Thinking: "thinking..."},
			},
			wantBlocks: []content.Block{
				&content.ToolUseBlock{ID: "id-1", Name: "Echo", Input: []byte(`{"x":1}`)},
				&content.ImageBlock{MediaType: "image/png", Source: content.ImageSource{Data: []byte{0x89}}},
				&content.RefusalBlock{Text: "no"},
				&content.TextBlock{Text: "the answer"},
				&content.ThinkingBlock{Thinking: "thinking..."},
			},
		},
		{
			name: "multiple tool_use blocks materialize in ascending Index order after thinking+text",
			chunks: []content.Chunk{
				&content.TextChunk{Text: "t"},
				&content.ToolUseChunk{Index: 1, ID: "id-b", Name: "B", InputJSON: `{"k":2}`},
				&content.ToolUseChunk{Index: 0, ID: "id-a", Name: "A", InputJSON: `{"k":1}`},
			},
			wantBlocks: []content.Block{
				&content.TextBlock{Text: "t"},
				&content.ToolUseBlock{ID: "id-a", Name: "A", Input: []byte(`{"k":1}`)},
				&content.ToolUseBlock{ID: "id-b", Name: "B", Input: []byte(`{"k":2}`)},
			},
		},
		{
			name: "interleaved thinking and tool calls retain provider emission order",
			chunks: []content.Chunk{
				&content.ThinkingChunk{Index: 0, Thinking: "first", Signature: "sig-0"},
				&content.ToolUseChunk{Index: 1, ID: "id-1", Name: "A", InputJSON: `{"x":1}`},
				&content.ThinkingChunk{Index: 2, Thinking: "second", Signature: "sig-2"},
				&content.ToolUseChunk{Index: 3, ID: "id-2", Name: "B", InputJSON: `{"x":2}`},
			},
			wantBlocks: []content.Block{
				content.NewThinkingBlock("first", "sig-0", nil, ""),
				&content.ToolUseBlock{ID: "id-1", Name: "A", Input: []byte(`{"x":1}`)},
				content.NewThinkingBlock("second", "sig-2", nil, ""),
				&content.ToolUseBlock{ID: "id-2", Name: "B", Input: []byte(`{"x":2}`)},
			},
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			st := feedBlock(tt.chunks)
			msg := st.AIMessage()
			if msg == nil {
				t.Fatal("AIMessage() = nil, want non-nil *content.AIMessage")
			}
			if msg.Role != content.RoleAssistant {
				t.Errorf("AIMessage().Role = %q, want %q", msg.Role, content.RoleAssistant)
			}
			if !reflect.DeepEqual(msg.Blocks, tt.wantBlocks) {
				t.Errorf("AIMessage().Blocks = %#v, want %#v", msg.Blocks, tt.wantBlocks)
			}
		})
	}
}

// TestBlockStateAIMessageEmitsEveryThinkingBlock proves a multi-reasoning-block
// response is materialized in full. Anthropic interleaved thinking opens a FRESH
// thinking (or redacted_thinking) block around every tool call, and each block
// carries its own signature or opaque provider state that must be replayed
// block-for-block. Emitting only the accumulator's lowest-index block silently
// discards reasoning blocks 2..N along with their continuation state.
func TestBlockStateAIMessageEmitsEveryThinkingBlock(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		chunks     []content.Chunk
		wantBlocks []content.Block
	}{
		{
			name: "several thinking blocks materialize in ascending Index order",
			chunks: []content.Chunk{
				&content.ThinkingChunk{Index: 1, Thinking: "second", Signature: "sig-1"},
				&content.ThinkingChunk{Index: 0, Thinking: "first", Signature: "sig-0"},
			},
			wantBlocks: []content.Block{
				content.NewThinkingBlock("first", "sig-0", nil, ""),
				content.NewThinkingBlock("second", "sig-1", nil, ""),
			},
		},
		{
			name: "each thinking block keeps its OWN provider state",
			chunks: []content.Chunk{
				&content.ThinkingChunk{Index: 0, Thinking: "first", ProviderState: json.RawMessage(`{"s":0}`), ProviderStateFormat: "anthropic"},
				&content.ThinkingChunk{Index: 1, Thinking: "second", ProviderState: json.RawMessage(`{"s":1}`), ProviderStateFormat: "anthropic"},
			},
			wantBlocks: []content.Block{
				content.NewThinkingBlock("first", "", json.RawMessage(`{"s":0}`), "anthropic"),
				content.NewThinkingBlock("second", "", json.RawMessage(`{"s":1}`), "anthropic"),
			},
		},
		{
			name: "all thinking blocks precede text and tool use",
			chunks: []content.Chunk{
				&content.ThinkingChunk{Index: 0, Thinking: "a"},
				&content.ThinkingChunk{Index: 2, Thinking: "b"},
				&content.TextChunk{Text: "answer"},
				&content.ToolUseChunk{Index: 0, ID: "id-1", Name: "Echo", InputJSON: `{"x":1}`},
			},
			wantBlocks: []content.Block{
				content.NewThinkingBlock("a", "", nil, ""),
				content.NewThinkingBlock("b", "", nil, ""),
				&content.TextBlock{Text: "answer"},
				&content.ToolUseBlock{ID: "id-1", Name: "Echo", Input: []byte(`{"x":1}`)},
			},
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := feedBlock(tt.chunks).AIMessage().Blocks
			if !reflect.DeepEqual(got, tt.wantBlocks) {
				t.Errorf("AIMessage().Blocks = %#v, want %#v", got, tt.wantBlocks)
			}
		})
	}
}

func TestBlockStateToolUses(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		chunks []content.Chunk
		want   []content.ToolUseBlock
	}{
		{
			name:   "empty: no tool-use chunks yields nil",
			chunks: nil,
			want:   nil,
		},
		{
			name:   "text and thinking only yields nil tool uses",
			chunks: []content.Chunk{&content.TextChunk{Text: "x"}, &content.ThinkingChunk{Thinking: "y"}},
			want:   nil,
		},
		{
			name: "tool uses are the executable view in ascending Index order",
			chunks: []content.Chunk{
				&content.ToolUseChunk{Index: 1, ID: "id-b", Name: "B", InputJSON: `{"k":2}`},
				&content.ToolUseChunk{Index: 0, ID: "id-a", Name: "A", InputJSON: `{"k":1}`},
			},
			want: []content.ToolUseBlock{
				{ID: "id-a", Name: "A", Input: []byte(`{"k":1}`)},
				{ID: "id-b", Name: "B", Input: []byte(`{"k":2}`)},
			},
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			st := feedBlock(tt.chunks)
			got := st.ToolUses()
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("ToolUses() = %#v, want %#v", got, tt.want)
			}
		})
	}
}

// TestBlockStateToolUsesIndependentOfAIMessage proves the executable ToolUses
// view is a distinct allocation from the AIMessage's child tool-use blocks, so
// the caller can sanitize the stored message's Input without affecting the raw
// executable view (the malformed-tool-use invariant the loop relies on).
func TestBlockStateToolUsesIndependentOfAIMessage(t *testing.T) {
	t.Parallel()

	st := feedBlock([]content.Chunk{
		&content.ToolUseChunk{Index: 0, ID: "id-1", Name: "Echo", InputJSON: `{not valid json`},
	})
	msg := st.AIMessage()
	raw := st.ToolUses()

	if len(raw) != 1 {
		t.Fatalf("ToolUses() len = %d, want 1", len(raw))
	}
	// Find the stored tool-use block and mutate its Input as runStep would.
	var stored *content.ToolUseBlock
	for _, b := range msg.Blocks {
		if x, ok := b.(*content.ToolUseBlock); ok {
			stored = x
		}
	}
	if stored == nil {
		t.Fatal("no ToolUseBlock in AIMessage")
	}
	stored.Input = []byte("{}")
	if string(raw[0].Input) != `{not valid json` {
		t.Errorf("mutating the stored block changed the executable view: raw Input = %q, want %q",
			string(raw[0].Input), `{not valid json`)
	}
}
