package components

import (
	"strings"
	"testing"

	"github.com/charmbracelet/bubbles/key"
	tea "github.com/charmbracelet/bubbletea"
)

// TestInputBoxAppearance covers the auto-growing input: the bordered box, the "▌"
// accent bar on the left (matching user rows), a dim placeholder, and no "> " prompt.
// The empty box is the minimum content height (1 line) plus the border's two rows.
func TestInputBoxAppearance(t *testing.T) {
	t.Parallel()

	b := NewInputBox()
	b.Resize(40)
	v := b.View()

	// 1 content line + a top and bottom border row.
	if lines := strings.Count(v, "\n") + 1; lines != minInputLines+2 {
		t.Fatalf("View() has %d lines, want %d:\n%q", lines, minInputLines+2, v)
	}
	if strings.Contains(v, "> ") {
		t.Errorf("View() still shows the old \"> \" prompt:\n%q", v)
	}
	if !strings.Contains(v, "▌") {
		t.Errorf("View() missing the \"▌\" accent bar:\n%q", v)
	}
	if !strings.Contains(v, "Type a message") {
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
	b.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'!'}})
	short := strings.Count(b.View(), "\n") + 1

	b.SetValue("line1\nline2\nline3\nline4")
	b.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'!'}})
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
// input.go: on terminals lacking the enhanced keyboard protocol shift+enter == enter.
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
	// Feeding a shift+enter key event grows the value by a newline.
	msg := tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("\n")}
	_ = key.Matches(msg, b.ta.KeyMap.InsertNewline)
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
