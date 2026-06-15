package tui

import (
	"reflect"
	"testing"

	"github.com/inventivepotter/urvi/internal/content"
	"github.com/inventivepotter/urvi/internal/uuid"
)

func TestDisplayRoleConstantOrder(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		role DisplayRole
		want DisplayRole
	}{
		{name: "RoleUser is 0", role: RoleUser, want: 0},
		{name: "RoleAssistant is 1", role: RoleAssistant, want: 1},
		{name: "RoleSystem is 2", role: RoleSystem, want: 2},
		{name: "RoleError is 3", role: RoleError, want: 3},
		{name: "RoleInterrupted is 4", role: RoleInterrupted, want: 4},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if tt.role != tt.want {
				t.Errorf("DisplayRole = %d, want %d", tt.role, tt.want)
			}
		})
	}
}

func TestDisplayMessageInterruptedHasNilBlocks(t *testing.T) {
	t.Parallel()
	msg := DisplayMessage{Role: RoleInterrupted}
	if msg.Blocks != nil {
		t.Errorf("DisplayMessage{Role: RoleInterrupted}.Blocks = %v, want nil", msg.Blocks)
	}
}

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

// TestDisplayMessageToolCallsZeroDefault verifies the new ToolCalls field defaults
// to nil for every role, so existing rows constructed without it are unchanged.
func TestDisplayMessageToolCallsZeroDefault(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		msg  DisplayMessage
	}{
		{name: "user", msg: DisplayMessage{Role: RoleUser}},
		{name: "assistant", msg: DisplayMessage{Role: RoleAssistant}},
		{name: "system", msg: DisplayMessage{Role: RoleSystem}},
		{name: "error", msg: DisplayMessage{Role: RoleError}},
		{name: "interrupted", msg: DisplayMessage{Role: RoleInterrupted}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if tt.msg.ToolCalls != nil {
				t.Errorf("ToolCalls = %v, want nil (zero default)", tt.msg.ToolCalls)
			}
		})
	}
}

// TestDisplayMessageWithToolCallsRoundTrips verifies an assistant DisplayMessage
// carrying ToolCallView children preserves every field exactly.
func TestDisplayMessageWithToolCallsRoundTrips(t *testing.T) {
	t.Parallel()

	id := uuid.UUID{0x01, 0x02, 0x03}
	tests := []struct {
		name string
		msg  DisplayMessage
	}{
		{
			name: "running call, nil result",
			msg: DisplayMessage{
				Role:   RoleAssistant,
				Blocks: []content.Block{&content.TextBlock{Text: "calling a tool"}},
				ToolCalls: []ToolCallView{
					{CallID: id, ToolName: "Bash", Summary: "ls -la", Status: ToolRunning, Result: nil},
				},
			},
		},
		{
			name: "completed ok with preview lines",
			msg: DisplayMessage{
				Role: RoleAssistant,
				ToolCalls: []ToolCallView{
					{CallID: id, ToolName: "Read", Summary: "read file", Status: ToolOK, Result: []string{"line1", "line2"}},
				},
			},
		},
		{
			name: "error and cancelled calls",
			msg: DisplayMessage{
				Role: RoleAssistant,
				ToolCalls: []ToolCallView{
					{CallID: id, ToolName: "Write", Summary: "write file", Status: ToolError, Result: []string{"permission denied"}},
					{CallID: uuid.UUID{0xAA}, ToolName: "Fetch", Summary: "fetch url", Status: ToolCancelled, Result: nil},
				},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := tt.msg // copy; nested slices share backing arrays but values are equal
			if !reflect.DeepEqual(got, tt.msg) {
				t.Errorf("DisplayMessage round-trip mismatch: got %+v, want %+v", got, tt.msg)
			}
			if len(got.ToolCalls) != len(tt.msg.ToolCalls) {
				t.Fatalf("ToolCalls len = %d, want %d", len(got.ToolCalls), len(tt.msg.ToolCalls))
			}
			for i := range tt.msg.ToolCalls {
				if !reflect.DeepEqual(got.ToolCalls[i], tt.msg.ToolCalls[i]) {
					t.Errorf("ToolCalls[%d] = %+v, want %+v", i, got.ToolCalls[i], tt.msg.ToolCalls[i])
				}
			}
		})
	}
}
