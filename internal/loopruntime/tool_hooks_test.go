package loopruntime

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/looprig/core/content"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/event"
	gatedomain "github.com/looprig/harness/pkg/gate"
	"github.com/looprig/harness/pkg/hook"
	"github.com/looprig/harness/pkg/identity"
	loopdomain "github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/tool"
)

type hookContextKey string

func compileRuntimeHooks(t *testing.T, set hook.Set) *hook.Runner {
	t.Helper()
	runner, err := hook.Compile(set)
	if err != nil {
		t.Fatalf("hook.Compile: %v", err)
	}
	return runner
}

func hookBatchRuntime(hooks *hook.Runner, emit func(event.Event)) BatchRuntime {
	return BatchRuntime{
		GateRegistrations: make(chan gateRegistration),
		IDGen:             uuid.New,
		Emit:              emit,
		Hooks:             hooks,
		Coordinates:       identity.Coordinates{SessionID: uuid.UUID{0x11}, LoopID: uuid.UUID{0x12}, TurnID: uuid.UUID{0x13}, StepID: uuid.UUID{0x14}},
		AgentName:         "tester",
		Cause:             identity.Cause{CommandID: uuid.UUID{0x15}},
	}
}

func TestToolHooks_GuardPrecedesResolutionAndDenialIsAudited(t *testing.T) {
	t.Parallel()
	var infoCalls, prepareCalls, accessCalls, runCalls atomic.Int32
	base := &fakeRunTool{name: "Secret", output: "should not run"}
	base.prepareFn = func(uuid.UUID, string) (tool.Request, tool.PreparedArtifact, error) {
		prepareCalls.Add(1)
		return tool.Request{}, nil, nil
	}
	base.onRun = func(context.Context) { runCalls.Add(1) }
	probe := &infoProbeTool{InvokableTool: base, calls: &infoCalls}
	access := accessProbe{calls: &accessCalls}
	finished := make(chan hook.Result, 1)
	hooks := compileRuntimeHooks(t, hook.Set{
		PolicyRevision: "deny-secret-v1",
		Guards: []hook.Guard{{
			Operation: hook.OperationToolCall,
			Check: func(_ context.Context, got hook.Call) error {
				if got.ToolCall.ToolName != "Secret" || string(got.ToolCall.ArgsJSON) != `{"token":"raw"}` {
					t.Fatalf("guard call = %+v", got.ToolCall)
				}
				return hook.Deny("policy.denied", "blocked by test policy")
			},
		}},
		Around: []hook.Around{{
			Operation: hook.OperationToolCall,
			Begin:     channelFinish(finished),
		}},
	})
	emit, events := collectEmit()

	results := RunBatch(context.Background(), []content.ToolUseBlock{
		{ID: "use-1", Name: "Secret", Input: []byte(`{"token":"raw"}`)},
	}, ToolSet{Registry: []tool.InvokableTool{probe}, Access: access}, hookBatchRuntime(hooks, emit))

	if infoCalls.Load() != 0 || prepareCalls.Load() != 0 || accessCalls.Load() != 0 || runCalls.Load() != 0 {
		t.Fatalf("side effects info=%d prepare=%d access=%d run=%d, want all zero",
			infoCalls.Load(), prepareCalls.Load(), accessCalls.Load(), runCalls.Load())
	}
	if len(results) != 1 || !results[0].IsError || !strings.Contains(resultText(results[0]), "blocked by test policy") {
		t.Fatalf("results = %+v, want bounded denial", results)
	}
	_, _, started, completed := startedCompletedOrder(events())
	if started != 1 || completed != 1 {
		t.Fatalf("audit counts started=%d completed=%d, want 1/1", started, completed)
	}
	if got := <-finished; got.Outcome != hook.OutcomeDenied ||
		got.ToolCall.ResultPreview != errToolHookDeniedPrefix+"blocked by test policy" {
		t.Fatalf("denied finish = %+v", got)
	}
}

func TestToolHooks_InternalGuardFailureIsRedacted(t *testing.T) {
	t.Parallel()
	const secret = "database-password"
	finished := make(chan hook.Result, 1)
	hooks := compileRuntimeHooks(t, hook.Set{
		PolicyRevision: "broken-v1",
		Guards: []hook.Guard{{
			Operation: hook.OperationToolCall,
			Check:     func(context.Context, hook.Call) error { return errors.New(secret) },
		}},
		Around: []hook.Around{{
			Operation: hook.OperationToolCall,
			Begin: func(ctx context.Context, _ hook.Call) (context.Context, hook.FinishFunc) {
				return ctx, func(result hook.Result) { finished <- result }
			},
		}},
	})
	emit, _ := collectEmit()

	results := RunBatch(context.Background(), []content.ToolUseBlock{
		{ID: "use-1", Name: "Missing", Input: []byte(`{}`)},
	}, ToolSet{}, hookBatchRuntime(hooks, emit))

	got := resultText(results[0])
	if !results[0].IsError || strings.Contains(got, secret) || got != errToolHookFailure {
		t.Fatalf("result = %q, want exact redacted hook failure %q", got, errToolHookFailure)
	}
	hookResult := <-finished
	if hookResult.Err == nil || strings.Contains(hookResult.Err.Error(), secret) {
		t.Fatalf("hook finish error = %v, want redacted non-nil error", hookResult.Err)
	}
}

func TestToolHooks_CallAndExecutionNestAndPropagateContexts(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	var sequence []string
	var callStart hook.Call
	var callFinish, executionFinish hook.Result
	base := &fakeRunTool{name: "T", output: "ok"}
	base.auditFn = func(string) string { return "T safe summary" }
	base.onRun = func(ctx context.Context) {
		if ctx.Value(hookContextKey("call")) != "call-context" {
			t.Error("tool did not receive tool-call derived context")
		}
		if ctx.Value(hookContextKey("execution")) != "execution-context" {
			t.Error("tool did not receive tool-execution derived context")
		}
		mu.Lock()
		sequence = append(sequence, "tool")
		mu.Unlock()
	}
	hooks := compileRuntimeHooks(t, hook.Set{Around: []hook.Around{
		{
			Operation: hook.OperationToolCall,
			Begin: func(ctx context.Context, got hook.Call) (context.Context, hook.FinishFunc) {
				mu.Lock()
				sequence = append(sequence, "call.begin")
				callStart = got
				mu.Unlock()
				return context.WithValue(ctx, hookContextKey("call"), "call-context"), func(result hook.Result) {
					mu.Lock()
					defer mu.Unlock()
					sequence = append(sequence, "call.finish")
					callFinish = result
				}
			},
		},
		{
			Operation: hook.OperationToolExecution,
			Begin: func(ctx context.Context, got hook.Call) (context.Context, hook.FinishFunc) {
				if ctx.Value(hookContextKey("call")) != "call-context" {
					t.Error("execution hook did not receive tool-call context")
				}
				mu.Lock()
				sequence = append(sequence, "execution.begin")
				mu.Unlock()
				return context.WithValue(ctx, hookContextKey("execution"), "execution-context"), func(result hook.Result) {
					mu.Lock()
					defer mu.Unlock()
					sequence = append(sequence, "execution.finish")
					executionFinish = result
				}
			},
		},
	}})
	emit, _ := collectEmit()

	results := RunBatch(context.Background(), []content.ToolUseBlock{
		{ID: "use-1", Name: "T", Input: []byte(`{"x":1}`)},
	}, ToolSet{Registry: []tool.InvokableTool{auditTool{fakeRunTool: base}}, Access: autoApproveGate{}}, hookBatchRuntime(hooks, emit))

	if len(results) != 1 || results[0].IsError {
		t.Fatalf("results = %+v, want success", results)
	}
	mu.Lock()
	defer mu.Unlock()
	if got, want := strings.Join(sequence, ","), "call.begin,execution.begin,tool,execution.finish,call.finish"; got != want {
		t.Fatalf("sequence = %q, want %q", got, want)
	}
	if callFinish.Outcome != hook.OutcomeCompleted || callFinish.ToolCall.ResultPreview != "ok" || callFinish.ToolCall.IsError {
		t.Fatalf("tool-call finish = %+v", callFinish)
	}
	if callFinish.ToolCall.Summary != "T safe summary" {
		t.Fatalf("tool-call summary = %q, want safe audited summary", callFinish.ToolCall.Summary)
	}
	if callFinish.ToolCall.ToolExecutionID != results[0].ToolExecutionID ||
		callFinish.ToolCall.ToolUseID != "use-1" ||
		string(callFinish.ToolCall.ArgsJSON) != `{"x":1}` {
		t.Fatalf("tool-call identity/args = %+v", callFinish.ToolCall)
	}
	if callStart.ToolCall.ResultPreview != "" || callStart.ToolCall.PermissionEffect != "" {
		t.Fatalf("tool-call start snapshot mutated after finish: %+v", callStart.ToolCall)
	}
	if callFinish.ToolCall.PermissionEffect != event.PermissionEffectApprove || callFinish.ToolCall.PermissionReason != "access_evaluated" {
		t.Fatalf("tool-call permission = %+v", callFinish.ToolCall)
	}
	if executionFinish.Outcome != hook.OutcomeCompleted || executionFinish.ToolExecution.ResultPreview != "ok" || executionFinish.ToolExecution.Result == nil {
		t.Fatalf("execution finish = %+v", executionFinish)
	}
}

func TestToolHooks_PanicFinishesExecutionFailedAndCallCompleted(t *testing.T) {
	t.Parallel()
	base := &fakeRunTool{name: "T", panicMsg: "private panic detail"}
	var mu sync.Mutex
	var finishes []hook.Result
	hooks := compileRuntimeHooks(t, hook.Set{Around: []hook.Around{
		{Operation: hook.OperationToolCall, Begin: captureFinish(&mu, &finishes)},
		{Operation: hook.OperationToolExecution, Begin: captureFinish(&mu, &finishes)},
	}})
	emit, events := collectEmit()

	results := RunBatch(context.Background(), []content.ToolUseBlock{
		{ID: "use-1", Name: "T", Input: []byte(`{}`)},
	}, ToolSet{Registry: []tool.InvokableTool{base}, Access: autoApproveGate{}}, hookBatchRuntime(hooks, emit))

	if len(results) != 1 || !results[0].IsError || resultText(results[0]) != errToolPanicRedacted {
		t.Fatalf("results = %+v, want fixed normalized panic %q", results, errToolPanicRedacted)
	}
	for _, emitted := range events() {
		if completed, ok := emitted.(event.ToolCallCompleted); ok {
			if completed.ResultPreview != errToolPanicRedacted || strings.Contains(completed.ResultPreview, "private panic detail") {
				t.Fatalf("completed preview = %q, want redacted", completed.ResultPreview)
			}
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(finishes) != 2 {
		t.Fatalf("finishes = %d, want 2", len(finishes))
	}
	outcomes := map[hook.Operation]hook.Outcome{}
	for _, finish := range finishes {
		if strings.Contains(errorString(finish.Err), "private panic detail") ||
			strings.Contains(finishPreview(finish), "private panic detail") {
			t.Fatalf("hook finish exposed panic detail: %+v", finish)
		}
		outcomes[finish.Operation] = finish.Outcome
	}
	if outcomes[hook.OperationToolExecution] != hook.OutcomeFailed || outcomes[hook.OperationToolCall] != hook.OutcomeCompleted {
		t.Fatalf("outcomes = %+v", outcomes)
	}
}

func TestToolHooks_ExecutionPanicsAreGenericInSerialAndParallelModes(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		sequential bool
	}{
		{name: "serial", sequential: true},
		{name: "parallel", sequential: false},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			base := &fakeRunTool{name: "T", panicMsg: "mode panic secret", sequential: tt.sequential}
			finished := make(chan hook.Result, 2)
			hooks := compileRuntimeHooks(t, hook.Set{Around: []hook.Around{
				{Operation: hook.OperationToolCall, Begin: channelFinish(finished)},
				{Operation: hook.OperationToolExecution, Begin: channelFinish(finished)},
			}})

			results := RunBatch(context.Background(), []content.ToolUseBlock{
				{ID: "use", Name: "T", Input: []byte(`{}`)},
			}, ToolSet{
				Registry: []tool.InvokableTool{sequentialTool{fakeRunTool: base}},
				Access:   autoApproveGate{},
			}, hookBatchRuntime(hooks, func(event.Event) {}))

			if len(results) != 1 || resultText(results[0]) != errToolPanicRedacted {
				t.Fatalf("results = %+v, want generic panic", results)
			}
			for range 2 {
				got := <-finished
				if strings.Contains(finishPreview(got), "mode panic secret") {
					t.Fatalf("hook finish exposed panic: %+v", got)
				}
			}
		})
	}
}

func TestToolHooks_ExecutionFailuresDoNotFailSemanticToolCall(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		tool *fakeRunTool
	}{
		{name: "returned error", tool: &fakeRunTool{name: "T", runErr: errors.New("run failed")}},
		{name: "empty result", tool: &fakeRunTool{name: "T", empty: true}},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			finished := make(chan hook.Result, 2)
			hooks := compileRuntimeHooks(t, hook.Set{Around: []hook.Around{
				{Operation: hook.OperationToolCall, Begin: channelFinish(finished)},
				{Operation: hook.OperationToolExecution, Begin: channelFinish(finished)},
			}})

			results := RunBatch(context.Background(), []content.ToolUseBlock{
				{ID: "use", Name: "T", Input: []byte(`{}`)},
			}, ToolSet{Registry: []tool.InvokableTool{tt.tool}, Access: autoApproveGate{}},
				hookBatchRuntime(hooks, func(event.Event) {}))

			if len(results) != 1 || !results[0].IsError {
				t.Fatalf("results = %+v, want normalized error", results)
			}
			outcomes := map[hook.Operation]hook.Outcome{}
			for range 2 {
				got := <-finished
				outcomes[got.Operation] = got.Outcome
			}
			if outcomes[hook.OperationToolExecution] != hook.OutcomeFailed {
				t.Fatalf("execution outcome = %v, want failed", outcomes[hook.OperationToolExecution])
			}
			if outcomes[hook.OperationToolCall] != hook.OutcomeCompleted {
				t.Fatalf("tool-call outcome = %v, want completed", outcomes[hook.OperationToolCall])
			}
		})
	}
}

func channelFinish(results chan<- hook.Result) hook.BeginFunc {
	return func(ctx context.Context, _ hook.Call) (context.Context, hook.FinishFunc) {
		return ctx, func(result hook.Result) { results <- result }
	}
}

func errorString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func finishPreview(result hook.Result) string {
	if result.ToolCall != nil {
		return result.ToolCall.ResultPreview
	}
	if result.ToolExecution != nil {
		return result.ToolExecution.ResultPreview
	}
	return ""
}

func TestToolHooks_OutcomesForPreExecutionFailures(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		tool        tool.InvokableTool
		access      loopdomain.AccessGate
		idGen       func() (uuid.UUID, error)
		input       []byte
		wantOutcome hook.Outcome
	}{
		{
			name: "id generation failure",
			idGen: func() (uuid.UUID, error) {
				return uuid.UUID{}, errors.New("entropy unavailable")
			},
			input:       []byte(`{}`),
			wantOutcome: hook.OutcomeFailed,
		},
		{
			name:        "unknown tool is normalized",
			idGen:       uuid.New,
			input:       []byte(`{}`),
			wantOutcome: hook.OutcomeCompleted,
		},
		{
			name:        "invalid arguments are normalized",
			tool:        &fakeRunTool{name: "T"},
			access:      autoApproveGate{},
			idGen:       uuid.New,
			input:       []byte(`not-json`),
			wantOutcome: hook.OutcomeCompleted,
		},
		{
			name:        "permission denial is normalized",
			tool:        &fakeRunTool{name: "T"},
			access:      denyAllGate{},
			idGen:       uuid.New,
			input:       []byte(`{}`),
			wantOutcome: hook.OutcomeCompleted,
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			finished := make(chan hook.Result, 1)
			hooks := compileRuntimeHooks(t, hook.Set{Around: []hook.Around{{
				Operation: hook.OperationToolCall,
				Begin: func(ctx context.Context, _ hook.Call) (context.Context, hook.FinishFunc) {
					return ctx, func(result hook.Result) { finished <- result }
				},
			}}})
			runtime := hookBatchRuntime(hooks, func(event.Event) {})
			runtime.IDGen = tt.idGen
			var registry []tool.InvokableTool
			if tt.tool != nil {
				registry = []tool.InvokableTool{tt.tool}
			}

			results := RunBatch(context.Background(), []content.ToolUseBlock{
				{ID: "use", Name: "T", Input: tt.input},
			}, ToolSet{Registry: registry, Access: tt.access}, runtime)

			if len(results) != 1 || !results[0].IsError {
				t.Fatalf("results = %+v, want normalized error", results)
			}
			if got := <-finished; got.Outcome != tt.wantOutcome {
				t.Fatalf("outcome = %v, want %v", got.Outcome, tt.wantOutcome)
			}
		})
	}
}

func TestToolHooks_CancellationFinishesCurrentToolCallCanceled(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	finished := make(chan hook.Result, 1)
	hooks := compileRuntimeHooks(t, hook.Set{Around: []hook.Around{{
		Operation: hook.OperationToolCall,
		Begin: func(ctx context.Context, _ hook.Call) (context.Context, hook.FinishFunc) {
			return ctx, func(result hook.Result) { finished <- result }
		},
	}}})
	base := &fakeRunTool{name: "T", output: "must not run"}
	emit, events := collectEmit()

	results := RunBatch(ctx, []content.ToolUseBlock{
		{ID: "use", Name: "T", Input: []byte(`{}`)},
	}, ToolSet{Registry: []tool.InvokableTool{base}, Access: cancelAwareAccess{}}, hookBatchRuntime(hooks, emit))

	if len(results) != 1 || !results[0].IsError {
		t.Fatalf("results = %+v, want canceled normalized error", results)
	}
	if got := <-finished; got.Outcome != hook.OutcomeCanceled {
		t.Fatalf("outcome = %v, want canceled", got.Outcome)
	}
	_, _, started, completed := startedCompletedOrder(events())
	if started != 1 || completed != 1 {
		t.Fatalf("audit counts started=%d completed=%d, want 1/1", started, completed)
	}
}

func TestToolHooks_ParallelFinishesExactlyOncePerCall(t *testing.T) {
	t.Parallel()
	const count = 12
	var finishes atomic.Int32
	hooks := compileRuntimeHooks(t, hook.Set{Around: []hook.Around{
		{Operation: hook.OperationToolCall, Begin: countFinish(&finishes)},
		{Operation: hook.OperationToolExecution, Begin: countFinish(&finishes)},
	}})
	base := &fakeRunTool{name: "T", output: "ok", delay: time.Millisecond}
	calls := make([]content.ToolUseBlock, count)
	for i := range calls {
		calls[i] = content.ToolUseBlock{ID: string(rune('a' + i)), Name: "T", Input: []byte(`{}`)}
	}

	results := RunBatch(context.Background(), calls,
		ToolSet{Registry: []tool.InvokableTool{base}, Access: autoApproveGate{}, MaxParallelToolCalls: 4},
		hookBatchRuntime(hooks, func(event.Event) {}))

	if len(results) != count {
		t.Fatalf("results = %d, want %d", len(results), count)
	}
	if got := finishes.Load(); got != count*2 {
		t.Fatalf("finishes = %d, want %d", got, count*2)
	}
}

func TestToolHooks_DependencyPanicsAreNormalizedAndFinishCall(t *testing.T) {
	t.Parallel()
	const panicDetail = "dependency panic secret"
	tests := []struct {
		name    string
		stage   string
		access  loopdomain.AccessGate
		runtime func(*hook.Runner, func(event.Event)) BatchRuntime
	}{
		{name: "id generator", stage: "id_gen"},
		{name: "tool info", stage: "info"},
		{name: "audit summary", stage: "audit"},
		{name: "prepare call", stage: "prepare"},
		{name: "write target", stage: "write_target"},
		{name: "sequential", stage: "sequential"},
		{name: "access authorization", stage: "access", access: panicAccess{detail: panicDetail}},
		{
			name:   "approval setup",
			stage:  "approval",
			access: approvalRequestAccess{},
			runtime: func(hooks *hook.Runner, emit func(event.Event)) BatchRuntime {
				runtime := hookBatchRuntime(hooks, emit)
				gateReg := make(chan gateRegistration)
				close(gateReg)
				runtime.GateRegistrations = gateReg
				return runtime
			},
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			finished := make(chan hook.Result, 2)
			hooks := compileRuntimeHooks(t, hook.Set{Around: []hook.Around{{
				Operation: hook.OperationToolCall,
				Begin:     channelFinish(finished),
			}}})
			emit, events := collectEmit()
			runtime := hookBatchRuntime(hooks, emit)
			if tt.runtime != nil {
				runtime = tt.runtime(hooks, emit)
			}
			if tt.stage == "id_gen" {
				runtime.IDGen = func() (uuid.UUID, error) { panic(panicDetail) }
			}
			access := tt.access
			if access == nil {
				access = autoApproveGate{}
			}
			toolStage := tt.stage
			if tt.stage == "id_gen" || tt.stage == "access" || tt.stage == "approval" {
				toolStage = ""
			}

			results, recovered := callRunBatch(func() []result {
				return RunBatch(context.Background(), []content.ToolUseBlock{
					{ID: "use", Name: "T", Input: []byte(`{}`)},
				}, ToolSet{
					Registry: []tool.InvokableTool{panicStageTool{stage: toolStage, detail: panicDetail}},
					Access:   access,
				}, runtime)
			})

			if recovered != nil {
				t.Fatalf("RunBatch leaked panic %q", recovered)
			}
			if len(results) != 1 || !results[0].IsError {
				t.Fatalf("results = %+v, want one normalized failure", results)
			}
			wantResult := "error: internal: tool call failed"
			if tt.stage == "id_gen" {
				wantResult = errIDGenFailure
			}
			if got := resultText(results[0]); got != wantResult || strings.Contains(got, panicDetail) {
				t.Fatalf("result = %q, want fixed generic failure %q", got, wantResult)
			}
			_, _, started, completed := startedCompletedOrder(events())
			if started != 1 || completed != 1 {
				t.Fatalf("audit counts started=%d completed=%d, want 1/1", started, completed)
			}
			select {
			case got := <-finished:
				if got.Outcome != hook.OutcomeFailed {
					t.Fatalf("finish outcome = %v, want failed", got.Outcome)
				}
			default:
				t.Fatal("ToolCall did not finish")
			}
			select {
			case extra := <-finished:
				t.Fatalf("extra ToolCall finish: %+v", extra)
			default:
			}
		})
	}
}

func TestToolHooks_RuntimeInvariantPanicFinishesEveryBegunCallBeforeRepanic(t *testing.T) {
	t.Parallel()
	finished := make(chan hook.Result, 2)
	hooks := compileRuntimeHooks(t, hook.Set{Around: []hook.Around{{
		Operation: hook.OperationToolCall,
		Begin:     channelFinish(finished),
	}}})
	calls := []content.ToolUseBlock{
		{ID: "one", Name: "T", Input: []byte(`{}`)},
		{ID: "two", Name: "T", Input: []byte(`{}`)},
	}

	_, recovered := callRunBatch(func() []result {
		return RunBatch(context.Background(), calls, ToolSet{
			Registry: []tool.InvokableTool{panicStageTool{}},
			Access:   autoApproveGate{},
		}, hookBatchRuntime(hooks, func(event.Event) { panic("event sink invariant") }))
	})

	if recovered != "event sink invariant" {
		t.Fatalf("recovered = %v, want invariant panic rethrown", recovered)
	}
	if len(finished) != len(calls) {
		t.Fatalf("ToolCall finishes = %d, want %d before repanic", len(finished), len(calls))
	}
	for range calls {
		if got := <-finished; got.Outcome != hook.OutcomeFailed {
			t.Fatalf("finish outcome = %v, want failed", got.Outcome)
		}
	}
}

func TestToolHooks_ParallelCompletionInvariantFinishesEveryCallBeforeRepanic(t *testing.T) {
	t.Parallel()
	finished := make(chan hook.Result, 2)
	hooks := compileRuntimeHooks(t, hook.Set{Around: []hook.Around{{
		Operation: hook.OperationToolCall,
		Begin:     channelFinish(finished),
	}}})
	var starts atomic.Int32
	emit := func(ev event.Event) {
		switch ev.(type) {
		case event.ToolCallStarted:
			starts.Add(1)
		case event.ToolCallCompleted:
			panic("parallel completion invariant")
		}
	}
	calls := []content.ToolUseBlock{
		{ID: "one", Name: "T", Input: []byte(`{}`)},
		{ID: "two", Name: "T", Input: []byte(`{}`)},
	}

	_, recovered := callRunBatch(func() []result {
		return RunBatch(context.Background(), calls, ToolSet{
			Registry:             []tool.InvokableTool{panicStageTool{}},
			Access:               autoApproveGate{},
			MaxParallelToolCalls: 2,
		}, hookBatchRuntime(hooks, emit))
	})

	if recovered != "parallel completion invariant" {
		t.Fatalf("recovered = %v, want parallel invariant rethrown", recovered)
	}
	if starts.Load() != int32(len(calls)) {
		t.Fatalf("starts = %d, want %d", starts.Load(), len(calls))
	}
	if len(finished) != len(calls) {
		t.Fatalf("ToolCall finishes = %d, want %d before repanic", len(finished), len(calls))
	}
}

func TestToolHooks_AccessEventSinkPanicIsInvariantNotToolFailure(t *testing.T) {
	t.Parallel()
	finished := make(chan hook.Result, 1)
	hooks := compileRuntimeHooks(t, hook.Set{Around: []hook.Around{{
		Operation: hook.OperationToolCall,
		Begin:     channelFinish(finished),
	}}})
	emit := func(ev event.Event) {
		if _, ok := ev.(event.PermissionDecided); ok {
			panic("permission event sink invariant")
		}
	}

	_, recovered := callRunBatch(func() []result {
		return RunBatch(context.Background(), []content.ToolUseBlock{
			{ID: "use", Name: "T", Input: []byte(`{}`)},
		}, ToolSet{
			Registry: []tool.InvokableTool{panicStageTool{}},
			Access:   autoApproveGate{},
		}, hookBatchRuntime(hooks, emit))
	})

	if recovered != "permission event sink invariant" {
		t.Fatalf("recovered = %v, want invariant rethrown", recovered)
	}
	if len(finished) != 1 {
		t.Fatalf("ToolCall finishes = %d, want 1 before repanic", len(finished))
	}
}

func TestToolHooks_InExecutionInvariantFinishesExecutionAndCallBeforeRepanic(t *testing.T) {
	t.Parallel()
	finished := make(chan hook.Result, 2)
	hooks := compileRuntimeHooks(t, hook.Set{Around: []hook.Around{
		{Operation: hook.OperationToolCall, Begin: channelFinish(finished)},
		{Operation: hook.OperationToolExecution, Begin: channelFinish(finished)},
	}})
	emit := func(ev event.Event) {
		if _, ok := ev.(event.UserInputRequested); ok {
			panic("tool emit invariant")
		}
	}

	_, recovered := callRunBatch(func() []result {
		return RunBatch(context.Background(), []content.ToolUseBlock{
			{ID: "use", Name: "T", Input: []byte(`{}`)},
		}, ToolSet{
			Registry: []tool.InvokableTool{emittingTool{}},
			Access:   autoApproveGate{},
		}, hookBatchRuntime(hooks, emit))
	})

	if recovered != "tool emit invariant" {
		t.Fatalf("recovered = %v, want invariant rethrown", recovered)
	}
	if len(finished) != 2 {
		t.Fatalf("finishes = %d, want ToolExecution and ToolCall", len(finished))
	}
}

type emittingTool struct{}

func (emittingTool) Info(context.Context) (*tool.ToolInfo, error) {
	return &tool.ToolInfo{Name: "T"}, nil
}

func (emittingTool) PrepareCall(context.Context, uuid.UUID, string) (tool.Request, tool.PreparedArtifact, error) {
	return tool.Request{}, nil, nil
}

func (emittingTool) InvokableRun(ctx context.Context, _ string) (*tool.ToolResult, error) {
	emit, _ := EmitFromContext(ctx)
	emit(event.UserInputRequested{})
	return tool.TextResult("unreachable"), nil
}

func TestToolHooks_DynamicDiagnosticsAreBoundedAndSanitized(t *testing.T) {
	t.Parallel()
	hostile := strings.Repeat("é", 800) + "\n\t\x00" + string([]byte{0xff}) + "SECRET_END"
	tests := []struct {
		name  string
		stage string
	}{
		{name: "audit summary", stage: "audit"},
		{name: "prepare error", stage: "prepare"},
		{name: "write target error", stage: "write_target"},
		{name: "execution error", stage: "execution"},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			emit, events := collectEmit()
			results := RunBatch(context.Background(), []content.ToolUseBlock{
				{ID: "use", Name: "T", Input: []byte(`{}`)},
			}, ToolSet{
				Registry: []tool.InvokableTool{diagnosticTool{stage: tt.stage, detail: hostile}},
				Access:   autoApproveGate{},
			}, hookBatchRuntime(nil, emit))

			var got string
			if tt.stage == "audit" {
				for _, emitted := range events() {
					if started, ok := emitted.(event.ToolCallStarted); ok {
						got = started.Summary
					}
				}
			} else {
				if len(results) != 1 || !results[0].IsError {
					t.Fatalf("results = %+v, want one normalized error", results)
				}
				got = resultText(results[0])
			}
			assertSafeDiagnostic(t, got)
			if strings.Contains(got, "SECRET_END") {
				t.Fatalf("diagnostic retained content beyond bound: %q", got)
			}
		})
	}
}

func TestToolHooks_ShortDynamicDiagnosticsRemainUnchanged(t *testing.T) {
	t.Parallel()
	tests := []struct {
		stage string
		want  string
	}{
		{stage: "audit", want: "short detail"},
		{stage: "prepare", want: errPreparePrefix + "short detail"},
		{stage: "write_target", want: errWriteTargetPrefix + "short detail"},
		{stage: "execution", want: errToolPrefix + "short detail"},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.stage, func(t *testing.T) {
			t.Parallel()
			emit, events := collectEmit()
			results := RunBatch(context.Background(), []content.ToolUseBlock{
				{ID: "use", Name: "T", Input: []byte(`{}`)},
			}, ToolSet{
				Registry: []tool.InvokableTool{diagnosticTool{stage: tt.stage, detail: "short detail"}},
				Access:   autoApproveGate{},
			}, hookBatchRuntime(nil, emit))
			got := ""
			if tt.stage == "audit" {
				for _, emitted := range events() {
					if started, ok := emitted.(event.ToolCallStarted); ok {
						got = started.Summary
					}
				}
			} else {
				got = resultText(results[0])
			}
			if got != tt.want {
				t.Fatalf("diagnostic = %q, want unchanged %q", got, tt.want)
			}
		})
	}
}

func TestToolHooks_HostileExecutionErrorTraversalCannotEscapeParallelWorkers(t *testing.T) {
	t.Parallel()
	const count = 6
	finished := make(chan hook.Result, count*2)
	hooks := compileRuntimeHooks(t, hook.Set{Around: []hook.Around{
		{Operation: hook.OperationToolCall, Begin: channelFinish(finished)},
		{Operation: hook.OperationToolExecution, Begin: channelFinish(finished)},
	}})
	calls := make([]content.ToolUseBlock, count)
	for i := range calls {
		calls[i] = content.ToolUseBlock{ID: fmt.Sprintf("use-%d", i), Name: "T", Input: []byte(`{}`)}
	}

	results, recovered := callRunBatch(func() []result {
		return RunBatch(context.Background(), calls, ToolSet{
			Registry:             []tool.InvokableTool{hostileErrorTool{}},
			Access:               autoApproveGate{},
			MaxParallelToolCalls: 3,
		}, hookBatchRuntime(hooks, func(event.Event) {}))
	})

	if recovered != nil {
		t.Fatalf("RunBatch leaked hostile error panic: %v", recovered)
	}
	if len(results) != count {
		t.Fatalf("results = %d, want %d", len(results), count)
	}
	for _, got := range results {
		if !got.IsError || resultText(got) != "error: short safe detail" {
			t.Fatalf("result = %+v, want bounded semantic error", got)
		}
	}
	outcomes := map[hook.Operation]int{}
	for range count * 2 {
		got := <-finished
		outcomes[got.Operation]++
		switch got.Operation {
		case hook.OperationToolExecution:
			if got.Outcome != hook.OutcomeFailed {
				t.Fatalf("execution outcome = %v, want failed", got.Outcome)
			}
			if got.Err == nil || got.Err.Error() != "loop: tool execution failed" {
				t.Fatalf("execution error = %v, want generic redacted error", got.Err)
			}
		case hook.OperationToolCall:
			if got.Outcome != hook.OutcomeCompleted {
				t.Fatalf("tool-call outcome = %v, want completed", got.Outcome)
			}
		}
	}
	if outcomes[hook.OperationToolExecution] != count || outcomes[hook.OperationToolCall] != count {
		t.Fatalf("finish counts = %+v, want %d each", outcomes, count)
	}
}

func TestToolHooks_DetachedToolCallContextStillHonorsParentCancellation(t *testing.T) {
	t.Parallel()
	parent, cancel := context.WithCancel(context.Background())
	entered := make(chan struct{})
	finished := make(chan hook.Result, 1)
	hooks := compileRuntimeHooks(t, hook.Set{Around: []hook.Around{{
		Operation: hook.OperationToolCall,
		Begin: func(context.Context, hook.Call) (context.Context, hook.FinishFunc) {
			return context.Background(), func(result hook.Result) { finished <- result }
		},
	}}})
	done := make(chan []result, 1)
	go func() {
		done <- RunBatch(parent, []content.ToolUseBlock{
			{ID: "use", Name: "T", Input: []byte(`{}`)},
		}, ToolSet{
			Registry: []tool.InvokableTool{&fakeRunTool{name: "T", output: "must not run"}},
			Access:   blockingAccess{entered: entered},
		}, hookBatchRuntime(hooks, func(event.Event) {}))
	}()
	<-entered
	cancel()

	select {
	case results := <-done:
		if len(results) != 1 || !results[0].IsError {
			t.Fatalf("results = %+v, want canceled error", results)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("detached ToolCall context ignored parent cancellation")
	}
	if got := <-finished; got.Outcome != hook.OutcomeCanceled {
		t.Fatalf("ToolCall outcome = %v, want canceled", got.Outcome)
	}
}

func TestToolHooks_DetachedExecutionContextStillHonorsParentCancellation(t *testing.T) {
	t.Parallel()
	parent, cancel := context.WithCancel(context.Background())
	entered := make(chan struct{})
	finished := make(chan hook.Result, 2)
	base := &fakeRunTool{name: "T"}
	base.onRun = func(ctx context.Context) {
		close(entered)
		<-ctx.Done()
		base.runErr = ctx.Err()
	}
	hooks := compileRuntimeHooks(t, hook.Set{Around: []hook.Around{
		{Operation: hook.OperationToolCall, Begin: channelFinish(finished)},
		{
			Operation: hook.OperationToolExecution,
			Begin: func(context.Context, hook.Call) (context.Context, hook.FinishFunc) {
				return context.Background(), func(result hook.Result) { finished <- result }
			},
		},
	}})
	done := make(chan []result, 1)
	go func() {
		done <- RunBatch(parent, []content.ToolUseBlock{
			{ID: "use", Name: "T", Input: []byte(`{}`)},
		}, ToolSet{
			Registry: []tool.InvokableTool{base},
			Access:   autoApproveGate{},
		}, hookBatchRuntime(hooks, func(event.Event) {}))
	}()
	<-entered
	cancel()

	select {
	case results := <-done:
		if len(results) != 1 || !results[0].IsError {
			t.Fatalf("results = %+v, want canceled error", results)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("detached ToolExecution context ignored parent cancellation")
	}
	outcomes := map[hook.Operation]hook.Outcome{}
	for range 2 {
		got := <-finished
		outcomes[got.Operation] = got.Outcome
	}
	if outcomes[hook.OperationToolExecution] != hook.OutcomeCanceled ||
		outcomes[hook.OperationToolCall] != hook.OutcomeCanceled {
		t.Fatalf("outcomes = %+v, want both canceled", outcomes)
	}
}

type blockingAccess struct{ entered chan<- struct{} }

func (b blockingAccess) Authorize(ctx context.Context, _ tool.Request) (gatedomain.Resolution, error) {
	close(b.entered)
	<-ctx.Done()
	return gatedomain.Resolution{}, ctx.Err()
}

type hostileErrorTool struct{}

func (hostileErrorTool) Info(context.Context) (*tool.ToolInfo, error) {
	return &tool.ToolInfo{Name: "T"}, nil
}

func (hostileErrorTool) PrepareCall(context.Context, uuid.UUID, string) (tool.Request, tool.PreparedArtifact, error) {
	return tool.Request{}, nil, nil
}

func (hostileErrorTool) InvokableRun(context.Context, string) (*tool.ToolResult, error) {
	return nil, hostileTraversalError{}
}

type hostileTraversalError struct{}

func (hostileTraversalError) Error() string { return "short safe detail" }
func (hostileTraversalError) Is(error) bool { panic("hostile Is") }
func (hostileTraversalError) As(any) bool   { panic("hostile As") }
func (hostileTraversalError) Unwrap() error { panic("hostile Unwrap") }

func assertSafeDiagnostic(t *testing.T, got string) {
	t.Helper()
	if len(got) > 1024 {
		t.Fatalf("diagnostic bytes = %d, want <= 1024", len(got))
	}
	if !utf8.ValidString(got) {
		t.Fatalf("diagnostic is not valid UTF-8: %q", got)
	}
	for _, r := range got {
		if unicode.IsControl(r) {
			t.Fatalf("diagnostic contains control %U: %q", r, got)
		}
	}
}

type diagnosticTool struct {
	stage  string
	detail string
}

func (d diagnosticTool) Info(context.Context) (*tool.ToolInfo, error) {
	return &tool.ToolInfo{Name: "T"}, nil
}

func (d diagnosticTool) AuditSummary(string) string {
	if d.stage == "audit" {
		return d.detail
	}
	return "T"
}

func (d diagnosticTool) PrepareCall(context.Context, uuid.UUID, string) (tool.Request, tool.PreparedArtifact, error) {
	if d.stage == "prepare" {
		return tool.Request{}, nil, diagnosticError(d.detail)
	}
	return tool.Request{}, nil, nil
}

func (d diagnosticTool) WriteTarget(string) (string, bool, error) {
	if d.stage == "write_target" {
		return "", false, diagnosticError(d.detail)
	}
	return "", false, nil
}

func (d diagnosticTool) InvokableRun(context.Context, string) (*tool.ToolResult, error) {
	if d.stage == "execution" {
		return nil, diagnosticError(d.detail)
	}
	return tool.TextResult("ok"), nil
}

type diagnosticError string

func (e diagnosticError) Error() string { return string(e) }

func callRunBatch(run func() []result) (results []result, recovered any) {
	defer func() { recovered = recover() }()
	results = run()
	return results, nil
}

type panicStageTool struct {
	stage  string
	detail string
}

func (p panicStageTool) Info(context.Context) (*tool.ToolInfo, error) {
	p.panicAt("info")
	return &tool.ToolInfo{Name: "T"}, nil
}

func (p panicStageTool) AuditSummary(string) string {
	p.panicAt("audit")
	return "T"
}

func (p panicStageTool) PrepareCall(context.Context, uuid.UUID, string) (tool.Request, tool.PreparedArtifact, error) {
	p.panicAt("prepare")
	return tool.Request{}, nil, nil
}

func (p panicStageTool) WriteTarget(string) (string, bool, error) {
	p.panicAt("write_target")
	return "", false, nil
}

func (p panicStageTool) Sequential() bool {
	p.panicAt("sequential")
	return false
}

func (p panicStageTool) InvokableRun(context.Context, string) (*tool.ToolResult, error) {
	return tool.TextResult("ok"), nil
}

func (p panicStageTool) panicAt(stage string) {
	if p.stage == stage {
		panic(p.detail)
	}
}

type panicAccess struct{ detail string }

func (p panicAccess) Authorize(context.Context, tool.Request) (gatedomain.Resolution, error) {
	panic(p.detail)
}

type approvalRequestAccess struct{}

func (approvalRequestAccess) Authorize(ctx context.Context, request tool.Request) (gatedomain.Resolution, error) {
	action, err := loopdomain.GateApprover().RequestApproval(ctx, gatedomain.ApprovalPrompt{Request: request})
	return gatedomain.Resolution{Approved: action != gatedomain.ApprovalDeny}, err
}

func countFinish(count *atomic.Int32) hook.BeginFunc {
	return func(ctx context.Context, _ hook.Call) (context.Context, hook.FinishFunc) {
		return ctx, func(hook.Result) { count.Add(1) }
	}
}

type cancelAwareAccess struct{}

func (cancelAwareAccess) Authorize(ctx context.Context, _ tool.Request) (gatedomain.Resolution, error) {
	return gatedomain.Resolution{}, ctx.Err()
}

func captureFinish(mu *sync.Mutex, results *[]hook.Result) hook.BeginFunc {
	return func(ctx context.Context, _ hook.Call) (context.Context, hook.FinishFunc) {
		return ctx, func(result hook.Result) {
			mu.Lock()
			*results = append(*results, result)
			mu.Unlock()
		}
	}
}

type infoProbeTool struct {
	tool.InvokableTool
	calls *atomic.Int32
}

func (p *infoProbeTool) Info(ctx context.Context) (*tool.ToolInfo, error) {
	p.calls.Add(1)
	return p.InvokableTool.Info(ctx)
}

type accessProbe struct{ calls *atomic.Int32 }

func (p accessProbe) Authorize(context.Context, tool.Request) (gatedomain.Resolution, error) {
	p.calls.Add(1)
	return gatedomain.Resolution{Approved: true}, nil
}
