package coding

import (
	"testing"

	"github.com/inventivepotter/urvi/internal/content"
)

// TestAIMessageText proves the projection helper concatenates only text blocks,
// ignores non-text blocks, and tolerates a nil message.
func TestAIMessageText(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		msg  *content.AIMessage
		want string
	}{
		{name: "nil message", msg: nil, want: ""},
		{
			name: "single text block",
			msg:  &content.AIMessage{Message: content.Message{Blocks: []content.Block{&content.TextBlock{Text: "hi"}}}},
			want: "hi",
		},
		{
			name: "multiple text blocks concatenate",
			msg:  &content.AIMessage{Message: content.Message{Blocks: []content.Block{&content.TextBlock{Text: "he"}, &content.TextBlock{Text: "llo"}}}},
			want: "hello",
		},
		{
			name: "non-text blocks ignored",
			msg:  &content.AIMessage{Message: content.Message{Blocks: []content.Block{&content.ThinkingBlock{Thinking: "secret"}, &content.TextBlock{Text: "ok"}}}},
			want: "ok",
		},
		{
			name: "no blocks",
			msg:  &content.AIMessage{Message: content.Message{Blocks: nil}},
			want: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := aiMessageText(tt.msg); got != tt.want {
				t.Errorf("aiMessageText() = %q, want %q", got, tt.want)
			}
		})
	}
}
