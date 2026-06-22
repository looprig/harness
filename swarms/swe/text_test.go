package swe

import (
	"strings"
	"testing"

	"github.com/ciram-co/looprig/pkg/content"
)

// aiMessageText concatenates the text of every *content.TextBlock in m, ignoring
// non-text blocks. A nil message yields the empty string. It is the package's
// final-text projection, used only by tests (this file and the
// integration-tagged operator_eval_integration_test.go) to flatten a turn's
// terminal AIMessage into a plain string. It lives in this untagged _test.go
// helper so it is available to both the default and -tags integration builds.
// Salvaged from the prior coding agent's text_test.go.
func aiMessageText(m *content.AIMessage) string {
	if m == nil {
		return ""
	}
	var b strings.Builder
	for _, blk := range m.Blocks {
		if tb, ok := blk.(*content.TextBlock); ok {
			b.WriteString(tb.Text)
		}
	}
	return b.String()
}

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
