package hook

import (
	"encoding/json"
	"fmt"
	"reflect"
	"testing"

	"github.com/looprig/core/content"
	"github.com/looprig/core/content/blocktest"
)

// TestCloneMessagesPreservesEveryExportedBlockField is the hook-payload twin of
// the loop runtime's anti-drift guard. pkg/hook keeps its own block clone (it
// must never import the loop runtime), so the two literals drift independently
// and both dropped ThinkingBlock.ProviderState / ToolUseBlock.ProviderState.
// Reflection-populated fixtures make a newly added core field fail here rather
// than disappear from every hook payload.
func TestCloneMessagesPreservesEveryExportedBlockField(t *testing.T) {
	t.Parallel()

	blocks := blocktest.Blocks(t)
	messages := content.AgenticMessages{
		&content.UserMessage{Message: content.Message{Role: content.RoleUser, Blocks: blocks}},
		&content.AIMessage{
			Message: content.Message{Role: content.RoleAssistant, Blocks: blocks},
			Usage:   &content.Usage{InputTokens: 3, OutputTokens: 5, ReasoningTokens: 7},
		},
		&content.SystemMessage{Message: content.Message{Role: content.RoleSystem, Blocks: blocks}},
		&content.ToolResultMessage{
			Message:   content.Message{Role: content.RoleTool, Blocks: blocks},
			ToolUseID: "call-1",
			IsError:   true,
		},
	}

	cloned := cloneMessages(messages)

	if !reflect.DeepEqual(cloned, messages) {
		t.Fatalf("cloneMessages() dropped or altered a field:\n got %#v\nwant %#v", cloned, messages)
	}
}

// TestCloneBlockPreservesRawMessageShape pins CloneCall's nil-versus-empty
// contract against the block fields backed by json.RawMessage. Core's
// constructors normalize an empty-but-non-nil raw message to nil (and drop a
// provider-state format with no state) for their own invariants; that is a
// normalization, not a copy, and a hook must observe the payload the runtime
// actually holds.
func TestCloneBlockPreservesRawMessageShape(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		block content.Block
	}{
		{
			name:  "tool use with empty non-nil input",
			block: &content.ToolUseBlock{ID: "call", Name: "tool", Input: json.RawMessage{}},
		},
		{
			name: "tool use with empty non-nil provider state",
			block: &content.ToolUseBlock{
				ID: "call", Name: "tool", Input: json.RawMessage(`{}`),
				ProviderState: json.RawMessage{}, ProviderStateFormat: "anthropic",
			},
		},
		{
			name: "thinking with empty non-nil provider state",
			block: &content.ThinkingBlock{
				Thinking: "reasoning", ProviderState: json.RawMessage{}, ProviderStateFormat: "anthropic",
			},
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := cloneBlock(tt.block); !reflect.DeepEqual(got, tt.block) {
				t.Fatalf("cloneBlock() = %#v, want %#v", got, tt.block)
			}
		})
	}
}

// TestCloneBlockPreservesEveryExportedField localizes a completeness failure to
// one block variant and proves the clone owns its own backing arrays.
func TestCloneBlockPreservesEveryExportedField(t *testing.T) {
	t.Parallel()

	for _, want := range blocktest.Blocks(t) {
		t.Run(fmt.Sprintf("%T", want), func(t *testing.T) {
			t.Parallel()
			got := cloneBlock(want)
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("cloneBlock() = %#v, want %#v", got, want)
			}
			blocktest.AssertIndependent(t, want, got)
		})
	}
}
