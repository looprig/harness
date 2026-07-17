package foreignloop

import (
	"context"

	"github.com/looprig/core/content"
	"github.com/looprig/harness/pkg/foreign"
)

// PermissionPosture is the typed, non-interactive permission mode passed to the
// foreign agent (no raw strings cross the boundary).
type PermissionPosture uint8

const (
	PostureDefault PermissionPosture = iota
	PostureAcceptEdits
)

// SIDMode selects whether the foreign sid is known when the loop is constructed
// or learned later from the agent's first ForeignInit.
type SIDMode uint8

const (
	SIDPrebound SIDMode = iota
	SIDLateBound
)

// EventPublisher is a temporary migration alias for foreign.EventPublisher.
type EventPublisher = foreign.EventPublisher

// ForeignAgent hides everything agent-specific (argv, system-prompt channel,
// stream framing, transcript layout). One implementation now: claude.
type ForeignAgent interface {
	Spawn(ctx context.Context, t ForeignTurn) (ForeignStream, error)
}

// ForeignTurn is one turn's input to an agent. In prebound mode, the sid is set at
// loop creation; StartNew selects --session-id (first turn) vs --resume.
type ForeignTurn struct {
	SystemPrompt string
	ForeignSID   string
	StartNew     bool
	Input        []content.Block
	Cwd          string
	Posture      PermissionPosture
}

// ForeignStream is the live decoded stream plus the deterministic transcript path.
type ForeignStream interface {
	Events() <-chan ForeignEvent
	TranscriptPath() string
	Close() error
}

// ForeignKind is the normalized event-union discriminant.
type ForeignKind uint8

const (
	ForeignInit ForeignKind = iota // carries the confirmed session id
	ForeignTextDelta
	ForeignThinkingDelta
	ForeignToolUse       // ToolUseID (string), ToolName
	ForeignToolResult    // ToolUseID (string), IsError, ResultPreview
	ForeignStepComplete  // an assistant round finished
	ForeignTerminalOK    // result success
	ForeignTerminalError // result error / max-turns
)

// ForeignEvent is the small normalized union both decoders emit and the mapper
// consumes. Only the fields relevant to Kind are set.
type ForeignEvent struct {
	Kind          ForeignKind
	SessionID     string             // ForeignInit
	Text          string             // text/thinking delta
	ToolUseID     string             // tool_use / tool_result
	ToolName      string             // tool_use
	IsError       bool               // tool_result / terminal
	ResultPreview string             // tool_result
	Message       *content.AIMessage // ForeignStepComplete / ForeignTerminalOK (authoritative)
	ErrText       string             // ForeignTerminalError
}

// Builder is a temporary migration alias for foreign.Builder.
type Builder = foreign.Builder

// Spec is the per-agent foreign wiring resolved at the composition root. It is NOT
// on loop.BoundDefinition (that would invert the package dependency).
type Spec struct {
	Agent    ForeignAgent
	ExecPath string
	Cwd      string
	Posture  PermissionPosture
	SIDMode  SIDMode
	Env      []string // whitelisted child environment (NOT os.Environ())
}
