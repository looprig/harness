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
		{name: "idle is empty", status: StatusIdle, wantEmpty: true},
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
