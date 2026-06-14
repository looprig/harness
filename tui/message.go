package tui

import "github.com/inventivepotter/urvi/internal/content"

// DisplayRole identifies the source/kind of a display row. TUI-only; no content/loop analog.
type DisplayRole uint8

const (
	RoleUser DisplayRole = iota
	RoleAssistant
	RoleSystem
	RoleError
	RoleInterrupted // tombstone — Blocks is nil
)

// DisplayMessage is one row of the TUI transcript. One uniform Blocks field for
// every role; the renderer type-switches on each block's concrete type.
type DisplayMessage struct {
	Role   DisplayRole
	Blocks []content.Block
}
