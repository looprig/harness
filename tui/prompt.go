package tui

import (
	"github.com/inventivepotter/urvi/internal/tool"
	"github.com/inventivepotter/urvi/internal/uuid"
)

// promptKind tags a prompt as a permission gate or a user-input request. Each is
// rendered and routed differently, so the kind is carried explicitly rather than
// inferred from which fields are populated.
type promptKind uint8

const (
	// promptPermission is a tool-call approval gate: the user approves (at a
	// scope) or denies.
	promptPermission promptKind = iota
	// promptUserInput is an AskUser request: the user picks a choice or types a
	// free-text answer.
	promptUserInput
)

// prompt is the interaction layer's view-model for one pending request, keyed by
// the gate's CallID. It carries everything the renderer needs and the selection
// state the modal key router (Task 8) mutates — but holds NO agent reference: the
// interactionModel only PRODUCES a uiAction; Screen drives the agent. A permission
// prompt uses ToolName/Description/Scopes; a user-input prompt uses
// Question/Choices/selected/freeText.
type prompt struct {
	CallID      uuid.UUID
	Kind        promptKind
	ToolName    string               // promptPermission: approval-prompt header
	Description string               // promptPermission: approval-prompt body (redacted)
	Scopes      []tool.ApprovalScope // promptPermission: scopes the request allows
	Question    string               // promptUserInput: the AskUser question
	Choices     []string             // promptUserInput: selectable choices (nil → free-text)
	selected    int                  // promptUserInput: cursor over Choices (or Scopes for permission)
	freeText    bool                 // promptUserInput: true when there are no Choices
}

// promptFromPermission builds a permission prompt view-model from a sealed
// PermissionRequest. ToolName/Description/Scopes are read off the request via its
// interface methods, so any concrete request type (Bash, FileWrite, Unknown, …)
// projects uniformly. freeText is false: a permission gate is never free-text.
func promptFromPermission(callID uuid.UUID, req tool.PermissionRequest) prompt {
	return prompt{
		CallID:      callID,
		Kind:        promptPermission,
		ToolName:    req.ToolName(),
		Description: req.Description(),
		Scopes:      req.AllowedScopes(),
	}
}

// promptFromUserInput builds a user-input prompt view-model. freeText is true
// exactly when there are no choices (an empty or nil slice), in which case the
// user types an answer rather than picking one.
func promptFromUserInput(callID uuid.UUID, question string, choices []string) prompt {
	return prompt{
		CallID:   callID,
		Kind:     promptUserInput,
		Question: question,
		Choices:  choices,
		freeText: len(choices) == 0,
	}
}
