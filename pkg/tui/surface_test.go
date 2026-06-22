package tui

import (
	"strconv"
	"strings"
	"testing"

	"charm.land/lipgloss/v2"

	"github.com/ciram-co/looprig/pkg/event"
	"github.com/ciram-co/looprig/pkg/tool"
	"github.com/ciram-co/looprig/pkg/tui/styles"
)

// TestLiveTailCap covers the pure active-surface budget: the live tail gets the
// terminal height minus the status line, the slash panel (when visible), and the
// bottom box (box frame + content; no separator row). It is floored at 0 and never
// negative.
func TestLiveTailCap(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name                            string
		term, statusH, slashH, contentH int
		want                            int
	}{
		{name: "ample room", term: 40, statusH: 1, slashH: 0, contentH: 1, want: 36},
		// bottomH = box frame(2) + content(1) = 3; 40 - 1 - 0 - 3 = 36 (no separator row)
		{name: "with slash panel", term: 40, statusH: 1, slashH: 3, contentH: 1, want: 33},
		{name: "grown composer shrinks tail", term: 40, statusH: 1, slashH: 0, contentH: 10, want: 27},
		{name: "exact fit floors at zero", term: 4, statusH: 1, slashH: 0, contentH: 1, want: 0},
		// bottomH = 3; 4 - 1 - 0 - 3 = 0
		{name: "overflow floored at zero never negative", term: 3, statusH: 1, slashH: 0, contentH: 1, want: 0},
		{name: "tiny terminal floored at zero", term: 0, statusH: 1, slashH: 0, contentH: 1, want: 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := liveTailCap(tt.term, tt.statusH, tt.slashH, tt.contentH)
			if got != tt.want {
				t.Errorf("liveTailCap(%d,%d,%d,%d) = %d, want %d",
					tt.term, tt.statusH, tt.slashH, tt.contentH, got, tt.want)
			}
			if got < 0 {
				t.Errorf("liveTailCap returned negative %d", got)
			}
		})
	}
}

// TestSurfaceViewCompose covers the composed active surface in compose mode: the
// capped live tail, then the borderless composer panel (no separator rule), then the
// status line — top to bottom, no transcript viewport.
func TestSurfaceViewCompose(t *testing.T) {
	t.Parallel()

	im := newInteractionModel()
	im.input.Resize(60)
	in := surfaceInputs{
		Interaction: im,
		LiveTail:    "● live narration\n  second tail line",
		Status:      StatusRunning,
		StatusState: statusInputs{streaming: true},
		Width:       60,
		Height:      24,
	}

	got := stripANSI(surfaceView(in))

	for _, sub := range []string{"live narration", "streaming…"} {
		if !strings.Contains(got, sub) {
			t.Errorf("surfaceView missing %q in:\n%s", sub, got)
		}
	}
	// The A2 composer is a borderless panel: there is NO separator rule above it.
	if strings.Contains(got, strings.Repeat("─", 10)) {
		t.Errorf("surfaceView should emit no separator rule now, got:\n%s", got)
	}
	// Order: live tail on top, the composer (its placeholder) in the middle, the status
	// line at the very bottom — composer sits directly below the tail, no separator.
	tailIdx := strings.Index(got, "live narration")
	composerIdx := strings.Index(got, "Type a message…")
	statusIdx := strings.LastIndex(got, "streaming…")
	if !(tailIdx < composerIdx && composerIdx < statusIdx) {
		t.Errorf("surfaceView order wrong: tail=%d composer=%d status=%d\n%s", tailIdx, composerIdx, statusIdx, got)
	}
}

// TestSurfaceViewPermissionPrompt covers the surface in permission mode: the bottom
// box is the prompt control (not the composer), and the status reads awaiting
// approval.
func TestSurfaceViewPermissionPrompt(t *testing.T) {
	t.Parallel()

	im := newInteractionModel()
	im = im.ApplyEvent(event.PermissionRequested{ToolExecutionID: callID(1), Request: tool.BashRequest{Command: "go test"}})
	in := surfaceInputs{
		Interaction: im,
		LiveTail:    "",
		Status:      StatusRunning,
		StatusState: statusInputs{permissionActive: true},
		Width:       70,
		Height:      24,
	}

	got := stripANSI(surfaceView(in))

	for _, sub := range []string{"Approve Bash?", "[y] once", "[n] deny", "awaiting approval"} {
		if !strings.Contains(got, sub) {
			t.Errorf("surfaceView (permission) missing %q in:\n%s", sub, got)
		}
	}
}

// TestSurfaceViewChoicePrompt covers the surface in choice mode: the bottom box is
// the AskUser choice control and the status reads awaiting input.
func TestSurfaceViewChoicePrompt(t *testing.T) {
	t.Parallel()

	im := newInteractionModel()
	im = im.ApplyEvent(event.UserInputRequested{ToolExecutionID: callID(2), Question: "Pick one", Choices: []string{"alpha", "beta"}})
	in := surfaceInputs{
		Interaction: im,
		Status:      StatusRunning,
		StatusState: statusInputs{userInputActive: true},
		Width:       70,
		Height:      24,
	}

	got := stripANSI(surfaceView(in))

	for _, sub := range []string{"alpha", "▸", "awaiting input"} {
		if !strings.Contains(got, sub) {
			t.Errorf("surfaceView (choice) missing %q in:\n%s", sub, got)
		}
	}
}

// TestSurfaceCappedTail covers that the live tail never exceeds liveTailCap rows on
// a small terminal: rows beyond the cap are dropped from the bottom-of-tail window
// (they are already committed to scrollback at the next boundary).
func TestSurfaceCappedTail(t *testing.T) {
	t.Parallel()

	var lines []string
	for i := 0; i < 20; i++ {
		lines = append(lines, "tail-row")
	}
	im := newInteractionModel()
	im.input.Resize(40)
	in := surfaceInputs{
		Interaction: im,
		LiveTail:    strings.Join(lines, "\n"),
		Status:      StatusIdle,
		Width:       40,
		Height:      10, // small: cap forces the tail to drop rows
	}

	got := surfaceView(in)
	// Total composed height must not exceed the terminal height.
	if h := lipgloss.Height(got); h > in.Height {
		t.Errorf("surfaceView height = %d, exceeds terminal height %d:\n%s", h, in.Height, got)
	}
}

// assertNoLineExceedsWidth fails if any line of surface is wider than width display
// columns. This is the active-surface invariant the bubbletea v2 inline renderer
// requires: a line wider than the terminal soft-wraps onto an extra physical row,
// which desyncs the renderer's line-count tracking and strands the prior frame
// (separator + box top) into native scrollback on each resize step.
func assertNoLineExceedsWidth(t *testing.T, surface string, width int) {
	t.Helper()
	for i, line := range strings.Split(surface, "\n") {
		if w := lipgloss.Width(line); w > width {
			t.Errorf("line %d display-width %d exceeds terminal width %d: %q", i, w, width, stripANSI(line))
		}
	}
}

// TestSurfaceViewNeverExceedsWidth is the resize-artifact regression: no line of the
// composed active surface may be wider than the terminal width, at any width and for
// any region (live tail, separator, bottom box, slash panel, status). It drives a
// rich live tail whose UNWRAPPED tool-card header (long tool summary) overflows pre-
// fix, across shrinking widths AND a tiny width that pre-fix overflowed the input
// box border. Pre-fix this cascade stranded the separator + input-box top border in
// scrollback on every WindowSizeMsg; the clampSurfaceWidth fail-safe prevents it.
func TestSurfaceViewNeverExceedsWidth(t *testing.T) {
	t.Parallel()

	longWord := strings.Repeat("x", 220) // unwrappable token wider than every case
	// A live tail exercising the regression source: a tool card whose header summary
	// is the unwrappable token (toolHeaderText is not width-wrapped at source).
	calls := []ToolCallView{{
		ToolName: "Bash",
		Summary:  longWord,
		Status:   ToolOK,
		Result:   []string{longWord, "ok"},
	}}
	tail := renderLiveAssistant("reasoning\n"+longWord, "narration "+longWord, calls, true, 80, animState{})

	// Widths cover an ample terminal, several shrinking steps (a resize drag), and a
	// tiny width where the input-box border itself overflowed pre-fix.
	for _, w := range []int{120, 80, 60, 40, 20, 10, 5, 3, 1} {
		w := w
		t.Run(strconv.Itoa(w), func(t *testing.T) {
			t.Parallel()

			im := newInteractionModel()
			im.input.Resize(w)
			im.input.SetValue(longWord) // a long composer value must not overflow either
			in := surfaceInputs{
				Interaction: im,
				LiveTail:    tail,
				Status:      StatusRunning,
				StatusState: statusInputs{streaming: true},
				Width:       w,
				Height:      30,
			}
			assertNoLineExceedsWidth(t, surfaceView(in), w)
		})
	}
}

// TestClampSurfaceWidth covers the fail-safe directly: each line is truncated to the
// width, a zero/negative width drops the surface, and styled (ANSI) lines are
// measured by display columns (the escape bytes do not count toward the width).
func TestClampSurfaceWidth(t *testing.T) {
	t.Parallel()

	styled := styles.StatusStyle.Render(strings.Repeat("─", 50)) // a wide, SGR-styled rule

	tests := []struct {
		name    string
		surface string
		width   int
		// wantMax is the maximum display width any output line may have; -1 means the
		// output must be exactly empty.
		wantMax int
	}{
		{name: "wide plain line truncated", surface: strings.Repeat("a", 100), width: 40, wantMax: 40},
		{name: "wide styled line truncated by display columns", surface: styled, width: 12, wantMax: 12},
		{name: "multiple lines each clamped", surface: strings.Repeat("a", 80) + "\n" + strings.Repeat("b", 80), width: 30, wantMax: 30},
		{name: "already-narrow line untouched", surface: "short", width: 40, wantMax: 5},
		{name: "zero width drops surface", surface: "anything", width: 0, wantMax: -1},
		{name: "negative width drops surface", surface: "anything", width: -5, wantMax: -1},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := clampSurfaceWidth(tt.surface, tt.width)
			if tt.wantMax < 0 {
				if got != "" {
					t.Errorf("clampSurfaceWidth(width=%d) = %q, want empty", tt.width, got)
				}
				return
			}
			for i, line := range strings.Split(got, "\n") {
				if w := lipgloss.Width(line); w > tt.wantMax {
					t.Errorf("line %d display-width %d exceeds %d: %q", i, w, tt.wantMax, stripANSI(line))
				}
			}
		})
	}
}
