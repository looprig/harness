package tui

import (
	"testing"

	"github.com/inventivepotter/urvi/internal/agent/loop/event"
	"github.com/inventivepotter/urvi/internal/uuid"
)

// TestDefaultEventFilter locks the single-loop TUI default filter shape: live
// Ephemeral tokens from the primary loop ONLY, and Enduring events from EVERY loop,
// with session-scoped events always passing. It drives a table through the shared
// event.ShouldDeliver predicate so the assertion is the real fan-out decision, not a
// re-derivation of it.
func TestDefaultEventFilter(t *testing.T) {
	t.Parallel()

	primary := loopID(1)
	subagent := loopID(2)
	filter := DefaultEventFilter(primary)

	tests := []struct {
		name string
		ev   event.Event
		want bool
	}{
		{
			name: "primary loop TokenDelta delivers (live tokens from the watched loop)",
			ev:   event.TokenDelta{Header: event.Header{LoopID: primary}},
			want: true,
		},
		{
			name: "subagent TokenDelta is filtered out (its firehose never enters egress)",
			ev:   event.TokenDelta{Header: event.Header{LoopID: subagent}},
			want: false,
		},
		{
			name: "primary loop StepDone delivers (finalized group)",
			ev:   event.StepDone{Header: event.Header{LoopID: primary}},
			want: true,
		},
		{
			name: "subagent StepDone delivers (all-loop Enduring: collapsed-but-present)",
			ev:   event.StepDone{Header: event.Header{LoopID: subagent}},
			want: true,
		},
		{
			name: "subagent TurnDone terminal delivers (all-loop Enduring)",
			ev:   event.TurnDone{Header: event.Header{LoopID: subagent}},
			want: true,
		},
		{
			name: "session-scoped SessionIdle always delivers (bypasses the loop filter)",
			ev:   event.SessionIdle{Header: event.Header{SessionID: loopID(9)}},
			want: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := event.ShouldDeliver(filter, tt.ev); got != tt.want {
				t.Errorf("ShouldDeliver(default, %T) = %v, want %v", tt.ev, got, tt.want)
			}
		})
	}
}

// loopID builds a deterministic non-zero loop uuid from one byte (callID is defined
// in screen_test.go with the same shape; this name documents the loop-id intent).
func loopID(b byte) uuid.UUID {
	var u uuid.UUID
	u[0] = b
	return u
}
