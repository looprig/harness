package command_test

import (
	"errors"
	"testing"

	"github.com/looprig/harness/pkg/command"
)

func TestCancelDelegateRequestValidateRequiresTransientAck(t *testing.T) {
	t.Parallel()
	ack := make(chan command.DelegateCancelResult, 1)
	if err := (command.CancelDelegateRequest{Ack: ack}).Validate(); err != nil {
		t.Fatalf("valid command: %v", err)
	}
	err := (command.CancelDelegateRequest{}).Validate()
	var invalid *command.InvalidCommandError
	if !errors.As(err, &invalid) || invalid.Field != command.CancelDelegateRequestAck {
		t.Fatalf("nil ack error = %T %v", err, err)
	}
}
