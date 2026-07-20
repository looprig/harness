package command_test

import (
	"testing"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/command"
	"github.com/looprig/harness/pkg/gate"
)

// gateRouter is the narrow accessor runLoop uses to route a control command to
// the gate waiting on its ToolExecutionID. Every control command must satisfy it.
type gateRouter interface {
	command.Command
	GateToolExecutionID() uuid.UUID
}

// TestControlCommandsSatisfyContracts asserts the three control commands satisfy
// Command (sealed), expose the shared GateToolExecutionID() accessor returning the set
// ToolExecutionID, and round-trip their embedded Header.ID. Table-driven across happy,
// boundary (zero ToolExecutionID/Header), and per-type cases.
func TestControlCommandsSatisfyContracts(t *testing.T) {
	t.Parallel()

	id := func(t *testing.T) uuid.UUID {
		t.Helper()
		u, err := uuid.New()
		if err != nil {
			t.Fatalf("uuid.New: %v", err)
		}
		return u
	}

	headerID := id(t)
	callID := id(t)

	tests := []struct {
		name       string
		cmd        gateRouter
		wantHeader uuid.UUID
		wantCallID uuid.UUID
	}{
		{
			name:       "ApproveToolCall set fields",
			cmd:        command.ApproveToolCall{Header: command.Header{CommandID: headerID}, GateRoute: command.GateRoute{ToolExecutionID: callID}, Action: gate.ApprovalApprove},
			wantHeader: headerID,
			wantCallID: callID,
		},
		{
			name:       "DenyToolCall set fields",
			cmd:        command.DenyToolCall{Header: command.Header{CommandID: headerID}, GateRoute: command.GateRoute{ToolExecutionID: callID}},
			wantHeader: headerID,
			wantCallID: callID,
		},
		{
			name:       "ProvideUserInput set fields",
			cmd:        command.ProvideUserInput{Header: command.Header{CommandID: headerID}, GateRoute: command.GateRoute{ToolExecutionID: callID}, Answer: "yes"},
			wantHeader: headerID,
			wantCallID: callID,
		},
		{
			name:       "ApproveToolCall zero ToolExecutionID is boundary",
			cmd:        command.ApproveToolCall{Header: command.Header{CommandID: headerID}},
			wantHeader: headerID,
			wantCallID: uuid.UUID{},
		},
		{
			name:       "DenyToolCall zero header is boundary",
			cmd:        command.DenyToolCall{GateRoute: command.GateRoute{ToolExecutionID: callID}},
			wantHeader: uuid.UUID{},
			wantCallID: callID,
		},
		{
			name:       "ProvideUserInput empty answer is boundary",
			cmd:        command.ProvideUserInput{Header: command.Header{CommandID: headerID}, GateRoute: command.GateRoute{ToolExecutionID: callID}, Answer: ""},
			wantHeader: headerID,
			wantCallID: callID,
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := tt.cmd.GateToolExecutionID(); got != tt.wantCallID {
				t.Errorf("GateToolExecutionID() = %v, want %v", got, tt.wantCallID)
			}
			if got := tt.cmd.CommandHeader().CommandID; got != tt.wantHeader {
				t.Errorf("CommandHeader().CommandID = %v, want %v", got, tt.wantHeader)
			}
		})
	}
}

// TestApproveActionPreserved asserts ApproveToolCall preserves the approval
// action (the only field runLoop consults beyond the ToolExecutionID), across
// both approve actions.
func TestApproveActionPreserved(t *testing.T) {
	t.Parallel()
	actions := []struct {
		name   string
		action gate.ApprovalAction
	}{
		{"approve once", gate.ApprovalApprove},
		{"approve always for this workspace", gate.ApprovalApproveAlwaysWorkspace},
	}
	for _, tc := range actions {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cmd := command.ApproveToolCall{Action: tc.action}
			if cmd.Action != tc.action {
				t.Errorf("Action = %v, want %v", cmd.Action, tc.action)
			}
		})
	}
}
