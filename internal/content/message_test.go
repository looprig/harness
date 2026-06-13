package content_test

import (
	"testing"

	"github.com/inventivepotter/urvi/internal/content"
)

// TestConversation_InterfaceCompliance is a compile-time check — no runtime assertions needed.
// Acceptable exception to the table-driven rule: purely compile-time, no runtime path to branch.
func TestConversation_InterfaceCompliance(t *testing.T) {
	var _ content.Conversation = (*content.UserMessage)(nil)
	var _ content.Conversation = (*content.AIMessage)(nil)
	var _ content.Conversation = (*content.SystemMessage)(nil)
	var _ content.Conversation = (*content.ToolMessage)(nil)
}

func TestRole_Constants(t *testing.T) {
	tests := []struct {
		name     string
		role     content.Role
		wantStr  string
	}{
		{name: "user role string value", role: content.RoleUser, wantStr: "user"},
		{name: "assistant role string value", role: content.RoleAssistant, wantStr: "assistant"},
		{name: "system role string value", role: content.RoleSystem, wantStr: "system"},
		{name: "tool role string value", role: content.RoleTool, wantStr: "tool"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if string(tt.role) != tt.wantStr {
				t.Errorf("Role = %q, want %q", tt.role, tt.wantStr)
			}
		})
	}
}

func TestMessage_EmbeddedAccess(t *testing.T) {
	block := &content.Block{Type: content.TypeText, Text: &content.TextBlock{Text: "hello"}}

	tests := []struct {
		name      string
		wantRole  content.Role
		wantBlock *content.Block
		getMsg    func() interface {
			GetRole() content.Role
			GetBlocks() []*content.Block
		}
	}{
		{
			name:      "UserMessage carries role and blocks",
			wantRole:  content.RoleUser,
			wantBlock: block,
			getMsg: func() interface {
				GetRole() content.Role
				GetBlocks() []*content.Block
			} {
				return &userMsgAccessor{&content.UserMessage{
					Message: content.Message{Role: content.RoleUser, Blocks: []*content.Block{block}},
				}}
			},
		},
		{
			name:      "AIMessage carries role and blocks",
			wantRole:  content.RoleAssistant,
			wantBlock: block,
			getMsg: func() interface {
				GetRole() content.Role
				GetBlocks() []*content.Block
			} {
				return &aiMsgAccessor{&content.AIMessage{
					Message: content.Message{Role: content.RoleAssistant, Blocks: []*content.Block{block}},
				}}
			},
		},
		{
			name:      "SystemMessage carries role and blocks",
			wantRole:  content.RoleSystem,
			wantBlock: block,
			getMsg: func() interface {
				GetRole() content.Role
				GetBlocks() []*content.Block
			} {
				return &sysMsgAccessor{&content.SystemMessage{
					Message: content.Message{Role: content.RoleSystem, Blocks: []*content.Block{block}},
				}}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			m := tt.getMsg()
			if m.GetRole() != tt.wantRole {
				t.Errorf("Role = %q, want %q", m.GetRole(), tt.wantRole)
			}
			blocks := m.GetBlocks()
			if len(blocks) != 1 || blocks[0] != tt.wantBlock {
				t.Errorf("Blocks = %v, want [%v]", blocks, tt.wantBlock)
			}
		})
	}
}

// Thin accessor wrappers used only in tests — they delegate to the embedded Message
// so we can test field promotion without using reflection.
type userMsgAccessor struct{ m *content.UserMessage }

func (a *userMsgAccessor) GetRole() content.Role        { return a.m.Role }
func (a *userMsgAccessor) GetBlocks() []*content.Block  { return a.m.Blocks }

type aiMsgAccessor struct{ m *content.AIMessage }

func (a *aiMsgAccessor) GetRole() content.Role        { return a.m.Role }
func (a *aiMsgAccessor) GetBlocks() []*content.Block  { return a.m.Blocks }

type sysMsgAccessor struct{ m *content.SystemMessage }

func (a *sysMsgAccessor) GetRole() content.Role        { return a.m.Role }
func (a *sysMsgAccessor) GetBlocks() []*content.Block  { return a.m.Blocks }

func TestToolMessage_Fields(t *testing.T) {
	block := &content.Block{Type: content.TypeToolResult, ToolResult: &content.ToolResultBlock{ToolUseID: "tu_1"}}

	tests := []struct {
		name          string
		msg           *content.ToolMessage
		wantRole      content.Role
		wantToolUseID string
		wantBlockLen  int
	}{
		{
			name: "ToolMessage has embedded Message and ToolUseID",
			msg: &content.ToolMessage{
				Message:   content.Message{Role: content.RoleTool, Blocks: []*content.Block{block}},
				ToolUseID: "tu_1",
			},
			wantRole:      content.RoleTool,
			wantToolUseID: "tu_1",
			wantBlockLen:  1,
		},
		{
			name: "ToolMessage with no blocks",
			msg: &content.ToolMessage{
				Message:   content.Message{Role: content.RoleTool},
				ToolUseID: "tu_2",
			},
			wantRole:      content.RoleTool,
			wantToolUseID: "tu_2",
			wantBlockLen:  0,
		},
		{
			name: "empty ToolUseID",
			msg: &content.ToolMessage{
				Message:   content.Message{Role: content.RoleTool},
				ToolUseID: "",
			},
			wantRole:      content.RoleTool,
			wantToolUseID: "",
			wantBlockLen:  0,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if tt.msg.Role != tt.wantRole {
				t.Errorf("Role = %q, want %q", tt.msg.Role, tt.wantRole)
			}
			if tt.msg.ToolUseID != tt.wantToolUseID {
				t.Errorf("ToolUseID = %q, want %q", tt.msg.ToolUseID, tt.wantToolUseID)
			}
			if len(tt.msg.Blocks) != tt.wantBlockLen {
				t.Errorf("len(Blocks) = %d, want %d", len(tt.msg.Blocks), tt.wantBlockLen)
			}
		})
	}
}

func TestAgenticMessages_MixedTypes(t *testing.T) {
	textBlock := &content.Block{Type: content.TypeText, Text: &content.TextBlock{Text: "prompt"}}
	toolBlock := &content.Block{Type: content.TypeToolResult, ToolResult: &content.ToolResultBlock{ToolUseID: "tu_1"}}

	tests := []struct {
		name     string
		thread   content.AgenticMessages
		wantLen  int
	}{
		{
			name:    "nil AgenticMessages is valid zero value",
			thread:  nil,
			wantLen: 0,
		},
		{
			name:    "empty AgenticMessages is valid",
			thread:  content.AgenticMessages{},
			wantLen: 0,
		},
		{
			name: "mixed conversation holds all four message types",
			thread: content.AgenticMessages{
				&content.SystemMessage{Message: content.Message{Role: content.RoleSystem, Blocks: []*content.Block{textBlock}}},
				&content.UserMessage{Message: content.Message{Role: content.RoleUser, Blocks: []*content.Block{textBlock}}},
				&content.AIMessage{Message: content.Message{Role: content.RoleAssistant, Blocks: []*content.Block{textBlock}}},
				&content.ToolMessage{Message: content.Message{Role: content.RoleTool, Blocks: []*content.Block{toolBlock}}, ToolUseID: "tu_1"},
			},
			wantLen: 4,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if len(tt.thread) != tt.wantLen {
				t.Errorf("len(AgenticMessages) = %d, want %d", len(tt.thread), tt.wantLen)
			}
		})
	}
}

func TestMessage_NilBlocks(t *testing.T) {
	tests := []struct {
		name   string
		msg    content.Message
		isNil  bool
	}{
		{
			name:  "Message with nil Blocks is a valid zero value",
			msg:   content.Message{Role: content.RoleUser},
			isNil: true,
		},
		{
			name:  "Message with non-nil empty Blocks slice",
			msg:   content.Message{Role: content.RoleUser, Blocks: []*content.Block{}},
			isNil: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if (tt.msg.Blocks == nil) != tt.isNil {
				t.Errorf("Blocks nil = %v, want %v", tt.msg.Blocks == nil, tt.isNil)
			}
		})
	}
}

func TestSystemMessage_SingleTextBlock(t *testing.T) {
	tests := []struct {
		name     string
		text     string
		wantText string
	}{
		{
			name:     "system prompt scenario with single text block",
			text:     "You are a helpful assistant.",
			wantText: "You are a helpful assistant.",
		},
		{
			name:     "empty system prompt text",
			text:     "",
			wantText: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			msg := &content.SystemMessage{
				Message: content.Message{
					Role: content.RoleSystem,
					Blocks: []*content.Block{
						{Type: content.TypeText, Text: &content.TextBlock{Text: tt.text}},
					},
				},
			}
			if len(msg.Blocks) != 1 {
				t.Fatalf("len(Blocks) = %d, want 1", len(msg.Blocks))
			}
			got := msg.Blocks[0].Text.Text
			if got != tt.wantText {
				t.Errorf("Text = %q, want %q", got, tt.wantText)
			}
		})
	}
}
