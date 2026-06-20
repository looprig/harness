package tui

import (
	"strings"

	"charm.land/lipgloss/v2"
)

// Active-surface row budget (design §"Input box · Active-surface budgeting"). The
// surface is the only managed region in scrollback-first mode: the terminal owns
// history, so there is no transcript viewport — only a capped live tail, a
// separator rule, the bottom box, an optional slash panel, and one status line.
const (
	statusH    = 1 // the single status line at the very bottom
	statusPadH = 1 // a blank line between the bottom box and the status line
	boxBorderH = 2 // the bottom box's top + bottom frame rows (a prompt box's border; the minimal composer has none, so this over-reserves harmlessly in compose mode)
)

// liveTailCap is the number of rows the live tail may occupy: the terminal height
// less the status line, the rows reserved below the tail (reservedH = the slash
// panel + the queued-input affordance, 0 when both hidden) and the bottom box
// (box frame + contentH). contentH is the composer height in compose/answer mode or
// the prompt-control height in prompt mode. The result is floored at 0 and is never
// negative — when the chrome alone fills the terminal the tail vanishes (its rows are
// already committed to scrollback at the next boundary).
func liveTailCap(term, statusH, reservedH, contentH int) int {
	bottomH := boxBorderH + contentH
	capacity := term - statusH - reservedH - bottomH
	if capacity < 0 {
		return 0
	}
	return capacity
}

// surfaceInputs is the synthetic, agent-free snapshot surfaceView composes from:
// the interaction model (mode + active prompt + composer + slash), the rendered
// live-tail string, the session status and its live signals, and the terminal
// dimensions. It carries no agent and is never mutated — surfaceView is pure.
type surfaceInputs struct {
	Interaction   interactionModel
	LiveTail      string // pre-rendered live thinking/text/tool ⋯ lines
	Queued        string // pre-rendered dim queued-input affordance lines (below the live tail)
	Status        Status
	StatusState   statusInputs
	Width, Height int
}

// surfaceView composes the active surface top to bottom: the capped live tail, the
// bottom box (composer, prompt control, or answer field by interaction mode), the
// slash-completion panel when visible, and one status line. The composer is a
// borderless, ▌-edged dark-gray panel — there is no separator rule above it (a
// full-width rule was the most visible artifact stranded into scrollback on a resize
// desync; see styles.BoxStyle). It allocates no transcript viewport. Empty regions are omitted so
// the surface never emits stray blank rows.
//
// Every composed line is clamped to in.Width as the final step (clampSurfaceWidth).
// This is a hard invariant for the bubbletea v2 inline renderer: it sizes its
// managed region from the View's LOGICAL line count (strings.Count(view, "\n")+1)
// and assumes each line occupies exactly one physical row. A line wider than the
// terminal soft-wraps onto an extra physical row, so the renderer's relative-cursor
// tracking under-counts the rows it drew; on the next frame (e.g. each step of a
// resize drag) it repaints from the wrong row and strands the prior frame's lines
// (the separator rule + input-box top border) into native scrollback — the resize
// artifact cascade. Truncating (never wrapping) keeps the logical line count equal
// to the physical row count, which is exactly what the renderer requires.
func surfaceView(in surfaceInputs) string {
	bottom := bottomBox(in)
	slash := slashPanel(in.Interaction)
	status := renderStatusLine(in.Status, in.StatusState)

	contentH := bottomContentHeight(in)
	// The queued affordance and slash panel are first-class reserved rows: they sit
	// between the live tail and the bottom box (queued) or below the bottom box
	// (slash), so the tail gets only the rows left after BOTH are reserved — keeping
	// the logical line count equal to the physical row count the v2 inline renderer
	// requires (see clampSurfaceWidth).
	reserved := lipgloss.Height(slash) + queuedHeight(in.Queued)
	// statusPadH (the blank line above the status row) is reserved alongside statusH so
	// the tail budget accounts for it.
	capacity := liveTailCap(in.Height, statusH+statusPadH, reserved, contentH)

	// The live tail carries a trailing blank line, mirroring the one
	// scrollbackModel.Flush appends after every committed entry — so the gap below the
	// streaming assistant already matches its committed look, and the layout does not
	// jump by a row when the turn commits. The spacer is reserved out of the tail
	// capacity (capacity-1) so the surface row budget is unchanged.
	tail := ""
	if in.LiveTail != "" {
		tail = cappedTail(in.LiveTail, capacity-1)
	}

	rows := make([]string, 0, 6)
	if tail != "" {
		rows = append(rows, tail, "") // tail + trailing blank (matches a committed entry's spacing)
	}
	rows = appendNonEmpty(rows, in.Queued)
	rows = appendNonEmpty(rows, bottom)
	rows = appendNonEmpty(rows, slash)
	// A blank line of padding (statusPadH) separates the bottom box from the status
	// row, which is always present (statusLabel never returns ""): it reads "▸ idle" at
	// rest and the live label during a turn. Keeping the row whatever the state holds
	// the composer's vertical position stable across the turn (both rows are budgeted).
	if status != "" {
		rows = append(rows, "", status)
	}
	return clampSurfaceWidth(strings.Join(rows, "\n"), in.Width)
}

// queuedHeight is the row count of the pre-rendered queued affordance (0 when
// empty), reserved out of the live-tail budget so the tail never overlaps the
// affordance. An empty affordance reserves nothing.
func queuedHeight(queued string) int {
	if queued == "" {
		return 0
	}
	return lipgloss.Height(queued)
}

// clampSurfaceWidth truncates every line of the composed active surface to width
// display columns, the fail-safe that guarantees no active-surface line is ever
// wider than the terminal (see surfaceView for why the bubbletea v2 inline renderer
// requires this). It uses lipgloss MaxWidth, which is ANSI-aware: it counts display
// columns and preserves the per-line SGR styling while cutting the overflow. A
// non-positive width is a degenerate terminal (no managed region) — the surface is
// dropped to the empty string rather than emitting unclamped lines.
func clampSurfaceWidth(surface string, width int) string {
	if width <= 0 {
		return ""
	}
	clamp := lipgloss.NewStyle().MaxWidth(width)
	lines := strings.Split(surface, "\n")
	for i := range lines {
		lines[i] = clamp.Render(lines[i])
	}
	return strings.Join(lines, "\n")
}

// bottomBox renders the bottom box for the current interaction mode: the prompt
// control (permission or AskUser choices) when a prompt is active, the reused
// composer otherwise (compose and answer modes both render the editor — in answer
// mode it IS the answer field, with the question framed by renderAskUserBox above).
func bottomBox(in surfaceInputs) string {
	m := in.Interaction
	p := m.ActivePrompt()
	switch m.mode {
	case modePermissionPrompt:
		if p != nil {
			return renderPermissionBox(*p, in.Width, m.PendingCount())
		}
	case modeChoicePrompt:
		if p != nil {
			return renderAskUserBox(*p, in.Width, promptControlBudget(in.Height), m.PendingCount())
		}
	case modeAnswerPrompt:
		return answerSection(in)
	}
	return m.input.View()
}

// answerSection stacks the free-text framing (question + "answer" header) above the
// reused composer editor: renderAskUserBox supplies the framing, the input box the
// live answer field. With no active prompt it falls back to the bare composer.
func answerSection(in surfaceInputs) string {
	m := in.Interaction
	p := m.ActivePrompt()
	if p == nil {
		return m.input.View()
	}
	frame := renderAskUserBox(*p, in.Width, promptControlBudget(in.Height), m.PendingCount())
	return frame + "\n" + m.input.View()
}

// bottomContentHeight is the contentH fed to liveTailCap: the composer's content
// height in compose/answer mode, or the rendered prompt-control content height
// (rows inside its border) in a prompt mode.
func bottomContentHeight(in surfaceInputs) int {
	m := in.Interaction
	switch m.mode {
	case modePermissionPrompt, modeChoicePrompt, modeAnswerPrompt:
		if p := m.ActivePrompt(); p != nil {
			return promptContentHeight(in)
		}
	}
	return m.input.Height()
}

// promptContentHeight is the row count of the active prompt control minus its
// border frame, the contentH the budget reserves for a prompt-mode bottom box.
func promptContentHeight(in surfaceInputs) int {
	box := bottomBox(in)
	h := lipgloss.Height(box) - boxBorderH
	if h < 1 {
		return 1
	}
	return h
}

// promptControlBudget is the row budget handed to renderAskUserBox for the choice
// window: roughly a third of the terminal so the control never dominates the
// surface, floored so at least a couple of choices show. It is intentionally simple
// — the window scroller keeps a high selection visible regardless of the budget.
func promptControlBudget(term int) int {
	budget := term / 3
	if budget < 3 {
		return 3
	}
	return budget
}

// slashPanel renders the slash-completion panel when visible, else "".
func slashPanel(m interactionModel) string {
	if m.slash == nil {
		return ""
	}
	return m.slash.View()
}

// cappedTail returns the last capacity lines of the (pre-rendered) live tail,
// dropping the oldest rows when the tail exceeds the budget. A capacity of 0 (or
// an empty tail) yields "". The dropped rows are already committed to scrollback
// at the next boundary, so nothing is lost.
func cappedTail(tail string, capacity int) string {
	if capacity <= 0 || tail == "" {
		return ""
	}
	lines := strings.Split(tail, "\n")
	if len(lines) <= capacity {
		return tail
	}
	return strings.Join(lines[len(lines)-capacity:], "\n")
}

// appendNonEmpty appends s to rows only when it is non-empty, so an omitted region
// (an empty tail, a hidden slash panel, an empty status) leaves no blank row.
func appendNonEmpty(rows []string, s string) []string {
	if s == "" {
		return rows
	}
	return append(rows, s)
}
