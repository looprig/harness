package tui

import "github.com/inventivepotter/urvi/tui/styles"

// Status labels — the legible one-line descriptions of the turn-lifecycle state,
// derived from the session Status plus the live interaction signals.
const (
	labelIdle         = "idle"
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
// line"). A pending prompt takes precedence over the streaming/thinking signals (its
// awaiting-* label is clearer); interrupting/clearing come straight from the session
// status; an idle session reads "idle". A Running turn with no live signal yet — the
// request is in flight but nothing has streamed back — reads "thinking…" (the same as
// when only thinking chunks have arrived), so the gap between submitting and the first
// token is never blank. statusLabel never returns "".
func statusLabel(status Status, in statusInputs) string {
	switch status {
	case StatusInterrupting:
		return labelInterrupting
	case StatusResetting:
		return labelClearing
	case StatusIdle:
		return labelIdle
	}
	switch {
	case in.permissionActive:
		return labelApproval
	case in.userInputActive:
		return labelInput
	case in.streaming:
		return labelStreaming
	default:
		// Thinking chunks present, OR a freshly-started turn still waiting for the
		// model's first token — both read as "thinking…".
		return labelThinking
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

// statusIcon is the small play glyph prefixing the status line.
const statusIcon = "▸"

// renderStatusLine styles the derived label through the faint StatusStyle, prefixed by
// the small play icon. statusLabel always returns a non-empty label (idle reads
// "idle"), so the status row is always present below the composer; the empty-label
// guard is a defensive no-op.
func renderStatusLine(status Status, in statusInputs) string {
	label := statusLabel(status, in)
	if label == "" {
		return ""
	}
	return styles.StatusStyle.Render(statusIcon + " " + label)
}
