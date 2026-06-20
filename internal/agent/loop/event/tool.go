package event

import (
	"github.com/inventivepotter/urvi/internal/tool"
	"github.com/inventivepotter/urvi/internal/uuid"
)

// PermissionRequested is emitted when a tool call needs interactive approval.
// The per-turn stream (TUI) renders Request.Description() — which can hold a Bash
// command, a file-diff preview, or a URL — so it gets the full Request.
type PermissionRequested struct {
	enduring
	loopScoped
	Header
	CallID  uuid.UUID
	Request tool.PermissionRequest
}

// UserInputRequested is emitted when a tool (AskUser) needs free-form input. The
// per-turn stream gets the full Question and Choices for rendering.
type UserInputRequested struct {
	enduring
	loopScoped
	Header
	CallID   uuid.UUID
	Question string
	Choices  []string
}

// ToolCallStarted is emitted when an approved tool begins executing. Summary is
// capped at construction (never raw args).
type ToolCallStarted struct {
	ephemeral
	loopScoped
	Header
	CallID   uuid.UUID
	ToolName string
	Summary  string
}

// ToolCallCompleted is emitted when a tool finishes. ResultPreview is the capped
// tool output for the TUI.
type ToolCallCompleted struct {
	ephemeral
	loopScoped
	Header
	CallID        uuid.UUID
	IsError       bool
	ResultPreview string
}

func (PermissionRequested) isEvent() {}
func (UserInputRequested) isEvent()  {}
func (ToolCallStarted) isEvent()     {}
func (ToolCallCompleted) isEvent()   {}
