package hook

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/looprig/core/content"
	"github.com/looprig/inference"
)

type runnerContextKey string

func validRunnerCall() Call {
	return Call{
		Operation: OperationToolCall,
		ToolCall: &ToolCallData{
			ToolUseID: "tool-use-1",
			ToolName:  "read",
			ArgsJSON:  []byte(`{"path":"README.md"}`),
		},
	}
}

func validRunnerResult() Result {
	return Result{Call: validRunnerCall(), Outcome: OutcomeCompleted}
}

func TestCompileValidatesAndClonesSet(t *testing.T) {
	t.Parallel()

	if _, err := Compile(Set{
		PolicyRevision: "policy-v1",
		Guards:         []Guard{{Operation: OperationStep, Check: func(context.Context, Call) error { return nil }}},
	}); err == nil {
		t.Fatal("Compile(invalid set) = nil error")
	} else {
		var configErr *ConfigError
		if !errors.As(err, &configErr) {
			t.Fatalf("Compile(invalid set) error = %T, want *ConfigError", err)
		}
	}

	var called atomic.Int32
	set := Set{Around: []Around{{
		Operation: OperationToolCall,
		Begin: func(ctx context.Context, _ Call) (context.Context, FinishFunc) {
			called.Add(1)
			return ctx, nil
		},
	}}}
	runner, err := Compile(set)
	if err != nil {
		t.Fatal(err)
	}
	set.Around[0] = Around{Operation: OperationToolCall, Begin: func(ctx context.Context, _ Call) (context.Context, FinishFunc) {
		called.Add(100)
		return ctx, nil
	}}

	if _, _, err := runner.Start(context.Background(), validRunnerCall()); err != nil {
		t.Fatal(err)
	}
	if got := called.Load(); got != 1 {
		t.Fatalf("compiled callback count = %d, want 1", got)
	}
}

func TestRunnerHandlesReportsCompiledRegistrations(t *testing.T) {
	t.Parallel()

	around, err := Compile(Set{Around: []Around{{
		Operation: OperationJournalAppend,
		Begin: func(ctx context.Context, _ Call) (context.Context, FinishFunc) {
			return ctx, nil
		},
	}}})
	if err != nil {
		t.Fatalf("Compile around: %v", err)
	}
	guard, err := Compile(Set{
		PolicyRevision: "turn-v1",
		Guards: []Guard{{
			Operation: OperationTurn,
			Check:     func(context.Context, Call) error { return nil },
		}},
	})
	if err != nil {
		t.Fatalf("Compile guard: %v", err)
	}
	var nilRunner *Runner
	if !around.Handles(OperationJournalAppend) || around.Handles(OperationTurn) {
		t.Fatal("around runner registration query mismatch")
	}
	if !guard.Handles(OperationTurn) || guard.Handles(OperationJournalAppend) {
		t.Fatal("guard runner registration query mismatch")
	}
	if nilRunner.Handles(OperationJournalAppend) {
		t.Fatal("nil runner reported a registration")
	}
}

func TestRunnerOrdersBeginsGuardsAndFinishesWithContextChaining(t *testing.T) {
	t.Parallel()

	var mu sync.Mutex
	var got []string
	appendEvent := func(event string) {
		mu.Lock()
		defer mu.Unlock()
		got = append(got, event)
	}
	set := Set{
		PolicyRevision: "policy-v1",
		Around: []Around{
			{
				Operation: OperationToolCall,
				Begin: func(ctx context.Context, _ Call) (context.Context, FinishFunc) {
					if ctx.Value(runnerContextKey("a")) != nil {
						t.Error("first begin received unexpected derived value")
					}
					appendEvent("a.begin")
					return context.WithValue(ctx, runnerContextKey("a"), "a"), func(Result) {
						appendEvent("a.finish")
					}
				},
			},
			{
				Operation: OperationToolCall,
				Begin: func(ctx context.Context, _ Call) (context.Context, FinishFunc) {
					if got := ctx.Value(runnerContextKey("a")); got != "a" {
						t.Errorf("second begin context value = %v, want a", got)
					}
					appendEvent("b.begin")
					return context.WithValue(ctx, runnerContextKey("b"), "b"), func(Result) {
						appendEvent("b.finish")
					}
				},
			},
			{
				Operation: OperationInference,
				Begin: func(ctx context.Context, _ Call) (context.Context, FinishFunc) {
					appendEvent("unrelated.begin")
					return ctx, nil
				},
			},
		},
		Guards: []Guard{
			{
				Operation: OperationToolCall,
				Check: func(ctx context.Context, _ Call) error {
					if got := ctx.Value(runnerContextKey("b")); got != "b" {
						t.Errorf("first guard context value = %v, want b", got)
					}
					appendEvent("g1")
					return nil
				},
			},
			{
				Operation: OperationToolCall,
				Check: func(ctx context.Context, _ Call) error {
					appendEvent("g2")
					return nil
				},
			},
		},
	}
	runner, err := Compile(set)
	if err != nil {
		t.Fatal(err)
	}

	ctx, finish, err := runner.Start(context.Background(), validRunnerCall())
	if err != nil {
		t.Fatal(err)
	}
	if got := ctx.Value(runnerContextKey("b")); got != "b" {
		t.Fatalf("returned context value = %v, want b", got)
	}
	appendEvent("operation")
	finish(validRunnerResult())

	want := []string{"a.begin", "b.begin", "g1", "g2", "operation", "b.finish", "a.finish"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("events = %v, want %v", got, want)
	}
}

func TestRunnerGuardFailureStopsLaterGuardsAndReturnsFinish(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		guardError func() error
		wantDenial bool
	}{
		{
			name:       "direct denial",
			guardError: func() error { return Deny("policy.blocked", "not permitted") },
			wantDenial: true,
		},
		{
			name: "wrapped denial",
			guardError: func() error {
				return fmt.Errorf("policy adapter: %w", Deny("policy.blocked", "not permitted"))
			},
			wantDenial: true,
		},
		{
			name:       "internal error",
			guardError: func() error { return errors.New("backend unavailable") },
		},
		{
			name: "invalid direct denial",
			guardError: func() error {
				return &Denial{Code: "INVALID", Reason: "not permitted"}
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			var laterCalled atomic.Bool
			var finishCalled atomic.Bool
			runner, err := Compile(Set{
				PolicyRevision: "policy-v1",
				Around: []Around{{
					Operation: OperationToolCall,
					Begin: func(ctx context.Context, _ Call) (context.Context, FinishFunc) {
						return ctx, func(Result) { finishCalled.Store(true) }
					},
				}},
				Guards: []Guard{
					{Operation: OperationToolCall, Check: func(context.Context, Call) error {
						return test.guardError()
					}},
					{Operation: OperationToolCall, Check: func(context.Context, Call) error {
						laterCalled.Store(true)
						return nil
					}},
				},
			})
			if err != nil {
				t.Fatal(err)
			}

			_, finish, startErr := runner.Start(context.Background(), validRunnerCall())
			if startErr == nil {
				t.Fatal("Start() = nil error, want guard block")
			}
			if laterCalled.Load() {
				t.Fatal("later guard ran after first error")
			}
			if test.wantDenial {
				if _, ok := AsDenial(startErr); !ok {
					t.Fatalf("Start() error = %T, want classified denial", startErr)
				}
				var guardErr *GuardError
				if errors.As(startErr, &guardErr) {
					t.Fatalf("denial wrapped as internal %T", guardErr)
				}
			} else {
				var guardErr *GuardError
				if !errors.As(startErr, &guardErr) {
					t.Fatalf("Start() error = %T, want *GuardError", startErr)
				}
			}
			if finish == nil {
				t.Fatal("Start() returned nil finish on guard block")
			}
			finish(Result{Call: validRunnerCall(), Outcome: OutcomeDenied, Err: startErr})
			if !finishCalled.Load() {
				t.Fatal("finish did not run after guard block")
			}
		})
	}
}

func TestRunnerGuardPanicBecomesGuardError(t *testing.T) {
	t.Parallel()

	runner, err := Compile(Set{
		PolicyRevision: "policy-v1",
		Guards: []Guard{{
			Operation: OperationToolCall,
			Check: func(context.Context, Call) error {
				panic("sensitive panic detail")
			},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}

	_, finish, startErr := runner.Start(context.Background(), validRunnerCall())
	var guardErr *GuardError
	if !errors.As(startErr, &guardErr) {
		t.Fatalf("Start() error = %T, want *GuardError", startErr)
	}
	if guardErr.Operation != OperationToolCall || guardErr.Index != 0 {
		t.Fatalf("GuardError = %#v", guardErr)
	}
	if finish == nil {
		t.Fatal("Start() returned nil finish")
	}
}

func TestRunnerNilContextRetainsPreviousContext(t *testing.T) {
	t.Parallel()

	key := runnerContextKey("preserved")
	base := context.WithValue(context.Background(), key, "yes")
	runner, err := Compile(Set{Around: []Around{
		{
			Operation: OperationToolCall,
			Begin: func(context.Context, Call) (context.Context, FinishFunc) {
				return nil, nil
			},
		},
		{
			Operation: OperationToolCall,
			Begin: func(ctx context.Context, _ Call) (context.Context, FinishFunc) {
				if got := ctx.Value(key); got != "yes" {
					t.Errorf("context value = %v, want yes", got)
				}
				return ctx, nil
			},
		},
	}})
	if err != nil {
		t.Fatal(err)
	}

	got, _, err := runner.Start(base, validRunnerCall())
	if err != nil {
		t.Fatal(err)
	}
	if got != base {
		t.Fatal("nil begin context replaced previous context")
	}
}

func TestRunnerRecoversObservationPanicsAndLogsOnlyMetadata(t *testing.T) {
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })

	var finishes atomic.Int32
	runner, err := Compile(Set{Around: []Around{
		{
			Operation: OperationToolCall,
			Begin: func(context.Context, Call) (context.Context, FinishFunc) {
				panic("begin-secret")
			},
		},
		{
			Operation: OperationToolCall,
			Begin: func(ctx context.Context, _ Call) (context.Context, FinishFunc) {
				return ctx, func(Result) { finishes.Add(1) }
			},
		},
		{
			Operation: OperationToolCall,
			Begin: func(ctx context.Context, _ Call) (context.Context, FinishFunc) {
				return ctx, func(Result) {
					finishes.Add(1)
					panic("finish-secret")
				}
			},
		},
		{
			Operation: OperationToolCall,
			Begin: func(ctx context.Context, _ Call) (context.Context, FinishFunc) {
				return ctx, func(Result) { finishes.Add(1) }
			},
		},
	}})
	if err != nil {
		t.Fatal(err)
	}

	_, finish, err := runner.Start(context.Background(), validRunnerCall())
	if err != nil {
		t.Fatal(err)
	}
	finish(validRunnerResult())

	if got := finishes.Load(); got != 3 {
		t.Fatalf("finish calls = %d, want 3", got)
	}
	output := logs.String()
	for _, want := range []string{"operation=", "callback_index=0", "callback_index=2"} {
		if !strings.Contains(output, want) {
			t.Errorf("logs missing %q: %s", want, output)
		}
	}
	for _, secret := range []string{"begin-secret", "finish-secret", "README.md"} {
		if strings.Contains(output, secret) {
			t.Errorf("logs exposed %q: %s", secret, output)
		}
	}
}

func TestRunnerClonesEachCallbackSnapshot(t *testing.T) {
	t.Parallel()

	var secondBeginArgs string
	var guardArgs string
	var firstFinishArgs string
	runner, err := Compile(Set{
		PolicyRevision: "policy-v1",
		Around: []Around{
			{
				Operation: OperationToolCall,
				Begin: func(ctx context.Context, call Call) (context.Context, FinishFunc) {
					call.ToolCall.ArgsJSON[0] = '['
					return ctx, func(result Result) {
						firstFinishArgs = string(result.ToolCall.ArgsJSON)
					}
				},
			},
			{
				Operation: OperationToolCall,
				Begin: func(ctx context.Context, call Call) (context.Context, FinishFunc) {
					secondBeginArgs = string(call.ToolCall.ArgsJSON)
					return ctx, func(result Result) {
						result.ToolCall.ArgsJSON[0] = '['
					}
				},
			},
		},
		Guards: []Guard{{
			Operation: OperationToolCall,
			Check: func(_ context.Context, call Call) error {
				guardArgs = string(call.ToolCall.ArgsJSON)
				call.ToolCall.ArgsJSON[0] = '['
				return nil
			},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	call := validRunnerCall()
	_, finish, err := runner.Start(context.Background(), call)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(call.ToolCall.ArgsJSON); got != `{"path":"README.md"}` {
		t.Fatalf("Start mutated caller call: %s", got)
	}
	if secondBeginArgs != `{"path":"README.md"}` || guardArgs != `{"path":"README.md"}` {
		t.Fatalf("callbacks shared call: second=%s guard=%s", secondBeginArgs, guardArgs)
	}

	result := validRunnerResult()
	finish(result)
	if firstFinishArgs != `{"path":"README.md"}` {
		t.Fatalf("finish callbacks shared result: %s", firstFinishArgs)
	}
	if got := string(result.ToolCall.ArgsJSON); got != `{"path":"README.md"}` {
		t.Fatalf("finish mutated caller result: %s", got)
	}
}

func TestRunnerFinishRunsOnce(t *testing.T) {
	t.Parallel()

	var count atomic.Int32
	runner, err := Compile(Set{Around: []Around{{
		Operation: OperationToolCall,
		Begin: func(ctx context.Context, _ Call) (context.Context, FinishFunc) {
			return ctx, func(Result) { count.Add(1) }
		},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	_, finish, err := runner.Start(context.Background(), validRunnerCall())
	if err != nil {
		t.Fatal(err)
	}

	var callers sync.WaitGroup
	for range 20 {
		callers.Add(1)
		go func() {
			defer callers.Done()
			finish(validRunnerResult())
		}()
	}
	callers.Wait()
	if got := count.Load(); got != 1 {
		t.Fatalf("finish calls = %d, want 1", got)
	}
}

func TestNilRunnerIsNoOp(t *testing.T) {
	t.Parallel()

	var runner *Runner
	ctx := context.Background()
	got, finish, err := runner.Start(ctx, Call{})
	if err != nil {
		t.Fatalf("nil Runner.Start() error = %v", err)
	}
	if got != ctx {
		t.Fatal("nil Runner.Start() changed context")
	}
	if finish == nil {
		t.Fatal("nil Runner.Start() returned nil finish")
	}
	finish(Result{})
}

func TestRunnerRejectsMalformedCallBeforeCallbacks(t *testing.T) {
	t.Parallel()

	var called atomic.Bool
	runner, err := Compile(Set{Around: []Around{{
		Operation: OperationToolCall,
		Begin: func(ctx context.Context, _ Call) (context.Context, FinishFunc) {
			called.Store(true)
			return ctx, nil
		},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	_, finish, startErr := runner.Start(context.Background(), Call{Operation: OperationToolCall})
	var callErr *CallError
	if !errors.As(startErr, &callErr) {
		t.Fatalf("Start() error = %T, want *CallError", startErr)
	}
	if called.Load() {
		t.Fatal("callback invoked for malformed call")
	}
	if finish != nil {
		t.Fatal("malformed call returned finish")
	}
}

func TestRunnerUnmatchedOperationReturnsBeforeDeepClone(t *testing.T) {
	t.Parallel()

	callWithUnsupportedNestedContent := Call{
		Operation: OperationInference,
		Inference: &InferenceData{Request: &inference.Request{
			Messages: content.AgenticMessages{&content.UserMessage{
				Message: content.Message{
					Role:   content.RoleUser,
					Blocks: []content.Block{nil},
				},
			}},
		}},
	}

	var unmatchedCalled atomic.Bool
	runner, err := Compile(Set{Around: []Around{{
		Operation: OperationTurn,
		Begin: func(ctx context.Context, _ Call) (context.Context, FinishFunc) {
			unmatchedCalled.Store(true)
			return ctx, nil
		},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, finish, startErr := runner.Start(ctx, Call{Operation: OperationInference}); startErr == nil {
		t.Fatal("unmatched malformed Start() = nil error")
	} else {
		var callErr *CallError
		if !errors.As(startErr, &callErr) {
			t.Fatalf("unmatched malformed Start() error = %T, want *CallError", startErr)
		}
		if finish != nil {
			t.Fatal("unmatched malformed Start() returned finish")
		}
	}
	got, finish, startErr := runner.Start(ctx, callWithUnsupportedNestedContent)
	if startErr != nil {
		t.Fatalf("unmatched Start() error = %v", startErr)
	}
	if got != ctx {
		t.Fatal("unmatched Start() changed context")
	}
	if finish == nil {
		t.Fatal("unmatched Start() returned nil finish")
	}
	finish(Result{})
	if unmatchedCalled.Load() {
		t.Fatal("unmatched callback ran")
	}

	var called atomic.Bool
	matching, err := Compile(Set{Around: []Around{{
		Operation: OperationInference,
		Begin: func(ctx context.Context, _ Call) (context.Context, FinishFunc) {
			called.Store(true)
			return ctx, nil
		},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	_, matchingFinish, startErr := matching.Start(context.Background(), Call{
		Operation: OperationInference,
		Inference: &InferenceData{},
	})
	if startErr != nil {
		t.Fatalf("matching Start() error = %v", startErr)
	}
	if !called.Load() {
		t.Fatal("matching callback did not run")
	}
	if matchingFinish == nil {
		t.Fatal("matching Start() returned nil finish")
	}
	called.Store(false)

	defer func() {
		recovered := recover()
		var cloneErr *CloneError
		if !errors.As(asError(recovered), &cloneErr) {
			t.Fatalf("matching Start() panic = %T(%v), want *CloneError", recovered, recovered)
		}
		if called.Load() {
			t.Fatal("matching callback ran before its snapshot was cloned")
		}
	}()
	_, _, _ = matching.Start(context.Background(), callWithUnsupportedNestedContent)
	t.Fatal("matching Start() did not deep-clone the call")
}

type panickingAsError struct {
	detail string
}

func (e *panickingAsError) Error() string {
	return e.detail
}

func (e *panickingAsError) As(any) bool {
	panic(e.detail)
}

type panickingUnwrapError struct {
	detail string
}

func (e *panickingUnwrapError) Error() string {
	return e.detail
}

func (e *panickingUnwrapError) Unwrap() error {
	panic(e.detail)
}

func TestRunnerDenialClassificationPanicBecomesRedactedGuardError(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  error
	}{
		{name: "As", err: &panickingAsError{detail: "sensitive-as-detail"}},
		{name: "Unwrap", err: &panickingUnwrapError{detail: "sensitive-unwrap-detail"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			runner, err := Compile(Set{
				PolicyRevision: "policy-v1",
				Guards: []Guard{{
					Operation: OperationToolCall,
					Check: func(context.Context, Call) error {
						return test.err
					},
				}},
			})
			if err != nil {
				t.Fatal(err)
			}

			_, finish, startErr := runner.Start(context.Background(), validRunnerCall())
			var guardErr *GuardError
			if !errors.As(startErr, &guardErr) {
				t.Fatalf("Start() error = %T, want *GuardError", startErr)
			}
			if guardErr.Operation != OperationToolCall || guardErr.Index != 0 {
				t.Fatalf("GuardError = %#v", guardErr)
			}
			if !errors.Is(startErr, errDenialClassificationPanicked) {
				t.Fatalf("GuardError does not unwrap to classification sentinel: %v", startErr)
			}
			if strings.Contains(startErr.Error(), test.err.Error()) {
				t.Fatalf("GuardError.Error exposed sensitive callback detail: %q", startErr)
			}
			if _, ok := AsDenial(startErr); ok {
				t.Fatal("classification panic was exposed as an intentional denial")
			}
			if finish == nil {
				t.Fatal("Start() returned nil finish after classification panic")
			}
		})
	}
}

func TestGuardErrorRedactsAndUnwrapsTrustedCause(t *testing.T) {
	t.Parallel()

	cause := errors.New("sensitive trusted cause")
	err := &GuardError{
		Operation: OperationCompaction,
		Index:     7,
		Cause:     cause,
	}
	if strings.Contains(err.Error(), cause.Error()) {
		t.Fatalf("GuardError.Error() exposed cause: %q", err)
	}
	if !errors.Is(err, cause) {
		t.Fatal("GuardError does not unwrap to trusted cause")
	}
}

func asError(value any) error {
	if value == nil {
		return nil
	}
	err, ok := value.(error)
	if ok {
		return err
	}
	return fmt.Errorf("%v", value)
}

func TestRunnerConcurrentDispatch(t *testing.T) {
	t.Parallel()

	var begins atomic.Int64
	var guards atomic.Int64
	var finishes atomic.Int64
	runner, err := Compile(Set{
		PolicyRevision: "policy-v1",
		Around: []Around{{
			Operation: OperationToolCall,
			Begin: func(ctx context.Context, call Call) (context.Context, FinishFunc) {
				begins.Add(1)
				call.ToolCall.ArgsJSON[0] = '['
				return ctx, func(result Result) {
					finishes.Add(1)
					result.ToolCall.ArgsJSON[0] = '['
				}
			},
		}},
		Guards: []Guard{{
			Operation: OperationToolCall,
			Check: func(_ context.Context, call Call) error {
				guards.Add(1)
				call.ToolCall.ArgsJSON[0] = '['
				return nil
			},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}

	const dispatches = 100
	var workers sync.WaitGroup
	for range dispatches {
		workers.Add(1)
		go func() {
			defer workers.Done()
			_, finish, startErr := runner.Start(context.Background(), validRunnerCall())
			if startErr != nil {
				t.Errorf("Start() error = %v", startErr)
				return
			}
			finish(validRunnerResult())
		}()
	}
	workers.Wait()
	if begins.Load() != dispatches || guards.Load() != dispatches || finishes.Load() != dispatches {
		t.Fatalf("counts = begin %d, guard %d, finish %d", begins.Load(), guards.Load(), finishes.Load())
	}
}
