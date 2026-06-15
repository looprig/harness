package tui

import (
	"regexp"
	"strings"
	"testing"

	"github.com/inventivepotter/urvi/internal/content"
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

func TestRenderMessages(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		msgs   []DisplayMessage
		live   liveSegment
		queued map[int]bool
		want   []string // substrings that must all appear in the output
	}{
		{
			name: "user text",
			msgs: []DisplayMessage{
				{Role: RoleUser, Blocks: []content.Block{&content.TextBlock{Text: "hello from user"}}},
			},
			want: []string{"hello from user"},
		},
		{
			name: "user image placeholder",
			msgs: []DisplayMessage{
				{Role: RoleUser, Blocks: []content.Block{
					&content.ImageBlock{
						MediaType: content.MediaTypeImagePNG,
						Source:    content.ImageSource{Data: make([]byte, 12)},
					},
				}},
			},
			want: []string{"[image: image/png, 12 bytes]"},
		},
		{
			name: "assistant markdown",
			msgs: []DisplayMessage{
				{Role: RoleAssistant, Blocks: []content.Block{&content.TextBlock{Text: "assistant reply text"}}},
			},
			want: []string{"assistant reply text"},
		},
		{
			name: "assistant concatenates text blocks",
			msgs: []DisplayMessage{
				{Role: RoleAssistant, Blocks: []content.Block{
					&content.TextBlock{Text: "alpha"},
					&content.TextBlock{Text: "beta"},
				}},
			},
			want: []string{"alpha", "beta"},
		},
		{
			name: "system",
			msgs: []DisplayMessage{
				{Role: RoleSystem, Blocks: []content.Block{&content.TextBlock{Text: "system notice"}}},
			},
			want: []string{"system notice"},
		},
		{
			name: "error",
			msgs: []DisplayMessage{
				{Role: RoleError, Blocks: []content.Block{&content.TextBlock{Text: "boom failure"}}},
			},
			want: []string{"boom failure"},
		},
		{
			name: "interrupted nil blocks",
			msgs: []DisplayMessage{
				{Role: RoleInterrupted, Blocks: nil},
			},
			want: []string{"interrupted"},
		},
		{
			name: "queued marker",
			msgs: []DisplayMessage{
				{Role: RoleUser, Blocks: []content.Block{&content.TextBlock{Text: "do this later"}}},
			},
			queued: map[int]bool{0: true},
			want:   []string{"do this later", "(queued)"},
		},
		{
			name: "live stream only",
			msgs: nil,
			live: liveSegment{text: "partial answer"},
			want: []string{"partial answer"},
		},
		{
			name: "stream appended after messages",
			msgs: []DisplayMessage{
				{Role: RoleUser, Blocks: []content.Block{&content.TextBlock{Text: "question"}}},
			},
			live: liveSegment{text: "streaming reply"},
			want: []string{"question", "streaming reply"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := stripANSI(renderMessages(tt.msgs, tt.live, tt.queued, false, 80))
			for _, w := range tt.want {
				if !strings.Contains(got, w) {
					t.Errorf("renderMessages() = %q, want to contain %q", got, w)
				}
			}
		})
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

// TestRenderRowAssistantNestsCards covers Task 3.3: an assistant row renders its
// markdown text followed by its tool-call cards indented beneath; a row with empty
// text but cards renders a bare dot bullet plus its cards (no empty markdown block).
func TestRenderRowAssistantNestsCards(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		row    DisplayMessage
		want   []string
		absent []string
	}{
		{
			name: "text plus cards",
			row: DisplayMessage{
				Role:   RoleAssistant,
				Blocks: []content.Block{&content.TextBlock{Text: "let me read the config"}},
				ToolCalls: []ToolCallView{
					{ToolName: "ReadFile", Summary: "config.yaml", Status: ToolOK, Result: []string{"port: 8080"}},
				},
			},
			want: []string{"let me read the config", "ReadFile", "config.yaml", glyphOK, "port: 8080"},
		},
		{
			name: "empty text with cards renders bare bullet plus cards",
			row: DisplayMessage{
				Role:   RoleAssistant,
				Blocks: nil, // bare segment whose only content is its tool cards
				ToolCalls: []ToolCallView{
					{ToolName: "Bash", Summary: "ls", Status: ToolOK, Result: []string{"a.go"}},
				},
			},
			want: []string{strings.TrimSpace(styles.Dot), "Bash", "ls", glyphOK, "a.go"},
		},
		{
			name: "text without cards renders no card connector",
			row: DisplayMessage{
				Role:   RoleAssistant,
				Blocks: []content.Block{&content.TextBlock{Text: "just text"}},
			},
			want:   []string{"just text"},
			absent: []string{cardConnector},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := stripANSI(renderRow(tt.row, false, 80))
			for _, w := range tt.want {
				if !strings.Contains(got, w) {
					t.Errorf("renderRow() = %q, want to contain %q", got, w)
				}
			}
			for _, a := range tt.absent {
				if strings.Contains(got, a) {
					t.Errorf("renderRow() = %q, want to NOT contain %q", got, a)
				}
			}
		})
	}
}

// TestRenderMessagesLiveCards covers Task 3.3: the live segment renders its text
// then its tool cards as the trailing in-progress block; a running card shows the
// running glyph.
func TestRenderMessagesLiveCards(t *testing.T) {
	t.Parallel()

	t.Run("live text plus running card", func(t *testing.T) {
		t.Parallel()

		live := liveSegment{
			text:  "checking now",
			calls: []ToolCallView{{ToolName: "Bash", Summary: "ls", Status: ToolRunning}},
		}
		got := stripANSI(renderMessages(nil, live, nil, false, 80))
		for _, w := range []string{"checking now", "Bash", "ls", glyphRunning} {
			if !strings.Contains(got, w) {
				t.Errorf("renderMessages() = %q, want to contain %q", got, w)
			}
		}
	})

	t.Run("live cards with empty text render bare bullet", func(t *testing.T) {
		t.Parallel()

		live := liveSegment{calls: []ToolCallView{{ToolName: "Bash", Status: ToolRunning}}}
		got := stripANSI(renderMessages(nil, live, nil, false, 80))
		for _, w := range []string{strings.TrimSpace(styles.Dot), "Bash", glyphRunning} {
			if !strings.Contains(got, w) {
				t.Errorf("renderMessages() = %q, want to contain %q", got, w)
			}
		}
	})
}

// TestRenderMessagesFullTranscriptNesting covers Task 3.3: a text→tool→text
// transcript renders each segment's cards beneath the segment that triggered them.
func TestRenderMessagesFullTranscriptNesting(t *testing.T) {
	t.Parallel()

	msgs := []DisplayMessage{
		{Role: RoleUser, Blocks: []content.Block{&content.TextBlock{Text: "fix the port"}}},
		{
			Role:   RoleAssistant,
			Blocks: []content.Block{&content.TextBlock{Text: "reading config"}},
			ToolCalls: []ToolCallView{
				{ToolName: "ReadFile", Summary: "config.yaml", Status: ToolOK, Result: []string{"port: 8080"}},
			},
		},
		{
			Role:   RoleAssistant,
			Blocks: []content.Block{&content.TextBlock{Text: "now fixing"}},
			ToolCalls: []ToolCallView{
				{ToolName: "EditFile", Summary: "config.yaml", Status: ToolOK, Result: []string{"port: 9090"}},
			},
		},
	}
	got := stripANSI(renderMessages(msgs, liveSegment{}, nil, false, 80))

	// Every segment's text and its own card appear, in transcript order.
	for _, w := range []string{"fix the port", "reading config", "ReadFile", "port: 8080", "now fixing", "EditFile", "port: 9090"} {
		if !strings.Contains(got, w) {
			t.Errorf("transcript missing %q in %q", w, got)
		}
	}
	// The first card precedes the second segment's narration (chronological nesting).
	if i, j := strings.Index(got, "ReadFile"), strings.Index(got, "now fixing"); i < 0 || j < 0 || i > j {
		t.Errorf("expected ReadFile card before 'now fixing' narration; got idx %d vs %d", i, j)
	}
}

// TestRenderThinking covers the dim reasoning block under the unified ctrl+t flag.
// Expanded: a "thinking" header followed by "│ "-prefixed lines, one per source
// line. Collapsed: a single compact summary line "thinking · N lines · ctrl+t"
// (N = number of thinking content lines, singularised to "1 line"), with NO
// "│ "-prefixed body. Empty or whitespace-only input renders nothing in either mode.
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
			name:         "expanded multi-line gets header and bar-prefixed lines",
			in:           "line one\nline two",
			expand:       true,
			wantContains: []string{"thinking", "│ line one", "│ line two"},
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

	// Expanded: the full "│ "-prefixed thinking body AND every tool-result line.
	for _, w := range []string{"│ reason one", "│ reason three", "line6", "line9"} {
		if !strings.Contains(expanded, w) {
			t.Errorf("expanded renderAssistant missing %q in %q", w, expanded)
		}
	}
	if strings.Contains(expanded, "more lines") {
		t.Errorf("expanded renderAssistant must NOT contain the more-marker in %q", expanded)
	}
}

// TestRenderUserAccentBar covers Task: a user row renders as left accent-bar lines
// (the "▌" marker) carrying the text, replacing the old bold-only style.
func TestRenderUserAccentBar(t *testing.T) {
	t.Parallel()

	row := DisplayMessage{Role: RoleUser, Blocks: []content.Block{&content.TextBlock{Text: "fix the port"}}}
	got := stripANSI(renderRow(row, false, 80))

	if !strings.Contains(got, "▌") {
		t.Errorf("renderRow(user) = %q, want to contain the accent bar %q", got, "▌")
	}
	if !strings.Contains(got, "fix the port") {
		t.Errorf("renderRow(user) = %q, want to contain the text", got)
	}
}

// TestRenderAssistantThinkingBlock covers an assistant row carrying a ThinkingBlock:
// when expanded the reasoning renders as the full thinking block (never as
// "[unsupported block]") and the narration still renders.
func TestRenderAssistantThinkingBlock(t *testing.T) {
	t.Parallel()

	row := DisplayMessage{
		Role: RoleAssistant,
		Blocks: []content.Block{
			&content.ThinkingBlock{Thinking: "my reasoning"},
			&content.TextBlock{Text: "the final answer"},
		},
	}
	got := stripANSI(renderRow(row, true, 80)) // expanded: assert the full thinking body renders

	for _, w := range []string{"thinking", "my reasoning", "the final answer"} {
		if !strings.Contains(got, w) {
			t.Errorf("renderRow(assistant) = %q, want to contain %q", got, w)
		}
	}
	if strings.Contains(got, "[unsupported block]") {
		t.Errorf("renderRow(assistant) = %q, must not render ThinkingBlock as [unsupported block]", got)
	}
}

// TestRenderMessagesLiveThinking covers the in-progress live segment carrying both
// streamed thinking and narration; expanded so the full thinking body is asserted.
func TestRenderMessagesLiveThinking(t *testing.T) {
	t.Parallel()

	live := liveSegment{thinking: "reasoning now", text: "answering"}
	got := stripANSI(renderMessages(nil, live, nil, true, 80))

	for _, w := range []string{"thinking", "reasoning now", "answering"} {
		if !strings.Contains(got, w) {
			t.Errorf("renderMessages(live thinking) = %q, want to contain %q", got, w)
		}
	}
}

// TestRenderMessagesNoStream verifies an empty stream is not appended as a row.
func TestRenderMessagesNoStream(t *testing.T) {
	t.Parallel()

	got := renderMessages(nil, liveSegment{}, nil, false, 80)
	if strings.TrimSpace(got) != "" {
		t.Errorf("renderMessages(nil, liveSegment{}, ...) = %q, want empty", got)
	}
}
