package tui

import (
	"fmt"
	"strconv"
	"strings"

	"charm.land/lipgloss/v2"

	"github.com/inventivepotter/urvi/internal/content"
	"github.com/inventivepotter/urvi/tui/styles"
)

// rowSep separates transcript rows in the rendered output.
const rowSep = "\n\n"

// queuedMarker is appended to a user row that is still queued for sending.
const queuedMarker = " (queued)"

// previewLineCap (K) is how many result-preview lines a collapsed tool card shows
// before the "… N more lines · ctrl+t" marker. Expanding (ctrl+t) shows all of the
// runner-capped preview, so this is purely a display fold, not a content cap.
const previewLineCap = 6

// noOutput is the placeholder shown for a completed tool call with no result lines.
const noOutput = "(no output)"

// hintSeparator joins the fields of a collapsed-fold hint (the thinking summary and
// the tool-fold "more lines" marker). Kept in one place so both hints stay
// consistent: " · " (a U+00B7 middle dot framed by single spaces).
const hintSeparator = " · "

// expandHint is the trailing fragment shared by both collapsed-fold hints; it names
// the key that expands the fold. Lowercase to match the design appendix mockups.
const expandHint = "ctrl+t"

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

// dotWidth is the display width of the assistant bullet prefix ("● "), which also
// matches glamour's "dark" document left margin. Narration wraps to this much less
// than the content width so continuation lines — indented to align under the first
// line — still fit.
const dotWidth = 2

// renderMD renders markdown to ANSI and prefixes it with the assistant bullet so the
// narration begins on the SAME line as the "●". glamour's "dark" style indents every
// line by a 2-column document margin and brackets the block with blank lines; those
// are stripped so the text aligns with the dot — first line "● text", continuation
// lines indented to clear the bullet. On a glamour construction or render error it
// falls back to the raw text behind the dot, so the UI always gets readable output.
func renderMD(md string, width int) string {
	if strings.TrimSpace(md) == "" {
		return ""
	}

	r, err := styles.NewMarkdownRenderer(max(0, width-dotWidth))
	if err != nil {
		return styles.Dot + md
	}
	out, err := r.Render(md)
	if err != nil {
		return styles.Dot + md
	}

	lines := dedentDocument(out)
	indent := strings.Repeat(" ", dotWidth)
	for i := range lines {
		if i == 0 {
			lines[i] = styles.Dot + lines[i]
		} else {
			lines[i] = indent + lines[i]
		}
	}
	return strings.Join(lines, "\n")
}

// dedentDocument strips glamour's document framing from rendered output: the
// dotWidth-column left margin on every line and the surrounding blank lines. It
// returns at least one line.
func dedentDocument(s string) []string {
	margin := strings.Repeat(" ", dotWidth)
	raw := strings.Split(s, "\n")
	out := make([]string, 0, len(raw))
	for _, ln := range raw {
		out = append(out, strings.TrimPrefix(strings.TrimRight(ln, " "), margin))
	}
	for len(out) > 0 && out[0] == "" {
		out = out[1:]
	}
	for len(out) > 0 && out[len(out)-1] == "" {
		out = out[:len(out)-1]
	}
	if len(out) == 0 {
		return []string{""}
	}
	return out
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
// a "… N more lines · ctrl+t" marker; when true every (already runner-capped) line
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
// "… N more lines · ctrl+t" marker (N = the remainder). When expanded, every line
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
	shown = append(shown, "… "+strconv.Itoa(remaining)+" more lines"+hintSeparator+expandHint)
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

// liveSegment is the legacy in-progress assistant segment used only by the
// DisplayMessage-based renderMessages/renderRow path (and its tests). The live
// scrollback-first path uses transcript.go's liveSeg + entryrender.go instead; this
// type and renderMessages survive as the shared exercise of the render primitives
// (renderUser/renderAssistant/renderToolCalls) and a potential future --fullscreen
// transcript renderer. Screen no longer references either.
type liveSegment struct {
	text     string
	thinking string
	calls    []ToolCallView
}

// renderMessages renders the whole transcript to a single string. It dispatches
// on each message's DisplayRole and, within a row, on each block's concrete
// type. Rows whose index is in queued get a trailing marker. A non-empty live
// segment is appended as a trailing in-progress assistant row carrying its
// streamed text and its (possibly still-running) tool cards.
//
// expand is the single ctrl+t flag: it controls whether an assistant row's thinking
// block renders as a compact summary (collapsed) or its full body (expanded) AND
// whether its tool-call previews render folded or fully. It is threaded to every
// assistant row and to the live block so one toggle drives thinking and tools alike.
func renderMessages(msgs []DisplayMessage, live liveSegment, queued map[int]bool, expand bool, width int) string {
	rows := make([]string, 0, len(msgs)+1)
	for i, m := range msgs {
		row := renderRow(m, expand, width)
		if queued[i] {
			row += queuedMarker
		}
		rows = append(rows, row)
	}
	if live.text != "" || live.thinking != "" || len(live.calls) > 0 {
		rows = append(rows, renderAssistant(live.thinking, live.text, live.calls, expand, width))
	}
	return strings.Join(rows, rowSep)
}

// renderRow renders a single transcript message according to its role. The
// RoleAssistant case nests the row's thinking block and tool-call cards beneath its
// narration text; the single expand flag is forwarded to both.
func renderRow(m DisplayMessage, expand bool, width int) string {
	switch m.Role {
	case RoleUser:
		return renderUser(renderInlineBlocks(m.Blocks), width)
	case RoleAssistant:
		return renderAssistant(thinkingText(m.Blocks), assistantText(m.Blocks), m.ToolCalls, expand, width)
	case RoleSystem:
		return styles.SystemStyle.Render(firstText(m.Blocks))
	case RoleError:
		return styles.ErrorStyle.Render(firstText(m.Blocks))
	case RoleInterrupted:
		return styles.InterruptedStyle.Render(interruptedTombstone)
	default:
		return ""
	}
}

// renderAssistant renders an assistant segment in order: its reasoning (thinking)
// block, its markdown narration, then its tool-call cards. A segment with empty
// narration but non-empty cards renders a bare dot bullet (no empty markdown block)
// before its cards, per design §3. Empty parts are omitted. The single expand flag
// drives BOTH the thinking block (compact summary vs full body) and the tool-card
// result folding, so ctrl+t toggles them together.
func renderAssistant(thinking, text string, calls []ToolCallView, expand bool, width int) string {
	var b strings.Builder

	if t := renderThinking(thinking, expand, width); t != "" {
		b.WriteString(t)
	}

	body := renderMD(text, width)
	if body == "" && len(calls) > 0 {
		body = strings.TrimRight(styles.Dot, " ") // bare bullet for a card-only segment
	}
	if body != "" {
		if b.Len() > 0 {
			b.WriteString("\n\n") // one blank line between the thinking block and the AI message
		}
		b.WriteString(body)
	}

	if len(calls) > 0 {
		if b.Len() > 0 {
			b.WriteString("\n") // cards nest tight beneath the segment they belong to
		}
		b.WriteString(renderToolCalls(calls, expand, width))
	}
	return b.String()
}

// barWidth is the display columns a left-bar prefix ("▌ " / "│ ") consumes.
const barWidth = 2

// renderUser renders a user message as left accent-bar lines: every width-wrapped
// line of text is prefixed with the styled "▌ " bar, left-aligned in the assistant
// column.
func renderUser(text string, width int) string {
	bar := styles.AccentBarStyle.Render(styles.AccentBarPrompt)
	var out []string
	for _, raw := range strings.Split(text, "\n") {
		for _, line := range wrapToWidth(raw, width-barWidth) {
			out = append(out, bar+line)
		}
	}
	return strings.Join(out, "\n")
}

// renderThinking renders the model's reasoning under the unified ctrl+t expand
// flag. When expanded it renders a dim block: a faint "thinking" header followed by
// "│ "-prefixed, width-wrapped lines. When collapsed it renders a single compact
// dim summary line ("thinking · N lines · ctrl+t", N = number of thinking content
// lines, singularised to "1 line" for one line) instead of the full body.
// Empty/whitespace-only reasoning renders nothing
// in either mode.
func renderThinking(s string, expand bool, width int) string {
	s = strings.TrimSpace(s) // drop the model's leading/trailing blank reasoning lines
	if s == "" {
		return ""
	}
	if !expand {
		n := strings.Count(s, "\n") + 1 // thinking content lines
		summary := styles.ThinkingHeader + hintSeparator + pluralLines(n) + hintSeparator + expandHint
		return styles.ThinkingStyle.Render(summary)
	}
	out := []string{styles.ThinkingStyle.Render(styles.ThinkingHeader)}
	for _, raw := range strings.Split(s, "\n") {
		for _, line := range wrapToWidth(raw, width-barWidth) {
			out = append(out, styles.ThinkingStyle.Render("│ "+line))
		}
	}
	return strings.Join(out, "\n")
}

// pluralLines renders a line count with grammatical agreement: "1 line" (singular)
// for n == 1, "N lines" (plural) otherwise. Used by the collapsed thinking summary.
func pluralLines(n int) string {
	if n == 1 {
		return "1 line"
	}
	return strconv.Itoa(n) + " lines"
}

// wrapToWidth word-wraps s to width columns and returns the resulting rows with
// trailing wrap padding trimmed. A non-positive width skips wrapping (single row).
func wrapToWidth(s string, width int) []string {
	if width <= 0 {
		return []string{s}
	}
	wrapped := lipgloss.NewStyle().Width(width).Render(s)
	rows := strings.Split(wrapped, "\n")
	for i := range rows {
		rows[i] = strings.TrimRight(rows[i], " ")
	}
	return rows
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

// assistantText concatenates the narration of an assistant segment for markdown
// rendering: every block except ThinkingBlock (rendered separately as the dim
// thinking block by renderThinking, so it must not be markdown-rendered here too).
func assistantText(blocks []content.Block) string {
	parts := make([]string, 0, len(blocks))
	for _, blk := range blocks {
		if _, ok := blk.(*content.ThinkingBlock); ok {
			continue
		}
		parts = append(parts, renderBlock(blk))
	}
	return strings.Join(parts, "\n")
}

// thinkingText concatenates the reasoning of every ThinkingBlock in blocks, the
// source for an assistant row's dim thinking block.
func thinkingText(blocks []content.Block) string {
	var b strings.Builder
	for _, blk := range blocks {
		if tb, ok := blk.(*content.ThinkingBlock); ok {
			b.WriteString(tb.Thinking)
		}
	}
	return b.String()
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
	case *content.ThinkingBlock:
		return b.Thinking
	case *content.ImageBlock:
		return fmt.Sprintf("[image: %s, %d bytes]", string(b.MediaType), len(b.Source.Data))
	default:
		return "[unsupported block]"
	}
}
