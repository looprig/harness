// Package transcript reconstructs a human-readable session model from the
// journaled record stream (events and user commands) and is the pure data layer
// an HTML export renders. It depends only on looprig's pure data packages
// (content, event, command, tool, uuid) — never on storage — so the model can be
// built anywhere the records can be read.
package transcript

import (
	"encoding/json"
	"time"

	"github.com/ciram-co/looprig/pkg/content"
	"github.com/ciram-co/looprig/pkg/event"
	"github.com/ciram-co/looprig/pkg/tool"
	"github.com/ciram-co/looprig/pkg/uuid"
)

// Outcome is the terminal state of a Turn. The zero value is OutcomeRunning: a
// turn opened but not yet closed at the snapshot edge is, by construction, still
// running.
type Outcome uint8

const (
	// OutcomeRunning is a turn with no terminal event yet (the zero value).
	OutcomeRunning Outcome = iota
	// OutcomeDone is a turn that completed normally (TurnDone).
	OutcomeDone
	// OutcomeFailed is a turn that ended in error (TurnFailed); see Turn.Err.
	OutcomeFailed
	// OutcomeInterrupted is a turn cut short by the user (TurnInterrupted).
	OutcomeInterrupted
)

// GateKind distinguishes the two kinds of human-in-the-loop gate a tool call can
// raise.
type GateKind uint8

const (
	// GateKindPermission is a permission-approval gate (PermissionRequested).
	GateKindPermission GateKind = iota
	// GateKindAskUser is an ask-the-user gate (UserInputRequested).
	GateKindAskUser
)

// Decision is the resolution of a GateAction. The zero value is DecisionPending:
// a gate raised but not yet resolved at the snapshot edge is still pending.
type Decision uint8

const (
	// DecisionPending is a gate with no resolving command yet (the zero value).
	DecisionPending Decision = iota
	// DecisionApproved is a permission gate the user approved.
	DecisionApproved
	// DecisionDenied is a permission gate the user denied.
	DecisionDenied
	// DecisionAnswered is an ask-user gate the user answered.
	DecisionAnswered
)

// NoticeKind classifies a session-lifecycle or out-of-band Notice.
type NoticeKind uint8

const (
	// NoticeSessionActive marks the session's Idle -> Active edge.
	NoticeSessionActive NoticeKind = iota
	// NoticeSessionIdle marks the session's Active -> Idle edge.
	NoticeSessionIdle
	// NoticeSessionStopped marks the session's shutdown.
	NoticeSessionStopped
	// NoticeRestoreStarted marks the start of a journal restore.
	NoticeRestoreStarted
	// NoticeRestoreDone marks a successful journal restore.
	NoticeRestoreDone
	// NoticeRestoreErrored marks a failed journal restore.
	NoticeRestoreErrored
)

// Session is the reconstructed root of a transcript: one agentic session and its
// loop tree, plus the lifecycle notices and reconstruction warnings surfaced into
// the rendered document.
type Session struct {
	SessionID  uuid.UUID
	Title      string
	Config     Config
	StartedAt  time.Time
	EndedAt    time.Time
	ExportedAt time.Time
	Root       *Loop
	Notices    []Notice
	Warnings   []Warning
}

// Config is the configuration fingerprint the session ran under, projected from
// the SessionStarted event's event.ConfigFingerprint.
type Config struct {
	ModelID           string
	AgentKind         string
	PermissionPosture string
	SystemPromptRev   string
}

// Loop is one agent loop in the session tree: the primary (ParentToolUseID == "")
// or a subagent spawned by a tool call. Its Turns are in journal order.
type Loop struct {
	LoopID          uuid.UUID
	AgentName       string
	ParentToolUseID string
	// SystemPrompt is the loop's resolved live system-prompt text (Decision 4). It is
	// "" when the prompt was unavailable (a restored session whose live config is gone);
	// in that case a Session.Warnings entry, keyed by this loop's id, records the gap.
	SystemPrompt string
	StartedAt    time.Time
	Turns        []*Turn
}

// Turn is one user-input-to-resolution cycle within a Loop, numbered per loop by
// Index. Outcome and Err record how it terminated (or that it is still running).
type Turn struct {
	Index     event.TurnIndex
	StartedAt time.Time
	EndedAt   time.Time
	User      *Message
	Steps     []*Step
	Outcome   Outcome
	// Err is the TurnFailed error text. It is empty on replayed (journal) records
	// because event.TurnFailed.Err is json:"-": it is only populated from an
	// in-memory event value, never reconstructed from the journal.
	Err string
}

// Step is one model step within a Turn: the AI message and the tool calls it
// requested, paired to their results.
type Step struct {
	StepID uuid.UUID
	AI     *Message
	Tools  []*ToolCall
	// Gates are every human-in-the-loop gate raised during this step, in arrival
	// order. A gate bound to one of this step's tool calls is ALSO referenced by
	// that ToolCall.Gate (the same pointer); an unbound gate lives only here.
	Gates []*GateAction
}

// ToolCall is a single tool invocation: the request, its paired result, and the
// optional gate (if it required approval / asked the user) or child loop (if it
// spawned a subagent).
type ToolCall struct {
	ToolUseID string
	Name      string
	Input     json.RawMessage
	Result    []content.Block
	IsError   bool
	At        time.Time
	Gate      *GateAction
	Child     *Loop
}

// GateAction is the human-in-the-loop interaction attached to a ToolCall: a
// permission approval or an ask-user prompt, and its resolution.
type GateAction struct {
	Kind     GateKind
	Decision Decision
	// Scope is meaningful only when Kind == GateKindPermission. An AskUser gate's
	// zero Scope is not significant, so callers must branch on Kind before reading
	// it.
	Scope tool.ApprovalScope
	// ToolName is the requesting tool's name, recovered from the durable
	// PermissionRequest (e.g. "Bash"). It is the key a permission gate binds to its
	// ToolCall by; empty for an AskUser gate.
	ToolName string
	// Description is the redacted approval-prompt body from the PermissionRequest
	// (never raw args). Empty for an AskUser gate.
	Description string
	Question    string
	Choices     []string
	Answer      string
	// ToolUseID is the content.ToolUseBlock.ID of the tool call this gate bound to,
	// or "" if the gate bound to no call (it then renders as a bare notification).
	ToolUseID string
	// OpenedAt is when the gate was raised (the PermissionRequested /
	// UserInputRequested event's CreatedAt).
	OpenedAt time.Time
	// DecidedAt is when the resolving user command landed; the zero time while the
	// gate is still pending.
	DecidedAt time.Time
}

// Message is a single conversation message: a role, its content blocks, and when
// it was recorded.
type Message struct {
	Role   content.Role
	Blocks []content.Block
	At     time.Time
}

// Notice is a session-lifecycle or out-of-band event surfaced into the rendered
// document, in journal order.
type Notice struct {
	Kind NoticeKind
	Text string
	At   time.Time
}

// Warning is a reconstruction anomaly (an unpaired, malformed, or unexpected
// record) surfaced into the rendered document rather than failing the build.
type Warning struct {
	Text string
	At   time.Time
}
