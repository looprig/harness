package tui

import (
	"strings"
	"testing"

	"github.com/inventivepotter/urvi/internal/content"
	"github.com/inventivepotter/urvi/tui/styles"
)

// TestRenderEntryUser locks the user-kind render: the submitted text renders as
// "▌ "-prefixed bold lines (the shared AccentBar prefix), one entry's worth of
// lines, with no assistant bullet.
func TestRenderEntryUser(t *testing.T) {
	t.Parallel()
	e := entry{ID: 1, Kind: kindUser, Blocks: []content.Block{&content.TextBlock{Text: "ship it"}}}
	lines := renderEntry(e, false, 80)
	if len(lines) == 0 {
		t.Fatal("renderEntry(user) returned no lines")
	}
	joined := stripANSI(strings.Join(lines, "\n"))
	if !strings.Contains(joined, styles.AccentBar) {
		t.Errorf("user render = %q, want it to contain the accent bar %q", joined, styles.AccentBar)
	}
	if !strings.Contains(joined, "ship it") {
		t.Errorf("user render = %q, want it to contain the text", joined)
	}
	if strings.Contains(joined, strings.TrimSpace(styles.Dot)) {
		t.Errorf("user render = %q, must NOT carry the assistant bullet", joined)
	}
}

// TestRenderEntryAssistant locks the assistant-kind render: it carries the "●"
// narration bullet, and its thinking block honors the expand flag (collapsed →
// the compact "thinking · N lines" summary; expanded → the full reasoning body).
func TestRenderEntryAssistant(t *testing.T) {
	t.Parallel()
	e := entry{
		ID:   1,
		Kind: kindAssistant,
		Blocks: []content.Block{
			&content.ThinkingBlock{Thinking: "weighing\noptions\ncarefully"},
			&content.TextBlock{Text: "Here is the plan."},
		},
	}
	collapsed := stripANSI(strings.Join(renderEntry(e, false, 80), "\n"))
	expanded := stripANSI(strings.Join(renderEntry(e, true, 80), "\n"))

	if !strings.Contains(collapsed, strings.TrimSpace(styles.Dot)) {
		t.Errorf("assistant render = %q, want the ● bullet", collapsed)
	}
	if !strings.Contains(collapsed, "Here is the plan.") {
		t.Errorf("assistant render = %q, want the narration", collapsed)
	}
	// collapsed: compact summary, NOT the reasoning body.
	if !strings.Contains(collapsed, "thinking"+hintSeparator) {
		t.Errorf("collapsed render = %q, want the compact thinking summary", collapsed)
	}
	if strings.Contains(collapsed, "carefully") {
		t.Errorf("collapsed render = %q, must NOT show the full thinking body", collapsed)
	}
	// expanded: the full reasoning body shows.
	if !strings.Contains(expanded, "carefully") {
		t.Errorf("expanded render = %q, want the full thinking body", expanded)
	}
}

// TestRenderEntryTool locks the tool-kind render: the resolved tool card with its
// header (ToolName + Summary + status glyph), and the result preview honoring the
// expand fold (collapsed → "… N more lines · ctrl+t"; expanded → every line).
func TestRenderEntryTool(t *testing.T) {
	t.Parallel()
	result := make([]string, 0, previewLineCap+3)
	for i := 0; i < previewLineCap+3; i++ {
		result = append(result, "line")
	}
	e := entry{
		ID:   1,
		Kind: kindTool,
		Calls: []ToolCallView{{
			CallID:   callID(1),
			ToolName: "Bash",
			Summary:  "ls -la",
			Status:   ToolOK,
			Result:   result,
		}},
	}
	collapsed := stripANSI(strings.Join(renderEntry(e, false, 80), "\n"))
	expanded := stripANSI(strings.Join(renderEntry(e, true, 80), "\n"))

	if !strings.Contains(collapsed, "Bash") || !strings.Contains(collapsed, "ls -la") {
		t.Errorf("tool render = %q, want the card header", collapsed)
	}
	if !strings.Contains(collapsed, glyphOK) {
		t.Errorf("tool render = %q, want the OK glyph", collapsed)
	}
	if !strings.Contains(collapsed, expandHint) {
		t.Errorf("collapsed tool render = %q, want the fold hint", collapsed)
	}
	if strings.Contains(expanded, expandHint) {
		t.Errorf("expanded tool render = %q, must NOT carry the fold hint", expanded)
	}
}

// TestRenderEntryPromptRecord locks the promptRecord render: the FULL prompt
// context (permission: an "Approve <ToolName>?" header + the wrapped Description;
// user input: the Question + every numbered choice). It is the SCROLLBACK record —
// it must NOT render the compact bottom-box control (no [y]/[s]/[n] key legend).
func TestRenderEntryPromptRecord(t *testing.T) {
	tests := []struct {
		name      string
		ctx       promptContext
		wantSubs  []string // substrings the full record MUST contain
		absentSub []string // substrings the compact control would have but the record must NOT
	}{
		{
			name: "permission renders Approve header and full description",
			ctx: promptContext{
				Kind:        promptPermission,
				ToolName:    "EditFile",
				Description: "cmd/cli/main.go  ·  +7 −0",
			},
			wantSubs:  []string{"Approve EditFile?", "cmd/cli/main.go", "+7 −0"},
			absentSub: []string{"[y] once", "[n] deny"},
		},
		{
			name: "user input renders the question and all numbered choices",
			ctx: promptContext{
				Kind:     promptUserInput,
				Question: "Which version source?",
				Choices:  []string{"version.Version()", "git tag", "CHANGELOG top"},
			},
			wantSubs:  []string{"Which version source?", "1. version.Version()", "2. git tag", "3. CHANGELOG top"},
			absentSub: []string{"↑/↓ select", "[o] other"},
		},
		{
			name: "free-text user input renders only the question (no choices)",
			ctx: promptContext{
				Kind:     promptUserInput,
				Question: "What should the output look like?",
			},
			wantSubs:  []string{"What should the output look like?"},
			absentSub: []string{"1.", "↑/↓ select"},
		},
		{
			name: "empty permission context renders the bare Approve header without panicking",
			ctx: promptContext{
				Kind: promptPermission,
			},
			wantSubs:  []string{"Approve"},
			absentSub: []string{"[y] once"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ctx := tt.ctx
			e := entry{ID: 1, Kind: kindPromptRecord, Prompt: &ctx}
			out := stripANSI(strings.Join(renderEntry(e, false, 80), "\n"))
			for _, sub := range tt.wantSubs {
				if !strings.Contains(out, sub) {
					t.Errorf("promptRecord render = %q, want substring %q", out, sub)
				}
			}
			for _, sub := range tt.absentSub {
				if strings.Contains(out, sub) {
					t.Errorf("promptRecord render = %q, must NOT contain compact-control substring %q", out, sub)
				}
			}
		})
	}
}

// TestRenderEntryChoicesNotFaint locks the styling of recorded user-input choices:
// they render at NORMAL weight, NOT the faint styles.ToolResultStyle reserved for
// subordinate tool output. The faint SGR sequence ToolResultStyle emits is computed
// here (from a known sample) and asserted ABSENT from the rendered, un-stripped
// choice lines — a strip-ANSI assertion cannot catch a wrong style, so this test
// deliberately does NOT strip ANSI.
func TestRenderEntryChoicesNotFaint(t *testing.T) {
	t.Parallel()

	// The faint SGR sequence the tool-result style prepends to any rendered string.
	// If a choice line carried this prefix it would render dim (the bug under fix).
	faintRendered := styles.ToolResultStyle.Render("x")
	faintPrefix, _, ok := strings.Cut(faintRendered, "x")
	if !ok || faintPrefix == "" {
		t.Fatalf("ToolResultStyle.Render(%q) = %q produced no leading SGR prefix to lock against", "x", faintRendered)
	}

	ctx := promptContext{
		Kind:     promptUserInput,
		Question: "Which version source?",
		Choices:  []string{"version.Version()", "git tag"},
	}
	e := entry{ID: 1, Kind: kindPromptRecord, Prompt: &ctx}
	out := strings.Join(renderEntry(e, false, 80), "\n") // NOT stripped — styling is under test

	if strings.Contains(out, faintPrefix) {
		t.Errorf("recorded choices render = %q, must NOT carry the faint tool-result SGR prefix %q", out, faintPrefix)
	}
	// And the choices must still be present at their numbered, indented layout.
	stripped := stripANSI(out)
	for _, want := range []string{choiceIndent + "1. version.Version()", choiceIndent + "2. git tag"} {
		if !strings.Contains(stripped, want) {
			t.Errorf("recorded choices render = %q, want plain numbered line %q", stripped, want)
		}
	}
}

// TestRenderEntryPermissionInteriorBlank locks blank-line preservation in the FULL
// permission record: a multi-line Description with an INTERIOR blank line keeps that
// blank (a multi-line command/diff can carry meaningful gaps), while a single
// leading/trailing blank is trimmed so the body reads tight against the header.
func TestRenderEntryPermissionInteriorBlank(t *testing.T) {
	t.Parallel()
	ctx := promptContext{
		Kind:        promptPermission,
		ToolName:    "Bash",
		Description: "\nline one\n\nline two\n", // leading + trailing trimmed; interior blank kept
	}
	e := entry{ID: 1, Kind: kindPromptRecord, Prompt: &ctx}
	lines := renderEntry(e, false, 80)
	stripped := make([]string, len(lines))
	for i, l := range lines {
		stripped[i] = stripANSI(l)
	}
	joined := strings.Join(stripped, "\n")

	if !strings.Contains(joined, "line one\n\nline two") {
		t.Errorf("permission record = %q, want the interior blank line preserved between the two body lines", joined)
	}
	// Header then body with no leading blank: header is line 0, body starts at line 1.
	if len(stripped) < 1 || !strings.Contains(stripped[0], "Approve Bash?") {
		t.Fatalf("permission record lines = %q, want the Approve header first", stripped)
	}
	if len(stripped) > 1 && stripped[1] == "" {
		t.Errorf("permission record = %q, leading blank line should have been trimmed", stripped)
	}
	if stripped[len(stripped)-1] == "" {
		t.Errorf("permission record = %q, trailing blank line should have been trimmed", stripped)
	}
}

// TestRenderEntryError locks the error-kind render: the failure message rendered
// in the error style. An empty message still yields output (the entry marks the
// failure even with no text).
func TestRenderEntryError(t *testing.T) {
	t.Parallel()
	e := entry{ID: 1, Kind: kindError, Blocks: []content.Block{&content.TextBlock{Text: "context deadline exceeded"}}}
	out := stripANSI(strings.Join(renderEntry(e, false, 80), "\n"))
	if !strings.Contains(out, "context deadline exceeded") {
		t.Errorf("error render = %q, want the failure message", out)
	}
}

// TestRenderEntryInterrupted locks the interrupted-kind render: the content-less
// tombstone line in the interrupted style.
func TestRenderEntryInterrupted(t *testing.T) {
	t.Parallel()
	e := entry{ID: 1, Kind: kindInterrupted}
	lines := renderEntry(e, false, 80)
	if len(lines) == 0 {
		t.Fatal("renderEntry(interrupted) returned no lines")
	}
	out := stripANSI(strings.Join(lines, "\n"))
	if !strings.Contains(out, "interrupted") {
		t.Errorf("interrupted render = %q, want the tombstone marker", out)
	}
}

// TestRenderEntryNilPromptIsSafe locks defense: a kindPromptRecord entry whose
// Prompt context is nil renders without panicking (fail-safe, not crash).
func TestRenderEntryNilPromptIsSafe(t *testing.T) {
	t.Parallel()
	e := entry{ID: 1, Kind: kindPromptRecord, Prompt: nil}
	// must not panic; lines may be empty.
	_ = renderEntry(e, false, 80)
}
