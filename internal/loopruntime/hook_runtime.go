package loopruntime

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/hook"
)

// operationHookError is the safe runtime projection of a guard failure. It
// deliberately omits the callback error: guard errors are trusted in-process
// values and may contain policy details or arbitrary user data.
type operationHookError struct {
	Operation hook.Operation
	Denied    bool
}

func (e *operationHookError) Error() string {
	if e.Denied {
		return fmt.Sprintf("loop: operation %d denied by hook policy", e.Operation)
	}
	return fmt.Sprintf("loop: operation %d blocked by hook failure", e.Operation)
}

type operationHookPanicError struct {
	Operation hook.Operation
}

func (e *operationHookPanicError) Error() string {
	return fmt.Sprintf("loop: operation %d panicked", e.Operation)
}

func safeHookError(operation hook.Operation, err error) error {
	_, denied := hook.AsDenial(err)
	return &operationHookError{Operation: operation, Denied: denied}
}

// hookOutcome maps a terminal error to the bounded hook result domain. A nil
// error is always a normalized success, even if an owner canceled its private
// context after producing that success. Cancellation is classified from err,
// because actor ownership cleanup may cancel ctx before a durable finish runs.
func hookOutcome(_ context.Context, err error) hook.Outcome {
	if err == nil {
		return hook.OutcomeCompleted
	}
	if _, denied := hook.AsDenial(err); denied {
		return hook.OutcomeDenied
	}
	var guardErr *hook.GuardError
	if errors.As(err, &guardErr) {
		return hook.OutcomeFailed
	}
	if errors.Is(err, context.Canceled) ||
		errors.Is(err, context.DeadlineExceeded) {
		return hook.OutcomeCanceled
	}
	return hook.OutcomeFailed
}

func finishHook(
	finish hook.FinishFunc,
	call hook.Call,
	outcome hook.Outcome,
	err error,
) {
	if finish == nil {
		return
	}
	finish(hook.Result{
		Call:    call,
		EndedAt: time.Now(),
		Outcome: outcome,
		Err:     err,
	})
}

func hookNow(now func() time.Time) time.Time {
	if now == nil {
		return time.Now()
	}
	return now()
}

func terminalHookError(terminal event.Event) error {
	if terminal == nil {
		return nil
	}
	switch value := terminal.(type) {
	case event.TurnDone:
		return nil
	case event.TurnFailed:
		return value.Err
	case event.TurnInterrupted:
		return context.Canceled
	default:
		return errors.New("loop: unknown turn terminal")
	}
}
