package command_test

import (
	"testing"

	"github.com/ciram-co/looprig/pkg/command"
	"github.com/ciram-co/looprig/pkg/tool"
	"github.com/ciram-co/looprig/pkg/uuid"
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
			cmd:        command.ApproveToolCall{Header: command.Header{CommandID: headerID}, GateRoute: command.GateRoute{ToolExecutionID: callID}, Scope: tool.ScopeSession},
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

// TestApproveScopeRoundTrips asserts ApproveToolCall preserves the granted scope
// (the only field runLoop consults beyond the ToolExecutionID), across every scope value.
func TestApproveScopeRoundTrips(t *testing.T) {
	t.Parallel()
	scopes := []struct {
		name  string
		scope tool.ApprovalScope
	}{
		{"once", tool.ScopeOnce},
		{"session", tool.ScopeSession},
		{"workspace", tool.ScopeWorkspace},
	}
	for _, sc := range scopes {
		sc := sc
		t.Run(sc.name, func(t *testing.T) {
			t.Parallel()
			cmd := command.ApproveToolCall{Scope: sc.scope}
			if cmd.Scope != sc.scope {
				t.Errorf("Scope = %v, want %v", cmd.Scope, sc.scope)
			}
		})
	}
}
