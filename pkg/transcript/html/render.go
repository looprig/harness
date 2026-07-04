// Package html renders a reconstructed transcript.Session to a single
// self-contained HTML document: inline <style>/<script>, no external assets, so
// the file opens offline. Markdown in message text is rendered by goldmark (GFM
// extension) with raw-HTML passthrough disabled — the safe configuration — and
// placed via template.HTML only after goldmark has escaped it; every other dynamic
// value (tool input/result, gate chips, notices, warnings, system prompts) flows
// through html/template contextual auto-escaping, never goldmark.
package html

import (
	"bytes"
	"encoding/json"
	"html/template"
	"io"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/looprig/core/content"
	"github.com/looprig/harness/pkg/tool"
	"github.com/looprig/harness/pkg/transcript"
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/extension"
)

// timestampLayout formats the absolute session timestamps; clockLayout formats
// per-message times. Both are fixed layouts: the renderer never reads a clock, so
// output is byte-deterministic for a given model.
const (
	timestampLayout = "2006-01-02 15:04:05 MST"
	clockLayout     = "15:04:05"
)

const fallbackTitleIDLen = 8

// resultByteCap bounds how many bytes of a tool result are rendered into the page.
// Output beyond the cap is dropped with a "… N bytes elided" note — the full text
// always remains in the journal. 16 KiB keeps a pathological tool result from
// bloating the export while comfortably fitting real file reads / test logs.
const resultByteCap = 16 << 10

// inlineLimit bounds a single-line value (a gate answer) folded into a chip.
const inlineLimit = 160

// markdown is the shared, safe GitHub-Flavored-Markdown renderer. The GFM
// extension adds tables, strikethrough, autolinks and task lists; it is created
// WITHOUT the renderer's WithUnsafe option, so goldmark omits raw HTML — the XSS
// boundary (Decision 11). renderMarkdown is the single chokepoint through which all
// message text flows; TestRenderMarkdownXSS and FuzzRenderMarkdown prove raw HTML
// can never survive as live markup. Tool input/result never pass through here.
var markdown = goldmark.New(goldmark.WithExtensions(extension.GFM))

// pageTemplate is parsed once from the embedded template at package init; a parse
// failure is a programmer error in the embedded asset, so a panic via Must is
// correct.
var pageTemplate = template.Must(template.New("transcript").Parse(templateSource))

// pageView is the root view model derived from a *transcript.Session. It carries
// only the presentation-ready fields the template needs, decoupling the template
// from the model's shape.
type pageView struct {
	SessionID  string
	Title      string
	ModelID    string
	AgentKind  string
	Posture    string
	TurnCount  int
	ToolCount  int
	GateCount  int
	StartedAt  string
	EndedAt    string
	ExportedAt string
	Styles     template.CSS
	Script     template.JS
	Loop       *loopView
	Notices    []noticeView
	Warnings   []warningView
}

// loopView is one agent loop's view model: its agent name, its (optional) system
// prompt, and its turns. Depth is the loop's nesting level (root = 0); it only
// feeds the data-depth attribute, a test/inspection hook — the VISUAL indentation
// of a nested subagent comes from compounding .subagent left-padding in the CSS,
// not from this number. A subagent's child loop is projected at Depth+1 via
// newToolView -> newLoopView.
type loopView struct {
	AgentName    string
	Depth        int
	SystemPrompt string
	Turns        []turnView
}

// turnView is one turn: its index, the user message, and the AI steps it drove.
// User is empty (and the template omits the user block) when the turn carries no
// user message.
type turnView struct {
	Index int
	At    string
	User  template.HTML
	Steps []stepView
}

// stepView is one model step: the AI message and thinking (each rendered from
// markdown), the tool cards it requested, and the user-action gate chips raised
// during the step.
type stepView struct {
	AgentName string
	At        string
	AI        template.HTML
	Thinking  template.HTML
	Tools     []toolView
	Gates     []gateView
}

// toolView is one tool invocation card: the tool name, its pretty-printed input
// JSON and (capped) result text — both rendered as escaped, NON-markdown text — an
// optional decision verb from a bound gate, and an optional nested child loop.
type toolView struct {
	Name          string
	InputJSON     string
	Result        string
	ResultElided  string
	IsError       bool
	DecisionVerb  string
	DecisionClass string
	At            string
	Child         *loopView
}

// gateView is one user-action notification chip: the human-readable text and a CSS
// class keying its color (approved / denied / answered / pending).
type gateView struct {
	Class string
	Text  string
}

// noticeView is one session-lifecycle notice: a short label, the notice text, the
// time, and a CSS class.
type noticeView struct {
	Label string
	Class string
	Text  string
	At    string
}

// warningView is one reconstruction warning surfaced into the document.
type warningView struct {
	Text string
	At   string
}

// Render writes s to w as one self-contained HTML document. It reads no clock:
// every timestamp comes from the model. On a template-execution or write failure
// it returns a *RenderError wrapping the cause; the model itself is treated as
// already-validated (reconstruction degraded anomalies to warnings upstream).
func Render(w io.Writer, s *transcript.Session) error {
	view, err := newPageView(s)
	if err != nil {
		return &RenderError{Cause: err}
	}
	var buf bytes.Buffer
	if err := pageTemplate.Execute(&buf, view); err != nil {
		return &RenderError{Cause: err}
	}
	if _, err := w.Write(buf.Bytes()); err != nil {
		return &RenderError{Cause: err}
	}
	return nil
}

// newPageView projects a session into the root view model, rendering message
// markdown and computing the header counts along the way. It returns an error only
// if markdown rendering fails.
func newPageView(s *transcript.Session) (pageView, error) {
	turns, tools, gates := countLoop(s.Root)
	pv := pageView{
		SessionID:  s.SessionID.String(),
		Title:      displayTitle(s),
		ModelID:    s.Config.ModelID,
		AgentKind:  s.Config.AgentKind,
		Posture:    s.Config.PermissionPosture,
		TurnCount:  turns,
		ToolCount:  tools,
		GateCount:  gates,
		StartedAt:  formatTimestamp(s.StartedAt),
		EndedAt:    formatTimestamp(s.EndedAt),
		ExportedAt: formatTimestamp(s.ExportedAt),
		Styles:     template.CSS(stylesCSS), // #nosec G203 -- embedded compile-time CSS, never session data.
		Script:     template.JS(appJS),      // #nosec G203 -- embedded compile-time JS, never session data.
	}
	if s.Root != nil {
		lv, err := newLoopView(s.Root, 0)
		if err != nil {
			return pageView{}, err
		}
		pv.Loop = &lv
	}
	for _, n := range s.Notices {
		pv.Notices = append(pv.Notices, makeNoticeView(n))
	}
	for _, w := range s.Warnings {
		pv.Warnings = append(pv.Warnings, warningView{Text: w.Text, At: formatClock(w.At)})
	}
	return pv, nil
}

func displayTitle(s *transcript.Session) string {
	if title := strings.TrimSpace(s.Title); title != "" {
		return title
	}
	id := s.SessionID.String()
	if len(id) > fallbackTitleIDLen {
		id = id[:fallbackTitleIDLen]
	}
	return "Session " + id
}

// countLoop totals the turns, tool calls and gate actions across a loop and every
// subagent loop nested beneath it (Definition: counts over all loops).
func countLoop(l *transcript.Loop) (turns, tools, gates int) {
	if l == nil {
		return 0, 0, 0
	}
	for _, t := range l.Turns {
		turns++
		for _, step := range t.Steps {
			gates += len(step.Gates)
			for _, tc := range step.Tools {
				tools++
				if tc.Child != nil {
					ct, cto, cg := countLoop(tc.Child)
					turns += ct
					tools += cto
					gates += cg
				}
			}
		}
	}
	return turns, tools, gates
}

// newLoopView projects a loop and its turns into a view model. depth is the loop's
// nesting level (the root loop is 0); newToolView passes depth+1 when recursing
// into a subagent's child loop.
func newLoopView(l *transcript.Loop, depth int) (loopView, error) {
	lv := loopView{AgentName: l.AgentName, Depth: depth, SystemPrompt: l.SystemPrompt}
	for _, turn := range l.Turns {
		tv, err := newTurnView(l.AgentName, turn, depth)
		if err != nil {
			return loopView{}, err
		}
		lv.Turns = append(lv.Turns, tv)
	}
	return lv, nil
}

// newTurnView projects a turn and its steps into a view model. agentName is the
// owning loop's agent name, used to label each AI step.
func newTurnView(agentName string, t *transcript.Turn, depth int) (turnView, error) {
	userHTML, err := renderMarkdown(messageText(t.User))
	if err != nil {
		return turnView{}, err
	}
	tv := turnView{
		Index: int(t.Index),
		At:    formatClock(messageTime(t.User, t.StartedAt)),
		User:  userHTML,
	}
	for _, step := range t.Steps {
		sv, err := newStepView(agentName, step, t.StartedAt, depth)
		if err != nil {
			return turnView{}, err
		}
		tv.Steps = append(tv.Steps, sv)
	}
	return tv, nil
}

// newStepView projects a step into a view model: the AI prose and thinking through
// the markdown seam, the tool cards, and the user-action chips. fallback is the
// owning turn's start time, used when the AI message has no timestamp.
func newStepView(agentName string, step *transcript.Step, fallback time.Time, depth int) (stepView, error) {
	aiHTML, err := renderMarkdown(messageText(step.AI))
	if err != nil {
		return stepView{}, err
	}
	thinkHTML, err := renderMarkdown(thinkingText(step.AI))
	if err != nil {
		return stepView{}, err
	}
	sv := stepView{
		AgentName: agentName,
		At:        formatClock(messageTime(step.AI, fallback)),
		AI:        aiHTML,
		Thinking:  thinkHTML,
	}
	for _, tc := range step.Tools {
		toolV, err := newToolView(tc, depth)
		if err != nil {
			return stepView{}, err
		}
		sv.Tools = append(sv.Tools, toolV)
	}
	for _, g := range step.Gates {
		sv.Gates = append(sv.Gates, gateChip(g))
	}
	return sv, nil
}

// newToolView projects a tool call into a card view model. Input is pretty-printed
// JSON and Result is the joined result text — both kept as plain strings so the
// template auto-escapes them (tool output is NOT markdown). A bound gate supplies
// the decision verb; a child loop recurses at depth+1 for inline nesting.
func newToolView(tc *transcript.ToolCall, depth int) (toolView, error) {
	shown, elided := truncateForDisplay(resultText(tc.Result))
	tv := toolView{
		Name:      tc.Name,
		InputJSON: prettyJSON(tc.Input),
		Result:    shown,
		IsError:   tc.IsError,
		At:        formatClock(tc.At),
	}
	if elided > 0 {
		tv.ResultElided = "… " + strconv.Itoa(elided) + " bytes elided"
	}
	if tc.Gate != nil {
		tv.DecisionVerb, tv.DecisionClass = decisionVerb(tc.Gate)
	}
	if tc.Child != nil {
		child, err := newLoopView(tc.Child, depth+1)
		if err != nil {
			return toolView{}, err
		}
		tv.Child = &child
	}
	return tv, nil
}

// decisionVerb maps a gate's resolution to the card badge verb and its CSS class.
func decisionVerb(g *transcript.GateAction) (verb, class string) {
	switch g.Decision {
	case transcript.DecisionApproved:
		return "Approved ✓", "approved"
	case transcript.DecisionDenied:
		return "Denied ✗", "denied"
	case transcript.DecisionAnswered:
		return "Answered ✓", "answered"
	default:
		return "Pending …", "pending"
	}
}

// gateChip renders one user action as a notification chip, visually distinct from
// agent content. Scope is included only for an approval (the sole gate kind for
// which it is meaningful).
func gateChip(g *transcript.GateAction) gateView {
	at := formatClock(g.DecidedAt)
	switch g.Decision {
	case transcript.DecisionApproved:
		return gateView{Class: "approved", Text: "You approved · " + scopeName(g.Scope) + " · " + at}
	case transcript.DecisionDenied:
		return gateView{Class: "denied", Text: "You denied · " + at}
	case transcript.DecisionAnswered:
		text := "You answered · " + at
		if q := inlineText(g.Question); q != "" {
			text += " — \"" + q + "\" → " + inlineText(g.Answer)
		} else {
			text += ": " + inlineText(g.Answer)
		}
		return gateView{Class: "answered", Text: text}
	default:
		return gateView{Class: "pending", Text: "Awaiting your response · " + formatClock(g.OpenedAt)}
	}
}

// scopeName renders an approval scope as the breadth word shown in a chip.
func scopeName(s tool.ApprovalScope) string {
	switch s {
	case tool.ScopeOnce:
		return "once"
	case tool.ScopeSession:
		return "session"
	case tool.ScopeWorkspace:
		return "workspace"
	default:
		return "unknown"
	}
}

// makeNoticeView projects a session notice into its view model.
func makeNoticeView(n transcript.Notice) noticeView {
	label, class := noticeMeta(n.Kind)
	return noticeView{Label: label, Class: class, Text: n.Text, At: formatClock(n.At)}
}

// noticeMeta maps a notice kind to its short label and CSS class.
func noticeMeta(k transcript.NoticeKind) (label, class string) {
	switch k {
	case transcript.NoticeSessionActive:
		return "active", "active"
	case transcript.NoticeSessionIdle:
		return "idle", "idle"
	case transcript.NoticeSessionStopped:
		return "stopped", "stopped"
	case transcript.NoticeRestoreStarted:
		return "restore", "restore"
	case transcript.NoticeRestoreDone:
		return "restore", "restore"
	case transcript.NoticeRestoreErrored:
		return "restore error", "error"
	default:
		return "notice", "notice"
	}
}

// joinTextBlocks newline-joins the strings the selector extracts from each block,
// skipping blocks the selector rejects. It is the single loop behind messageText,
// thinkingText and resultText — each just supplies the block type it wants.
func joinTextBlocks(blocks []content.Block, selectText func(content.Block) (string, bool)) string {
	var b strings.Builder
	for _, blk := range blocks {
		text, ok := selectText(blk)
		if !ok {
			continue
		}
		if b.Len() > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(text)
	}
	return b.String()
}

// textBlockText selects a *content.TextBlock's text, used by messageText/resultText.
func textBlockText(blk content.Block) (string, bool) {
	tb, ok := blk.(*content.TextBlock)
	if !ok {
		return "", false
	}
	return tb.Text, true
}

// messageText concatenates the text of a message's TextBlocks, newline-joined.
// Non-text blocks (images, thinking, tool use) are skipped; thinking is surfaced
// separately via thinkingText. A nil message yields "".
func messageText(m *transcript.Message) string {
	if m == nil {
		return ""
	}
	return joinTextBlocks(m.Blocks, textBlockText)
}

// thinkingText concatenates the reasoning of a message's ThinkingBlocks,
// newline-joined. A nil message (or one with no thinking) yields "".
func thinkingText(m *transcript.Message) string {
	if m == nil {
		return ""
	}
	return joinTextBlocks(m.Blocks, func(blk content.Block) (string, bool) {
		tb, ok := blk.(*content.ThinkingBlock)
		if !ok {
			return "", false
		}
		return tb.Thinking, true
	})
}

// resultText concatenates the text of a tool result's TextBlocks, newline-joined.
// Non-text result blocks are skipped. The returned string is rendered as escaped,
// non-markdown text by the template.
func resultText(blocks []content.Block) string {
	return joinTextBlocks(blocks, textBlockText)
}

// prettyJSON re-indents raw JSON for display. It operates on the raw bytes
// (json.Indent preserves key order), so output is deterministic. Invalid JSON is
// returned as-is — the template escapes it either way.
func prettyJSON(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var buf bytes.Buffer
	if err := json.Indent(&buf, raw, "", "  "); err != nil {
		return string(raw)
	}
	return buf.String()
}

// truncateForDisplay caps s at resultByteCap bytes, backing off to a UTF-8 rune
// boundary so a multibyte rune is never split. It returns the shown prefix and the
// number of bytes elided (0 when nothing was cut).
func truncateForDisplay(s string) (shown string, elided int) {
	if len(s) <= resultByteCap {
		return s, 0
	}
	cut := resultByteCap
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut], len(s) - cut
}

// inlineText collapses all whitespace in s to single spaces and caps it at
// inlineLimit bytes (backing off to a rune boundary), appending an ellipsis when
// truncated. Used to fold a gate answer onto a single chip line.
func inlineText(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) <= inlineLimit {
		return s
	}
	cut := inlineLimit
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}

// messageTime returns the message's own timestamp, falling back to a default when
// the message is nil or its time is zero.
func messageTime(m *transcript.Message, fallback time.Time) time.Time {
	if m != nil && !m.At.IsZero() {
		return m.At
	}
	return fallback
}

// renderMarkdown converts CommonMark source to safe HTML via the shared goldmark
// renderer. The result is goldmark-escaped (raw HTML passthrough is off), so it is
// safe to mark as template.HTML. Empty input yields empty HTML.
func renderMarkdown(src string) (template.HTML, error) {
	if src == "" {
		return "", nil
	}
	var buf bytes.Buffer
	if err := markdown.Convert([]byte(src), &buf); err != nil {
		return "", err
	}
	return template.HTML(buf.String()), nil // #nosec G203 -- goldmark escapes raw HTML; XSS tests and fuzz pin this boundary.
}

// formatTimestamp renders an absolute timestamp deterministically; a zero time
// renders as an em dash.
func formatTimestamp(t time.Time) string {
	if t.IsZero() {
		return "—"
	}
	return t.Format(timestampLayout)
}

// formatClock renders a wall-clock time deterministically; a zero time renders as
// an empty string.
func formatClock(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Format(clockLayout)
}
