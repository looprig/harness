package tui

import (
	"strings"
	"testing"
)

func TestStatusConstantOrder(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		got  Status
		want uint8
	}{
		{name: "idle", got: StatusIdle, want: 0},
		{name: "running", got: StatusRunning, want: 1},
		{name: "interrupting", got: StatusInterrupting, want: 2},
		{name: "resetting", got: StatusResetting, want: 3},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if uint8(tt.got) != tt.want {
				t.Errorf("Status %s = %d, want %d", tt.name, uint8(tt.got), tt.want)
			}
		})
	}
}

func TestRenderStatusLine(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		status     Status
		wantEmpty  bool
		wantSubstr string
	}{
		{name: "idle reads idle", status: StatusIdle, wantSubstr: "idle"},
		{name: "running contains thinking", status: StatusRunning, wantSubstr: "thinking"},
		{name: "interrupting contains interrupting", status: StatusInterrupting, wantSubstr: "interrupting"},
		{name: "resetting contains clearing", status: StatusResetting, wantSubstr: "clearing"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := RenderStatusLine(tt.status)

			if tt.wantEmpty {
				if got != "" {
					t.Errorf("RenderStatusLine(%v) = %q, want empty", tt.status, got)
				}
				return
			}

			if got == "" {
				t.Errorf("RenderStatusLine(%v) = empty, want non-empty", tt.status)
			}
			if !strings.Contains(got, tt.wantSubstr) {
				t.Errorf("RenderStatusLine(%v) = %q, want substring %q", tt.status, got, tt.wantSubstr)
			}
		})
	}
}

// TestStatusLabel covers the pure status-label derivation table (design §"Thinking
// & status line"): the label is computed from the session Status plus the
// interaction state (a prompt active, only-thinking-so-far, streaming text), with
// the awaiting-approval / awaiting-input prompt labels taking precedence over the
// underlying Running status.
func TestStatusLabel(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		status Status
		st     statusInputs
		want   string
	}{
		{name: "idle reads idle", status: StatusIdle, st: statusInputs{}, want: "idle"},
		{
			name:   "streaming text",
			status: StatusRunning,
			st:     statusInputs{streaming: true},
			want:   "streaming…",
		},
		{
			name:   "only thinking so far",
			status: StatusRunning,
			st:     statusInputs{thinking: true},
			want:   "thinking…",
		},
		{
			name:   "permission prompt active",
			status: StatusRunning,
			st:     statusInputs{permissionActive: true, streaming: true},
			want:   "awaiting approval",
		},
		{
			name:   "askuser prompt active",
			status: StatusRunning,
			st:     statusInputs{userInputActive: true},
			want:   "awaiting input",
		},
		{
			name:   "interrupting",
			status: StatusInterrupting,
			st:     statusInputs{},
			want:   "interrupting…",
		},
		{
			name:   "clearing",
			status: StatusResetting,
			st:     statusInputs{},
			want:   "clearing…",
		},
		{
			name:   "running with no signal reads waiting (request in flight)",
			status: StatusRunning,
			st:     statusInputs{},
			want:   "waiting…",
		},
		{
			name:   "permission beats streaming",
			status: StatusRunning,
			st:     statusInputs{permissionActive: true, thinking: true, streaming: true},
			want:   "awaiting approval",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := statusLabel(tt.status, tt.st); got != tt.want {
				t.Errorf("statusLabel(%v, %+v) = %q, want %q", tt.status, tt.st, got, tt.want)
			}
		})
	}
}

// TestStatusDot covers the leading status dot: a hollow ring at rest, a filled dot
// while a turn is live, a lime↔white pulse (output differs by blink phase) while
// waiting/thinking, and a steady dot while streaming.
func TestStatusDot(t *testing.T) {
	t.Parallel()

	glyph := []struct {
		name   string
		status Status
		in     statusInputs
		want   string
	}{
		{name: "idle is hollow", status: StatusIdle, in: statusInputs{}, want: dotHollow},
		{name: "waiting is filled", status: StatusRunning, in: statusInputs{}, want: dotFilled},
		{name: "thinking is filled", status: StatusRunning, in: statusInputs{thinking: true}, want: dotFilled},
		{name: "streaming is filled", status: StatusRunning, in: statusInputs{streaming: true}, want: dotFilled},
		{name: "prompt is filled", status: StatusRunning, in: statusInputs{permissionActive: true}, want: dotFilled},
	}
	for _, tt := range glyph {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := stripANSI(statusDot(tt.status, tt.in, false)); got != tt.want {
				t.Errorf("statusDot(%v, %+v) glyph = %q, want %q", tt.status, tt.in, got, tt.want)
			}
		})
	}

	// Waiting/thinking pulse: the rendered dot must differ between blink phases.
	pulse := []statusInputs{{}, {thinking: true}}
	for _, in := range pulse {
		if statusDot(StatusRunning, in, true) == statusDot(StatusRunning, in, false) {
			t.Errorf("statusDot(running, %+v) does not pulse across the blink phase", in)
		}
	}
	// Streaming holds steady (no pulse).
	if statusDot(StatusRunning, statusInputs{streaming: true}, true) != statusDot(StatusRunning, statusInputs{streaming: true}, false) {
		t.Error("statusDot(streaming) should not pulse across the blink phase")
	}
}
