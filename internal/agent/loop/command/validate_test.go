package command_test

import (
	"errors"
	"testing"

	"github.com/inventivepotter/urvi/internal/agent/loop/command"
	"github.com/inventivepotter/urvi/internal/agent/loop/identity"
)

// TestValidateCommandValid asserts every addressed command type, populated to its
// full required identity/addressing, passes ValidateCommand. This is the happy-path
// row per command type (including the no-addressing control commands).
func TestValidateCommandValid(t *testing.T) {
	t.Parallel()

	cmdID := newID(t)
	sess := newID(t)
	loop := newID(t)
	toolID := newID(t)
	target := newID(t)

	route := command.GateRoute{Coordinates: identity.Coordinates{LoopID: loop}, ToolExecutionID: toolID}
	hdr := command.Header{CommandID: cmdID}

	tests := []struct {
		name string
		cmd  command.Command
	}{
		{"UserInput", command.UserInput{Header: hdr}},
		{"SubagentResult", command.SubagentResult{Header: hdr, Coordinates: identity.Coordinates{LoopID: loop}}},
		{"CancelQueuedInput", command.CancelQueuedInput{Header: hdr, Coordinates: identity.Coordinates{SessionID: sess, LoopID: loop}, TargetCommandID: target}},
		{"ApproveToolCall", command.ApproveToolCall{Header: hdr, GateRoute: route}},
		{"DenyToolCall", command.DenyToolCall{Header: hdr, GateRoute: route}},
		{"ProvideUserInput", command.ProvideUserInput{Header: hdr, GateRoute: route, Answer: "x"}},
		{"Interrupt (session-wide, only CommandID)", command.Interrupt{Header: hdr}},
		{"Shutdown (session-wide, only CommandID)", command.Shutdown{Header: hdr}},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if err := command.ValidateCommand(tt.cmd); err != nil {
				t.Errorf("ValidateCommand(%s) = %v, want nil", tt.name, err)
			}
		})
	}
}

// TestValidateCommandInvalid asserts each forbidden/missing case returns a typed
// *CommandValidationError naming the offending field + rule. Covers: zero CommandID;
// SubagentResult missing parent LoopID; CancelQueuedInput missing SessionID, LoopID,
// and TargetCommandID; a gate reply with zero GateRoute.LoopID and zero
// ToolExecutionID.
func TestValidateCommandInvalid(t *testing.T) {
	t.Parallel()

	cmdID := newID(t)
	sess := newID(t)
	loop := newID(t)
	toolID := newID(t)
	target := newID(t)
	hdr := command.Header{CommandID: cmdID}

	tests := []struct {
		name      string
		cmd       command.Command
		wantCmd   command.CommandName
		wantField command.CommandField
		wantRule  command.Rule
	}{
		{
			name:      "zero CommandID is invalid",
			cmd:       command.UserInput{},
			wantCmd:   command.CommandUserInput,
			wantField: command.FieldCommandID,
			wantRule:  command.RuleRequired,
		},
		{
			name:      "SubagentResult missing parent LoopID",
			cmd:       command.SubagentResult{Header: hdr},
			wantCmd:   command.CommandSubagentResult,
			wantField: command.FieldLoopID,
			wantRule:  command.RuleRequired,
		},
		{
			name:      "CancelQueuedInput missing SessionID",
			cmd:       command.CancelQueuedInput{Header: hdr, Coordinates: identity.Coordinates{LoopID: loop}, TargetCommandID: target},
			wantCmd:   command.CommandCancelQueuedInput,
			wantField: command.FieldSessionID,
			wantRule:  command.RuleRequired,
		},
		{
			name:      "CancelQueuedInput missing LoopID",
			cmd:       command.CancelQueuedInput{Header: hdr, Coordinates: identity.Coordinates{SessionID: sess}, TargetCommandID: target},
			wantCmd:   command.CommandCancelQueuedInput,
			wantField: command.FieldLoopID,
			wantRule:  command.RuleRequired,
		},
		{
			name:      "CancelQueuedInput missing TargetCommandID",
			cmd:       command.CancelQueuedInput{Header: hdr, Coordinates: identity.Coordinates{SessionID: sess, LoopID: loop}},
			wantCmd:   command.CommandCancelQueuedInput,
			wantField: command.FieldTargetCommandID,
			wantRule:  command.RuleRequired,
		},
		{
			name:      "ApproveToolCall gate route with zero LoopID",
			cmd:       command.ApproveToolCall{Header: hdr, GateRoute: command.GateRoute{ToolExecutionID: toolID}},
			wantCmd:   command.CommandApproveToolCall,
			wantField: command.FieldLoopID,
			wantRule:  command.RuleRequired,
		},
		{
			name:      "DenyToolCall gate route with zero ToolExecutionID",
			cmd:       command.DenyToolCall{Header: hdr, GateRoute: command.GateRoute{Coordinates: identity.Coordinates{LoopID: loop}}},
			wantCmd:   command.CommandDenyToolCall,
			wantField: command.FieldToolExecutionID,
			wantRule:  command.RuleRequired,
		},
		{
			name:      "ProvideUserInput gate route with zero LoopID and zero ToolExecutionID",
			cmd:       command.ProvideUserInput{Header: hdr, GateRoute: command.GateRoute{}, Answer: "x"},
			wantCmd:   command.CommandProvideUserInput,
			wantField: command.FieldLoopID,
			wantRule:  command.RuleRequired,
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := command.ValidateCommand(tt.cmd)
			var ve *command.CommandValidationError
			if !errors.As(err, &ve) {
				t.Fatalf("ValidateCommand error = %v (%T), want *command.CommandValidationError", err, err)
			}
			if ve.Command != tt.wantCmd {
				t.Errorf("Command = %q, want %q", ve.Command, tt.wantCmd)
			}
			if ve.Field != tt.wantField {
				t.Errorf("Field = %q, want %q", ve.Field, tt.wantField)
			}
			if ve.Rule != tt.wantRule {
				t.Errorf("Rule = %q, want %q", ve.Rule, tt.wantRule)
			}
		})
	}
}
