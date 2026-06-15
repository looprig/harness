package components

import (
	"github.com/charmbracelet/bubbles/textarea"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/inventivepotter/urvi/tui/styles"
)

// inputHeight is the fixed visible height of the prompt editor, in lines. View
// clamps the rendered box to exactly this many rows.
const inputHeight = 2

// placeholder is the dim hint shown while the editor is empty.
const placeholder = "Type a message…"

// InputBox wraps a bubbles textarea: a fixed 2-line editor with the shared "▌"
// accent bar as its prompt (matching user-message rows), shown whether or not the
// user is typing. No char limit, no line numbers, no "> " prompt.
type InputBox struct {
	ta textarea.Model
}

// NewInputBox returns a configured, focused prompt editor.
func NewInputBox() InputBox {
	ta := textarea.New()
	ta.CharLimit = 0
	ta.ShowLineNumbers = false
	ta.Prompt = styles.AccentBarPrompt
	ta.FocusedStyle.Prompt = styles.AccentBarStyle
	ta.BlurredStyle.Prompt = styles.AccentBarStyle
	ta.Placeholder = placeholder
	// The bubbles textarea highlights the focused line with a black background
	// (DefaultStyles: CursorLine bg "0"), which appears as a stray dark patch only
	// as wide as the text. Clear it so the input is plain like the user-message rows.
	ta.FocusedStyle.CursorLine = lipgloss.NewStyle()
	ta.SetHeight(inputHeight)
	ta.Focus()
	return InputBox{ta: ta}
}

// Value returns the current text.
func (b *InputBox) Value() string {
	return b.ta.Value()
}

// Reset clears the text.
func (b *InputBox) Reset() {
	b.ta.Reset()
}

// SetValue replaces the text.
func (b *InputBox) SetValue(s string) {
	b.ta.SetValue(s)
}

// Resize sets the width; the height stays at the fixed line count.
func (b *InputBox) Resize(width int) {
	b.ta.SetWidth(width)
}

// Focus focuses the editor and returns its Blink command.
func (b *InputBox) Focus() tea.Cmd {
	return b.ta.Focus()
}

// Update forwards the message to the textarea and returns its command.
func (b *InputBox) Update(msg tea.Msg) tea.Cmd {
	var cmd tea.Cmd
	b.ta, cmd = b.ta.Update(msg)
	return cmd
}

// View renders the editor clamped to exactly inputHeight rows. Forcing the height
// keeps the input from ever contributing more rows than the layout reserved — a
// taller frame would overflow the terminal and stack stale chrome.
func (b *InputBox) View() string {
	return lipgloss.NewStyle().Height(inputHeight).MaxHeight(inputHeight).Render(b.ta.View())
}
