package command_test

import (
	"errors"
	"testing"

	"github.com/looprig/harness/pkg/command"
	"github.com/looprig/harness/pkg/identity"
)

func TestCompactValidation(t *testing.T) {
	t.Parallel()
	commandID := newID(t)
	sessionID := newID(t)
	loopID := newID(t)
	tests := []struct {
		name      string
		value     command.Compact
		wantField command.CommandField
		wantRule  command.Rule
	}{
		{
			name: "manual compact is valid",
			value: command.Compact{Header: command.Header{CommandID: commandID, Agency: identity.AgencyUser},
				Coordinates: identity.Coordinates{SessionID: sessionID, LoopID: loopID}},
		},
		{
			name: "automatic compact is valid",
			value: command.Compact{Header: command.Header{CommandID: commandID, Agency: identity.AgencyMachine},
				Coordinates: identity.Coordinates{SessionID: sessionID, LoopID: loopID}},
		},
		{name: "command id required", value: command.Compact{Header: command.Header{Agency: identity.AgencyUser}, Coordinates: identity.Coordinates{SessionID: sessionID, LoopID: loopID}}, wantField: command.FieldCommandID, wantRule: command.RuleRequired},
		{name: "session id required", value: command.Compact{Header: command.Header{CommandID: commandID, Agency: identity.AgencyUser}, Coordinates: identity.Coordinates{LoopID: loopID}}, wantField: command.FieldSessionID, wantRule: command.RuleRequired},
		{name: "loop id required", value: command.Compact{Header: command.Header{CommandID: commandID, Agency: identity.AgencyUser}, Coordinates: identity.Coordinates{SessionID: sessionID}}, wantField: command.FieldLoopID, wantRule: command.RuleRequired},
		{name: "unknown agency rejected", value: command.Compact{Header: command.Header{CommandID: commandID, Agency: identity.Agency(2)}, Coordinates: identity.Coordinates{SessionID: sessionID, LoopID: loopID}}, wantField: command.FieldAgency, wantRule: command.RuleInvalid},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := command.ValidateCommand(tt.value)
			if tt.wantField == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			var validation *command.CommandValidationError
			if !errors.As(err, &validation) {
				t.Fatalf("error = %T %v, want *CommandValidationError", err, err)
			}
			if validation.Command != command.CommandCompact || validation.Field != tt.wantField || validation.Rule != tt.wantRule {
				t.Errorf("validation = %+v, want command=%q field=%q rule=%q", validation, command.CommandCompact, tt.wantField, tt.wantRule)
			}
		})
	}
}
