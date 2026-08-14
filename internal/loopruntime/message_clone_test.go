package loopruntime

import (
	"reflect"
	"testing"

	"github.com/looprig/core/content"
	"github.com/looprig/core/content/blocktest"
)

// TestCloneMessagesPreservesEveryExportedBlockField is the anti-drift guard on
// cloneMessages. The block copy itself now belongs to content.CloneBlocks and
// is guarded there; what this covers is the message layer above it — the
// conversation type switch and the per-message fields — which still enumerates
// its variants here.
//
// A field added to core's content package is dropped SILENTLY by a hand-written
// struct literal: the literal still compiles and every field-named test still
// passes. That is how ThinkingBlock.ProviderState and ToolUseBlock.ProviderState
// — the provider-private reasoning state that makes signature replay possible —
// were lost on all three load-bearing paths (request build, durable commit,
// restore seed) at once.
//
// The fixtures are populated by reflection (core/content/blocktest), so a new core
// field is carried into this test automatically and fails it loudly here
// instead of vanishing in production.
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

// TestCloneMessagesOwnsItsBytes proves the message clone hands back memory of
// its own, not just equal fields: a struct copy compares equal while still
// aliasing the original's backing arrays. Per-variant field completeness and
// independence are proved once, in content's own tests; what has to be checked
// here is that the message layer actually routes through that copy.
func TestCloneMessagesOwnsItsBytes(t *testing.T) {
	t.Parallel()

	blocks := blocktest.Blocks(t)
	messages := content.AgenticMessages{
		&content.AIMessage{Message: content.Message{Role: content.RoleAssistant, Blocks: blocks}},
	}

	cloned := cloneMessages(messages)

	original := messages[0].(*content.AIMessage)
	duplicate := cloned[0].(*content.AIMessage)
	if original == duplicate {
		t.Fatal("cloneMessages() returned the original message pointer")
	}
	for index := range blocks {
		blocktest.AssertIndependent(t, original.Blocks[index], duplicate.Blocks[index])
	}
}
