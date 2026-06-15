package components

import (
	"regexp"
	"strings"
	"testing"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
)

// ansiEscape matches ANSI CSI/SGR escape sequences (e.g. "\x1b[7;37m"). v2's focused
// textarea inverts the first placeholder rune with the virtual cursor, splitting the
// placeholder string with escape codes; stripping them lets appearance assertions
// match the visible text rather than the styled bytes.
var ansiEscape = regexp.MustCompile(`\x1b\[[0-9;]*m`)

// stripANSI removes SGR escape sequences so a test can assert on visible glyphs.
func stripANSI(s string) string { return ansiEscape.ReplaceAllString(s, "") }

// TestInputBoxAppearance covers the auto-growing input: the bordered box, the "▌"
// accent bar on the left (matching user rows), a dim placeholder, and no "> " prompt.
// The empty box is the minimum content height (1 line) plus the border's two rows.
func TestInputBoxAppearance(t *testing.T) {
	t.Parallel()

	b := NewInputBox()
	b.Resize(40)
	v := b.View()
	// Strip styling so substring checks match the visible glyphs: v2's focused
	// textarea inverts the placeholder's first rune with the virtual cursor, which
	// otherwise splits "Type a message…" with ANSI escapes.
	plain := stripANSI(v)

	// 1 content line + a top and bottom border row.
	if lines := strings.Count(v, "\n") + 1; lines != minInputLines+2 {
		t.Fatalf("View() has %d lines, want %d:\n%q", lines, minInputLines+2, v)
	}
	if strings.Contains(plain, "> ") {
		t.Errorf("View() still shows the old \"> \" prompt:\n%q", v)
	}
	if !strings.Contains(plain, "▌") {
		t.Errorf("View() missing the \"▌\" accent bar:\n%q", v)
	}
	if !strings.Contains(plain, "Type a message") {
		t.Errorf("View() missing the placeholder text:\n%q", v)
	}
}

// TestInputBoxGrows checks the content height clamps to [minInputLines, maxInputLines]
// and grows with the number of logical lines in between.
func TestInputBoxGrows(t *testing.T) {
	tests := []struct {
		name, value string
		wantHeight  int
	}{
		{"empty is min", "", 1},
		{"two lines", "a\nb", 2},
		{"caps at max", strings.Repeat("x\n", 20), 10},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			b := NewInputBox()
			b.Resize(60)
			b.SetValue(tt.value)
			if got := b.Height(); got != tt.wantHeight {
				t.Errorf("Height() = %d, want %d", got, tt.wantHeight)
			}
		})
	}
}

// TestInputBoxViewGrowsWithContent confirms the rendered box is taller when content
// spans more lines, and that View carries the textarea's content.
func TestInputBoxViewGrowsWithContent(t *testing.T) {
	t.Parallel()

	b := NewInputBox()
	b.Resize(60)
	b.SetValue("only one line")
	// Drive an Update so the editor's height tracks the content.
	b.Update(tea.KeyPressMsg{Text: "!", Code: '!'})
	short := strings.Count(b.View(), "\n") + 1

	b.SetValue("line1\nline2\nline3\nline4")
	b.Update(tea.KeyPressMsg{Text: "!", Code: '!'})
	tall := strings.Count(b.View(), "\n") + 1

	if tall <= short {
		t.Errorf("View() height did not grow: short=%d tall=%d", short, tall)
	}
	if !strings.Contains(b.View(), "line4") {
		t.Errorf("View() missing content line:\n%q", b.View())
	}
}

// TestInputBoxShiftEnterNewline asserts Shift+Enter (not Enter) is bound to insert a
// newline, leaving Enter free for screen.go to use as submit. See the doc note on
// input.go: on terminals supporting the Kitty/enhanced keyboard protocol (which v2
// requests basic disambiguation for by default), shift+enter is a distinct key; on
// terminals lacking it, shift+enter arrives as plain enter.
func TestInputBoxShiftEnterNewline(t *testing.T) {
	t.Parallel()

	b := NewInputBox()
	bound := b.ta.KeyMap.InsertNewline.Keys()
	if !contains(bound, "shift+enter") {
		t.Errorf("InsertNewline keys = %v, want to include %q", bound, "shift+enter")
	}
	if contains(bound, "enter") {
		t.Errorf("InsertNewline keys = %v, must NOT include %q (Enter is submit)", bound, "enter")
	}

	// A shift+enter key event must match the InsertNewline binding (and NOT plain
	// enter). In v2, Shift+Enter is a tea.KeyPressMsg{Code: KeyEnter, Mod: ModShift}
	// whose String() is "shift+enter".
	shiftEnter := tea.KeyPressMsg{Code: tea.KeyEnter, Mod: tea.ModShift}
	if !key.Matches(shiftEnter, b.ta.KeyMap.InsertNewline) {
		t.Errorf("shift+enter (%q) does not match InsertNewline binding", shiftEnter.String())
	}
	plainEnter := tea.KeyPressMsg{Code: tea.KeyEnter}
	if key.Matches(plainEnter, b.ta.KeyMap.InsertNewline) {
		t.Errorf("plain enter (%q) matches InsertNewline binding; Enter must stay free for submit", plainEnter.String())
	}

	// Feeding the shift+enter event to the editor inserts a literal newline: a
	// single-line value becomes two logical lines.
	b.SetValue("ab")
	before := b.ta.LineCount()
	b.Update(shiftEnter)
	if after := b.ta.LineCount(); after != before+1 {
		t.Errorf("LineCount after shift+enter = %d, want %d (newline inserted)", after, before+1)
	}
	if got := b.Value(); !strings.Contains(got, "\n") {
		t.Errorf("Value() after shift+enter = %q, want it to contain a newline", got)
	}
}

// TestInputBoxCtrlJNewlineFallback asserts the universal newline fallback: Ctrl+J is
// also bound to InsertNewline (alongside shift+enter), so terminals that cannot deliver
// a distinct Shift+Enter (Apple Terminal, many VS Code setups — they lack the Kitty
// keyboard protocol) still have a way to type a literal newline. Ctrl+J is the LF byte
// (0x0A), which every terminal delivers without any protocol; v2 decodes it as
// tea.KeyPressMsg{Code:'j', Mod: ModCtrl} whose String() == "ctrl+j". Shift+Enter stays
// primary; Ctrl+J is purely additive.
func TestInputBoxCtrlJNewlineFallback(t *testing.T) {
	t.Parallel()

	b := NewInputBox()
	bound := b.ta.KeyMap.InsertNewline.Keys()
	if !contains(bound, "ctrl+j") {
		t.Errorf("InsertNewline keys = %v, want to include %q (universal newline fallback)", bound, "ctrl+j")
	}
	// shift+enter must remain bound — the fallback is additive, not a replacement.
	if !contains(bound, "shift+enter") {
		t.Errorf("InsertNewline keys = %v, want to STILL include %q (primary)", bound, "shift+enter")
	}

	// Ctrl+J is the LF byte; v2 represents it as Code 'j' + ModCtrl, String()=="ctrl+j".
	ctrlJ := tea.KeyPressMsg{Code: 'j', Mod: tea.ModCtrl}
	if got := ctrlJ.String(); got != "ctrl+j" {
		t.Fatalf("ctrlJ.String() = %q, want %q (v2 key representation drifted)", got, "ctrl+j")
	}
	if !key.Matches(ctrlJ, b.ta.KeyMap.InsertNewline) {
		t.Errorf("ctrl+j (%q) does not match InsertNewline binding", ctrlJ.String())
	}

	// Feeding ctrl+j to the editor inserts a literal newline.
	b.SetValue("ab")
	before := b.ta.LineCount()
	b.Update(ctrlJ)
	if after := b.ta.LineCount(); after != before+1 {
		t.Errorf("LineCount after ctrl+j = %d, want %d (newline inserted)", after, before+1)
	}
	if got := b.Value(); !strings.Contains(got, "\n") {
		t.Errorf("Value() after ctrl+j = %q, want it to contain a newline", got)
	}
}

func contains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}

func TestInputBoxValueResetSetValue(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		set   string
		reset bool
		want  string
	}{
		{name: "set value", set: "hi", want: "hi"},
		{name: "set then reset", set: "hi", reset: true, want: ""},
		{name: "set empty", set: "", want: ""},
		{name: "set multiline", set: "a\nb", want: "a\nb"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			b := NewInputBox()
			b.SetValue(tt.set)
			if tt.reset {
				b.Reset()
			}
			if got := b.Value(); got != tt.want {
				t.Errorf("Value() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestInputBoxResizeView(t *testing.T) {
	t.Parallel()

	b := NewInputBox()
	b.Resize(40)
	if got := b.View(); got == "" {
		t.Error("View() = empty after Resize(40), want non-empty")
	}
}

func TestInputBoxFocus(t *testing.T) {
	t.Parallel()

	b := NewInputBox()
	if cmd := b.Focus(); cmd == nil {
		t.Error("Focus() = nil cmd, want non-nil (Blink)")
	}
}
