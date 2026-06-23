package tui

import (
	"fmt"
	"strconv"
	"strings"

	"charm.land/lipgloss/v2"

	"github.com/ciram-co/looprig/pkg/content"
	"github.com/ciram-co/looprig/pkg/tui/styles"
)

// previewLineCap is the HARD cap on result-preview lines a tool card shows: a result with
// more lines is trimmed to the first previewLineCap lines plus a "… N more lines" marker.
// The cap applies ALWAYS — regardless of the ctrl+t expand fold (which now governs only the
// thinking block). A huge tool result (e.g. a long diff) otherwise fills the live tail,
// scrolling the assistant bullet off the top AND, on commit, stranding a screen-height
// scrollback gap (the inline renderer's insertAbove math is sized off the tall live tail).
const previewLineCap = 3

// noOutput is the placeholder shown for a completed tool call with no result lines.
const noOutput = "(no output)"

// hintSeparator joins the fields of a collapsed-fold hint (the thinking summary and
// the tool-fold "more lines" marker). Kept in one place so both hints stay
// consistent: " · " (a U+00B7 middle dot framed by single spaces).
const hintSeparator = " · "

// expandHint is the trailing fragment shared by both collapsed-fold hints; it names
// the key that expands the fold. Lowercase to match the design appendix mockups.
const expandHint = "ctrl+t"

// cardConnector is the tree connector that prefixes each tool-call card line. It is
// dotWidth (2) columns — the "⎿" glyph plus a space — so a card's body aligns under it.
const cardConnector = "⎿ "

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

// multipleActionsHeadline is the committed bullet headline for an empty-text step that
// ran MORE THAN ONE tool call: a single "● Multiple actions" umbrella, with each call
// committed below it as its own card (carrying the per-tool name, args, ✓/✗ and any
// Approved/Denied verb). A single-tool empty-text step has no umbrella — its one card
// is promoted to the bullet directly (renderPromotedTool). The LIVE counterpart of an
// empty-text step is a rotating workingWord.
const multipleActionsHeadline = "Multiple actions"

// renderMD renders markdown to ANSI behind the static committed bullet (styles.LitDot,
// the DotColor-foregrounded "●"). It is the committed/scrollback path: a frozen
// assistant "●" never animates, so it always uses the lit dot. The live tail uses
// renderMDDot with a blink-phased bullet.
func renderMD(md string, width int) string {
	return renderMDDot(md, width, styles.LitDot)
}

// renderMDDot renders markdown to ANSI and prefixes it with dot so the narration
// begins on the SAME line as the bullet. glamour's "dark" style indents every line by
// a 2-column document margin and brackets the block with blank lines; those are
// stripped so the text aligns with the dot — first line "<dot>text", continuation
// lines indented to clear the bullet. On a glamour construction or render error it
// falls back to the raw text behind the dot, so the UI always gets readable output.
// dot MUST be dotWidth (2) columns wide so continuation-line alignment holds; callers
// pass either the static styles.Dot (committed) or a blink-phased live bullet.
func renderMDDot(md string, width int, dot string) string {
	if strings.TrimSpace(md) == "" {
		return ""
	}

	r, err := styles.NewMarkdownRenderer(max(0, width-dotWidth))
	if err != nil {
		return dot + md
	}
	out, err := r.Render(md)
	if err != nil {
		return dot + md
	}

	lines := dedentDocument(out)
	indent := strings.Repeat(" ", dotWidth)
	for i := range lines {
		if i == 0 {
			lines[i] = dot + lines[i]
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
// header line ("⎿ ToolName(Summary)  <glyph>") followed by its result preview. The
// preview is HARD-capped to previewLineCap lines plus a "… N more lines" marker
// regardless of expandTools (the ctrl+t fold governs only the thinking block now), so a
// huge result can never fill the live tail. An empty result renders "(no output)". An
// error card's result is capped the same way, never hidden. Lines are width-wrapped so a
// long card never blows the viewport. Returns "" when there are no calls.
func renderToolCalls(calls []ToolCallView, expandTools bool, width int) string {
	// Committed/scrollback path: full cards, static glyphs, never header-only (a
	// stray running card committed at a terminal still shows its body).
	return renderToolCallsGlyph(calls, expandTools, width, toolGlyph, false)
}

// renderToolCallsGlyph is the shared card renderer: it maps each call's status to a
// glyph via glyph, the indirection that lets the LIVE path animate a running card's
// glyph (spinnerGlyph) while the committed path keeps the static toolGlyph. When
// liveRunning is true (the LIVE tail only), a still-RUNNING card renders header-only
// — see renderToolCard. Returns "" when there are no calls.
func renderToolCallsGlyph(calls []ToolCallView, expandTools bool, width int, glyph func(ToolStatus) string, liveRunning bool) string {
	if len(calls) == 0 {
		return ""
	}
	parts := make([]string, 0, len(calls))
	for i := range calls {
		parts = append(parts, renderToolCard(calls[i], expandTools, width, glyph, liveRunning))
	}
	return strings.Join(parts, "\n")
}

// renderToolCard renders one tool card: the styled header line then its styled,
// indented result-preview lines. glyph maps the call's status to its header glyph.
//
// liveRunning collapses a still-RUNNING card to its header line ALONE (no result
// body) — the live→committed handoff fix (design Option B). A running card has no
// result yet, so its body is only the "(no output)" placeholder; dropping it in the
// LIVE tail means the compact one-line running indicator is replaced by the full
// committed card (which inserts above via tea.Println) without a multi-line live-tail
// shrink, so the running→completed transition reads as a clean continuation rather
// than a split. It applies ONLY to ToolRunning cards on the live path; resolved cards
// (live or committed) and the committed path always render their full body.
func renderToolCard(c ToolCallView, expandTools bool, width int, glyph func(ToolStatus) string, liveRunning bool) string {
	header := cardIndent + styles.ToolCallStyle.Render(
		cardConnector+toolHeaderText(c, glyph(c.Status)))

	if liveRunning && c.Status == ToolRunning {
		return header // compact one-line running indicator; body appears once, on commit
	}

	lines := make([]string, 0, previewLineCap+2)
	lines = append(lines, header)
	for _, rl := range previewLines(c.Result, expandTools) {
		lines = append(lines, indentWrap(rl, resultIndent, width))
	}
	return strings.Join(lines, "\n")
}

// toolHeaderText assembles a card header's body: "<verb >ToolName(Summary)  glyph",
// where verb is the permission decision ("Approved "/"Denied ") for a call that
// prompted (empty for an ungated/pre-approved call) and the args summary is shown in
// parens. The parens and the summary gap are omitted when there is no summary.
func toolHeaderText(c ToolCallView, glyph string) string {
	head := c.ToolName
	if c.Summary != "" {
		head = c.ToolName + "(" + c.Summary + ")"
	}
	if v := decisionVerb(c.Decision); v != "" {
		head = v + " " + head
	}
	return head + "  " + glyph
}

// decisionVerb maps a permission decision to its card-header verb. A call that never
// prompted (gateNone) — ungated or pre-approved by an existing grant — gets no verb;
// a still-pending gate likewise shows none (it resolves before the card commits).
func decisionVerb(d gateDecision) string {
	switch d {
	case gateApproved:
		return "Approved"
	case gateDenied:
		return "Denied"
	default:
		return ""
	}
}

// previewLines selects the result lines to display for a card. An empty result
// yields the single "(no output)" placeholder. When collapsed and the result has
// more than previewLineCap lines, it returns the first previewLineCap lines plus a
// "… N more lines · ctrl+t" marker (N = the remainder). When expanded, every line
// shows (the runner already capped the preview — no extra TUI cap).
func previewLines(result []string, _ bool) []string {
	// The expand flag is intentionally IGNORED for tool results: the preview is HARD-capped
	// to previewLineCap lines regardless of ctrl+t, so a huge result can never fill the live
	// tail (hiding the assistant bullet) or strand a commit-time scrollback gap. ctrl+t still
	// folds the thinking block. (The bool param is retained for call-site compatibility.)
	if len(result) == 0 {
		return []string{noOutput}
	}
	if len(result) <= previewLineCap {
		return result
	}
	remaining := len(result) - previewLineCap
	shown := make([]string, 0, previewLineCap+1)
	shown = append(shown, result[:previewLineCap]...)
	shown = append(shown, "… "+strconv.Itoa(remaining)+" more lines")
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

// renderAssistant renders a committed assistant segment in order: its reasoning
// (thinking) block, then its markdown narration OR a bold bullet headline. When the
// narration is non-empty it renders "● <text>"; when it is empty and headline is set —
// the empty-text MULTI-tool step's "● Multiple actions" umbrella — it renders
// "● <headline>". When both are empty it renders the thinking rail alone with no
// bullet (a thinking-only message, or a single-tool empty-text step whose one card is
// promoted to the bullet separately by renderPromotedTool). Committed tool cards are
// their OWN kindTool entries, so this never renders cards inline. expand drives the
// thinking block's compact/full fold.
func renderAssistant(thinking, text, headline string, expand bool, width int) string {
	var b strings.Builder

	if t := renderThinking(thinking, expand, width); t != "" {
		b.WriteString(t)
	}

	body := renderMD(text, width)
	if body == "" && headline != "" {
		body = strings.TrimRight(styles.LitDot, " ") + " " + styles.HeadlineStyle.Render(headline) // "● <headline>"
	}
	if body != "" {
		if b.Len() > 0 {
			b.WriteString("\n\n") // one blank line between the thinking block and the AI message
		}
		b.WriteString(body)
	}
	return b.String()
}

// renderPromotedTool renders a single tool card promoted to the assistant bullet — the
// committed form of an empty-text step that ran exactly ONE tool call. Instead of a
// "Multiple actions" umbrella plus a child card, the one call IS the headline:
// "● <verb >ToolName(args)  glyph" beside the lit dot, with its result preview indented
// below (same fold as a normal card). It is the committed counterpart of the live
// working-word for the single-tool case.
func renderPromotedTool(c ToolCallView, expand bool, width int) string {
	header := strings.TrimRight(styles.LitDot, " ") + " " +
		styles.HeadlineStyle.Render(toolHeaderText(c, toolGlyph(c.Status)))
	lines := make([]string, 0, previewLineCap+2)
	lines = append(lines, header)
	for _, rl := range previewLines(c.Result, expand) {
		lines = append(lines, indentWrap(rl, resultIndent, width))
	}
	return strings.Join(lines, "\n")
}

// subagentTerminalVerb maps a child loop's terminal status to its done-line verb
// (design §4): subDone→"done", subFailed→"failed", subInterrupted→"interrupted". An
// outstanding child (subRunning, the zero value) reads "running" — a defensive label
// for a card committed before its terminal (a card normally commits only after the
// child has handed back, so this is the rare in-flight case).
func subagentTerminalVerb(s subStatus) string {
	switch s {
	case subDone:
		return "done"
	case subFailed:
		return "failed"
	case subInterrupted:
		return "interrupted"
	default:
		return "running"
	}
}

// renderSubagentCard renders a committed Subagent card (design §5/§4): a "●"-level
// header "Subagent(<agent>)  \"<task>\"" beside the lit dot, the subagent's nested tool
// calls as ordinary "⎿" cards ONE indent level under the header (never "⎿ ⎿"), then a
// final "⎿ <verb> · N steps — \"<summary>\"" line whose verb comes from SubStatus and
// whose summary is the card's own (suppressed-elsewhere) Result — so the hand-back text
// shows ONLY here, never doubled as a normal result body. A subInterrupted card omits
// the summary; subFailed shows its error text. When Nested > 0 a trailing
// "⎿ +M nested subagent steps" line collapses the depth-2 activity (design §6). expand
// drives the child cards' result-preview fold.
func renderSubagentCard(c ToolCallView, expand bool, width int) string {
	head := strings.TrimRight(styles.LitDot, " ") + " " +
		styles.HeadlineStyle.Render(subagentHeaderText(c))
	// Cap accounts for: header (1), the children's ONE joined element from
	// renderToolCalls (1), the done line (1), and the optional nested line (1).
	lines := make([]string, 0, 4)
	lines = append(lines, head)

	// Children render as the existing "⎿" tool cards, one indent level under the header.
	if body := renderToolCalls(c.Children, expand, width); body != "" {
		lines = append(lines, body)
	}

	// The done/failed/interrupted child carries the step count and (for done/failed) the
	// summary — the ONLY place the hand-back text appears (no doubling).
	lines = append(lines, subagentDoneLine(c, width))

	if c.Nested > 0 {
		nested := cardIndent + styles.ToolCallStyle.Render(
			cardConnector+"+"+strconv.Itoa(c.Nested)+" nested subagent steps")
		lines = append(lines, nested)
	}
	return strings.Join(lines, "\n")
}

// subagentHeaderText assembles a Subagent card's header body: the standard tool-card
// form "Subagent(<agent>)" (tool name + the agent as its argument) plus the truncated
// task in quotes when present. The task quotes are omitted for an empty task.
func subagentHeaderText(c ToolCallView) string {
	head := c.ToolName + "(" + c.Agent + ")"
	if c.Task != "" {
		head += `  "` + c.Task + `"`
	}
	return head
}

// subagentDoneLine builds the Subagent card's terminal "⎿" child: "<verb> · N steps"
// from SubStatus + Steps, plus the truncated summary (the card's own Result, the
// suppressed hand-back text) appended as `— "<summary>"` for done/failed. An
// interrupted child omits the summary (design §4). It is width-wrapped like a card body.
// Result is expected single-line, pre-truncated at the reduce layer; this only joins it.
func subagentDoneLine(c ToolCallView, width int) string {
	line := subagentTerminalVerb(c.SubStatus) + hintSeparator + plural(c.Steps, "step")
	if summary := strings.Join(c.Result, " "); summary != "" && c.SubStatus != subInterrupted {
		line += " — " + `"` + summary + `"`
	}
	return cardIndent + styles.ToolCallStyle.Render(cardConnector+line)
}

// plural renders a count with grammatical agreement on unit: "1 <unit>" (singular) for
// n == 1, "N <unit>s" (plural) otherwise. Used by the Subagent done line ("step") and
// the collapsed thinking summary ("line").
func plural(n int, unit string) string {
	if n == 1 {
		return "1 " + unit
	}
	return strconv.Itoa(n) + " " + unit + "s"
}

// nonSubagentCalls returns the calls that are NOT raw Subagent tool cards (ToolName ==
// subagentToolName). The LIVE tail suppresses the orchestrator's own raw Subagent running
// card (a generic "Subagent(Subagent)" row) because that activity is shown by the nested
// pending Subagent card instead (pendingSubagentCards → renderSubagentCard); rendering
// both would double it. It returns nil when every call is a Subagent call (so the
// working-word headline path is also suppressed for a subagent-only step). The result is a
// fresh slice; the input is not mutated.
func nonSubagentCalls(calls []ToolCallView) []ToolCallView {
	var out []ToolCallView
	for _, c := range calls {
		if c.ToolName == subagentToolName {
			continue
		}
		out = append(out, c)
	}
	return out
}

// renderLiveAssistant renders the in-progress (live) assistant segment with the
// animation state threaded in: the leading bullet blinks (liveDot) and a still-running
// tool card's glyph cycles through the spinner (spinnerGlyph), while resolved cards
// keep their static ✓/✗. It mirrors renderAssistant's ordering (thinking → narration
// → cards) but is the LIVE path ONLY — the committed renderAssistant stays static and
// is never given an anim. Empty parts are omitted.
//
// calls is the NON-Subagent live tool list (the caller filters the raw Subagent call out
// via nonSubagentCalls); subagentCards are the in-flight nested Subagent cards
// (pendingSubagentCards), each rendered as its own "●"-level card (renderSubagentCard)
// below the ordinary calls, separated by a blank line. The working-word headline shows
// ONLY when there is no narration AND at least one ORDINARY call (len(calls) > 0) — a step
// that only spawned subagents does NOT show "◦ Whirring", its activity is the nested card.
func renderLiveAssistant(thinking, text string, calls, subagentCards []ToolCallView, expand bool, width int, a animState) string {
	var b strings.Builder

	if t := renderThinking(thinking, expand, width); t != "" {
		b.WriteString(t)
	}

	body := renderMDDot(text, width, liveDot(a.blink))
	if body == "" && len(calls) > 0 {
		// Live empty-text tool step: a rotating working-word beside the blinking dot —
		// the provisional, pre-StepDone counterpart of the committed promoted-tool /
		// "Multiple actions" headline. The word may rotate while the step runs. It is
		// keyed on the NON-Subagent calls, so a subagent-only step shows no working-word.
		body = strings.TrimRight(liveDot(a.blink), " ") + " " + styles.HeadlineStyle.Render(workingWord(a.frame))
	}
	if body != "" {
		if b.Len() > 0 {
			b.WriteString("\n\n")
		}
		b.WriteString(body)
	}

	if len(calls) > 0 {
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		// liveRunning=true: a still-running card renders header-only in the live tail
		// so the live→committed handoff is a one-line→full-card continuation, not a
		// multi-line live shrink (see renderToolCard).
		b.WriteString(renderToolCallsGlyph(calls, expand, width, liveToolGlyph(a.frame), true))
	}

	// Each pending subagent card is its OWN "●"-level card (like the committed form),
	// separated by a blank line from whatever precedes it.
	for i := range subagentCards {
		if b.Len() > 0 {
			b.WriteString("\n\n")
		}
		b.WriteString(renderSubagentCard(subagentCards[i], expand, width))
	}
	return b.String()
}

// liveToolGlyph returns a status→glyph resolver for the LIVE path: a running call
// shows the animated spinner cell for frame; every other (resolved) status falls
// through to the static toolGlyph. It closes over frame so renderToolCallsGlyph can
// stay frame-agnostic.
func liveToolGlyph(frame uint) func(ToolStatus) string {
	return func(s ToolStatus) string {
		if s == ToolRunning {
			return spinnerGlyph(frame)
		}
		return toolGlyph(s)
	}
}

// barWidth is the display columns a left-bar prefix ("▌ " / "│ ") consumes.
const barWidth = 2

// renderUser renders a committed user message as MARKDOWN behind the gray "▌ " rail:
// the text goes through the same glamour renderer as assistant narration (so a fenced
// file attachment shows as a code block, and lists/headings/inline-code/links render),
// then every output line is prefixed with the bar so the message reads as the user's
// with its left-accent identity. This is display-only — the literal text is what was
// sent to the model. On a glamour construction/render error it falls back to the raw,
// width-wrapped text behind the bar (bad markdown shows as-is).
func renderUser(text string, width int) string {
	return renderMDRail(text, width, styles.AccentBarStyle.Render(styles.AccentBarPrompt))
}

// renderMDRail renders md to ANSI (glamour) and prefixes EVERY line with bar — a left
// rail down the whole block. It dedents glamour's document margin first so the content
// sits flush behind the bar (bar is barWidth == dotWidth columns), and falls back to
// the raw, width-wrapped text behind the bar on any glamour error.
func renderMDRail(md string, width int, bar string) string {
	if strings.TrimSpace(md) == "" {
		return ""
	}
	raw := func() string {
		var out []string
		for _, para := range strings.Split(md, "\n") {
			for _, line := range wrapToWidth(para, width-barWidth) {
				out = append(out, bar+line)
			}
		}
		return strings.Join(out, "\n")
	}
	r, err := styles.NewMarkdownRenderer(max(0, width-barWidth))
	if err != nil {
		return raw()
	}
	out, err := r.Render(md)
	if err != nil {
		return raw()
	}
	lines := dedentDocument(out)
	for i := range lines {
		lines[i] = bar + lines[i]
	}
	return strings.Join(lines, "\n")
}

// renderQueued renders the pending queued-input affordances as compact, DIM
// user-style lines — the transient "this is queued, not yet running" hint shown
// below the live tail until the loop's TurnStarted/TurnFoldedInto promotes each to
// a committed user row (or InputCancelled/TurnRejected drops it). Each queued
// message's first line of text is prefixed with the gray "▌ " accent bar and
// rendered faint (QueuedStyle), so it reads as a quieter echo of the bold
// committed user row. Empty queued yields "" so the surface omits the region. It is
// deliberately a one-line-per-message preview, not a full word-wrap: this is a
// throwaway affordance, never committed scrollback.
func renderQueued(messages [][]content.Block, width int) string {
	if len(messages) == 0 {
		return ""
	}
	bar := styles.AccentBarStyle.Render(styles.AccentBarPrompt)
	var out []string
	for _, blocks := range messages {
		text := firstLine(renderInlineBlocks(blocks))
		for _, line := range wrapToWidth(text, width-barWidth) {
			out = append(out, bar+styles.QueuedStyle.Render(line))
		}
	}
	return strings.Join(out, "\n")
}

// firstLine returns the first "\n"-delimited line of s (s itself when single-line).
// The queued affordance previews only the first line of a multi-line submission —
// it is a compact pending hint, not the full committed row.
func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// thinkingRail is the left-rail margin ("│ ") that prefixes EVERY line of the
// expanded thinking block — header included — so the block renders as one unbroken
// vertical rail attaching the reasoning to the assistant turn it precedes.
const thinkingRail = "│ "

// renderThinking renders the model's reasoning under the unified ctrl+t expand
// flag. When expanded it renders a dim block whose every line carries the "│ " left
// rail: a "│ thinking" header followed by "│ "-prefixed, width-wrapped reasoning
// lines — an unbroken rail down the left margin. When collapsed it renders a single
// compact dim summary line ("thinking · N lines · ctrl+t", N = number of thinking
// content lines, singularised to "1 line" for one line) — a one-liner, so no rail.
// Empty/whitespace-only reasoning renders nothing in either mode.
func renderThinking(s string, expand bool, width int) string {
	s = strings.TrimSpace(s) // drop the model's leading/trailing blank reasoning lines
	if s == "" {
		return ""
	}
	if !expand {
		n := strings.Count(s, "\n") + 1 // thinking content lines
		summary := styles.ThinkingHeader + hintSeparator + plural(n, "line") + hintSeparator + expandHint
		return styles.ThinkingStyle.Render(summary)
	}
	out := []string{styles.ThinkingStyle.Render(thinkingRail + styles.ThinkingHeader)}
	for _, raw := range strings.Split(s, "\n") {
		for _, line := range wrapToWidth(raw, width-barWidth) {
			out = append(out, styles.ThinkingStyle.Render(thinkingRail+line))
		}
	}
	return strings.Join(out, "\n")
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
// Used by single-block roles (the leveled notice).
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
