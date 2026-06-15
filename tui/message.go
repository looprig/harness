package tui

import (
	"github.com/inventivepotter/urvi/internal/content"
	"github.com/inventivepotter/urvi/internal/uuid"
)

// DisplayRole identifies the source/kind of a display row. TUI-only; no content/loop analog.
type DisplayRole uint8

const (
	RoleUser DisplayRole = iota
	RoleAssistant
	RoleSystem
	RoleError
	RoleInterrupted // tombstone — Blocks is nil
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

// DisplayMessage is one row of the TUI transcript. One uniform Blocks field for
// every role; the renderer type-switches on each block's concrete type. ToolCalls
// holds the tool-call children of an assistant segment; it is populated only for
// RoleAssistant rows and is nil by default for every other role.
type DisplayMessage struct {
	Role      DisplayRole
	Blocks    []content.Block
	ToolCalls []ToolCallView // children; populated only for RoleAssistant segments
}
