package tui

import (
	"strconv"
	"strings"

	"charm.land/lipgloss/v2"

	"github.com/inventivepotter/urvi/tui/styles"
)

// interruptedTombstone is the content-less marker rendered for an interrupted turn.
const interruptedTombstone = "└─ interrupted"

// renderEntry turns one committed transcript entry into its scrollback lines,
// dispatching on the entry's kind. It is the per-entry renderer scrollbackModel.Flush
// is given: each kind reuses the render.go primitives (renderUser/renderAssistant/
// renderToolCalls/styles) so scrollback and the live tail render identically. expand
// is the single ctrl+t flag, threaded to the assistant kind's thinking and tool-card
// folds. An unknown kind returns no lines (fail-safe). Returned lines carry no
// trailing blank line — the caller (Flush) owns inter-entry spacing.
func renderEntry(e entry, expand bool, width int) []string {
	switch e.Kind {
	case kindUser:
		return splitNonEmpty(renderUser(renderInlineBlocks(e.Blocks), width))
	case kindAssistant:
		return splitNonEmpty(renderAssistant(thinkingText(e.Blocks), assistantText(e.Blocks), e.Calls, e.doneHeadline, expand, width))
	case kindTool:
		return splitNonEmpty(renderToolCalls(e.Calls, expand, width))
	case kindPromptRecord:
		return renderPromptRecord(e.Prompt, width)
	case kindNotice:
		return renderNotice(e.Level, firstText(e.Blocks), width)
	case kindInterrupted:
		return []string{styles.InterruptedStyle.Render(interruptedTombstone)}
	default:
		return nil
	}
}

// renderNotice renders one leveled notice as "▌ "-bar lines colored per level (info
// neutral, warn yellow, error red — see styles.NoticeStyle): every width-wrapped line
// of text is prefixed with the level-colored accent bar, matching the user-message
// bar layout. An empty text still yields a single bar line so the notice marks the
// event. The bar and the text share the per-level style so the row reads as one
// coherent colored unit.
func renderNotice(level noticeLevel, text string, width int) []string {
	style := styles.NoticeStyle(uint8(level))
	out := barWrap(style, strings.Split(text, "\n"), width)
	if len(out) == 0 {
		out = append(out, style.Render(styles.AccentBarPrompt))
	}
	return out
}

// barWrap renders each raw line as a "▌ "-bar-prefixed, width-wrapped row in style:
// the accent bar and the wrapped text share one style so the rows read as a single
// coherent colored unit (the leveled-notice / info-notification layout). Every wrapped
// row keeps the bar, and the wrap budget reserves barWidth columns for it. It is the
// shared bar-rendering primitive reused by renderNotice and the scrollback prompt
// record so neither duplicates the bar layout.
func barWrap(style lipgloss.Style, rawLines []string, width int) []string {
	bar := style.Render(styles.AccentBarPrompt)
	var out []string
	for _, raw := range rawLines {
		for _, line := range wrapToWidth(raw, width-barWidth) {
			out = append(out, bar+style.Render(line))
		}
	}
	return out
}

// renderPromptRecord renders the FULL prompt context committed to scrollback — the
// copyable record, NOT the compact bottom-box control (prompt.go renders that). It is
// rendered as one info notification: every line carries the neutral "▌ " info bar
// (styles.NoticeInfoStyle), matching the leveled-notice family. A permission record
// renders an "Approve <ToolName>?" header then its wrapped Description (the command /
// diff / url); a user-input record renders the Question then every choice. A nil
// context renders nothing (fail-safe).
func renderPromptRecord(p *promptContext, width int) []string {
	if p == nil {
		return nil
	}
	if p.Kind == promptUserInput {
		return renderUserInputRecord(*p, width)
	}
	return renderPermissionRecord(*p, width)
}

// renderPermissionRecord renders a permission gate's scrollback record as one info
// notification: a "▌ "-barred, bold "Approve <ToolName>?" header followed by the
// "▌ "-barred, width-wrapped, copyable Description lines — every line carries the
// neutral info bar (styles.NoticeInfoStyle) so the block reads as one information
// notice (unifying it with the leveled-notice family). The Description (Bash command,
// file diff, fetch url) is shown in full so scrollback retains exactly what was
// approved/denied — INTERIOR blank lines are preserved (a multi-line command or diff
// can carry meaningful gaps); only a single leading/trailing empty line is trimmed so
// the record reads tight against the header.
func renderPermissionRecord(p promptContext, width int) []string {
	out := barWrap(styles.PromptRecordHeaderStyle, []string{"Approve " + p.ToolName + "?"}, width)
	body := trimEdgeBlanks(strings.Split(p.Description, "\n"))
	return append(out, barWrap(styles.NoticeInfoStyle, body, width)...)
}

// trimEdgeBlanks drops a single leading and a single trailing empty line from rows,
// preserving every interior blank line. It is how the permission record keeps a
// multi-line Description's meaningful interior gaps while not opening or closing the
// body on a stray blank. A slice that is all-blank collapses to empty.
func trimEdgeBlanks(rows []string) []string {
	if len(rows) > 0 && rows[0] == "" {
		rows = rows[1:]
	}
	if len(rows) > 0 && rows[len(rows)-1] == "" {
		rows = rows[:len(rows)-1]
	}
	return rows
}

// renderUserInputRecord renders an AskUser request's scrollback record as one info
// notification: the "▌ "-barred Question followed by every offered choice as a
// "▌ "-barred numbered line (promptChoiceLines). Every line carries the neutral info
// bar (styles.NoticeInfoStyle) so the request reads as one information notice. A
// free-text request (no choices) records just the question.
func renderUserInputRecord(p promptContext, width int) []string {
	lines := append([]string{p.Question}, promptChoiceLines(p.Choices)...)
	return barWrap(styles.NoticeInfoStyle, lines, width)
}

// choiceIndent is the 2-space lead applied to every recorded-choice line so a
// wrapped "N. text" choice's continuation rows align under the "N. " number.
const choiceIndent = "  "

// promptChoiceLines builds each choice as a "  N. text" numbered raw line (1-based),
// the choiceIndent leading every line so a barWrap-wrapped continuation row aligns
// under the number. Width-wrapping and the info bar are applied by barWrap (which the
// caller runs these through) — this only lays out the numbered text. Returns nil for
// an empty choice list (a free-text question records no choices).
func promptChoiceLines(choices []string) []string {
	out := make([]string, 0, len(choices))
	for i, c := range choices {
		out = append(out, choiceIndent+strconv.Itoa(i+1)+". "+c)
	}
	return out
}

// splitNonEmpty splits a rendered block on newlines into scrollback lines,
// returning nil for an empty render so an empty kind contributes no lines.
func splitNonEmpty(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}
