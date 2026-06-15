package tui

import (
	"strings"

	"charm.land/lipgloss/v2"

	"github.com/inventivepotter/urvi/tui/styles"
)

// Active-surface row budget (design §"Input box · Active-surface budgeting"). The
// surface is the only managed region in scrollback-first mode: the terminal owns
// history, so there is no transcript viewport — only a capped live tail, a
// separator rule, the bottom box, an optional slash panel, and one status line.
const (
	statusH    = 1 // the single status line at the very bottom
	sepH       = 1 // the separator rule above the bottom box
	boxBorderH = 2 // a bottom box's top + bottom border rows
)

// liveTailCap is the number of rows the live tail may occupy: the terminal height
// less the status line, the slash panel (slashH, 0 when hidden) and the bottom box
// (sep + box border + contentH). contentH is the composer height in compose/answer
// mode or the prompt-control height in prompt mode. The result is floored at 0 and
// is never negative — when the chrome alone fills the terminal the tail vanishes
// (its rows are already committed to scrollback at the next boundary).
func liveTailCap(term, statusH, slashH, contentH int) int {
	bottomH := sepH + boxBorderH + contentH
	capacity := term - statusH - slashH - bottomH
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
	Status        Status
	StatusState   statusInputs
	Width, Height int
}

// surfaceView composes the active surface top to bottom: the capped live tail, the
// full-width separator rule, the bottom box (composer, prompt control, or answer
// field by interaction mode), the slash-completion panel when visible, and one
// status line. It allocates no transcript viewport. Empty regions are omitted so
// the surface never emits stray blank rows.
func surfaceView(in surfaceInputs) string {
	bottom := bottomBox(in)
	slash := slashPanel(in.Interaction)
	status := renderStatusLine(in.Status, in.StatusState)

	contentH := bottomContentHeight(in)
	capacity := liveTailCap(in.Height, statusH, lipgloss.Height(slash), contentH)
	tail := cappedTail(in.LiveTail, capacity)

	rows := make([]string, 0, 5)
	rows = appendNonEmpty(rows, tail)
	rows = append(rows, styles.SeparatorRule(in.Width))
	rows = appendNonEmpty(rows, bottom)
	rows = appendNonEmpty(rows, slash)
	rows = appendNonEmpty(rows, status)
	return strings.Join(rows, "\n")
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
