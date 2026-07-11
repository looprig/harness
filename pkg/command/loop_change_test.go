package command

import (
	"errors"
	"testing"

	"github.com/looprig/inference"
)

// ackErrKind classifies the reply-channel contract violation a Validate should report.
type ackErrKind int

const (
	ackOK         ackErrKind = iota // no error
	ackMissing                      // nil channel  -> *InvalidCommandError
	ackUnbuffered                   // cap 0 channel -> *UnbufferedAckError
)

func TestSetLoopModeValidate(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		cmd  SetLoopMode
		want ackErrKind
	}{
		{name: "happy path with buffered ack", cmd: SetLoopMode{Mode: "build", Ack: make(chan LoopChangeResult, 1)}, want: ackOK},
		{name: "empty mode name is valid (base mode) with ack", cmd: SetLoopMode{Mode: "", Ack: make(chan LoopChangeResult, 1)}, want: ackOK},
		{name: "nil ack is rejected", cmd: SetLoopMode{Mode: "build"}, want: ackMissing},
		{name: "unbuffered ack is rejected", cmd: SetLoopMode{Mode: "build", Ack: make(chan LoopChangeResult)}, want: ackUnbuffered},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assertAckValidation(t, tt.cmd.Validate(), tt.want, CommandSetLoopMode, SetLoopModeAck)
		})
	}
}

func TestChangeLoopInferenceValidate(t *testing.T) {
	t.Parallel()
	valid := make(chan LoopChangeResult, 1)
	tests := []struct {
		name string
		cmd  ChangeLoopInference
		want ackErrKind
	}{
		{name: "happy path model change with ack", cmd: ChangeLoopInference{Model: inference.Model{Name: "m"}, SetModel: true, Ack: valid}, want: ackOK},
		{name: "happy path effort change with ack", cmd: ChangeLoopInference{Effort: inference.EffortHigh, SetEffort: true, Ack: valid}, want: ackOK},
		{name: "no fields set is still structurally valid with ack", cmd: ChangeLoopInference{Ack: valid}, want: ackOK},
		{name: "nil ack is rejected", cmd: ChangeLoopInference{Model: inference.Model{Name: "m"}, SetModel: true}, want: ackMissing},
		{name: "unbuffered ack is rejected", cmd: ChangeLoopInference{SetEffort: true, Effort: inference.EffortHigh, Ack: make(chan LoopChangeResult)}, want: ackUnbuffered},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assertAckValidation(t, tt.cmd.Validate(), tt.want, CommandChangeLoopInference, ChangeLoopInferenceAck)
		})
	}
}

// assertAckValidation asserts a Validate result matches the expected reply-channel contract
// outcome, checking the concrete typed error (and its Command/Field) for the two failures.
func assertAckValidation(t *testing.T, err error, want ackErrKind, cmd CommandName, field CommandField) {
	t.Helper()
	switch want {
	case ackOK:
		if err != nil {
			t.Fatalf("Validate() = %v, want nil", err)
		}
	case ackMissing:
		var invalid *InvalidCommandError
		if !errors.As(err, &invalid) {
			t.Fatalf("error = %v, want *InvalidCommandError", err)
		}
		if invalid.Command != cmd || invalid.Field != field {
			t.Fatalf("error = %+v, want command=%q field=%q", invalid, cmd, field)
		}
	case ackUnbuffered:
		var unbuffered *UnbufferedAckError
		if !errors.As(err, &unbuffered) {
			t.Fatalf("error = %v, want *UnbufferedAckError", err)
		}
		if unbuffered.Command != cmd || unbuffered.Field != field {
			t.Fatalf("error = %+v, want command=%q field=%q", unbuffered, cmd, field)
		}
	}
}

func TestLoopChangeCommandsAreCommands(t *testing.T) {
	t.Parallel()
	var _ Command = SetLoopMode{}
	var _ Command = ChangeLoopInference{}
	// CommandHeader is promoted; a set CommandID round-trips through it.
	id := SetLoopMode{Header: Header{}}.CommandHeader()
	if !id.CommandID.IsZero() {
		t.Fatalf("zero header CommandID = %v, want zero", id.CommandID)
	}
}
