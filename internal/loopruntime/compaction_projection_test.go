package loopruntime

import (
	"reflect"
	"strings"
	"testing"

	"github.com/looprig/core/content"
)

func TestProjectCompactionTranscriptPreservesOrderAndText(t *testing.T) {
	t.Parallel()
	messages := content.AgenticMessages{
		&content.SystemMessage{Message: content.Message{
			Role:   content.RoleSystem,
			Blocks: []content.Block{&content.TextBlock{Text: "system"}},
		}},
		&content.UserMessage{Message: content.Message{
			Role:   content.RoleUser,
			Blocks: []content.Block{&content.TextBlock{Text: "user"}},
		}},
		&content.AIMessage{Message: content.Message{
			Role:   content.RoleAssistant,
			Blocks: []content.Block{&content.TextBlock{Text: "assistant"}},
		}},
		&content.ToolResultMessage{Message: content.Message{
			Role:   content.RoleTool,
			Blocks: []content.Block{&content.TextBlock{Text: "tool result"}},
		}, ToolUseID: "call-1", IsError: true},
	}

	projected, err := projectCompactionTranscript(messages)
	if err != nil {
		t.Fatalf("projectCompactionTranscript() error = %v", err)
	}
	if got, want := len(projected), len(messages); got != want {
		t.Fatalf("projected message count = %d, want %d", got, want)
	}
	wantRoles := []content.Role{content.RoleSystem, content.RoleUser, content.RoleAssistant, content.RoleTool}
	wantText := []string{"system", "user", "assistant", "tool result"}
	for index, message := range projected {
		if role := projectedRole(message); role != wantRoles[index] {
			t.Errorf("projected[%d] role = %q, want %q", index, role, wantRoles[index])
		}
		if text := projectedText(t, message); text != wantText[index] {
			t.Errorf("projected[%d] text = %q, want %q", index, text, wantText[index])
		}
	}
}

func TestProjectCompactionTranscriptTurnsToolUseIntoText(t *testing.T) {
	t.Parallel()
	messages := content.AgenticMessages{&content.AIMessage{Message: content.Message{
		Role: content.RoleAssistant,
		Blocks: []content.Block{
			&content.TextBlock{Text: "before"},
			&content.ToolUseBlock{ID: "call-1", Name: "search", Input: []byte(`{"q":"x"}`)},
			&content.TextBlock{Text: "after"},
		},
	}}}

	projected, err := projectCompactionTranscript(messages)
	if err != nil {
		t.Fatalf("projectCompactionTranscript() error = %v", err)
	}
	if got, want := projectedText(t, projected[0]), "before[called tool: search]after"; got != want {
		t.Fatalf("projected text = %q, want %q", got, want)
	}
}

func TestProjectCompactionTranscriptOmitsOversizedToolInput(t *testing.T) {
	t.Parallel()
	messages := content.AgenticMessages{&content.AIMessage{Message: content.Message{
		Role: content.RoleAssistant,
		Blocks: []content.Block{&content.ToolUseBlock{
			ID: "oversized-call", Name: "search",
			Input: []byte(`{"payload":"` + strings.Repeat("x", 64<<10) + `"}`),
		}},
	}}}

	projected, err := projectCompactionTranscript(messages)
	if err != nil {
		t.Fatalf("projectCompactionTranscript() error = %v", err)
	}
	if got, want := projectedText(t, projected[0]), "[called tool: search]"; got != want {
		t.Fatalf("oversized tool input projection = %q, want %q", got, want)
	}
}

func TestProjectCompactionTranscriptCapsToolResultRunesDeterministically(t *testing.T) {
	t.Parallel()
	const sourceRunes = 2501
	messages := content.AgenticMessages{&content.ToolResultMessage{Message: content.Message{
		Role:   content.RoleTool,
		Blocks: []content.Block{&content.TextBlock{Text: strings.Repeat("界", sourceRunes)}},
	}, ToolUseID: "call-1"}}

	first, err := projectCompactionTranscript(messages)
	if err != nil {
		t.Fatalf("first projection error = %v", err)
	}
	second, err := projectCompactionTranscript(messages)
	if err != nil {
		t.Fatalf("second projection error = %v", err)
	}
	firstText := projectedText(t, first[0])
	secondText := projectedText(t, second[0])
	if firstText != secondText {
		t.Fatalf("tool result projection is not deterministic: %q != %q", firstText, secondText)
	}
	if got := len([]rune(firstText)); got > compactionToolResultRunes {
		t.Fatalf("projected tool result rune count = %d, want <= %d", got, compactionToolResultRunes)
	}
	if !strings.Contains(firstText, "[tool result truncated for compaction") {
		t.Fatalf("projected tool result = %q, want deterministic truncation marker", firstText)
	}
}

func TestProjectCompactionTranscriptCapsNestedOversizedToolResult(t *testing.T) {
	t.Parallel()
	messages := content.AgenticMessages{&content.ToolResultMessage{Message: content.Message{
		Role: content.RoleTool,
		Blocks: []content.Block{
			&content.ToolResultBlock{
				ToolUseID: "nested-call",
				Content:   []content.Block{&content.TextBlock{Text: strings.Repeat("界", 2501)}},
			},
			&content.TextBlock{Text: ` suffix "quoted"`},
		},
	}, ToolUseID: "call-1"}}

	projected, err := projectCompactionTranscript(messages)
	if err != nil {
		t.Fatalf("projectCompactionTranscript() error = %v", err)
	}
	text := projectedText(t, projected[0])
	if got := len([]rune(text)); got > compactionToolResultRunes {
		t.Fatalf("nested tool result projection rune count = %d, want <= %d", got, compactionToolResultRunes)
	}
	if !strings.Contains(text, "[tool result truncated for compaction") {
		t.Fatalf("nested tool result projection = %q, want truncation marker", text)
	}
}

func TestProjectCompactionTranscriptUsesTypedPlaceholders(t *testing.T) {
	t.Parallel()
	messages := content.AgenticMessages{&content.UserMessage{Message: content.Message{
		Role: content.RoleUser,
		Blocks: []content.Block{
			&content.ThinkingBlock{Thinking: "secret", Signature: "sig"},
			&content.ImageBlock{MediaType: content.MediaTypeImagePNG, Source: content.ImageSource{URL: "https://example.test/image"}},
			&content.AudioBlock{MediaType: content.MediaTypeAudioWAV, Data: []byte{1, 2}},
			&content.DocumentBlock{MediaType: content.MediaTypeDocumentText, Name: "notes", Text: "private"},
		},
	}}}

	projected, err := projectCompactionTranscript(messages)
	if err != nil {
		t.Fatalf("projectCompactionTranscript() error = %v", err)
	}
	if got, want := projectedText(t, projected[0]), "[thinking omitted for compaction][image omitted for compaction][audio omitted for compaction][document omitted for compaction]"; got != want {
		t.Fatalf("projected placeholders = %q, want %q", got, want)
	}
}

func TestProjectCompactionTranscriptRejectsMalformedContent(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		messages content.AgenticMessages
	}{
		{name: "nil message", messages: content.AgenticMessages{nil}},
		{name: "typed nil message", messages: content.AgenticMessages{(*content.UserMessage)(nil)}},
		{name: "invalid role", messages: content.AgenticMessages{&content.UserMessage{Message: content.Message{Role: content.RoleAssistant}}}},
		{name: "nil block", messages: content.AgenticMessages{&content.UserMessage{Message: content.Message{Role: content.RoleUser, Blocks: []content.Block{nil}}}}},
		{name: "typed nil block", messages: content.AgenticMessages{&content.UserMessage{Message: content.Message{Role: content.RoleUser, Blocks: []content.Block{(*content.TextBlock)(nil)}}}}},
		{name: "nested typed nil block", messages: content.AgenticMessages{&content.ToolResultMessage{Message: content.Message{Role: content.RoleTool, Blocks: []content.Block{&content.ToolResultBlock{Content: []content.Block{(*content.TextBlock)(nil)}}}}, ToolUseID: "call-1"}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if projected, err := projectCompactionTranscript(tt.messages); err == nil || projected != nil {
				t.Fatalf("projectCompactionTranscript() = %#v, %v, want deterministic malformed-content error", projected, err)
			}
		})
	}
}

func TestProjectCompactionTranscriptDoesNotAliasInput(t *testing.T) {
	t.Parallel()
	usage := &content.Usage{InputTokens: 1, OutputTokens: 2}
	messages := content.AgenticMessages{
		&content.UserMessage{Message: content.Message{Role: content.RoleUser, Blocks: []content.Block{&content.TextBlock{Text: "user"}}}},
		&content.AIMessage{Message: content.Message{Role: content.RoleAssistant, Blocks: []content.Block{&content.TextBlock{Text: "assistant"}}}, Usage: usage},
		&content.ToolResultMessage{Message: content.Message{Role: content.RoleTool, Blocks: []content.Block{&content.TextBlock{Text: "result"}}}, ToolUseID: "call-1", IsError: true},
	}

	projected, err := projectCompactionTranscript(messages)
	if err != nil {
		t.Fatalf("projectCompactionTranscript() error = %v", err)
	}
	if &projected[0] == &messages[0] || reflect.ValueOf(projected[0]).Pointer() == reflect.ValueOf(messages[0]).Pointer() {
		t.Fatal("projected message aliases input message")
	}
	projected[0].(*content.UserMessage).Blocks[0].(*content.TextBlock).Text = "changed"
	if got := messages[0].(*content.UserMessage).Blocks[0].(*content.TextBlock).Text; got != "user" {
		t.Fatalf("input user text mutated through projection: %q", got)
	}
	projected[1].(*content.AIMessage).Usage.OutputTokens = 99
	if got := usage.OutputTokens; got != 2 {
		t.Fatalf("input usage mutated through projection: %d", got)
	}
	projected[2].(*content.ToolResultMessage).ToolUseID = "changed"
	if got := messages[2].(*content.ToolResultMessage).ToolUseID; got != "call-1" {
		t.Fatalf("input tool metadata mutated through projection: %q", got)
	}
}

func projectedRole(message content.Conversation) content.Role {
	switch typed := message.(type) {
	case *content.UserMessage:
		return typed.Role
	case *content.AIMessage:
		return typed.Role
	case *content.SystemMessage:
		return typed.Role
	case *content.ToolResultMessage:
		return typed.Role
	default:
		return ""
	}
}

func projectedText(t *testing.T, message content.Conversation) string {
	t.Helper()
	var blocks []content.Block
	switch typed := message.(type) {
	case *content.UserMessage:
		blocks = typed.Blocks
	case *content.AIMessage:
		blocks = typed.Blocks
	case *content.SystemMessage:
		blocks = typed.Blocks
	case *content.ToolResultMessage:
		blocks = typed.Blocks
	default:
		t.Fatalf("unexpected projected message type %T", message)
	}
	var builder strings.Builder
	for index, block := range blocks {
		text, ok := block.(*content.TextBlock)
		if !ok || text == nil {
			t.Fatalf("projected block[%d] = %T, want non-nil *content.TextBlock", index, block)
		}
		builder.WriteString(text.Text)
	}
	return builder.String()
}
