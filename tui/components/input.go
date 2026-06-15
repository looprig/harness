package components

import (
	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/textarea"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/inventivepotter/urvi/tui/styles"
)

// minInputLines and maxInputLines bound the composer's content height in lines. The
// editor starts at one row and grows with content up to the cap, after which the
// bubbles textarea scrolls internally (keeping the cursor visible) rather than
// pushing the surrounding layout off-screen.
const (
	minInputLines = 1
	maxInputLines = 10
)

// placeholder is the dim hint shown while the editor is empty.
const placeholder = "Type a message…"

// InputBox wraps a bubbles textarea: an auto-growing editor with the shared "▌"
// accent bar as its prompt (matching user-message rows), rendered inside a bordered
// box. No char limit, no line numbers, no "> " prompt. The box height tracks the
// content between minInputLines and maxInputLines.
type InputBox struct {
	ta textarea.Model
}

// NewInputBox returns a configured, focused prompt editor.
//
// Enter is left unbound on the textarea so screen.go can use it as submit; instead
// Shift+Enter inserts a newline. NOTE: distinguishing shift+enter from enter requires
// the terminal's enhanced (Kitty) keyboard protocol. On terminals lacking it,
// shift+enter is delivered as plain enter and therefore submits — an accepted
// limitation; such terminals simply cannot type a literal newline in the composer.
func NewInputBox() InputBox {
	ta := textarea.New()
	ta.CharLimit = 0
	ta.ShowLineNumbers = false
	ta.Prompt = styles.AccentBarPrompt
	ta.FocusedStyle.Prompt = styles.AccentBarStyle
	ta.BlurredStyle.Prompt = styles.AccentBarStyle
	ta.Placeholder = placeholder
	// Rebind newline insertion to Shift+Enter, freeing Enter for submit in screen.go.
	ta.KeyMap.InsertNewline = key.NewBinding(
		key.WithKeys("shift+enter"),
		key.WithHelp("shift+enter", "insert newline"),
	)
	// The bubbles textarea highlights the focused line with a black background
	// (DefaultStyles: CursorLine bg "0"), which appears as a stray dark patch only
	// as wide as the text. Clear it so the input is plain like the user-message rows.
	ta.FocusedStyle.CursorLine = lipgloss.NewStyle()
	ta.SetHeight(minInputLines)
	ta.Focus()
	return InputBox{ta: ta}
}

// Height is the editor's content height in lines: the textarea's logical line count
// clamped to [minInputLines, maxInputLines]. It excludes the border frame.
func (b InputBox) Height() int {
	return clamp(b.ta.LineCount(), minInputLines, maxInputLines)
}

// Value returns the current text.
func (b *InputBox) Value() string {
	return b.ta.Value()
}

// Reset clears the text.
func (b *InputBox) Reset() {
	b.ta.Reset()
	b.ta.SetHeight(b.Height())
}

// SetValue replaces the text.
func (b *InputBox) SetValue(s string) {
	b.ta.SetValue(s)
	b.ta.SetHeight(b.Height())
}

// Resize sets the box width; the inner textarea is the box width minus the border's
// horizontal frame. The height auto-grows with content, so it is not set here.
func (b *InputBox) Resize(width int) {
	inner := width - styles.BoxStyle.GetHorizontalFrameSize()
	if inner < 1 {
		inner = 1
	}
	b.ta.SetWidth(inner)
}

// Focus focuses the editor and returns its Blink command.
func (b *InputBox) Focus() tea.Cmd {
	return b.ta.Focus()
}

// Update forwards the message to the textarea and grows the editor to fit the
// current content (capped at maxInputLines, past which it scrolls internally).
func (b *InputBox) Update(msg tea.Msg) tea.Cmd {
	var cmd tea.Cmd
	b.ta, cmd = b.ta.Update(msg)
	b.ta.SetHeight(b.Height())
	return cmd
}

// View renders the editor inside the bordered box. The box grows with the content
// because the inner textarea height tracks Height().
func (b *InputBox) View() string {
	return styles.BoxStyle.Render(b.ta.View())
}

// clamp constrains v to the inclusive range [lo, hi].
func clamp(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
