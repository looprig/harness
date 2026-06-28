// Package html renders a reconstructed transcript.Session to a single
// self-contained HTML document: inline <style>/<script>, no external assets, so
// the file opens offline. Markdown in message text is rendered by goldmark with
// raw-HTML passthrough disabled (the default, safe configuration) and placed via
// template.HTML only after goldmark has escaped it; every other dynamic value
// flows through html/template contextual auto-escaping.
package html

import (
	"bytes"
	"html/template"
	"io"
	"strings"
	"time"

	"github.com/ciram-co/looprig/pkg/content"
	"github.com/ciram-co/looprig/pkg/transcript"
	"github.com/yuin/goldmark"
)

// timestampLayout formats the absolute session timestamps; clockLayout formats
// per-message times. Both are fixed layouts: the renderer never reads a clock, so
// output is byte-deterministic for a given model.
const (
	timestampLayout = "2006-01-02 15:04:05 MST"
	clockLayout     = "15:04:05"
)

// markdown is the shared, safe CommonMark renderer. It is created WITHOUT
// html.WithUnsafe(): goldmark's default omits raw HTML, which is the XSS boundary
// (Task 8 hardens and tests this; the safe config is wired here so the dependency
// is justified and the minimal golden shows rendered markdown).
var markdown = goldmark.New()

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
	StartedAt  string
	EndedAt    string
	ExportedAt string
	Styles     template.CSS
	Script     template.JS
	Loop       *loopView
}

// loopView is one agent loop's view model: its agent name and its turns. Depth is
// the loop's nesting level (root = 0); it drives the data-depth attribute so Task
// 9 can recurse into ToolCall.Child with Depth+1 and indent nested subagents.
type loopView struct {
	AgentName string
	Depth     int
	Turns     []turnView
}

// turnView is one turn: its index, the user message, and the AI steps it drove.
type turnView struct {
	Index int
	At    string
	User  template.HTML
	Steps []stepView
}

// stepView is one model step: the AI message, rendered from markdown.
type stepView struct {
	AgentName string
	At        string
	AI        template.HTML
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
// markdown along the way. It returns an error only if markdown rendering fails.
func newPageView(s *transcript.Session) (pageView, error) {
	pv := pageView{
		SessionID:  s.SessionID.String(),
		Title:      s.Title,
		ModelID:    s.Config.ModelID,
		AgentKind:  s.Config.AgentKind,
		Posture:    s.Config.PermissionPosture,
		StartedAt:  formatTimestamp(s.StartedAt),
		EndedAt:    formatTimestamp(s.EndedAt),
		ExportedAt: formatTimestamp(s.ExportedAt),
		Styles:     template.CSS(stylesCSS),
		Script:     template.JS(appJS),
	}
	if s.Root != nil {
		lv, err := newLoopView(s.Root, 0)
		if err != nil {
			return pageView{}, err
		}
		pv.Loop = &lv
	}
	return pv, nil
}

// newLoopView projects a loop and its turns into a view model. depth is the loop's
// nesting level (the root loop is 0); Task 9 will pass depth+1 when recursing into
// a subagent's child loop.
func newLoopView(l *transcript.Loop, depth int) (loopView, error) {
	lv := loopView{AgentName: l.AgentName, Depth: depth}
	for _, turn := range l.Turns {
		tv, err := newTurnView(l.AgentName, turn)
		if err != nil {
			return loopView{}, err
		}
		lv.Turns = append(lv.Turns, tv)
	}
	return lv, nil
}

// newTurnView projects a turn and its steps into a view model. agentName is the
// owning loop's agent name, used to label each AI step.
func newTurnView(agentName string, t *transcript.Turn) (turnView, error) {
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
		aiHTML, err := renderMarkdown(messageText(step.AI))
		if err != nil {
			return turnView{}, err
		}
		tv.Steps = append(tv.Steps, stepView{
			// AgentName is intentionally denormalized onto every step: it is
			// constant within a loop, not per-step data. Copying it here keeps
			// the AI-message label local to the step view (convenient once Task 9
			// labels nested-subagent steps) at the cost of trivial duplication.
			AgentName: agentName,
			At:        formatClock(messageTime(step.AI, t.StartedAt)),
			AI:        aiHTML,
		})
	}
	return tv, nil
}

// messageText concatenates the text of a message's TextBlocks, newline-joined.
// Non-text blocks (images, thinking, tool use) are skipped here; the minimal
// skeleton renders only message prose. A nil message yields "".
func messageText(m *transcript.Message) string {
	if m == nil {
		return ""
	}
	var b strings.Builder
	for _, blk := range m.Blocks {
		tb, ok := blk.(*content.TextBlock)
		if !ok {
			continue
		}
		if b.Len() > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(tb.Text)
	}
	return b.String()
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
	return template.HTML(buf.String()), nil
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
