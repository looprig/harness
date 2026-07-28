package loopruntime

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/looprig/harness/pkg/hook"
)

type hostileAsError struct{}

func (hostileAsError) Error() string { return "private hostile As detail" }
func (hostileAsError) As(any) bool   { panic("hostile As") }

type hostileIsError struct{}

func (hostileIsError) Error() string { return "private hostile Is detail" }
func (hostileIsError) Is(error) bool { panic("hostile Is") }

type hostileUnwrapError struct{}

func (hostileUnwrapError) Error() string { return "private hostile Unwrap detail" }
func (hostileUnwrapError) Unwrap() error { panic("hostile Unwrap") }

func TestHookRuntimeClassificationNeverPanicsOnHostileErrors(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		err  error
	}{
		{name: "As", err: hostileAsError{}},
		{name: "Is", err: hostileIsError{}},
		{name: "Unwrap", err: hostileUnwrapError{}},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			defer func() {
				if recovered := recover(); recovered != nil {
					t.Fatalf("runtime classifier panicked: %v", recovered)
				}
			}()
			if got := hookOutcome(context.Background(), test.err); got != hook.OutcomeFailed {
				t.Fatalf("hookOutcome = %v, want failed", got)
			}
			safe := safeHookError(hook.OperationInference, test.err)
			if safe == nil {
				t.Fatal("safeHookError = nil")
			}
			if strings.Contains(safe.Error(), "private") || strings.Contains(safe.Error(), "hostile") {
				t.Fatalf("safeHookError leaked callback detail: %q", safe.Error())
			}
		})
	}
}

func TestFinishHookClampsEndedAtToStartedAt(t *testing.T) {
	t.Parallel()
	started := time.Now().Add(24 * time.Hour)
	var result hook.Result
	finishHook(func(value hook.Result) { result = value }, hook.Call{
		Operation: hook.OperationStep,
		StartedAt: started,
		Step:      &hook.StepData{},
	}, hook.OutcomeCompleted, nil)
	if result.EndedAt.Before(started) {
		t.Fatalf("EndedAt = %v, before StartedAt %v", result.EndedAt, started)
	}
}
