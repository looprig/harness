package tui

import "github.com/inventivepotter/urvi/tui/styles"

// Status labels — the legible one-line descriptions of the turn-lifecycle state,
// derived from the session Status plus the live interaction signals.
const (
	labelStreaming    = "streaming…"
	labelThinking     = "thinking…"
	labelApproval     = "awaiting approval"
	labelInput        = "awaiting input"
	labelInterrupting = "interrupting…"
	labelClearing     = "clearing…"
)

// statusInputs carries the live interaction signals the status label is derived
// from, alongside the session Status. It is a plain value (no agent, no mutation)
// so statusLabel stays a pure, table-testable function: the surface fills it from
// the interaction model and live segment before rendering.
type statusInputs struct {
	permissionActive bool // a permission gate is the active prompt
	userInputActive  bool // an AskUser request is the active prompt
	streaming        bool // the live segment has narration text
	thinking         bool // the live segment has only thinking chunks so far
}

// statusLabel derives the active-surface status label (design §"Thinking & status
// line"). A pending prompt takes precedence over the streaming/thinking signals
// (its awaiting-* label is clearer); interrupting/clearing come straight from the
// session status; an idle session (or Running with no live signal) is empty — the
// composer prompt is the cue.
func statusLabel(status Status, in statusInputs) string {
	switch status {
	case StatusInterrupting:
		return labelInterrupting
	case StatusResetting:
		return labelClearing
	case StatusIdle:
		return ""
	}
	switch {
	case in.permissionActive:
		return labelApproval
	case in.userInputActive:
		return labelInput
	case in.streaming:
		return labelStreaming
	case in.thinking:
		return labelThinking
	default:
		return ""
	}
}

// RenderStatusLine returns the one-line status indicator for the given status. It
// derives the label from the session status alone (no live interaction signals), so
// a Running turn reads "thinking…" until the surface — which knows the live segment
// — refines it via renderStatusLine. The empty label renders to "", every other
// label through the faint StatusStyle. Retained for callers holding only the status.
func RenderStatusLine(s Status) string {
	return renderStatusLine(s, statusInputs{thinking: s == StatusRunning})
}

// renderStatusLine styles the derived label, returning "" for the empty (idle) label.
// surfaceView keeps the bottom row regardless — rendering an empty label as a blank
// breathing-room line below the composer — so the composer's position stays stable.
func renderStatusLine(status Status, in statusInputs) string {
	label := statusLabel(status, in)
	if label == "" {
		return ""
	}
	return styles.StatusStyle.Render(label)
}
