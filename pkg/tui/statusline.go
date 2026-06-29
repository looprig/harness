package tui

import "github.com/ciram-co/looprig/pkg/tui/styles"

// Status labels — the legible one-line descriptions of the turn-lifecycle state,
// derived from the session Status plus the live interaction signals.
const (
	labelIdle         = "idle"
	labelWaiting      = "waiting…"
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
// status; an idle session reads "idle". A Running turn progresses through the live
// signals: "waiting…" the moment it starts (request in flight, no token back yet) →
// "thinking…" once thinking chunks arrive → "streaming…" once narration text streams.
// statusLabel never returns "".
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
	case in.thinking:
		return labelThinking
	default:
		// Running, but nothing has streamed back yet — the request is in flight.
		return labelWaiting
	}
}

// RenderStatusLine returns the one-line status indicator for the given status. It
// derives the label from the session status alone (no live interaction signals), so
// a Running turn reads "thinking…" until the surface — which knows the live segment
// — refines it via renderStatusLine. The empty label renders to "", every other
// label as a (here static, phase-0) lime↔blue gradient. Retained for callers holding
// only the status.
func RenderStatusLine(s Status) string {
	return renderStatusLine(s, statusInputs{thinking: s == StatusRunning}, 0)
}

// Status-line dot glyphs: a hollow ring at rest, a filled dot while a turn is live.
const (
	dotHollow = "○"
	dotFilled = "●"
)

// dotGradientPos is the dot's sample position on the gradient wave, in label-glyph columns:
// the dot sits two columns left of label glyph 0 (the dot itself plus the separating
// space), so sampling at −2 makes the dot ride the same flowing band as the label — one
// continuous gradient across "● <label>".
const dotGradientPos = -2

// renderTip renders the rotating educational hint as a faint "Tips: …" line below the
// status row, or "" when there is no tip (so the surface omits the row).
func renderTip(tip string) string {
	if tip == "" {
		return ""
	}
	return styles.StatusStyle.Render("Tips: " + tip)
}

// renderStatusLine renders the derived label as an animated lime↔blue gradient
// (gradientLabel), prefixed by the status dot, which rides the same gradient (see
// statusDot). phase is the live animation frame that flows both the label and the dot
// (0 at rest → a static gradient). statusLabel always returns a non-empty label (idle
// reads "idle"), so the status row is always present above the composer; the empty-label
// guard is a defensive no-op.
func renderStatusLine(status Status, in statusInputs, phase uint) string {
	label := statusLabel(status, in)
	if label == "" {
		return ""
	}
	return statusDot(status, phase) + " " + gradientLabel(label, phase)
}

// statusDot renders the leading status dot, colored by the flowing gradient so it sweeps
// lime↔blue in step with the label (replacing the old lime/white pulse). The glyph is a
// hollow ring (○) at rest and a filled dot (●) once a turn is live; both ride the gradient
// at dotGradientPos, animating with phase while a turn runs and holding a static gradient
// at rest. The Running sub-state (waiting / thinking / streaming / blocked-on-a-prompt) is
// carried by the label, not the dot, so the dot needs only the status to pick its glyph.
func statusDot(status Status, phase uint) string {
	glyph := dotFilled
	if status == StatusIdle {
		glyph = dotHollow
	}
	return gradientGlyph(glyph, dotGradientPos, phase)
}
