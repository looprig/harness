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

// contentHeightCeiling is the textarea's own MaxHeight. It is set far above
// maxInputLines on purpose: MaxHeight in the bubbles textarea is BOTH the visible-row
// cap AND an input gate — once the logical line count reaches MaxHeight the textarea
// refuses to insert further newlines (textarea.atContentLimit), silently dropping
// content. We must never drop the user's text, so the textarea's MaxHeight is parked
// high and the VISIBLE cap (maxInputLines) is enforced separately via SetHeight on the
// viewport window. The value just needs to exceed any realistic composer input.
const contentHeightCeiling = 10000

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
	// No per-line prompt: the ▌ accent is now the composer panel's left border (drawn
	// by styles.BoxStyle over every row, including the top/bottom padding), so a
	// textarea prompt would double it. The leading gap is the box's PaddingLeft.
	ta.Prompt = ""
	ta.Placeholder = placeholder
	// Bind newline insertion to Shift+Enter (primary) OR Ctrl+J (universal fallback),
	// freeing Enter for submit in screen.go. See the doc comment above for why both.
	ta.KeyMap.InsertNewline = key.NewBinding(
		key.WithKeys("shift+enter", "ctrl+j"),
		key.WithHelp("shift+enter", "insert newline"),
	)
	// v2 restructures the per-state styles under a single Styles value accessed via
	// Styles()/SetStyles. The composer paints NO background (lowest resize-stranding
	// footprint — see styles.BoxStyle), so the only style fix is clearing the focused
	// CursorLine: the default DefaultDarkStyles gives it a black background ("0"), a
	// stray dark patch as wide as the text. An empty style leaves the editor plain, like
	// the user-message rows.
	//
	// The default Cursor style is left untouched, and is safe to leave so: textarea's
	// DefaultDarkStyles is built by resolving lipgloss's LightDark light/dark *closure*
	// at construction with a literal dark choice, so the resulting style holds only
	// static colors. No LightDark value survives into the live style, so rendering it
	// never triggers a runtime OSC-11 background query (which the codebase deliberately
	// avoids; see styles.NewMarkdownRenderer).
	s := ta.Styles()
	s.Focused.CursorLine = lipgloss.NewStyle()
	ta.SetStyles(s)
	// DynamicHeight makes the textarea recompute its height from the VISUAL (soft-wrap
	// aware) line count on every mutation, and — crucially — clamp its internal viewport
	// scroll offset back to the top when the content fits. Without it, the viewport stays
	// scrolled to wherever the cursor was as the user typed each new line: SetHeight only
	// ever scrolls DOWN to reveal the cursor, never back UP to collapse the slack above
	// it, so the box hid the first line(s) and showed a phantom trailing blank. MaxHeight
	// is parked at contentHeightCeiling (NOT maxInputLines) because MaxHeight doubles as
	// an input gate that drops newlines once reached; the visible [1, maxInputLines] cap
	// is applied separately in capHeight via SetHeight.
	ta.DynamicHeight = true
	ta.MinHeight = minInputLines
	ta.MaxHeight = contentHeightCeiling
	ta.Focus()
	b := InputBox{ta: ta}
	b.capHeight()
	return b
}

// Height is the editor's visible content height in rows: the textarea's current row
// count clamped to [minInputLines, maxInputLines]. It excludes the border frame.
//
// It reads ta.Height() rather than ta.LineCount() so it tracks VISUAL rows (a single
// long logical line that soft-wraps occupies several rows), matching what View()
// actually renders. DynamicHeight keeps ta.Height() equal to the total visual line
// count (capped at contentHeightCeiling, far above maxInputLines), so once capHeight
// has applied the visible cap this returns that capped value, and before capping it
// returns the true content height — both already within [min, max] after clamp.
func (b InputBox) Height() int {
	return clamp(b.ta.Height(), minInputLines, maxInputLines)
}

// capHeight pins the visible viewport window to [minInputLines, maxInputLines]. The
// textarea's DynamicHeight grows ta.Height() to the full visual line count on each
// mutation (and resets its scroll to the top while the content fits); this caps the
// visible window so the box grows only up to maxInputLines, after which the textarea
// scrolls internally to keep the cursor in view. Call after every mutation.
func (b *InputBox) capHeight() {
	b.ta.SetHeight(b.Height())
}

// Value returns the current text.
func (b *InputBox) Value() string {
	return b.ta.Value()
}

// Reset clears the text.
func (b *InputBox) Reset() {
	b.ta.Reset()
	b.capHeight()
}

// SetValue replaces the text.
func (b *InputBox) SetValue(s string) {
	b.ta.SetValue(s)
	b.capHeight()
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
	b.capHeight()
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
