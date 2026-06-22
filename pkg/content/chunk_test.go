package content_test

import (
	"testing"

	"github.com/ciram-co/looprig/pkg/content"
)

// TestChunk_ConcretePayloads verifies the concrete Chunk variants carry their
// delta text. The concrete type is the discriminator; there is no Type field.
func TestChunk_ConcretePayloads(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		chunk     content.Chunk
		wantText  string               // expected text for a *TextChunk
		wantThink string               // expected thinking for a *ThinkingChunk
		wantTool  content.ToolUseChunk // expected fields for a *ToolUseChunk
	}{
		{
			name:     "text chunk carries text payload",
			chunk:    &content.TextChunk{Text: "hello"},
			wantText: "hello",
		},
		{
			name:      "thinking chunk carries thinking payload",
			chunk:     &content.ThinkingChunk{Thinking: "reasoning"},
			wantThink: "reasoning",
		},
		{
			name:     "text chunk with empty string is a valid delta",
			chunk:    &content.TextChunk{Text: ""},
			wantText: "",
		},
		{
			name:      "thinking chunk with empty string is a valid delta",
			chunk:     &content.ThinkingChunk{Thinking: ""},
			wantThink: "",
		},
		{
			name:     "tool-use chunk carries index/id/name/inputjson payload",
			chunk:    &content.ToolUseChunk{Index: 1, ID: "call_1", Name: "read", InputJSON: `{"p":`},
			wantTool: content.ToolUseChunk{Index: 1, ID: "call_1", Name: "read", InputJSON: `{"p":`},
		},
		{
			name:     "tool-use chunk with empty id/name (non-first delta) carries only fragment",
			chunk:    &content.ToolUseChunk{Index: 0, InputJSON: `"x"}`},
			wantTool: content.ToolUseChunk{Index: 0, InputJSON: `"x"}`},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			switch c := tt.chunk.(type) {
			case *content.TextChunk:
				if c.Text != tt.wantText {
					t.Errorf("TextChunk.Text = %q, want %q", c.Text, tt.wantText)
				}
			case *content.ThinkingChunk:
				if c.Thinking != tt.wantThink {
					t.Errorf("ThinkingChunk.Thinking = %q, want %q", c.Thinking, tt.wantThink)
				}
			case *content.ToolUseChunk:
				if *c != tt.wantTool {
					t.Errorf("ToolUseChunk = %+v, want %+v", *c, tt.wantTool)
				}
			default:
				t.Fatalf("unexpected chunk type %T", c)
			}
		})
	}
}

// TestChunk_InterfaceCompliance is a compile-time check that the concrete chunk
// types satisfy the sealed Chunk interface.
// Acceptable exception to the table-driven rule: purely compile-time, no runtime path to branch.
func TestChunk_InterfaceCompliance(t *testing.T) {
	var _ content.Chunk = (*content.TextChunk)(nil)
	var _ content.Chunk = (*content.ThinkingChunk)(nil)
	var _ content.Chunk = (*content.ToolUseChunk)(nil)
}
