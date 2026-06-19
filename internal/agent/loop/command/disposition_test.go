package command_test

import (
	"testing"

	"github.com/inventivepotter/urvi/internal/agent/loop/command"
)

// TestCancelResultVariants asserts both CancelResult variants are sealed and
// round-trip. Cancelled is an empty marker; AlreadyCommitted carries the TurnID.
func TestCancelResultVariants(t *testing.T) {
	t.Parallel()

	turnID := newID(t)

	tests := []struct {
		name   string
		r      command.CancelResult
		assert func(t *testing.T, r command.CancelResult)
	}{
		{
			name: "Cancelled is the removed-before-commit marker",
			r:    command.Cancelled{},
			assert: func(t *testing.T, r command.CancelResult) {
				if _, ok := r.(command.Cancelled); !ok {
					t.Errorf("r = %T, want Cancelled", r)
				}
			},
		},
		{
			name: "AlreadyCommitted carries the TurnID",
			r:    command.AlreadyCommitted{TurnID: turnID},
			assert: func(t *testing.T, r command.CancelResult) {
				ac, ok := r.(command.AlreadyCommitted)
				if !ok || ac.TurnID != turnID {
					t.Errorf("r = %+v, want AlreadyCommitted{%v}", r, turnID)
				}
			},
		},
		{
			name: "AlreadyCommitted zero TurnID is boundary",
			r:    command.AlreadyCommitted{},
			assert: func(t *testing.T, r command.CancelResult) {
				ac, _ := r.(command.AlreadyCommitted)
				if !ac.TurnID.IsZero() {
					t.Errorf("TurnID = %v, want zero", ac.TurnID)
				}
			},
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			tt.assert(t, tt.r)
		})
	}
}
