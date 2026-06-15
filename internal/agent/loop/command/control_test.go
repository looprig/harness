package command_test

import (
	"testing"

	"github.com/inventivepotter/urvi/internal/agent/loop/command"
	"github.com/inventivepotter/urvi/internal/tool"
	"github.com/inventivepotter/urvi/internal/uuid"
)

// gateRouter is the narrow accessor listen uses to route a control command to
// the gate waiting on its CallID. Every control command must satisfy it.
type gateRouter interface {
	command.Command
	GateCallID() uuid.UUID
}

// TestControlCommandsSatisfyContracts asserts the three control commands satisfy
// Command (sealed), expose the shared GateCallID() accessor returning the set
// CallID, and round-trip their embedded Header.ID. Table-driven across happy,
// boundary (zero CallID/Header), and per-type cases.
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
			cmd:        command.ApproveToolCall{Header: command.Header{ID: headerID}, CallID: callID, Scope: tool.ScopeSession},
			wantHeader: headerID,
			wantCallID: callID,
		},
		{
			name:       "DenyToolCall set fields",
			cmd:        command.DenyToolCall{Header: command.Header{ID: headerID}, CallID: callID},
			wantHeader: headerID,
			wantCallID: callID,
		},
		{
			name:       "ProvideUserInput set fields",
			cmd:        command.ProvideUserInput{Header: command.Header{ID: headerID}, CallID: callID, Answer: "yes"},
			wantHeader: headerID,
			wantCallID: callID,
		},
		{
			name:       "ApproveToolCall zero CallID is boundary",
			cmd:        command.ApproveToolCall{Header: command.Header{ID: headerID}},
			wantHeader: headerID,
			wantCallID: uuid.UUID{},
		},
		{
			name:       "DenyToolCall zero header is boundary",
			cmd:        command.DenyToolCall{CallID: callID},
			wantHeader: uuid.UUID{},
			wantCallID: callID,
		},
		{
			name:       "ProvideUserInput empty answer is boundary",
			cmd:        command.ProvideUserInput{Header: command.Header{ID: headerID}, CallID: callID, Answer: ""},
			wantHeader: headerID,
			wantCallID: callID,
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := tt.cmd.GateCallID(); got != tt.wantCallID {
				t.Errorf("GateCallID() = %v, want %v", got, tt.wantCallID)
			}
			if got := tt.cmd.CommandHeader().ID; got != tt.wantHeader {
				t.Errorf("CommandHeader().ID = %v, want %v", got, tt.wantHeader)
			}
		})
	}
}

// TestApproveScopeRoundTrips asserts ApproveToolCall preserves the granted scope
// (the only field listen consults beyond the CallID), across every scope value.
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
