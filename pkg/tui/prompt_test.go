package tui

import (
	"testing"

	"github.com/ciram-co/looprig/pkg/tool"
)

// TestPromptFromPermission covers building a prompt view-model from a concrete
// sealed PermissionRequest: the ToolName/Description/Scopes are copied straight
// off the request, Kind is promptPermission, and freeText stays false (a
// permission prompt is never free-text).
func TestPromptFromPermission(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		req         tool.PermissionRequest
		wantTool    string
		wantDesc    string
		wantScopes  []tool.ApprovalScope
		wantFreeTxt bool
	}{
		{
			name:        "bash offers once/session/workspace",
			req:         tool.BashRequest{Command: "go build"},
			wantTool:    "Bash",
			wantDesc:    "go build",
			wantScopes:  []tool.ApprovalScope{tool.ScopeOnce, tool.ScopeSession, tool.ScopeWorkspace},
			wantFreeTxt: false,
		},
		{
			name:        "unknown offers only once",
			req:         tool.UnknownRequest{Tool: "Mystery", Summary: "does a thing"},
			wantTool:    "Mystery",
			wantDesc:    "does a thing",
			wantScopes:  []tool.ApprovalScope{tool.ScopeOnce},
			wantFreeTxt: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			id := callID(7)
			p := promptFromPermission(id, tt.req)

			if p.ToolExecutionID != id {
				t.Errorf("ToolExecutionID = %v, want %v", p.ToolExecutionID, id)
			}
			if p.Kind != promptPermission {
				t.Errorf("Kind = %d, want promptPermission (%d)", p.Kind, promptPermission)
			}
			if p.ToolName != tt.wantTool {
				t.Errorf("ToolName = %q, want %q", p.ToolName, tt.wantTool)
			}
			if p.Description != tt.wantDesc {
				t.Errorf("Description = %q, want %q", p.Description, tt.wantDesc)
			}
			if !scopesEqual(p.Scopes, tt.wantScopes) {
				t.Errorf("Scopes = %v, want %v", p.Scopes, tt.wantScopes)
			}
			if p.freeText != tt.wantFreeTxt {
				t.Errorf("freeText = %v, want %v", p.freeText, tt.wantFreeTxt)
			}
		})
	}
}

// TestPromptFromUserInput covers building a user-input prompt view-model: the
// Question/Choices are copied straight off the event, Kind is promptUserInput,
// and freeText is true exactly when there are no choices.
func TestPromptFromUserInput(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		question     string
		choices      []string
		wantFreeText bool
	}{
		{
			name:         "with choices is a choice prompt",
			question:     "pick one",
			choices:      []string{"a", "b"},
			wantFreeText: false,
		},
		{
			name:         "no choices is free-text",
			question:     "what is your name?",
			choices:      nil,
			wantFreeText: true,
		},
		{
			name:         "empty choices slice is free-text",
			question:     "describe it",
			choices:      []string{},
			wantFreeText: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			id := callID(3)
			p := promptFromUserInput(id, tt.question, tt.choices)

			if p.ToolExecutionID != id {
				t.Errorf("ToolExecutionID = %v, want %v", p.ToolExecutionID, id)
			}
			if p.Kind != promptUserInput {
				t.Errorf("Kind = %d, want promptUserInput (%d)", p.Kind, promptUserInput)
			}
			if p.Question != tt.question {
				t.Errorf("Question = %q, want %q", p.Question, tt.question)
			}
			if len(p.Choices) != len(tt.choices) {
				t.Errorf("Choices len = %d, want %d", len(p.Choices), len(tt.choices))
			}
			if p.freeText != tt.wantFreeText {
				t.Errorf("freeText = %v, want %v", p.freeText, tt.wantFreeText)
			}
			if p.selected != 0 {
				t.Errorf("selected = %d, want 0 (head selection)", p.selected)
			}
		})
	}
}

// scopesEqual reports whether two ApprovalScope slices are element-wise equal.
func scopesEqual(a, b []tool.ApprovalScope) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
