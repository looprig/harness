package tui

import (
	"github.com/inventivepotter/urvi/internal/uuid"
)

// ToolStatus is the lifecycle state of a tool call rendered in the transcript.
type ToolStatus uint8

const (
	ToolRunning   ToolStatus = iota // started, no completion seen yet
	ToolOK                          // completed without error
	ToolError                       // completed with an error
	ToolCancelled                   // turn interrupted while the call was still running
)

// ToolCallView is one tool call rendered as a child of its assistant segment. It
// is reconstructed from the turn event stream (ToolCallStarted / ToolCallCompleted),
// correlated by CallID.
type ToolCallView struct {
	CallID   uuid.UUID
	ToolName string     // ToolCallStarted.ToolName
	Summary  string     // ToolCallStarted.Summary (already redacted, one line)
	Status   ToolStatus // lifecycle state
	Result   []string   // capped preview lines from ToolCallCompleted; nil while running
}
