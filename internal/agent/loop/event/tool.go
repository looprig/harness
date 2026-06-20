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
	ToolExecutionID uuid.UUID `json:"tool_execution_id,omitzero"`
	// Request is a sealed interface (tool.PermissionRequest) with no generic JSON
	// codec or type discriminator, so a journal cannot round-trip it (the common
	// non-nil case would marshal to lossy, un-keyed PascalCase). It is excluded from
	// serialization entirely — like TokenDelta.Chunk and TurnFailed.Err — and stays
	// an in-memory-only field for the TUI to render.
	Request tool.PermissionRequest `json:"-"`
}

// UserInputRequested is emitted when a tool (AskUser) needs free-form input. The
// per-turn stream gets the full Question and Choices for rendering.
type UserInputRequested struct {
	enduring
	loopScoped
	Header
	ToolExecutionID uuid.UUID `json:"tool_execution_id,omitzero"`
	Question        string    `json:"question,omitempty"`
	Choices         []string  `json:"choices,omitempty"`
}

// ToolCallStarted is emitted when an approved tool begins executing. Summary is
// capped at construction (never raw args).
type ToolCallStarted struct {
	ephemeral
	loopScoped
	Header
	ToolExecutionID uuid.UUID `json:"tool_execution_id,omitzero"`
	ToolName        string    `json:"tool_name,omitempty"`
	Summary         string    `json:"summary,omitempty"`
}

// ToolCallCompleted is emitted when a tool finishes. ResultPreview is the capped
// tool output for the TUI.
type ToolCallCompleted struct {
	ephemeral
	loopScoped
	Header
	ToolExecutionID uuid.UUID `json:"tool_execution_id,omitzero"`
	IsError         bool      `json:"is_error,omitzero"`
	ResultPreview   string    `json:"result_preview,omitempty"`
}

func (PermissionRequested) isEvent() {}
func (UserInputRequested) isEvent()  {}
func (ToolCallStarted) isEvent()     {}
func (ToolCallCompleted) isEvent()   {}
