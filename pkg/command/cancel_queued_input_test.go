package command_test

import (
	"testing"

	"github.com/looprig/harness/pkg/command"
	"github.com/looprig/harness/pkg/identity"
	"github.com/looprig/harness/pkg/uuid"
)

// TestCancelQueuedInputSatisfiesCommand asserts CancelQueuedInput is a sealed
// Command, round-trips its Header, and carries the Coordinates + TargetCommandID
// used to resolve the queued submit.
func TestCancelQueuedInputSatisfiesCommand(t *testing.T) {
	t.Parallel()

	headerID := newID(t)
	loopID := newID(t)
	inputID := newID(t)

	tests := []struct {
		name        string
		cmd         command.CancelQueuedInput
		wantHeader  uuid.UUID
		wantLoopID  uuid.UUID
		wantInputID uuid.UUID
	}{
		{
			name:        "fully addressed",
			cmd:         command.CancelQueuedInput{Header: command.Header{CommandID: headerID}, Coordinates: identity.Coordinates{LoopID: loopID}, TargetCommandID: inputID},
			wantHeader:  headerID,
			wantLoopID:  loopID,
			wantInputID: inputID,
		},
		{
			name:        "zero header and coordinates is boundary",
			cmd:         command.CancelQueuedInput{TargetCommandID: inputID},
			wantHeader:  uuid.UUID{},
			wantLoopID:  uuid.UUID{},
			wantInputID: inputID,
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var _ command.Command = tt.cmd
			if got := tt.cmd.CommandHeader().CommandID; got != tt.wantHeader {
				t.Errorf("Header.CommandID = %v, want %v", got, tt.wantHeader)
			}
			if got := tt.cmd.LoopID; got != tt.wantLoopID {
				t.Errorf("Coordinates.LoopID = %v, want %v", got, tt.wantLoopID)
			}
			if tt.cmd.TargetCommandID != tt.wantInputID {
				t.Errorf("TargetCommandID = %v, want %v", tt.cmd.TargetCommandID, tt.wantInputID)
			}
		})
	}
}

// TestGateCommandsCarryRoute asserts the gate commands embed a GateRoute whose
// ToolExecutionID the loop matches against: GateToolExecutionID() must return the
// embedded GateRoute.ToolExecutionID.
func TestGateCommandsCarryRoute(t *testing.T) {
	t.Parallel()

	callID := newID(t)
	loopID := newID(t)
	route := command.GateRoute{Coordinates: identity.Coordinates{LoopID: loopID}, ToolExecutionID: callID}

	tests := []struct {
		name string
		cmd  gateRouter
	}{
		{name: "ApproveToolCall", cmd: command.ApproveToolCall{GateRoute: route}},
		{name: "DenyToolCall", cmd: command.DenyToolCall{GateRoute: route}},
		{name: "ProvideUserInput", cmd: command.ProvideUserInput{GateRoute: route, Answer: "x"}},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := tt.cmd.GateToolExecutionID(); got != callID {
				t.Errorf("GateToolExecutionID() = %v, want %v", got, callID)
			}
		})
	}
}
