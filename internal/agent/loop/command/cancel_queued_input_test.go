package command_test

import (
	"testing"

	"github.com/inventivepotter/urvi/internal/agent/loop/command"
	"github.com/inventivepotter/urvi/internal/uuid"
)

// TestCancelQueuedInputSatisfiesCommand asserts CancelQueuedInput is a sealed
// Command, round-trips its Header, and carries the Route + InputID used to resolve
// the queued submit.
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
			cmd:         command.CancelQueuedInput{Header: command.Header{ID: headerID}, Route: command.Route{LoopID: loopID}, InputID: inputID},
			wantHeader:  headerID,
			wantLoopID:  loopID,
			wantInputID: inputID,
		},
		{
			name:        "zero header and route is boundary",
			cmd:         command.CancelQueuedInput{InputID: inputID},
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
			if got := tt.cmd.CommandHeader().ID; got != tt.wantHeader {
				t.Errorf("Header.ID = %v, want %v", got, tt.wantHeader)
			}
			if got := tt.cmd.Route.LoopID; got != tt.wantLoopID {
				t.Errorf("Route.LoopID = %v, want %v", got, tt.wantLoopID)
			}
			if tt.cmd.InputID != tt.wantInputID {
				t.Errorf("InputID = %v, want %v", tt.cmd.InputID, tt.wantInputID)
			}
		})
	}
}

// TestGateCommandsCarryRoute asserts the gate commands gained a Route field while
// keeping CallID working: the loop still routes by CallID for now, so GateCallID()
// must return the set CallID regardless of Route.
func TestGateCommandsCarryRoute(t *testing.T) {
	t.Parallel()

	callID := newID(t)
	loopID := newID(t)
	route := command.Route{LoopID: loopID, ToolCallID: callID}

	tests := []struct {
		name string
		cmd  gateRouter
	}{
		{name: "ApproveToolCall", cmd: command.ApproveToolCall{Route: route, CallID: callID}},
		{name: "DenyToolCall", cmd: command.DenyToolCall{Route: route, CallID: callID}},
		{name: "ProvideUserInput", cmd: command.ProvideUserInput{Route: route, CallID: callID, Answer: "x"}},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := tt.cmd.GateCallID(); got != callID {
				t.Errorf("GateCallID() = %v, want %v (Route must not change CallID routing)", got, callID)
			}
		})
	}
}
