package tui

import (
	"strconv"
	"strings"

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
		return splitNonEmpty(renderAssistant(thinkingText(e.Blocks), assistantText(e.Blocks), e.Calls, expand, width))
	case kindTool:
		return splitNonEmpty(renderToolCalls(e.Calls, expand, width))
	case kindPromptRecord:
		return renderPromptRecord(e.Prompt, width)
	case kindError:
		return splitNonEmpty(styles.ErrorStyle.Render(firstText(e.Blocks)))
	case kindSystem:
		return splitNonEmpty(styles.SystemStyle.Render(firstText(e.Blocks)))
	case kindInterrupted:
		return []string{styles.InterruptedStyle.Render(interruptedTombstone)}
	default:
		return nil
	}
}

// renderPromptRecord renders the FULL prompt context committed to scrollback — the
// copyable record, NOT the compact bottom-box control (prompt.go renders that). A
// permission record renders an "Approve <ToolName>?" header then its wrapped
// Description (the command / diff / url); a user-input record renders the Question
// then every choice (renderPromptChoices). A nil context renders nothing (fail-safe).
func renderPromptRecord(p *promptContext, width int) []string {
	if p == nil {
		return nil
	}
	if p.Kind == promptUserInput {
		return renderUserInputRecord(*p, width)
	}
	return renderPermissionRecord(*p, width)
}

// renderPermissionRecord renders a permission gate's scrollback record: a bold
// "Approve <ToolName>?" header followed by the width-wrapped, copyable Description
// lines. The Description (Bash command, file diff, fetch url) is shown in full so
// scrollback retains exactly what was approved/denied — INTERIOR blank lines are
// preserved (a multi-line command or diff can carry meaningful gaps); only a single
// leading/trailing empty line is trimmed so the record reads tight against the header.
func renderPermissionRecord(p promptContext, width int) []string {
	out := []string{styles.PromptHeaderStyle.Render("Approve " + p.ToolName + "?")}
	for _, raw := range trimEdgeBlanks(strings.Split(p.Description, "\n")) {
		out = append(out, wrapToWidth(raw, width)...)
	}
	return out
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

// renderUserInputRecord renders an AskUser request's scrollback record: the
// width-wrapped Question followed by every offered choice as a numbered line
// (renderPromptChoices). A free-text request (no choices) records just the question.
func renderUserInputRecord(p promptContext, width int) []string {
	out := make([]string, 0, len(p.Choices)+2)
	out = append(out, wrapToWidth(p.Question, width)...)
	out = append(out, renderPromptChoices(p.Choices, width)...)
	return out
}

// choiceIndent is the 2-space lead applied to every recorded-choice line so a
// wrapped "N. text" choice's continuation rows align under the "N. " number.
const choiceIndent = "  "

// renderPromptChoices renders each choice as a "  N. text" numbered line (1-based),
// width-wrapped under the number indent so a long choice never blows the width.
// Choices render at NORMAL weight via wrapToWidth (which applies no style) — they
// are user-facing record lines, NOT subordinate tool output, so they must not pick
// up the faint styles.ToolResultStyle that indentWrap unconditionally applies.
// Returns nil for an empty choice list (a free-text question records no choices).
func renderPromptChoices(choices []string, width int) []string {
	out := make([]string, 0, len(choices))
	for i, c := range choices {
		for _, row := range wrapToWidth(strconv.Itoa(i+1)+". "+c, width-len(choiceIndent)) {
			out = append(out, choiceIndent+row)
		}
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
