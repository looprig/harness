package tui

import (
	"regexp"
	"strings"
	"testing"

	"github.com/inventivepotter/urvi/tui/styles"
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
		calls    []ToolCallView
		want     []string
		absent   []string
	}{
		{
			name:  "text plus cards",
			text:  "let me read the config",
			calls: []ToolCallView{{ToolName: "ReadFile", Summary: "config.yaml", Status: ToolOK, Result: []string{"port: 8080"}}},
			want:  []string{"let me read the config", "ReadFile", "config.yaml", glyphOK, "port: 8080"},
		},
		{
			name:  "empty text with cards renders bare bullet plus cards",
			text:  "", // bare segment whose only content is its tool cards
			calls: []ToolCallView{{ToolName: "Bash", Summary: "ls", Status: ToolOK, Result: []string{"a.go"}}},
			want:  []string{strings.TrimSpace(styles.Dot), "Bash", "ls", glyphOK, "a.go"},
		},
		{
			name:   "text without cards renders no card connector",
			text:   "just text",
			want:   []string{"just text"},
			absent: []string{cardConnector},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := stripANSI(renderAssistant(tt.thinking, tt.text, tt.calls, false, 80))
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

// TestRenderAssistantLiveCards covers the in-progress segment rendering its text
// then its tool cards (a running card shows the running glyph), and a card-only
// segment with empty text rendering the bare dot bullet plus its cards. This is the
// same renderAssistant the live tail (screen.go's renderLiveTail) drives.
func TestRenderAssistantLiveCards(t *testing.T) {
	t.Parallel()

	t.Run("text plus running card", func(t *testing.T) {
		t.Parallel()

		calls := []ToolCallView{{ToolName: "Bash", Summary: "ls", Status: ToolRunning}}
		got := stripANSI(renderAssistant("", "checking now", calls, false, 80))
		for _, w := range []string{"checking now", "Bash", "ls", glyphRunning} {
			if !strings.Contains(got, w) {
				t.Errorf("renderAssistant() = %q, want to contain %q", got, w)
			}
		}
	})

	t.Run("cards with empty text render bare bullet", func(t *testing.T) {
		t.Parallel()

		calls := []ToolCallView{{ToolName: "Bash", Status: ToolRunning}}
		got := stripANSI(renderAssistant("", "", calls, false, 80))
		for _, w := range []string{strings.TrimSpace(styles.Dot), "Bash", glyphRunning} {
			if !strings.Contains(got, w) {
				t.Errorf("renderAssistant() = %q, want to contain %q", got, w)
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

	collapsed := stripANSI(renderAssistant(thinking, "the answer", calls, false, 80))
	expanded := stripANSI(renderAssistant(thinking, "the answer", calls, true, 80))

	// Collapsed: thinking is the compact summary (count + ctrl+t), no "│ " body;
	// the tool result is folded (first K lines, a more-marker, later lines hidden).
	for _, w := range []string{"thinking", "3 lines", "ctrl+t", "line0", "line5", "more lines"} {
		if !strings.Contains(collapsed, w) {
			t.Errorf("collapsed renderAssistant missing %q in %q", w, collapsed)
		}
	}
	for _, a := range []string{"│ reason one", "line6", "line9"} {
		if strings.Contains(collapsed, a) {
			t.Errorf("collapsed renderAssistant must NOT contain %q in %q", a, collapsed)
		}
	}

	// Expanded: the full "│ "-prefixed thinking body — header included — AND every
	// tool-result line.
	for _, w := range []string{"│ thinking", "│ reason one", "│ reason three", "line6", "line9"} {
		if !strings.Contains(expanded, w) {
			t.Errorf("expanded renderAssistant missing %q in %q", w, expanded)
		}
	}
	if strings.Contains(expanded, "more lines") {
		t.Errorf("expanded renderAssistant must NOT contain the more-marker in %q", expanded)
	}
}

// TestRenderAssistantThinkingBlock covers an assistant segment carrying reasoning:
// when expanded the reasoning renders as the full thinking block (never as
// "[unsupported block]") and the narration still renders. It exercises renderAssistant
// the way the kindAssistant entry render feeds it (thinkingText + assistantText).
func TestRenderAssistantThinkingBlock(t *testing.T) {
	t.Parallel()

	got := stripANSI(renderAssistant("my reasoning", "the final answer", nil, true, 80)) // expanded

	for _, w := range []string{"│ thinking", "│ my reasoning", "the final answer"} {
		if !strings.Contains(got, w) {
			t.Errorf("renderAssistant() = %q, want to contain %q", got, w)
		}
	}
	if strings.Contains(got, "[unsupported block]") {
		t.Errorf("renderAssistant() = %q, must not render reasoning as [unsupported block]", got)
	}
}
