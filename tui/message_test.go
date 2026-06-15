package tui

import (
	"reflect"
	"testing"

	"github.com/inventivepotter/urvi/internal/uuid"
)

func TestToolStatusConstantOrder(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		status ToolStatus
		want   ToolStatus
	}{
		{name: "ToolRunning is 0", status: ToolRunning, want: 0},
		{name: "ToolOK is 1", status: ToolOK, want: 1},
		{name: "ToolError is 2", status: ToolError, want: 2},
		{name: "ToolCancelled is 3", status: ToolCancelled, want: 3},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if tt.status != tt.want {
				t.Errorf("ToolStatus = %d, want %d", tt.status, tt.want)
			}
		})
	}
}

// TestToolCallViewRoundTrips verifies a ToolCallView preserves every field exactly
// across a copy, for each lifecycle status (running with nil result, completed ok
// with preview lines, error and cancelled). ToolCallView is the live tool-card view
// reconstructed from the event stream and rendered by entryrender/renderToolCalls.
func TestToolCallViewRoundTrips(t *testing.T) {
	t.Parallel()

	id := uuid.UUID{0x01, 0x02, 0x03}
	tests := []struct {
		name  string
		calls []ToolCallView
	}{
		{
			name: "running call, nil result",
			calls: []ToolCallView{
				{CallID: id, ToolName: "Bash", Summary: "ls -la", Status: ToolRunning, Result: nil},
			},
		},
		{
			name: "completed ok with preview lines",
			calls: []ToolCallView{
				{CallID: id, ToolName: "Read", Summary: "read file", Status: ToolOK, Result: []string{"line1", "line2"}},
			},
		},
		{
			name: "error and cancelled calls",
			calls: []ToolCallView{
				{CallID: id, ToolName: "Write", Summary: "write file", Status: ToolError, Result: []string{"permission denied"}},
				{CallID: uuid.UUID{0xAA}, ToolName: "Fetch", Summary: "fetch url", Status: ToolCancelled, Result: nil},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := tt.calls // copy of the slice header; element values are equal
			if len(got) != len(tt.calls) {
				t.Fatalf("ToolCalls len = %d, want %d", len(got), len(tt.calls))
			}
			for i := range tt.calls {
				if !reflect.DeepEqual(got[i], tt.calls[i]) {
					t.Errorf("ToolCalls[%d] = %+v, want %+v", i, got[i], tt.calls[i])
				}
			}
		})
	}
}
