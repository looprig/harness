package command_test

import (
	"testing"

	"github.com/inventivepotter/urvi/internal/agent/loop/command"
)

// TestDispositionVariants asserts every Disposition variant is sealed (implements
// the interface) and round-trips its fields. The table covers each concrete type
// plus boundary (zero ids) and every RejectReason.
func TestDispositionVariants(t *testing.T) {
	t.Parallel()

	turnID := newID(t)
	inputID := newID(t)

	tests := []struct {
		name   string
		d      command.Disposition
		assert func(t *testing.T, d command.Disposition)
	}{
		{
			name: "Started carries TurnID and InputID",
			d:    command.Started{TurnID: turnID, InputID: inputID},
			assert: func(t *testing.T, d command.Disposition) {
				s, ok := d.(command.Started)
				if !ok {
					t.Fatalf("d = %T, want Started", d)
				}
				if s.TurnID != turnID || s.InputID != inputID {
					t.Errorf("Started = %+v, want TurnID=%v InputID=%v", s, turnID, inputID)
				}
			},
		},
		{
			name: "InputQueued carries InputID, no TurnID",
			d:    command.InputQueued{InputID: inputID},
			assert: func(t *testing.T, d command.Disposition) {
				q, ok := d.(command.InputQueued)
				if !ok || q.InputID != inputID {
					t.Errorf("d = %+v, want InputQueued{%v}", d, inputID)
				}
			},
		},
		{
			name: "TurnRejected RejectBusy",
			d:    command.TurnRejected{Reason: command.RejectBusy},
			assert: func(t *testing.T, d command.Disposition) {
				r, ok := d.(command.TurnRejected)
				if !ok || r.Reason != command.RejectBusy {
					t.Errorf("d = %+v, want TurnRejected{RejectBusy}", d)
				}
			},
		},
		{
			name: "TurnRejected RejectQueueFull",
			d:    command.TurnRejected{Reason: command.RejectQueueFull},
			assert: func(t *testing.T, d command.Disposition) {
				r, _ := d.(command.TurnRejected)
				if r.Reason != command.RejectQueueFull {
					t.Errorf("reason = %d, want RejectQueueFull", r.Reason)
				}
			},
		},
		{
			name: "TurnRejected RejectShuttingDown",
			d:    command.TurnRejected{Reason: command.RejectShuttingDown},
			assert: func(t *testing.T, d command.Disposition) {
				r, _ := d.(command.TurnRejected)
				if r.Reason != command.RejectShuttingDown {
					t.Errorf("reason = %d, want RejectShuttingDown", r.Reason)
				}
			},
		},
		{
			name: "Started zero ids is boundary",
			d:    command.Started{},
			assert: func(t *testing.T, d command.Disposition) {
				s, _ := d.(command.Started)
				if !s.TurnID.IsZero() || !s.InputID.IsZero() {
					t.Errorf("zero Started = %+v, want zero ids", s)
				}
			},
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			tt.assert(t, tt.d)
		})
	}
}

// TestRejectReasonValues pins the RejectReason enum ordering.
func TestRejectReasonValues(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		r    command.RejectReason
		want command.RejectReason
	}{
		{name: "RejectBusy is zero", r: command.RejectBusy, want: 0},
		{name: "RejectQueueFull is one", r: command.RejectQueueFull, want: 1},
		{name: "RejectShuttingDown is two", r: command.RejectShuttingDown, want: 2},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if tt.r != tt.want {
				t.Errorf("reason = %d, want %d", tt.r, tt.want)
			}
		})
	}
}

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
