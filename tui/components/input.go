package components

import (
	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/textarea"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

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
// Enter is left unbound on the textarea so screen.go can use it as submit; newline
// insertion is bound to TWO keys so it works regardless of terminal capability:
//
//   - Shift+Enter (PRIMARY, preferred) — only distinguishable from plain Enter on
//     terminals that implement the Kitty keyboard protocol AND only when the program
//     requests "report all keys as escape codes" (flag 8). screen.go's View() sets
//     KeyboardEnhancements.ReportAllKeysAsEscapeCodes for exactly this reason; without
//     it the Kitty spec keeps Enter as a legacy byte and Shift+Enter arrives as plain
//     Enter (→ submit). Supported on kitty, Ghostty, WezTerm, foot, Alacritty, and
//     recent iTerm2 (with the protocol option enabled).
//   - Ctrl+J (UNIVERSAL FALLBACK) — the LF byte (0x0A), delivered by EVERY terminal
//     with no protocol required; v2 decodes it as Code 'j' + ModCtrl (String()=="ctrl+j").
//     This is the only way to type a literal newline on terminals that cannot deliver a
//     distinct Shift+Enter (Apple Terminal, many VS Code setups). It is purely additive
//     — Shift+Enter stays primary. Ctrl+J does not collide with any global binding in
//     screen.go (which handles only ctrl+c, ctrl+t, and esc).
func NewInputBox() InputBox {
	ta := textarea.New()
	ta.CharLimit = 0
	ta.ShowLineNumbers = false
	ta.Prompt = styles.AccentBarPrompt
	ta.Placeholder = placeholder
	// Bind newline insertion to Shift+Enter (primary) OR Ctrl+J (universal fallback),
	// freeing Enter for submit in screen.go. See the doc comment above for why both.
	ta.KeyMap.InsertNewline = key.NewBinding(
		key.WithKeys("shift+enter", "ctrl+j"),
		key.WithHelp("shift+enter", "insert newline"),
	)
	// v2 restructures the per-state styles under a single Styles value accessed via
	// Styles()/SetStyles. Color the focused and blurred prompts with the shared accent
	// bar, and clear the focused CursorLine: the default DefaultDarkStyles gives it a
	// black background ("0"), which appears as a stray dark patch only as wide as the
	// text. Clearing it makes the input plain, like the user-message rows.
	//
	// The default Cursor style is left untouched, and is safe to leave so: textarea's
	// DefaultDarkStyles is built by resolving lipgloss's LightDark light/dark *closure*
	// at construction with a literal dark choice, so the resulting style holds only
	// static colors. No LightDark value survives into the live style, so rendering it
	// never triggers a runtime OSC-11 background query (which the codebase deliberately
	// avoids; see styles.NewMarkdownRenderer).
	s := ta.Styles()
	s.Focused.Prompt = styles.AccentBarStyle
	s.Blurred.Prompt = styles.AccentBarStyle
	s.Focused.CursorLine = lipgloss.NewStyle()
	ta.SetStyles(s)
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
