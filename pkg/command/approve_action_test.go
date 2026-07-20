package command

import (
	"errors"
	"testing"

	"github.com/looprig/harness/pkg/gate"
	"github.com/looprig/harness/pkg/identity"
)

// fullGateRoute builds a validation-complete GateRoute for approval-wire tests.
func fullGateRoute() GateRoute {
	return GateRoute{Coordinates: identity.Coordinates{LoopID: seededUUID(0x33)}, ToolExecutionID: seededUUID(0x77)}
}

// TestApproveToolCallActionRoundTrip proves the approval command wire carries
// exactly one of the two approve actions and survives a marshal/unmarshal
// cycle intact.
func TestApproveToolCallActionRoundTrip(t *testing.T) {
	t.Parallel()

	for _, action := range []gate.ApprovalAction{gate.ApprovalApprove, gate.ApprovalApproveAlwaysWorkspace} {
		in := ApproveToolCall{Header: fullHeader(), GateRoute: fullGateRoute(), Action: action}
		data, err := MarshalCommand(in)
		if err != nil {
			t.Fatalf("MarshalCommand(%q) error = %v", action, err)
		}
		decoded, err := UnmarshalCommand(data)
		if err != nil {
			t.Fatalf("UnmarshalCommand(%q) error = %v", action, err)
		}
		out, ok := decoded.(ApproveToolCall)
		if !ok {
			t.Fatalf("decoded = %T, want ApproveToolCall", decoded)
		}
		if out.Action != action {
			t.Errorf("round-trip Action = %q, want %q", out.Action, action)
		}
	}
}

// TestApproveToolCallActionValidation proves the approval command fails closed
// on any action other than the two approve actions: a Deny action belongs on
// DenyToolCall, an empty or unknown action is invalid, and a legacy record
// (scope wire, no action) is rejected rather than resurrected.
func TestApproveToolCallActionValidation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		action gate.ApprovalAction
	}{
		{name: "empty action", action: ""},
		{name: "deny travels on DenyToolCall", action: gate.ApprovalDeny},
		{name: "unknown action", action: "Approve always for this session"},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cmd := ApproveToolCall{Header: fullHeader(), GateRoute: fullGateRoute(), Action: tt.action}
			err := ValidateCommand(cmd)
			var validation *CommandValidationError
			if !errors.As(err, &validation) {
				t.Fatalf("ValidateCommand() error = %v, want *CommandValidationError", err)
			}
			if validation.Field != FieldAction {
				t.Errorf("validation field = %q, want %q", validation.Field, FieldAction)
			}
			if _, err := MarshalCommand(cmd); err == nil {
				// The marshal path does not validate (mirrors existing header
				// behavior); the decode path must.
				data, _ := MarshalCommand(ApproveToolCall{Header: fullHeader(), GateRoute: fullGateRoute(), Action: gate.ApprovalApprove})
				_ = data
			}
		})
	}
}

// TestUnmarshalRejectsLegacyApproveScopeWire proves a durable ApproveToolCall
// record written under the retired scope wire (no action field) fails closed
// at the untrusted restore boundary.
func TestUnmarshalRejectsLegacyApproveScopeWire(t *testing.T) {
	t.Parallel()

	valid, err := MarshalCommand(ApproveToolCall{Header: fullHeader(), GateRoute: fullGateRoute(), Action: gate.ApprovalApprove})
	if err != nil {
		t.Fatalf("MarshalCommand() error = %v", err)
	}
	legacy := []byte(`{"type":"ApproveToolCall","v":1,` + string(valid[1:]))
	_ = legacy // shape sanity only; the real legacy record below is explicit

	legacyRecord := []byte(`{"type":"ApproveToolCall","v":1,` +
		`"command_id":"123e4567-e89b-12d3-a456-426614174001",` +
		`"agency":"user",` +
		`"created_at":"2026-07-20T12:00:00Z",` +
		`"session_id":"123e4567-e89b-12d3-a456-426614174002",` +
		`"loop_id":"123e4567-e89b-12d3-a456-426614174003",` +
		`"tool_execution_id":"123e4567-e89b-12d3-a456-426614174004",` +
		`"scope":2,"accepted_grants":["t1"]}`)
	if _, err := UnmarshalCommand(legacyRecord); err == nil {
		t.Fatal("UnmarshalCommand(legacy scope wire) error = nil, want fail-closed rejection")
	}
}

// TestUnmarshalRejectsRemovedSecurityLimitTags proves the hard cut: durable
// records carrying the retired security-limit command tags fail restore with
// the codec's typed unknown-tag error instead of being skipped or migrated.
func TestUnmarshalRejectsRemovedSecurityLimitTags(t *testing.T) {
	t.Parallel()

	for _, tag := range []string{"SetSecurityLimit", "SetSecurityCeiling"} {
		record := []byte(`{"type":"` + tag + `","v":1,` +
			`"command_id":"123e4567-e89b-12d3-a456-426614174001",` +
			`"agency":"user","created_at":"2026-07-20T12:00:00Z","level":2}`)
		_, err := UnmarshalCommand(record)
		var unknown *UnknownCommandTypeError
		if !errors.As(err, &unknown) {
			t.Fatalf("UnmarshalCommand(%s) error = %v, want *UnknownCommandTypeError", tag, err)
		}
		if string(unknown.Type) != tag {
			t.Errorf("UnknownCommandTypeError.Type = %q, want %q", unknown.Type, tag)
		}
	}
}
