package tui

import "testing"

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
