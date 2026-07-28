package loopruntime

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

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
	emit, _ := collectEmit()

	results := RunBatch(context.Background(), []content.ToolUseBlock{
		{ID: "use-1", Name: "T", Input: []byte(`{}`)},
	}, ToolSet{Registry: []tool.InvokableTool{base}, Access: autoApproveGate{}}, hookBatchRuntime(hooks, emit))

	if len(results) != 1 || !results[0].IsError || !strings.Contains(resultText(results[0]), errPanicPrefix) {
		t.Fatalf("results = %+v, want normalized panic", results)
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
