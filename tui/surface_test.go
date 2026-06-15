package tui

import (
	"strings"
	"testing"

	"charm.land/lipgloss/v2"

	"github.com/inventivepotter/urvi/internal/agent/loop/event"
	"github.com/inventivepotter/urvi/internal/tool"
)

// TestLiveTailCap covers the pure active-surface budget: the live tail gets the
// terminal height minus the status line, the slash panel (when visible), and the
// bottom box (separator + box border + content). It is floored at 0 and never
// negative.
func TestLiveTailCap(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name                            string
		term, statusH, slashH, contentH int
		want                            int
	}{
		{name: "ample room", term: 40, statusH: 1, slashH: 0, contentH: 1, want: 35},
		// bottomH = sep(1) + border(2) + content(1) = 4; 40 - 1 - 0 - 4 = 35
		{name: "with slash panel", term: 40, statusH: 1, slashH: 3, contentH: 1, want: 32},
		{name: "grown composer shrinks tail", term: 40, statusH: 1, slashH: 0, contentH: 10, want: 26},
		{name: "exact fit floors at zero", term: 5, statusH: 1, slashH: 0, contentH: 1, want: 0},
		// bottomH = 4; 5 - 1 - 0 - 4 = 0
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
// capped live tail, then the separator rule, then the composer box, then the status
// line — top to bottom, no transcript viewport.
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
	// A separator rule (run of horizontal-rule chars) must appear above the box.
	if !strings.Contains(got, strings.Repeat("─", 10)) {
		t.Errorf("surfaceView missing a separator rule in:\n%s", got)
	}
	// Order: live tail above the separator, status line at the very bottom.
	tailIdx := strings.Index(got, "live narration")
	sepIdx := strings.Index(got, strings.Repeat("─", 10))
	statusIdx := strings.LastIndex(got, "streaming…")
	if !(tailIdx < sepIdx && sepIdx < statusIdx) {
		t.Errorf("surfaceView order wrong: tail=%d sep=%d status=%d\n%s", tailIdx, sepIdx, statusIdx, got)
	}
}

// TestSurfaceViewPermissionPrompt covers the surface in permission mode: the bottom
// box is the prompt control (not the composer), and the status reads awaiting
// approval.
func TestSurfaceViewPermissionPrompt(t *testing.T) {
	t.Parallel()

	im := newInteractionModel()
	im = im.ApplyEvent(event.PermissionRequested{CallID: callID(1), Request: tool.BashRequest{Command: "go test"}})
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
	im = im.ApplyEvent(event.UserInputRequested{CallID: callID(2), Question: "Pick one", Choices: []string{"alpha", "beta"}})
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
