// Package styles holds the shared lipgloss styles and glamour helpers for the
// Nexus CLI TUI. It is a leaf package: it depends only on charm libraries and
// must never import the tui package or any of its other subpackages.
package styles

import (
	"charm.land/glamour/v2"
	glamourstyles "charm.land/glamour/v2/styles"
	"charm.land/lipgloss/v2"
)

// Dot is the leading marker rendered before assistant/markdown blocks — the PLAIN
// layout form (the bullet glyph + a trailing space, dotWidth columns). The rendered
// bullet is COLORED via LitDot; Dot itself stays uncolored so it doubles as the
// width/layout reference (and the ANSI-stripped substring tests match against).
const Dot = "● "

// DotColor is the assistant bullet's foreground color.
var DotColor = lipgloss.Color("#D4F84D")

// Markdown palette overrides applied by NewMarkdownRenderer over glamour's
// DarkStyleConfig. MarkdownHeadingColor replaces glamour's heading blue (ANSI 256
// "39") and MarkdownInlineCodeColor replaces glamour's inline `code` red (ANSI 256
// "203"), both with the same softer brand blue. They are hex strings (not
// lipgloss.Color) because glamour's StylePrimitive.Color is a *string.
//
// MarkdownCodeNeutralColor recolors the RED structural symbols inside fenced/indented
// CODE BLOCKS. Glamour's DarkStyleConfig code-block chroma theme paints the chroma
// Operator token (#EF8080, salmon-red → ANSI 256 "210") — which covers structural
// punctuation like "/", "+", "-", "=", "->" — and the GenericDeleted token (#FD5B5B,
// red → ANSI 256 "203") used for diff "-" lines. Those made path slashes, arrows and
// +/- markers render red in the live TUI. We retone both to #C4C4C4 — the chroma
// theme's OWN neutral Text/Name foreground (already used for unhighlighted code) — so
// structural symbols read as plain code text instead of red, while every other syntax
// color (keywords, strings, functions, …) is left untouched.
var (
	MarkdownHeadingColor     = "#A2D2FF"
	MarkdownInlineCodeColor  = "#A2D2FF"
	MarkdownCodeNeutralColor = "#C4C4C4"
)

// LitDot is the COLORED leading marker actually rendered before an assistant bullet:
// the DotColor-foregrounded glyph plus a plain trailing space. Its display width equals
// Dot's (the color is zero-width ANSI), so narration alignment is unchanged.
var LitDot = lipgloss.NewStyle().Foreground(DotColor).Render("●") + " "

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
	InterruptedStyle = lipgloss.NewStyle().Faint(true).Italic(true)
	StatusStyle      = lipgloss.NewStyle().Faint(true)
	// StatusWorkingStyle / StatusWorkingAltStyle are the two phases of the status-line
	// icon while the model is actively working: lit (the assistant lime) and its blink
	// alternate (white). Waiting/thinking alternate between them on the blink tick for a
	// gentle pulse; streaming holds the lit lime. At rest / when blocked the icon falls
	// back to the faint StatusStyle.
	StatusWorkingStyle    = lipgloss.NewStyle().Foreground(DotColor)
	StatusWorkingAltStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("15"))
	// QueuedStyle renders the transient queued-input affordance — the pending,
	// not-yet-running echo of a submitted user message shown below the live tail. It
	// is FAINT (not bold like UserStyle) so a queued line reads as a quieter "this is
	// waiting" hint, distinct from the bold committed user row it later promotes to.
	QueuedStyle = lipgloss.NewStyle().Faint(true)
)

// HeadlineStyle renders the bold headline word shown beside the assistant dot for an
// empty-text tool step (the live "working" synonym and the committed "Done") — design
// §3 rule 4. Bold so the headline stands out beside the bullet, matching the bold
// emphasis the user message and prompt headers use.
var HeadlineStyle = lipgloss.NewStyle().Bold(true)

// Notice styles color a leveled notification's shared "▌ " accent bar (and text) by
// severity. All three reuse the SAME accent-bar wrapper as user messages and differ
// only in foreground color: info is a neutral gray (color 8), warn is bright yellow
// (color 11), error is red (color 9). They are selected per entry via NoticeStyle;
// callers must not branch on the level themselves.
var (
	NoticeInfoStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))  // neutral gray (user-message tone)
	NoticeWarnStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("11")) // bright yellow
	NoticeErrorStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("9"))  // red
)

// NoticeStyle maps a notice level (0=info, 1=warn, 2=error) to its style. An unknown
// level falls back to the neutral info style (fail-safe: an unrecognised level must
// never panic or pick an alarming color). The level argument is a plain uint8 so the
// leaf styles package need not import package tui's noticeLevel type (it depends only
// on charm libraries — see the package doc).
func NoticeStyle(level uint8) lipgloss.Style {
	switch level {
	case 1:
		return NoticeWarnStyle
	case 2:
		return NoticeErrorStyle
	default:
		return NoticeInfoStyle
	}
}

// Tool-call styles: a tool card and its result preview render dim, subordinate to
// the assistant narration they nest beneath.
var (
	ToolCallStyle   = lipgloss.NewStyle().Faint(true) // "└ ToolName  Summary  <glyph>" lines
	ToolResultStyle = lipgloss.NewStyle().Faint(true) // indented result-preview lines
)

// AccentBarStyle colors the left accent bar ("▌") on user rows (and the queued-input
// echo) a mid gray (#737373).
var AccentBarStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("#737373"))

// InputAccent colors the composer's left ▌ edge — the same mid gray as the
// AccentBarStyle bar on user rows, so the input reads as the same accent.
var InputAccent = lipgloss.Color("#737373")

// composerBorder draws ONLY a left ▌ edge (no top/right/bottom), so the accent runs
// down the left of the (auto-growing) editor.
var composerBorder = lipgloss.Border{Left: AccentBar}

// BoxStyle frames the composer (input) box as the lowest-footprint affordance: a
// left-only ▌ accent edge + one space of left padding, and NOTHING else — no border
// box, no background fill, no top/bottom padding. This is deliberate: the bubbletea v2
// inline renderer strands the composer's rows into scrollback when a resize desyncs
// its relative cursor, and the loudness of that artifact scales with how much each row
// paints. A single, full-width-tinted or bordered row strands as a bar/box; a bare
// "▌ text" row strands as a faint one-glyph fragment. So the box paints no full-width
// glyphs and occupies the fewest rows possible (just the editor's content lines). The
// editor renders to the right of the edge; callers subtract the horizontal frame (edge
// + left padding = 2 cols) from the box width to size the inner textarea.
var BoxStyle = lipgloss.NewStyle().
	Border(composerBorder, false, false, false, true).
	BorderForeground(InputAccent).
	PaddingLeft(1)

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

// SubagentCursor is the leading marker of a collapsed subagent activity line — the
// "▸ <agent>: <verb>" row attributing a subagent loop's StepDone to its agent. It
// reuses the same ▸ glyph as the prompt choice cursor (a "drill-in"/secondary marker),
// plus a trailing space.
const SubagentCursor = "▸ "

// SubagentStyle renders a collapsed subagent activity line ("▸ <agent>: done"). It is
// FAINT so a subagent's collapsed-but-present step reads as quieter, subordinate
// chatter beneath the primary (orchestrator) narration — matching the faint tool-card
// and queued-affordance tone, distinct from the bold primary user/assistant rows.
var SubagentStyle = lipgloss.NewStyle().Faint(true)

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
//
// The Nexus palette is applied to glamour's defaults: markdown headings (glamour's
// blue) and inline `code` spans (glamour's red) are recolored to MarkdownHeadingColor
// and MarkdownInlineCodeColor. The inline `code` span additionally drops glamour's
// dark background fill and its U+00A0 prefix/suffix (which padded each span with a
// leading/trailing space), so a `code` span renders as bare colored text. The H2–H6
// heading prefixes (glamour's literal "## ", "### ", … markers) are cleared so a
// heading renders as clean styled text rather than echoing its markdown hashes; H1
// keeps its colored background bar (it never carried a "#" marker).
//
// The code-block chroma theme's two RED structural tokens are also retoned to
// MarkdownCodeNeutralColor: Operator (glamour's salmon-red #EF8080, which colors "/",
// "+", "-", "=", "->", … inside highlighted code) and GenericDeleted (glamour's red
// #FD5B5B, the diff "-" line color). This stops path slashes, arrows and +/- markers
// from rendering red inside code blocks. Chroma is a *struct pointer shared with the
// package-level DarkStyleConfig, so it is deep-copied (value copy of the pointee, then
// re-pointed) before its fields are mutated — otherwise the override would leak into
// the shared global. Every other chroma color is left at glamour's defaults.
//
// cfg is a value copy of DarkStyleConfig, so reassigning its (non-pointer) fields never
// mutates the shared package-level config (the same copy-then-override pattern as the
// document margin).
//
// Returns an error if glamour fails to construct (caller decides fallback).
func NewMarkdownRenderer(width int) (*glamour.TermRenderer, error) {
	cfg := glamourstyles.DarkStyleConfig
	noMargin := uint(0)
	cfg.Document.Margin = &noMargin

	heading := MarkdownHeadingColor
	cfg.Heading.Color = &heading
	cfg.H2.Prefix = "" // drop glamour's literal "## " heading marker
	cfg.H3.Prefix = "" // drop "### "
	cfg.H4.Prefix = "" // drop "#### "
	cfg.H5.Prefix = "" // drop "##### "
	cfg.H6.Prefix = "" // drop "###### "

	code := MarkdownInlineCodeColor
	cfg.Code.Color = &code
	cfg.Code.BackgroundColor = nil // no background fill
	cfg.Code.Prefix = ""           // no leading U+00A0 padding space
	cfg.Code.Suffix = ""           // no trailing U+00A0 padding space

	// Retone the code-block chroma theme's two RED structural tokens (Operator,
	// GenericDeleted) to a neutral gray. cfg.CodeBlock.Chroma is a *Chroma shared with
	// the package-level DarkStyleConfig, so deep-copy the pointee before mutating —
	// otherwise the override would corrupt the shared global on every call.
	if cfg.CodeBlock.Chroma != nil {
		chromaCopy := *cfg.CodeBlock.Chroma
		neutral := MarkdownCodeNeutralColor
		chromaCopy.Operator.Color = &neutral       // "/", "+", "-", "=", "->" in code
		chromaCopy.GenericDeleted.Color = &neutral // diff "-" lines
		cfg.CodeBlock.Chroma = &chromaCopy
	}

	return glamour.NewTermRenderer(
		glamour.WithStyles(cfg),
		glamour.WithWordWrap(width),
	)
}
