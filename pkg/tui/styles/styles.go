// Package styles holds the shared lipgloss styles and glamour helpers for the
// Nexus CLI TUI. It is a leaf package: it depends only on charm libraries and
// must never import the tui package or any of its other subpackages.
package styles

import (
	"charm.land/glamour/v2"
	"charm.land/glamour/v2/ansi"
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

// Nexus markdown palette applied by NewMarkdownRenderer over glamour's DarkStyleConfig
// for non-code markdown, plus the base neutral shared with the code-block theme below.
// MarkdownHeadingColor replaces glamour's heading blue (ANSI 256 "39") and
// MarkdownInlineCodeColor replaces glamour's inline `code` red (ANSI 256 "203"), both
// with the same softer brand blue. MarkdownCodeNeutralColor is the base foreground for
// code text. They are hex strings (not lipgloss.Color) because glamour's
// StylePrimitive.Color is a *string.
var (
	MarkdownHeadingColor     = "#A2D2FF"
	MarkdownInlineCodeColor  = "#A2D2FF"
	MarkdownCodeNeutralColor = "#C4C4C4"
)

// Nexus code-block syntax-highlighting palette — four calm tones over a dark fill.
// Deliberately contains NO red: glamour's stock chroma theme is full of reds (salmon
// operators, red diff/deleted lines, and a RED-background Error token that painted
// box-drawing file trees like "├──" as alarm bars), and the old approach of inheriting
// that theme and patching reds token-by-token left every un-patched token a latent red.
// nexusChroma owns the whole theme instead, so a token can only ever be a color we chose.
var (
	codeKeywordColor = "#A2D2FF" // keywords, types, builtins, function/class names — brand blue
	codeStringColor  = "#D4F84D" // string literals — lime accent (matches the assistant dot)
	codeCommentColor = "#737373" // comments — faint gray (matches the input/accent bars)
	codeBgColor      = "#373737" // code-block fill — dark gray (glamour's default block bg)
)

// colorPtr / flagPtr adapt palette values to glamour's pointer-typed StylePrimitive fields.
func colorPtr(s string) *string { return &s }
func flagPtr(b bool) *bool      { return &b }

// nexusChroma is the single source of truth for code-block syntax highlighting: a
// complete chroma theme built from the Nexus palette. NewMarkdownRenderer assigns it to
// cfg.CodeBlock.Chroma, so glamour highlights against our colors directly and never
// touches glamour's DarkStyleConfig chroma defaults. Every token is set explicitly — an
// unset token would fall back to chroma's own built-in style — and none is red. It is
// read-only package state and is never mutated, so sharing the pointer is safe.
var nexusChroma = ansi.Chroma{
	// Base — identifiers, operators, punctuation, numbers, and chroma's Error catch-all
	// (the token emitted for input a lexer can't parse, e.g. box-drawing tree glyphs):
	// all neutral, so untokenizable text reads as plain code rather than a red alarm.
	Text:          ansi.StylePrimitive{Color: colorPtr(MarkdownCodeNeutralColor)},
	Error:         ansi.StylePrimitive{Color: colorPtr(MarkdownCodeNeutralColor)},
	Operator:      ansi.StylePrimitive{Color: colorPtr(MarkdownCodeNeutralColor)},
	Punctuation:   ansi.StylePrimitive{Color: colorPtr(MarkdownCodeNeutralColor)},
	Name:          ansi.StylePrimitive{Color: colorPtr(MarkdownCodeNeutralColor)},
	NameAttribute: ansi.StylePrimitive{Color: colorPtr(MarkdownCodeNeutralColor)},
	NameConstant:  ansi.StylePrimitive{Color: colorPtr(MarkdownCodeNeutralColor)},
	NameOther:     ansi.StylePrimitive{Color: colorPtr(MarkdownCodeNeutralColor)},
	Literal:       ansi.StylePrimitive{Color: colorPtr(MarkdownCodeNeutralColor)},
	LiteralNumber: ansi.StylePrimitive{Color: colorPtr(MarkdownCodeNeutralColor)},
	LiteralDate:   ansi.StylePrimitive{Color: colorPtr(MarkdownCodeNeutralColor)},

	// Language constructs — keywords, types, builtins, definitions, decorators, tags.
	Keyword:          ansi.StylePrimitive{Color: colorPtr(codeKeywordColor)},
	KeywordReserved:  ansi.StylePrimitive{Color: colorPtr(codeKeywordColor)},
	KeywordNamespace: ansi.StylePrimitive{Color: colorPtr(codeKeywordColor)},
	KeywordType:      ansi.StylePrimitive{Color: colorPtr(codeKeywordColor)},
	NameBuiltin:      ansi.StylePrimitive{Color: colorPtr(codeKeywordColor)},
	NameClass:        ansi.StylePrimitive{Color: colorPtr(codeKeywordColor), Bold: flagPtr(true)},
	NameFunction:     ansi.StylePrimitive{Color: colorPtr(codeKeywordColor)},
	NameDecorator:    ansi.StylePrimitive{Color: colorPtr(codeKeywordColor)},
	NameTag:          ansi.StylePrimitive{Color: colorPtr(codeKeywordColor)},
	NameException:    ansi.StylePrimitive{Color: colorPtr(codeKeywordColor)},

	// Strings — lime.
	LiteralString:       ansi.StylePrimitive{Color: colorPtr(codeStringColor)},
	LiteralStringEscape: ansi.StylePrimitive{Color: colorPtr(codeStringColor)},

	// Comments & meta — faint gray.
	Comment:           ansi.StylePrimitive{Color: colorPtr(codeCommentColor)},
	CommentPreproc:    ansi.StylePrimitive{Color: colorPtr(codeCommentColor)},
	GenericSubheading: ansi.StylePrimitive{Color: colorPtr(codeCommentColor)},

	// Diff markers stay neutral (no red removed-lines, no green added-lines); emphasis
	// and strong keep their weight rather than a color.
	GenericDeleted:  ansi.StylePrimitive{Color: colorPtr(MarkdownCodeNeutralColor)},
	GenericInserted: ansi.StylePrimitive{Color: colorPtr(MarkdownCodeNeutralColor)},
	GenericEmph:     ansi.StylePrimitive{Color: colorPtr(MarkdownCodeNeutralColor), Italic: flagPtr(true)},
	GenericStrong:   ansi.StylePrimitive{Color: colorPtr(MarkdownCodeNeutralColor), Bold: flagPtr(true)},

	// The dark fill behind highlighted code.
	Background: ansi.StylePrimitive{BackgroundColor: colorPtr(codeBgColor)},
}

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
// Code-block syntax highlighting uses the Nexus theme outright: cfg.CodeBlock.Chroma is
// pointed at the package-level nexusChroma rather than inheriting glamour's reds-laden
// DarkStyleConfig chroma and patching it token by token. See nexusChroma for the palette
// and the rationale (this is why box-drawing file trees no longer render on a red
// background, and why path slashes, arrows and +/- markers are not red).
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

	// Own the code-block syntax theme outright: point at the Nexus chroma palette rather
	// than inheriting glamour's DarkStyleConfig chroma and patching its reds token by
	// token. nexusChroma is read-only and never mutated, so sharing the pointer is safe.
	cfg.CodeBlock.Chroma = &nexusChroma

	return glamour.NewTermRenderer(
		glamour.WithStyles(cfg),
		glamour.WithWordWrap(width),
	)
}
