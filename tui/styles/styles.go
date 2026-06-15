// Package styles holds the shared lipgloss styles and glamour helpers for the
// Nexus CLI TUI. It is a leaf package: it depends only on charm libraries and
// must never import the tui package or any of its other subpackages.
package styles

import (
	"strings"

	"charm.land/glamour/v2"
	glamourstyles "charm.land/glamour/v2/styles"
	"charm.land/lipgloss/v2"
)

// Dot is the leading marker rendered before assistant/markdown blocks.
const Dot = "● "

// AccentBar is the left bar marker shared by user-message rows and the input
// prompt. AccentBarPrompt is the bar plus its trailing space, used as the prompt.
const (
	AccentBar       = "▌"
	AccentBarPrompt = AccentBar + " "
)

// ThinkingHeader labels the model's reasoning block.
const ThinkingHeader = "thinking"

// Role styles (exported so package tui can use them).
var (
	UserStyle        = lipgloss.NewStyle().Bold(true)
	SystemStyle      = lipgloss.NewStyle().Faint(true)
	ErrorStyle       = lipgloss.NewStyle().Foreground(lipgloss.Color("9")) // red
	InterruptedStyle = lipgloss.NewStyle().Faint(true).Italic(true)
	StatusStyle      = lipgloss.NewStyle().Faint(true)
)

// Tool-call styles: a tool card and its result preview render dim, subordinate to
// the assistant narration they nest beneath.
var (
	ToolCallStyle   = lipgloss.NewStyle().Faint(true) // "└ ToolName  Summary  <glyph>" lines
	ToolResultStyle = lipgloss.NewStyle().Faint(true) // indented result-preview lines
)

// AccentBarStyle colors the left accent bar on user rows and the input prompt.
var AccentBarStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))

// BoxStyle is the border drawn around the composer (input) box. The auto-growing
// editor renders inside it; callers subtract the style's horizontal frame from the
// box width to size the inner textarea.
var BoxStyle = lipgloss.NewStyle().Border(lipgloss.NormalBorder())

// PromptBoxStyle is the emphasised border drawn around an active permission/AskUser
// prompt control. It uses a rounded border (visually distinct from the composer's
// square NormalBorder and from the faint, borderless tool cards) so a pending gate
// reads as a foreground, action-required affordance rather than scrollback narration.
var PromptBoxStyle = lipgloss.NewStyle().Border(lipgloss.RoundedBorder())

// PromptHeaderStyle renders a prompt box's header label (e.g. "Approve Bash?"),
// bold so the action reads at a glance above the body.
var PromptHeaderStyle = lipgloss.NewStyle().Bold(true)

// PromptHintStyle renders a prompt box's faint secondary hints — the key legend
// ("↑/↓ select · …") and the "(+N more pending)" queue-depth note.
var PromptHintStyle = lipgloss.NewStyle().Faint(true)

// PromptCursorStyle renders the ▸ cursor marking the selected choice row, bold so
// the selection stands out from the unselected rows.
var PromptCursorStyle = lipgloss.NewStyle().Bold(true)

// separatorRune is the horizontal-rule glyph for the active-surface separator.
const separatorRune = "─"

// SeparatorRule returns a faint full-width horizontal rule of width columns, the
// divider drawn between the live tail (native scrollback above) and the bottom box.
// A non-positive width yields the empty string.
func SeparatorRule(width int) string {
	if width <= 0 {
		return ""
	}
	return StatusStyle.Render(strings.Repeat(separatorRune, width))
}

// ThinkingStyle renders the model's reasoning block: faint (never italic),
// subordinate to the assistant narration it precedes. Italic is deliberately
// omitted — it skewed the "│ " left rail and broke the column alignment; a
// non-italic rail renders as a clean, unbroken vertical line.
var ThinkingStyle = lipgloss.NewStyle().Faint(true)

// NewMarkdownRenderer builds a glamour renderer for the given wrap width.
//
// It uses the static DarkStyleConfig deliberately — never glamour.WithAutoStyle().
// Auto style calls termenv.HasDarkBackground(), which writes an OSC-11 background
// query plus a CSI-6n cursor probe to the terminal and reads the replies back off
// stdin. Inside a Bubble Tea program — which owns stdin in raw mode — those replies
// race the input reader and (a) leak into the UI as stray bytes like "]11;rgb:…" and
// "[…;…R", (b) desync the renderer's cursor tracking, and (c) stall the render loop.
// The static config does no terminal I/O.
//
// The document's left margin is zeroed so narration aligns flush under the "●"
// bullet that package tui prepends (otherwise glamour indents every line by 2).
// Returns an error if glamour fails to construct (caller decides fallback).
func NewMarkdownRenderer(width int) (*glamour.TermRenderer, error) {
	cfg := glamourstyles.DarkStyleConfig
	noMargin := uint(0)
	cfg.Document.Margin = &noMargin
	return glamour.NewTermRenderer(
		glamour.WithStyles(cfg),
		glamour.WithWordWrap(width),
	)
}
