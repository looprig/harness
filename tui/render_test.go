package tui

import (
	"strings"
	"testing"

	"github.com/inventivepotter/urvi/internal/content"
)

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

			got := renderMD(tt.md, tt.width)
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

			got := renderMessages(tt.msgs, tt.live, tt.queued, false, 80)
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
			want:   []string{"line0", "line5", "4 more lines", "Ctrl+T"},
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

// TestRenderMessagesNoStream verifies an empty stream is not appended as a row.
func TestRenderMessagesNoStream(t *testing.T) {
	t.Parallel()

	got := renderMessages(nil, liveSegment{}, nil, false, 80)
	if strings.TrimSpace(got) != "" {
		t.Errorf("renderMessages(nil, liveSegment{}, ...) = %q, want empty", got)
	}
}
