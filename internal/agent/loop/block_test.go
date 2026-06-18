package loop

import (
	"reflect"
	"testing"

	"github.com/inventivepotter/urvi/internal/content"
)

// feedBlock folds a slice of chunks into a fresh blockState, dispatching by the
// chunk's concrete type the same way the chunk layer does. It returns the
// blockState so a test can materialize the AIMessage / ToolUses.
func feedBlock(chunks []content.Chunk) *blockState {
	b := newBlock(blockState{})
	st := &b.state
	for _, c := range chunks {
		switch v := c.(type) {
		case *content.TextChunk:
			st.msgs.text.Add(v)
		case *content.ThinkingChunk:
			st.msgs.thinking.Add(v)
		case *content.ToolUseChunk:
			st.msgs.toolUses.Add(v)
		}
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
	// Find the stored tool-use block and mutate its Input as streamOnce would.
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
