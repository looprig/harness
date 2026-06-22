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

// TestInputBoxAppearance covers the auto-growing input: the borderless ▌-edged editor
// (the "▌" accent bar on the left, matching user rows), a dim placeholder, and no
// "> " prompt. The box paints no border or padding rows, so the empty box is exactly
// the minimum content height (1 line).
func TestInputBoxAppearance(t *testing.T) {
	t.Parallel()

	b := NewInputBox()
	b.Resize(40)
	v := b.View()
	// Strip styling so substring checks match the visible glyphs: v2's focused
	// textarea inverts the placeholder's first rune with the virtual cursor, which
	// otherwise splits "Type a message…" with ANSI escapes.
	plain := stripANSI(v)

	// Just the content line — no border/padding rows.
	if lines := strings.Count(v, "\n") + 1; lines != minInputLines {
		t.Fatalf("View() has %d lines, want %d:\n%q", lines, minInputLines, v)
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

// visibleContentRows returns the editor's content rows (those carrying the "▌" accent
// bar) from a rendered View, with ANSI stripped and the box border trimmed off. The
// "▌ " prompt is dropped and surrounding box-padding whitespace is trimmed so each
// element is the bare visible text of one editor row (empty string for a blank row).
func visibleContentRows(view string) []string {
	// The composer is a borderless ▌-edged editor (styles.BoxStyle): no box, no padding
	// rows — the ▌ left edge runs down each editor content line. Every line carries a ▌,
	// so each is a content row; strip the ▌ edge and the surrounding space to get the
	// row's text (a phantom trailing blank, if the bug being guarded against reappears,
	// survives as an empty row).
	var rows []string
	for _, line := range strings.Split(stripANSI(view), "\n") {
		if !strings.Contains(line, "▌") {
			continue
		}
		text := strings.TrimSpace(line)
		text = strings.TrimPrefix(text, "▌")
		rows = append(rows, strings.TrimSpace(text))
	}
	return rows
}

// typeInto drives the editor through the real keystroke path: each rune as a key press,
// and a literal newline (\n) as Ctrl+J (the universal InsertNewline binding). This is
// the path a user takes in the terminal — and the one that surfaced the scroll bug,
// where the composer hid the first line(s) and showed a phantom trailing blank because
// the textarea's viewport stayed scrolled to the cursor instead of resetting to the top
// once the content fit. SetValue alone never reproduced it.
func typeInto(b *InputBox, s string) {
	for _, r := range s {
		if r == '\n' {
			b.Update(tea.KeyPressMsg{Code: 'j', Mod: tea.ModCtrl})
			continue
		}
		b.Update(tea.KeyPressMsg{Text: string(r), Code: r})
	}
}

// TestInputBoxMultiLineTopAligned is the regression test for the composer scroll bug:
// multi-line content must render top-aligned with EVERY line visible and NO phantom
// trailing blank row, until the content exceeds maxInputLines (then it scrolls to keep
// the cursor visible). It drives input through the real keystroke path (typeInto), which
// is what reproduced the bug; SetValue alone always rendered correctly.
func TestInputBoxMultiLineTopAligned(t *testing.T) {
	tests := []struct {
		name     string
		value    string
		wantRows []string // exact visible content rows, top to bottom
		wantHt   int      // expected Height() (visible content rows); 0 = don't assert
		mustShow []string // substrings that must appear somewhere in the view
		mustHide []string // substrings that must NOT appear in the view
	}{
		{
			name:     "two lines: both shown, no hidden first line, no phantom blank",
			value:    "AAA\nBBB",
			wantRows: []string{"AAA", "BBB"},
			wantHt:   2,
			mustShow: []string{"AAA", "BBB"},
		},
		{
			name:     "three lines: all shown top-aligned, no phantom blank",
			value:    "AAA\nBBB\nCCC",
			wantRows: []string{"AAA", "BBB", "CCC"},
			wantHt:   3,
			mustShow: []string{"AAA", "BBB", "CCC"},
		},
		{
			name:     "single line: one row, no phantom blank",
			value:    "AAA",
			wantRows: []string{"AAA"},
			wantHt:   1,
			mustShow: []string{"AAA"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			b := NewInputBox()
			b.Resize(60)
			typeInto(&b, tt.value)

			rows := visibleContentRows(b.View())
			if len(rows) != len(tt.wantRows) {
				t.Fatalf("visible content rows = %d %q, want %d %q\nview:\n%s",
					len(rows), rows, len(tt.wantRows), tt.wantRows, stripANSI(b.View()))
			}
			for i := range tt.wantRows {
				if rows[i] != tt.wantRows[i] {
					t.Errorf("content row %d = %q, want %q\nview:\n%s",
						i, rows[i], tt.wantRows[i], stripANSI(b.View()))
				}
			}
			if tt.wantHt != 0 {
				if got := b.Height(); got != tt.wantHt {
					t.Errorf("Height() = %d, want %d", got, tt.wantHt)
				}
			}
			plain := stripANSI(b.View())
			for _, s := range tt.mustShow {
				if !strings.Contains(plain, s) {
					t.Errorf("View() missing %q\nview:\n%s", s, plain)
				}
			}
			for _, s := range tt.mustHide {
				if strings.Contains(plain, s) {
					t.Errorf("View() unexpectedly contains %q\nview:\n%s", s, plain)
				}
			}
		})
	}
}

// TestInputBoxScrollsPastMax verifies the grow-then-scroll behavior past the cap: with
// more logical lines than maxInputLines the box height caps at maxInputLines, NO content
// is dropped (every typed line is in Value()), and the view scrolls so the LAST lines —
// where the cursor sits — stay visible while the earliest lines scroll off the top.
func TestInputBoxScrollsPastMax(t *testing.T) {
	t.Parallel()

	const total = maxInputLines + 2 // 12 logical lines
	var sb strings.Builder
	for i := 0; i < total; i++ {
		if i > 0 {
			sb.WriteByte('\n')
		}
		sb.WriteString("L")
		sb.WriteByte(byte('A' + i)) // L A, L B, ... distinct per line
	}
	want := sb.String()

	b := NewInputBox()
	b.Resize(60)
	typeInto(&b, want)

	// No content lost despite exceeding the visible cap.
	if got := b.Value(); got != want {
		t.Fatalf("Value() = %q, want %q (content dropped past the cap?)", got, want)
	}
	// Height caps at maxInputLines.
	if got := b.Height(); got != maxInputLines {
		t.Errorf("Height() = %d, want %d (cap)", got, maxInputLines)
	}
	// Exactly maxInputLines visible rows, and the LAST line (cursor) is among them while
	// the FIRST line has scrolled off.
	rows := visibleContentRows(b.View())
	if len(rows) != maxInputLines {
		t.Fatalf("visible rows = %d, want %d\nview:\n%s", len(rows), maxInputLines, stripANSI(b.View()))
	}
	plain := stripANSI(b.View())
	lastLine := "L" + string(rune('A'+total-1)) // last typed line
	if !strings.Contains(plain, lastLine) {
		t.Errorf("View() missing last line %q (cursor must stay visible)\nview:\n%s", lastLine, plain)
	}
	if strings.Contains(plain, "LA") {
		t.Errorf("View() still shows first line %q; it should have scrolled off the top\nview:\n%s", "LA", plain)
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
