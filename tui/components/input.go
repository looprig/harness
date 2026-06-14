package components

import (
	"github.com/charmbracelet/bubbles/textarea"
	tea "github.com/charmbracelet/bubbletea"
)

// inputHeight is the fixed visible height of the prompt editor, in lines.
const inputHeight = 3

// InputBox wraps a bubbles textarea: fixed 3-line height, no char limit, no line
// numbers — the user's prompt editor.
type InputBox struct {
	ta textarea.Model
}

// NewInputBox returns a configured, focused prompt editor.
func NewInputBox() InputBox {
	ta := textarea.New()
	ta.CharLimit = 0
	ta.ShowLineNumbers = false
	ta.Prompt = "> "
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

// View renders the editor.
func (b *InputBox) View() string {
	return b.ta.View()
}
