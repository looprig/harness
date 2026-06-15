package tui

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/charmbracelet/lipgloss"

	"github.com/inventivepotter/urvi/internal/content"
	"github.com/inventivepotter/urvi/tui/styles"
)

// rowSep separates transcript rows in the rendered output.
const rowSep = "\n\n"

// queuedMarker is appended to a user row that is still queued for sending.
const queuedMarker = " (queued)"

// previewLineCap (K) is how many result-preview lines a collapsed tool card shows
// before the "… N more lines (Ctrl+T)" marker. Expanding (Ctrl+T) shows all of the
// runner-capped preview, so this is purely a display fold, not a content cap.
const previewLineCap = 6

// noOutput is the placeholder shown for a completed tool call with no result lines.
const noOutput = "(no output)"

// cardConnector is the tree connector that prefixes each tool-call card line.
const cardConnector = "└ "

// cardIndent / resultIndent are the leading indents for a card line and for its
// result-preview lines (design §3: cards indent 2, result lines 4).
const (
	cardIndent   = "  "
	resultIndent = "    "
)

// Status glyphs for a tool card (design §3). A tick-driven spinner is a future
// enhancement; v1 uses the static running glyph.
const (
	glyphRunning   = "⋯"
	glyphOK        = "✓"
	glyphError     = "✗"
	glyphCancelled = "⊘"
)

// renderMD renders markdown to ANSI for the given wrap width. On a glamour
// construction or render error it falls back to the raw text prefixed with the
// dot marker, so the UI always gets readable output and never an error.
func renderMD(md string, width int) string {
	if md == "" {
		return ""
	}

	r, err := styles.NewMarkdownRenderer(width)
	if err != nil {
		return styles.Dot + md
	}
	out, err := r.Render(md)
	if err != nil {
		return styles.Dot + md
	}
	return styles.Dot + strings.TrimRight(out, "\n")
}

// toolGlyph maps a tool-call status to its single-rune display glyph (design §3).
// An unrecognised status falls back to the running glyph (fail-visible, not panic).
func toolGlyph(s ToolStatus) string {
	switch s {
	case ToolOK:
		return glyphOK
	case ToolError:
		return glyphError
	case ToolCancelled:
		return glyphCancelled
	default: // ToolRunning and any unknown value
		return glyphRunning
	}
}

// renderToolCalls renders a segment's tool-call children as indented cards, each a
// header line ("└ ToolName  Summary  <glyph>") followed by its result preview. When
// expandTools is false the preview is folded to the first previewLineCap lines plus
// a "… N more lines (Ctrl+T)" marker; when true every (already runner-capped) line
// shows. An empty result renders "(no output)". An error card's result always shows
// (subject to the same fold), never hidden. Lines are width-wrapped so a long card
// never blows the viewport. Returns "" when there are no calls.
func renderToolCalls(calls []ToolCallView, expandTools bool, width int) string {
	if len(calls) == 0 {
		return ""
	}
	parts := make([]string, 0, len(calls))
	for i := range calls {
		parts = append(parts, renderToolCard(calls[i], expandTools, width))
	}
	return strings.Join(parts, "\n")
}

// renderToolCard renders one tool card: the styled header line then its styled,
// indented result-preview lines.
func renderToolCard(c ToolCallView, expandTools bool, width int) string {
	header := cardIndent + styles.ToolCallStyle.Render(
		cardConnector+toolHeaderText(c.ToolName, c.Summary, toolGlyph(c.Status)))

	lines := make([]string, 0, previewLineCap+2)
	lines = append(lines, header)
	for _, rl := range previewLines(c.Result, expandTools) {
		lines = append(lines, indentWrap(rl, resultIndent, width))
	}
	return strings.Join(lines, "\n")
}

// toolHeaderText assembles the "ToolName  Summary  glyph" body of a card header,
// omitting the summary gap when there is no summary.
func toolHeaderText(name, summary, glyph string) string {
	if summary == "" {
		return name + "  " + glyph
	}
	return name + "  " + summary + "  " + glyph
}

// previewLines selects the result lines to display for a card. An empty result
// yields the single "(no output)" placeholder. When collapsed and the result has
// more than previewLineCap lines, it returns the first previewLineCap lines plus a
// "… N more lines (Ctrl+T)" marker (N = the remainder). When expanded, every line
// shows (the runner already capped the preview — no extra TUI cap).
func previewLines(result []string, expandTools bool) []string {
	if len(result) == 0 {
		return []string{noOutput}
	}
	if expandTools || len(result) <= previewLineCap {
		return result
	}
	remaining := len(result) - previewLineCap
	shown := make([]string, 0, previewLineCap+1)
	shown = append(shown, result[:previewLineCap]...)
	shown = append(shown, "… "+strconv.Itoa(remaining)+" more lines (Ctrl+T)")
	return shown
}

// indentWrap word-wraps s to the column budget left after the indent, then prefixes
// every wrapped row with indent. A non-positive width skips wrapping (the indent is
// still applied). Trailing wrap padding is trimmed so output stays clean for tests
// and copy/paste.
func indentWrap(s, indent string, width int) string {
	avail := width - len(indent)
	if avail <= 0 {
		return indent + s
	}
	wrapped := lipgloss.NewStyle().Width(avail).Render(s)
	rows := strings.Split(wrapped, "\n")
	for i := range rows {
		rows[i] = styles.ToolResultStyle.Render(indent + strings.TrimRight(rows[i], " "))
	}
	return strings.Join(rows, "\n")
}

// renderMessages renders the whole transcript to a single string. It dispatches
// on each message's DisplayRole and, within a row, on each block's concrete
// type. Rows whose index is in queued get a trailing marker. A non-empty live
// segment is appended as a trailing in-progress assistant row carrying its
// streamed text and its (possibly still-running) tool cards.
//
// expandTools controls whether tool-call previews render folded (first K lines +
// a marker) or fully; it is threaded to every assistant row and to the live block.
func renderMessages(msgs []DisplayMessage, live liveSegment, queued map[int]bool, expandTools bool, width int) string {
	rows := make([]string, 0, len(msgs)+1)
	for i, m := range msgs {
		row := renderRow(m, expandTools, width)
		if queued[i] {
			row += queuedMarker
		}
		rows = append(rows, row)
	}
	if live.text != "" || len(live.calls) > 0 {
		rows = append(rows, renderAssistant(live.text, live.calls, expandTools, width))
	}
	return strings.Join(rows, rowSep)
}

// renderRow renders a single transcript message according to its role. The
// RoleAssistant case nests the row's tool-call cards beneath its narration text;
// expandTools is forwarded to that nesting.
func renderRow(m DisplayMessage, expandTools bool, width int) string {
	switch m.Role {
	case RoleUser:
		return styles.UserStyle.Render(renderInlineBlocks(m.Blocks))
	case RoleAssistant:
		return renderAssistant(assistantText(m.Blocks), m.ToolCalls, expandTools, width)
	case RoleSystem:
		return styles.SystemStyle.Render(firstText(m.Blocks))
	case RoleError:
		return styles.ErrorStyle.Render(firstText(m.Blocks))
	case RoleInterrupted:
		return styles.InterruptedStyle.Render("└─ interrupted")
	default:
		return ""
	}
}

// renderAssistant renders an assistant segment: its markdown narration followed by
// its tool-call cards. A segment with empty narration but non-empty cards renders a
// bare dot bullet (no empty markdown block) before its cards, per design §3.
func renderAssistant(text string, calls []ToolCallView, expandTools bool, width int) string {
	body := renderMD(text, width)
	if body == "" && len(calls) > 0 {
		body = strings.TrimRight(styles.Dot, " ") // bare bullet for a card-only segment
	}
	if len(calls) == 0 {
		return body
	}
	return body + "\n" + renderToolCalls(calls, expandTools, width)
}

// renderInlineBlocks renders each block to plain text and joins with newlines.
// Used for user rows where blocks are shown verbatim (no markdown).
func renderInlineBlocks(blocks []content.Block) string {
	parts := make([]string, 0, len(blocks))
	for _, blk := range blocks {
		parts = append(parts, renderBlock(blk))
	}
	return strings.Join(parts, "\n")
}

// assistantText concatenates the text of every TextBlock and renders any other
// block as its placeholder, joined with newlines, for markdown rendering.
func assistantText(blocks []content.Block) string {
	parts := make([]string, 0, len(blocks))
	for _, blk := range blocks {
		parts = append(parts, renderBlock(blk))
	}
	return strings.Join(parts, "\n")
}

// firstText returns the text of the first TextBlock, or "" if there is none.
// Used by single-block roles (system, error).
func firstText(blocks []content.Block) string {
	for _, blk := range blocks {
		if tb, ok := blk.(*content.TextBlock); ok {
			return tb.Text
		}
	}
	return ""
}

// renderBlock renders one block to its display string via a type switch over the
// sealed Block interface. Unknown types fall through to a safe placeholder.
func renderBlock(blk content.Block) string {
	switch b := blk.(type) {
	case *content.TextBlock:
		return b.Text
	case *content.ImageBlock:
		return fmt.Sprintf("[image: %s, %d bytes]", string(b.MediaType), len(b.Source.Data))
	default:
		return "[unsupported block]"
	}
}
