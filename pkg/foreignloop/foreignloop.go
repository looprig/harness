package foreignloop

import (
	"context"

	"github.com/looprig/harness/pkg/content"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/core/uuid"
)

// PermissionPosture is the typed, non-interactive permission mode passed to the
// foreign agent (no raw strings cross the boundary).
type PermissionPosture uint8

const (
	PostureDefault PermissionPosture = iota
	PostureAcceptEdits
)

// EventPublisher is the foreign loop's narrow consumer of the session event
// fan-in. Defined here (exported) because the native loop's equivalent is
// unexported; *session.Session satisfies it via PublishEvent.
type EventPublisher interface {
	PublishEvent(context.Context, event.Event) error
}

// ForeignAgent hides everything agent-specific (argv, system-prompt channel,
// stream framing, transcript layout). One implementation now: claude.
type ForeignAgent interface {
	Spawn(ctx context.Context, t ForeignTurn) (ForeignStream, error)
}

// ForeignTurn is one turn's input to an agent. The sid is ALWAYS set (minted at
// loop creation); StartNew selects --session-id (first turn) vs --resume.
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

// Builder is the composition-root seam Session uses to construct a foreign loop.
// EventPublisher is foreignloop.EventPublisher; returns the Backend and the minted
// ForeignSID (stamped onto LoopStarted by the caller).
type Builder func(loopCtx context.Context, sessionID, loopID uuid.UUID,
	parent loop.Provenance, pub EventPublisher, cfg loop.Config,
	idGen func() (uuid.UUID, error), fac *event.Factory) (loop.Backend, string, error)

// Spec is the per-agent foreign wiring resolved at the composition root. It is NOT
// on loop.Config (that would invert the package dependency).
type Spec struct {
	Agent    ForeignAgent
	ExecPath string
	Cwd      string
	Posture  PermissionPosture
	Env      []string // whitelisted child environment (NOT os.Environ())
}
