package hook

import (
	"context"
	"errors"
	"log/slog"
	"sync"
)

var (
	errGuardPanicked                = errors.New("hook: guard callback panicked")
	errDenialClassificationPanicked = errors.New("hook: denial classification panicked")
)

// Runner is an immutable, compiled hook set safe for concurrent dispatch.
type Runner struct {
	guards []Guard
	around []Around
}

type registeredFinish struct {
	index  int
	finish FinishFunc
}

// Compile validates a hook set and takes independent ownership of its
// registration slices.
func Compile(set Set) (*Runner, error) {
	if err := ValidateSet(set); err != nil {
		return nil, err
	}
	return &Runner{
		guards: append([]Guard(nil), set.Guards...),
		around: append([]Around(nil), set.Around...),
	}, nil
}

// Start begins observation and evaluates policy for one valid operation call.
// Matching begin callbacks run in registration order with chained contexts,
// followed by matching guards in registration order. Every callback receives
// an independent snapshot.
//
// Observer panics are logged without callback-owned details and fail open. A
// guard or denial-classification panic fails closed as *GuardError. A validated
// intentional denial is returned as *Denial; every other guard failure is
// returned as *GuardError.
//
// The returned FinishFunc runs completed observers in reverse registration
// order exactly once, including when a guard blocks. The caller must supply a
// valid Result for the same operation with a valid terminal Outcome.
func (r *Runner) Start(
	ctx context.Context,
	call Call,
) (context.Context, FinishFunc, error) {
	if r == nil {
		return ctx, func(Result) {}, nil
	}
	if err := ValidateCall(call); err != nil {
		return ctx, nil, err
	}
	if !r.matches(call.Operation) {
		return ctx, func(Result) {}, nil
	}
	snapshot := CloneCall(call)

	finishes := make([]registeredFinish, 0, len(r.around))
	for index, around := range r.around {
		if around.Operation != snapshot.Operation {
			continue
		}
		next, finish, panicked := beginAround(ctx, snapshot, around.Begin)
		if panicked {
			logObservationFailure("hook: begin callback panicked", snapshot.Operation, index)
			continue
		}
		if next == nil {
			logObservationFailure("hook: begin callback returned nil context", snapshot.Operation, index)
		} else {
			ctx = next
		}
		if finish != nil {
			finishes = append(finishes, registeredFinish{index: index, finish: finish})
		}
	}

	finish := aggregateFinish(snapshot.Operation, finishes)
	for index, guard := range r.guards {
		if guard.Operation != snapshot.Operation {
			continue
		}
		err, panicked := checkGuard(ctx, snapshot, guard.Check)
		if panicked {
			return ctx, finish, &GuardError{
				Operation: snapshot.Operation,
				Index:     index,
				Cause:     errGuardPanicked,
			}
		}
		if err == nil {
			continue
		}
		denial, denied, classificationPanicked := classifyDenial(err)
		if classificationPanicked {
			return ctx, finish, &GuardError{
				Operation: snapshot.Operation,
				Index:     index,
				Cause:     errDenialClassificationPanicked,
			}
		}
		if denied {
			return ctx, finish, denial
		}
		return ctx, finish, &GuardError{
			Operation: snapshot.Operation,
			Index:     index,
			Cause:     err,
		}
	}
	return ctx, finish, nil
}

func (r *Runner) matches(operation Operation) bool {
	for _, around := range r.around {
		if around.Operation == operation {
			return true
		}
	}
	for _, guard := range r.guards {
		if guard.Operation == operation {
			return true
		}
	}
	return false
}

func classifyDenial(err error) (denial *Denial, denied bool, panicked bool) {
	completed := false
	defer func() {
		if completed {
			return
		}
		_ = recover()
		denial = nil
		denied = false
		panicked = true
	}()
	denial, denied = AsDenial(err)
	completed = true
	return denial, denied, false
}

func beginAround(
	ctx context.Context,
	call Call,
	begin BeginFunc,
) (next context.Context, finish FinishFunc, panicked bool) {
	defer func() {
		if recover() != nil {
			next = ctx
			finish = nil
			panicked = true
		}
	}()
	next, finish = begin(ctx, CloneCall(call))
	return next, finish, false
}

func checkGuard(
	ctx context.Context,
	call Call,
	check GuardFunc,
) (err error, panicked bool) {
	defer func() {
		if recover() != nil {
			err = errGuardPanicked
			panicked = true
		}
	}()
	return check(ctx, CloneCall(call)), false
}

func aggregateFinish(
	operation Operation,
	finishes []registeredFinish,
) FinishFunc {
	var once sync.Once
	return func(result Result) {
		once.Do(func() {
			for index := len(finishes) - 1; index >= 0; index-- {
				registered := finishes[index]
				if finishAround(registered.finish, result) {
					logObservationFailure(
						"hook: finish callback panicked",
						operation,
						registered.index,
					)
				}
			}
		})
	}
}

func finishAround(finish FinishFunc, result Result) (panicked bool) {
	defer func() {
		if recover() != nil {
			panicked = true
		}
	}()
	finish(CloneResult(result))
	return false
}

func logObservationFailure(message string, operation Operation, callbackIndex int) {
	slog.Default().Error(
		message,
		slog.Uint64("operation", uint64(operation)),
		slog.Int("callback_index", callbackIndex),
	)
}
