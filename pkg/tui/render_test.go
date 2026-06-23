package tui

import (
	"regexp"
	"strings"
	"testing"

	"github.com/ciram-co/looprig/pkg/tui/styles"
)

// ansiSGR matches ANSI SGR (color/style) escape sequences. The markdown renderer
// emits per-word color spans, so substring assertions on narration text must strip
// styling first — they verify rendered TEXT, not the incidental color codes (which
// depend on the runtime color profile and would otherwise split words like
// "reading config" across two escapes).
var ansiSGR = regexp.MustCompile("\x1b\\[[0-9;]*m")

// stripANSI removes SGR escape sequences so content assertions match the visible text.
func stripANSI(s string) string { return ansiSGR.ReplaceAllString(s, "") }

func TestRenderMD(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		md       string
		width    int
		wantWord string // substring expected in the output (empty → expect blank)
	}{
		{name: "happy path", md: "hello world", width: 80, wantWord: "hello"},
		{name: "heading", md: "# Title here", width: 80, wantWord: "Title"},
		{name: "narrow width", md: "wrapme please", width: 10, wantWord: "wrapme"},
		{name: "empty", md: "", width: 80, wantWord: ""},
		{name: "zero width", md: "zerowidth", width: 0, wantWord: "zerowidth"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := stripANSI(renderMD(tt.md, tt.width))
			if tt.wantWord == "" {
				if strings.TrimSpace(got) != "" {
					t.Errorf("renderMD(%q) = %q, want empty/whitespace", tt.md, got)
				}
				return
			}
			if !strings.Contains(got, tt.wantWord) {
				t.Errorf("renderMD(%q) = %q, want to contain %q", tt.md, got, tt.wantWord)
			}
		})
	}
}

// TestRenderMDAlignsWithDot covers aligning the AI message with its bullet: the
// narration starts on the SAME line as the "●" dot, not the dot alone with the text
// indented on the next line.
func TestRenderMDAlignsWithDot(t *testing.T) {
	t.Parallel()

	got := stripANSI(renderMD("Hello there friend", 60))
	first := got
	if i := strings.IndexByte(got, '\n'); i >= 0 {
		first = got[:i]
	}
	if !strings.HasPrefix(first, styles.Dot) {
		t.Errorf("first line = %q, want it to start with the dot %q", first, styles.Dot)
	}
	if !strings.Contains(first, "Hello there friend") {
		t.Errorf("first line = %q, want the narration on the same line as the dot", first)
	}
}

// makeLines returns a slice of n distinct result lines ("line0".."lineN-1").
func makeLines(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = "line" + itoa(i)
	}
	return out
}

// itoa is a tiny base-10 int→string for test fixtures (avoids importing strconv
// just for the table builder).
func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b [20]byte
	p := len(b)
	for i > 0 {
		p--
		b[p] = byte('0' + i%10)
		i /= 10
	}
	return string(b[p:])
}

// TestToolGlyph covers the status→glyph mapping (design §3).
func TestToolGlyph(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		status ToolStatus
		want   string
	}{
		{name: "running", status: ToolRunning, want: glyphRunning},
		{name: "ok", status: ToolOK, want: glyphOK},
		{name: "error", status: ToolError, want: glyphError},
		{name: "cancelled", status: ToolCancelled, want: glyphCancelled},
		{name: "unknown falls back to running glyph", status: ToolStatus(99), want: glyphRunning},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := toolGlyph(tt.status); got != tt.want {
				t.Errorf("toolGlyph(%d) = %q, want %q", tt.status, got, tt.want)
			}
		})
	}
}

// TestRenderToolCalls covers card rendering: glyphs, collapsed vs expanded preview,
// the truncation marker, (no output), error-always-shown, multi-card batches, and
// width wrapping (design §3).
func TestRenderToolCalls(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		calls       []ToolCallView
		expandTools bool
		width       int
		want        []string // substrings that must appear
		absent      []string // substrings that must NOT appear
	}{
		{
			name:  "running card shows running glyph and name+summary",
			calls: []ToolCallView{{ToolName: "ReadFile", Summary: "config.yaml", Status: ToolRunning}},
			width: 80,
			want:  []string{"ReadFile", "config.yaml", glyphRunning},
		},
		{
			name:  "ok glyph",
			calls: []ToolCallView{{ToolName: "ReadFile", Status: ToolOK}},
			width: 80,
			want:  []string{glyphOK},
		},
		{
			name:  "error glyph",
			calls: []ToolCallView{{ToolName: "Bash", Status: ToolError, Result: []string{"boom"}}},
			width: 80,
			want:  []string{glyphError},
		},
		{
			name:  "cancelled glyph",
			calls: []ToolCallView{{ToolName: "Bash", Status: ToolCancelled}},
			width: 80,
			want:  []string{glyphCancelled},
		},
		{
			name:        "collapsed shows first K lines and a more-marker",
			calls:       []ToolCallView{{ToolName: "ReadFile", Status: ToolOK, Result: makeLines(10)}},
			expandTools: false,
			width:       80,
			// K = 6 → lines 0..5 shown, lines 6..9 hidden, "4 more" marker.
			want:   []string{"line0", "line5", "4 more lines", "ctrl+t"},
			absent: []string{"line6", "line9"},
		},
		{
			name:        "expanded shows all lines and no marker",
			calls:       []ToolCallView{{ToolName: "ReadFile", Status: ToolOK, Result: makeLines(10)}},
			expandTools: true,
			width:       80,
			want:        []string{"line0", "line6", "line9"},
			absent:      []string{"more lines"},
		},
		{
			name:        "exactly K lines shows all with no marker",
			calls:       []ToolCallView{{ToolName: "ReadFile", Status: ToolOK, Result: makeLines(previewLineCap)}},
			expandTools: false,
			width:       80,
			want:        []string{"line0", "line5"},
			absent:      []string{"more lines"},
		},
		{
			name:   "empty result shows (no output)",
			calls:  []ToolCallView{{ToolName: "Noop", Status: ToolOK, Result: nil}},
			width:  80,
			want:   []string{noOutput},
			absent: []string{"more lines"},
		},
		{
			name:        "error card shows its result even collapsed",
			calls:       []ToolCallView{{ToolName: "Bash", Status: ToolError, Result: []string{"error: permission denied"}}},
			expandTools: false,
			width:       80,
			want:        []string{glyphError, "error: permission denied"},
		},
		{
			name: "parallel batch renders all cards",
			calls: []ToolCallView{
				{ToolName: "ReadFile", Summary: "a.go", Status: ToolOK, Result: []string{"alpha"}},
				{ToolName: "Bash", Summary: "ls", Status: ToolOK, Result: []string{"beta"}},
			},
			width: 80,
			want:  []string{"ReadFile", "Bash", "alpha", "beta"},
		},
		{
			name:  "no calls renders empty",
			calls: nil,
			width: 80,
		},
		{
			name:        "long result line is width-wrapped",
			calls:       []ToolCallView{{ToolName: "Bash", Status: ToolOK, Result: []string{"aaaa bbbb cccc dddd eeee ffff gggg"}}},
			expandTools: true,
			width:       20,
			// At width 20 the line cannot fit on one row → at least one wrap newline.
			want: []string{"aaaa", "gggg"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := renderToolCalls(tt.calls, tt.expandTools, tt.width)
			if len(tt.calls) == 0 {
				if got != "" {
					t.Errorf("renderToolCalls(nil) = %q, want empty", got)
				}
				return
			}
			for _, w := range tt.want {
				if !strings.Contains(got, w) {
					t.Errorf("renderToolCalls() = %q, want to contain %q", got, w)
				}
			}
			for _, a := range tt.absent {
				if strings.Contains(got, a) {
					t.Errorf("renderToolCalls() = %q, want to NOT contain %q", got, a)
				}
			}
		})
	}
}

// TestRenderToolCallsWidthWrap asserts a long result line actually breaks onto
// multiple display rows when the width is too small to hold it.
func TestRenderToolCallsWidthWrap(t *testing.T) {
	t.Parallel()

	calls := []ToolCallView{{ToolName: "Bash", Status: ToolOK, Result: []string{"aaaa bbbb cccc dddd eeee ffff gggg hhhh"}}}
	narrow := renderToolCalls(calls, true, 16)
	wide := renderToolCalls(calls, true, 200)

	narrowRows := strings.Count(narrow, "\n")
	wideRows := strings.Count(wide, "\n")
	if narrowRows <= wideRows {
		t.Errorf("narrow render rows = %d, wide render rows = %d; want narrow to wrap into more rows", narrowRows, wideRows)
	}
}

// TestRenderAssistantNestsCards covers an assistant segment rendering its markdown
// text followed by its tool-call cards indented beneath; a segment with empty text
// but cards renders a bare dot bullet plus its cards (no empty markdown block); and
// a text-only segment carries no card connector. This exercises the live
// renderAssistant primitive that the kindAssistant entry render (entryrender.go) drives.
func TestRenderAssistantNestsCards(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		thinking string
		text     string
		headline string
		want     []string
		absent   []string
	}{
		{
			name: "narration renders the dot bullet, no card connector",
			text: "let me read the config",
			want: []string{strings.TrimSpace(styles.Dot), "let me read the config"},
			// committed cards are their OWN kindTool entries — never nested here.
			absent: []string{cardConnector},
		},
		{
			name:     "empty text with a headline renders the umbrella bullet",
			headline: multipleActionsHeadline,
			want:     []string{strings.TrimSpace(styles.Dot), multipleActionsHeadline},
		},
		{
			name:     "thinking only renders the rail with no bullet",
			thinking: "mulling it over",
			want:     []string{"thinking"},
			absent:   []string{strings.TrimSpace(styles.Dot)},
		},
		{
			name:   "fully empty renders nothing",
			absent: []string{strings.TrimSpace(styles.Dot)},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := stripANSI(renderAssistant(tt.thinking, tt.text, tt.headline, false, 80))
			for _, w := range tt.want {
				if !strings.Contains(got, w) {
					t.Errorf("renderAssistant() = %q, want to contain %q", got, w)
				}
			}
			for _, a := range tt.absent {
				if strings.Contains(got, a) {
					t.Errorf("renderAssistant() = %q, want to NOT contain %q", got, a)
				}
			}
		})
	}
}

// TestRenderLiveAssistantCards covers the in-progress (LIVE) segment rendering its
// text then its tool cards (a running card shows the running glyph header-only), and a
// card-only segment with empty text rendering the working-word bullet plus its cards.
// This is the path screen.go's renderLiveTail drives.
func TestRenderLiveAssistantCards(t *testing.T) {
	t.Parallel()

	a := animState{}

	t.Run("text plus running card", func(t *testing.T) {
		t.Parallel()

		calls := []ToolCallView{{ToolName: "Bash", Summary: "ls", Status: ToolRunning}}
		got := stripANSI(renderLiveAssistant("", "checking now", calls, false, 80, a))
		for _, w := range []string{"checking now", "Bash", "ls"} {
			if !strings.Contains(got, w) {
				t.Errorf("renderLiveAssistant() = %q, want to contain %q", got, w)
			}
		}
	})

	t.Run("cards with empty text render the working-word bullet", func(t *testing.T) {
		t.Parallel()

		calls := []ToolCallView{{ToolName: "Bash", Status: ToolRunning}}
		got := stripANSI(renderLiveAssistant("", "", calls, false, 80, a))
		for _, w := range []string{strings.TrimSpace(styles.Dot), "Bash"} {
			if !strings.Contains(got, w) {
				t.Errorf("renderLiveAssistant() = %q, want to contain %q", got, w)
			}
		}
	})
}

// TestRenderLiveRunningCardIsHeaderOnly locks the live→committed handoff fix
// (Option B): a still-RUNNING tool card in the LIVE tail renders as a SINGLE compact
// header line (spinner + tool name + summary) with NO result body — not the
// "(no output)" placeholder a committed/resolved card carries. This minimises the
// live-tail height that must be removed when the card commits to scrollback, so the
// running→completed handoff composes cleanly (the committed full card replaces a
// one-line live indicator, not a multi-line live card). Resolved cards co-resident in
// the live tail (a batch sibling that finished but hasn't committed yet) keep their
// full body, and the committed scrollback path is unchanged (full card always).
func TestRenderLiveRunningCardIsHeaderOnly(t *testing.T) {
	t.Parallel()

	a := animState{}

	t.Run("running card is one header line, no body", func(t *testing.T) {
		t.Parallel()

		calls := []ToolCallView{{ToolName: "Fetch", Summary: "GET weather.com", Status: ToolRunning}}
		got := stripANSI(renderLiveAssistant("", "", calls, true, 80, a))
		// The bare bullet (●/◦) is its own line; the running card is exactly one more.
		lines := strings.Split(got, "\n")
		if len(lines) != 2 {
			t.Fatalf("live running card: got %d lines %q, want 2 (bullet + one-line card)", len(lines), lines)
		}
		card := lines[1]
		for _, w := range []string{"Fetch", "GET weather.com"} {
			if !strings.Contains(card, w) {
				t.Errorf("live running card = %q, want to contain %q", card, w)
			}
		}
		if strings.Contains(got, noOutput) {
			t.Errorf("live running card must NOT show the %q body placeholder; got %q", noOutput, got)
		}
	})

	t.Run("resolved card in live tail keeps its body", func(t *testing.T) {
		t.Parallel()

		// A finished batch sibling that has not yet committed must still show its
		// result in the live tail (it is NOT a running card).
		calls := []ToolCallView{{
			ToolName: "Bash", Summary: "ls", Status: ToolOK, Result: []string{"file-a", "file-b"},
		}}
		got := stripANSI(renderLiveAssistant("", "", calls, true, 80, a))
		for _, w := range []string{"Bash", "ls", "file-a", "file-b"} {
			if !strings.Contains(got, w) {
				t.Errorf("resolved live card = %q, want to contain %q", got, w)
			}
		}
	})

	t.Run("committed running card (defensive) keeps full body", func(t *testing.T) {
		t.Parallel()

		// The committed/scrollback path renders the full card regardless of status;
		// a (stray) running card committed at a terminal still shows its body.
		calls := []ToolCallView{{ToolName: "Fetch", Summary: "GET x", Status: ToolRunning}}
		got := stripANSI(renderToolCalls(calls, true, 80))
		if !strings.Contains(got, noOutput) {
			t.Errorf("committed running card should still show %q body; got %q", noOutput, got)
		}
	})
}

// TestRenderThinking covers the dim reasoning block under the unified ctrl+t flag.
// Expanded: EVERY line carries the "│ " left rail — the header renders as
// "│ thinking" and each body line as "│ <text>", producing an unbroken vertical
// rail down the left margin. Collapsed: a single compact summary line
// "thinking · N lines · ctrl+t" (N = number of thinking content lines, singularised
// to "1 line"), with NO "│ "-prefixed body and NO rail (it is a one-liner). Empty or
// whitespace-only input renders nothing in either mode.
func TestRenderThinking(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		in           string
		expand       bool
		wantContains []string
		wantAbsent   []string
		wantEmpty    bool
	}{
		{name: "empty renders nothing collapsed", in: "", expand: false, wantEmpty: true},
		{name: "empty renders nothing expanded", in: "", expand: true, wantEmpty: true},
		{name: "whitespace renders nothing collapsed", in: "   \n  ", expand: false, wantEmpty: true},
		{name: "whitespace renders nothing expanded", in: "   \n  ", expand: true, wantEmpty: true},
		{
			// Expanded: the header carries the rail ("│ thinking", not bare "thinking")
			// and every body line carries the rail too — an unbroken left rail.
			name:         "expanded multi-line rails every line including header",
			in:           "line one\nline two",
			expand:       true,
			wantContains: []string{"│ thinking", "│ line one", "│ line two"},
			wantAbsent:   []string{"\nthinking", "more lines"},
		},
		{
			// Collapsed two-line thinking → a compact summary mentioning "thinking",
			// the line count (2), and "ctrl+t"; the "│ "-prefixed body is hidden.
			name:         "collapsed multi-line is a compact summary line",
			in:           "line one\nline two",
			expand:       false,
			wantContains: []string{"thinking", "2 lines", "ctrl+t"},
			wantAbsent:   []string{"│ line one", "│ line two"},
		},
		{
			// A single thinking line still renders a sensible summary (count = 1),
			// singularised to "1 line" (not "1 lines").
			name:         "collapsed single line summary",
			in:           "only one line",
			expand:       false,
			wantContains: []string{"thinking", "1 line", "ctrl+t"},
			wantAbsent:   []string{"│ only one line", "1 lines"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := stripANSI(renderThinking(tt.in, tt.expand, 80))
			if tt.wantEmpty {
				if got != "" {
					t.Errorf("renderThinking(%q, %v) = %q, want empty", tt.in, tt.expand, got)
				}
				return
			}
			for _, w := range tt.wantContains {
				if !strings.Contains(got, w) {
					t.Errorf("renderThinking(%q, %v) = %q, want to contain %q", tt.in, tt.expand, got, w)
				}
			}
			for _, a := range tt.wantAbsent {
				if strings.Contains(got, a) {
					t.Errorf("renderThinking(%q, %v) = %q, want to NOT contain %q", tt.in, tt.expand, got, a)
				}
			}
		})
	}
}

// TestRenderThinkingExpandedRailOnEveryLine asserts the expanded thinking block is
// an UNBROKEN left rail: every rendered line — the header AND each body line —
// begins with the "│ " rail in the same column, so the block reads as a sub-block
// attached to the assistant turn. No line (not even the header) is left bare.
func TestRenderThinkingExpandedRailOnEveryLine(t *testing.T) {
	t.Parallel()

	const rail = "│ "
	got := stripANSI(renderThinking("line one\nline two\nline three", true, 80))
	lines := strings.Split(got, "\n")
	if len(lines) < 4 { // header + three body lines
		t.Fatalf("expanded thinking = %q, want at least 4 lines (header + body)", got)
	}
	if got, want := lines[0], rail+styles.ThinkingHeader; got != want {
		t.Errorf("header line = %q, want %q (rail on the header, not bare)", got, want)
	}
	for i, ln := range lines {
		if !strings.HasPrefix(ln, rail) {
			t.Errorf("line %d = %q, want it to start with the rail %q (unbroken rail)", i, ln, rail)
		}
	}
}

// TestRenderAssistantUnifiedExpand covers Task 12: ONE flag drives BOTH the
// thinking block and the tool-result folding. Collapsed (expand=false): thinking
// renders as the compact summary line (no "│ " body) AND the long tool result is
// folded (first K lines + "more lines" marker). Expanded (expand=true): the full
// "│ "-prefixed thinking body renders AND the tool result shows every line. The
// SAME flag flips both — there is no separate thinking key.
func TestRenderAssistantUnifiedExpand(t *testing.T) {
	t.Parallel()

	const thinking = "reason one\nreason two\nreason three"
	calls := []ToolCallView{{ToolName: "ReadFile", Status: ToolOK, Result: makeLines(10)}}

	// The thinking fold lives in renderAssistant; the tool-result fold in renderToolCalls.
	// Screen threads the SAME expand flag to both (entryrender.go), so collapsed/expanded
	// must flip them together.
	thinkCollapsed := stripANSI(renderAssistant(thinking, "the answer", "", false, 80))
	toolCollapsed := stripANSI(renderToolCalls(calls, false, 80))
	thinkExpanded := stripANSI(renderAssistant(thinking, "the answer", "", true, 80))
	toolExpanded := stripANSI(renderToolCalls(calls, true, 80))

	// Collapsed: thinking is the compact summary (count + ctrl+t), no "│ " body;
	// the tool result is folded (first K lines, a more-marker, later lines hidden).
	for _, w := range []string{"thinking", "3 lines", "ctrl+t"} {
		if !strings.Contains(thinkCollapsed, w) {
			t.Errorf("collapsed thinking missing %q in %q", w, thinkCollapsed)
		}
	}
	if strings.Contains(thinkCollapsed, "│ reason one") {
		t.Errorf("collapsed thinking must NOT show the body in %q", thinkCollapsed)
	}
	for _, w := range []string{"line0", "line5", "more lines"} {
		if !strings.Contains(toolCollapsed, w) {
			t.Errorf("collapsed tool missing %q in %q", w, toolCollapsed)
		}
	}
	for _, a := range []string{"line6", "line9"} {
		if strings.Contains(toolCollapsed, a) {
			t.Errorf("collapsed tool must NOT contain %q in %q", a, toolCollapsed)
		}
	}

	// Expanded: the full "│ "-prefixed thinking body — header included — AND every
	// tool-result line.
	for _, w := range []string{"│ thinking", "│ reason one", "│ reason three"} {
		if !strings.Contains(thinkExpanded, w) {
			t.Errorf("expanded thinking missing %q in %q", w, thinkExpanded)
		}
	}
	for _, w := range []string{"line6", "line9"} {
		if !strings.Contains(toolExpanded, w) {
			t.Errorf("expanded tool missing %q in %q", w, toolExpanded)
		}
	}
	if strings.Contains(toolExpanded, "more lines") {
		t.Errorf("expanded tool must NOT contain the more-marker in %q", toolExpanded)
	}
}

// TestRenderAssistantThinkingBlock covers an assistant segment carrying reasoning:
// when expanded the reasoning renders as the full thinking block (never as
// "[unsupported block]") and the narration still renders. It exercises renderAssistant
// the way the kindAssistant entry render feeds it (thinkingText + assistantText).
func TestRenderAssistantThinkingBlock(t *testing.T) {
	t.Parallel()

	got := stripANSI(renderAssistant("my reasoning", "the final answer", "", true, 80)) // expanded

	for _, w := range []string{"│ thinking", "│ my reasoning", "the final answer"} {
		if !strings.Contains(got, w) {
			t.Errorf("renderAssistant() = %q, want to contain %q", got, w)
		}
	}
	if strings.Contains(got, "[unsupported block]") {
		t.Errorf("renderAssistant() = %q, must not render reasoning as [unsupported block]", got)
	}
}

// TestRenderAssistantHeadline covers the empty-text MULTI-tool umbrella: an assistant
// entry with no narration but a headline renders a bold "● Multiple actions" beside the
// dot. With neither narration nor headline it renders nothing (no bare lone "●") — a
// single-tool empty-text step promotes its one card to the bullet instead.
func TestRenderAssistantHeadline(t *testing.T) {
	t.Parallel()

	dot := strings.TrimSpace(styles.Dot)

	got := stripANSI(renderAssistant("", "", multipleActionsHeadline, false, 80))
	if !strings.Contains(got, dot) {
		t.Errorf("renderAssistant(headline) = %q, want the dot glyph %q", got, dot)
	}
	if !strings.Contains(got, multipleActionsHeadline) {
		t.Errorf("renderAssistant(headline) = %q, want the %q headline beside the dot", got, multipleActionsHeadline)
	}

	empty := stripANSI(renderAssistant("", "", "", false, 80))
	if strings.Contains(empty, dot) {
		t.Errorf("renderAssistant(no text, no headline) = %q, want no bullet", empty)
	}
}

// TestRenderEntryPromotedTool covers a single-tool empty-text step's promoted card: a
// kindTool entry with promoted set renders AS the assistant bullet
// ("● <verb >ToolName(args)" + result), never an indented "⎿ …" card.
func TestRenderEntryPromotedTool(t *testing.T) {
	t.Parallel()

	e := entry{
		Kind:     kindTool,
		promoted: true,
		Calls:    []ToolCallView{{ToolName: "Bash", Summary: "date", Status: ToolOK, Result: []string{"Fri"}, Decision: gateApproved}},
	}
	got := stripANSI(strings.Join(renderEntry(e, false, 80), "\n"))
	for _, w := range []string{strings.TrimSpace(styles.Dot), "Approved", "Bash(date)", "Fri"} {
		if !strings.Contains(got, w) {
			t.Errorf("renderEntry(promoted) = %q, want %q", got, w)
		}
	}
	if strings.Contains(got, cardConnector) {
		t.Errorf("renderEntry(promoted) = %q, must NOT use the ⎿ card connector", got)
	}
}

// TestRenderEntryHeadline covers the committed-entry threading: a kindAssistant entry
// with a headline renders the bold "● Multiple actions" bullet. Pins entryrender →
// renderAssistant wiring.
func TestRenderEntryHeadline(t *testing.T) {
	t.Parallel()

	e := entry{Kind: kindAssistant, headline: multipleActionsHeadline}
	got := stripANSI(strings.Join(renderEntry(e, false, 80), "\n"))
	if !strings.Contains(got, multipleActionsHeadline) {
		t.Errorf("renderEntry(headline) = %q, want the %q headline", got, multipleActionsHeadline)
	}
	if strings.TrimSpace(got) == strings.TrimSpace(styles.Dot) {
		t.Errorf("renderEntry(headline) = %q, want a headline, not a bare lone dot", got)
	}
}

// TestRenderEntrySubagentCard covers the committed Subagent card render (design §5/§4):
// a kindTool entry whose ToolCallView has Agent set renders as a "●"-level card
// "Subagent(<agent>)  \"<task>\"" with its children as "⎿" rows and a final
// "⎿ done · N steps — \"<summary>\"" line. The card's own Result (the done summary)
// must appear ONLY in that done child, never also as a separate result body (no
// doubling). The "+M nested subagent steps" line shows only when Nested > 0.
func TestRenderEntrySubagentCard(t *testing.T) {
	t.Parallel()

	e := entry{
		Kind: kindTool,
		Calls: []ToolCallView{{
			ToolName:  "Subagent",
			Agent:     "explorer",
			Task:      "map repo",
			Steps:     6,
			SubStatus: subDone,
			Result:    []string{"found 12 packages"},
			Children: []ToolCallView{
				{ToolName: "Grep", Status: ToolOK, Result: []string{"hit"}},
				{ToolName: "Read", Status: ToolOK, Result: []string{"contents"}},
			},
		}},
	}
	got := stripANSI(strings.Join(renderEntry(e, false, 100), "\n"))

	// Header: the "●" bullet, the standard tool-card "Subagent(explorer)" form, and the
	// task in quotes.
	for _, w := range []string{strings.TrimSpace(styles.Dot), "Subagent(explorer)", `"map repo"`} {
		if !strings.Contains(got, w) {
			t.Errorf("subagent card = %q, want %q", got, w)
		}
	}
	// Two child cards under the header, each at the ⎿ level.
	if !strings.Contains(got, "Grep") || !strings.Contains(got, "Read") {
		t.Errorf("subagent card = %q, want the Grep and Read child rows", got)
	}
	// The done child: verb + step count + summary.
	for _, w := range []string{"done", "6 steps", "found 12 packages"} {
		if !strings.Contains(got, w) {
			t.Errorf("subagent card = %q, want the done child %q", got, w)
		}
	}
	// No doubling: "found 12 packages" appears exactly once (only in the done child),
	// not also as a separate result-preview body.
	if n := strings.Count(got, "found 12 packages"); n != 1 {
		t.Errorf("subagent card = %q, summary appears %d times, want exactly 1 (no doubling)", got, n)
	}
	// Never a doubled "⎿ ⎿" connector — children are ONE indent level under the header.
	if strings.Contains(got, cardConnector+cardConnector) {
		t.Errorf("subagent card = %q, must NOT nest ⎿ under ⎿", got)
	}
	// Nested == 0 → no nested-steps line.
	if strings.Contains(got, "nested subagent steps") {
		t.Errorf("subagent card = %q, must NOT show the nested line when Nested==0", got)
	}
}

// TestRenderEntrySubagentCardTerminals covers the done-line verb per SubStatus and the
// nested-steps line: failed shows the error text, interrupted omits the summary, and a
// positive Nested adds the "+M nested subagent steps" line.
func TestRenderEntrySubagentCardTerminals(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		status subStatus
		result []string
		nested int
		want   []string
		absent []string
	}{
		{
			name:   "done shows the summary",
			status: subDone,
			result: []string{"all good"},
			want:   []string{"done", "all good"},
		},
		{
			name:   "failed shows the error text",
			status: subFailed,
			result: []string{"boom: it broke"},
			want:   []string{"failed", "boom: it broke"},
			absent: []string{"done"},
		},
		{
			name:   "interrupted omits the summary",
			status: subInterrupted,
			result: []string{"ignored summary"},
			want:   []string{"interrupted"},
			absent: []string{"ignored summary"},
		},
		{
			name:   "nested counter shows when positive",
			status: subDone,
			result: []string{"ok"},
			nested: 3,
			want:   []string{"+3 nested subagent steps"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			e := entry{Kind: kindTool, Calls: []ToolCallView{{
				ToolName:  "Subagent",
				Agent:     "explorer",
				Task:      "task",
				Steps:     2,
				SubStatus: tt.status,
				Result:    tt.result,
				Nested:    tt.nested,
			}}}
			got := stripANSI(strings.Join(renderEntry(e, false, 100), "\n"))
			for _, w := range tt.want {
				if !strings.Contains(got, w) {
					t.Errorf("subagent card (%s) = %q, want %q", tt.name, got, w)
				}
			}
			for _, a := range tt.absent {
				if strings.Contains(got, a) {
					t.Errorf("subagent card (%s) = %q, must NOT contain %q", tt.name, got, a)
				}
			}
		})
	}
}

// TestRenderLiveAssistantWorkingWord covers the LIVE empty-text tool step (design §3
// rule 4): a card-only live segment renders a working-word from workingWords beside the
// blinking dot — a live "doing work" affordance — rather than a bare bullet. The
// committed form (TestRenderAssistantDoneHeadline) is the static "Done"; this is the
// provisional, pre-StepDone surface.
func TestRenderLiveAssistantWorkingWord(t *testing.T) {
	t.Parallel()

	calls := []ToolCallView{{ToolName: "Bash", Status: ToolRunning}}
	for _, frame := range []uint{0, 1, 5} {
		got := stripANSI(renderLiveAssistant("", "", calls, false, 80, animState{frame: frame}))
		headline := got
		if i := strings.IndexByte(got, '\n'); i >= 0 {
			headline = got[:i] // the headline is the first line; cards follow below
		}
		found := false
		for _, w := range workingWords {
			if strings.Contains(headline, w) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("frame %d: live headline = %q, want one of the working words %v", frame, headline, workingWords)
		}
	}
}
