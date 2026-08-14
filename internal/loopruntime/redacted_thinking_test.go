package loopruntime

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/looprig/core/content"
)

// redactedThinking builds the block shape Anthropic returns for REDACTED
// thinking: the reasoning text is withheld, so Thinking is empty and the whole
// block is the opaque ProviderState blob that must be replayed verbatim on the
// next request. A Gemini thoughtSignature attached to an empty-text part decodes
// to the same shape.
func redactedThinking() *content.ThinkingBlock {
	return content.NewThinkingBlock("", "", json.RawMessage(`{"data":"redacted-blob"}`), "anthropic")
}

// TestIsEmptyAssistantMessageCountsProviderStateAsContent proves a
// redacted-only assistant reply is NOT treated as an empty response. Gating
// solely on Thinking != "" fails a perfectly valid turn with EmptyResponseError
// whenever the model returns only redacted reasoning.
func TestIsEmptyAssistantMessageCountsProviderStateAsContent(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		blocks []content.Block
		want   bool
	}{
		{name: "no blocks is empty", blocks: nil, want: true},
		{
			name:   "zero-length text and thinking is empty",
			blocks: []content.Block{&content.TextBlock{}, &content.ThinkingBlock{}},
			want:   true,
		},
		{
			name:   "thinking text is content",
			blocks: []content.Block{&content.ThinkingBlock{Thinking: "reasoning"}},
			want:   false,
		},
		{
			name:   "redacted thinking is content even with empty Thinking",
			blocks: []content.Block{redactedThinking()},
			want:   false,
		},
		{
			name:   "redacted thinking alongside an empty text block is content",
			blocks: []content.Block{&content.TextBlock{}, redactedThinking()},
			want:   false,
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			aiMsg := &content.AIMessage{Message: content.Message{Role: content.RoleAssistant, Blocks: tt.blocks}}
			if got := isEmptyAssistantMessage(aiMsg, nil); got != tt.want {
				t.Errorf("isEmptyAssistantMessage() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestSanitizeAssistantBlocksKeepsRedactedThinking proves the stored assistant
// message retains a redacted thinking block. Dropping it deletes the only copy
// of the continuation state the provider requires on the next request, and the
// loss is invisible: the turn simply stops replaying reasoning.
func TestSanitizeAssistantBlocksKeepsRedactedThinking(t *testing.T) {
	t.Parallel()

	redacted := redactedThinking()
	tests := []struct {
		name  string
		in    []content.Block
		want  []content.Block
		exact bool
	}{
		{
			name: "empty thinking with no provider state is still dropped",
			in:   []content.Block{&content.ThinkingBlock{}},
			want: []content.Block{},
		},
		{
			name: "redacted thinking survives",
			in:   []content.Block{redacted},
			want: []content.Block{redacted},
		},
		{
			name: "redacted thinking survives beside dropped empty text",
			in:   []content.Block{&content.TextBlock{}, redacted, &content.TextBlock{Text: "answer"}},
			want: []content.Block{redacted, &content.TextBlock{Text: "answer"}},
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := sanitizeAssistantBlocks(tt.in); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("sanitizeAssistantBlocks() = %#v, want %#v", got, tt.want)
			}
		})
	}
}
