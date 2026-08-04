package hustleruntime

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/looprig/core/content"
	"github.com/looprig/harness/pkg/hustle"
	"github.com/looprig/harness/pkg/tool"
	"github.com/looprig/inference"
	"github.com/looprig/inference/failure"
	"github.com/looprig/inference/stream"
)

func TestRetryClassificationIsClosed(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  error
		want retryFailureClass
	}{
		{name: "network", err: &failure.NetworkError{Err: &net.DNSError{IsTemporary: true}}, want: retryFailureTransientInference},
		{name: "request timeout", err: &failure.APIError{Status: 408, Message: "secret"}, want: retryFailureTransientInference},
		{name: "rate limit", err: &failure.APIError{Status: 429, Message: "secret"}, want: retryFailureTransientInference},
		{name: "server error", err: &failure.APIError{Status: 503, Message: "secret"}, want: retryFailureTransientInference},
		{name: "recoverable terminal parse", err: toolResponseError(ToolResponseFailureInvalidTerminal), want: retryFailureRecoverableTerminal},
		{name: "canceled", err: context.Canceled},
		{name: "deadline", err: context.DeadlineExceeded},
		{name: "client API", err: &failure.APIError{Status: 400, Message: "secret"}},
		{name: "unknown API status", err: &failure.APIError{Status: 700, Message: "secret"}},
		{name: "unknown error", err: errors.New("transient timeout please retry")},
		{name: "unknown tool", err: toolResponseError(ToolResponseFailureUnknownTool)},
		{name: "malformed evidence arguments", err: toolResponseError(ToolResponseFailureMalformedArguments)},
		{name: "finish contradiction", err: toolResponseError(ToolResponseFailureFinishReason)},
		{name: "mixed response", err: toolResponseError(ToolResponseFailureMixed)},
		{name: "duplicate terminal", err: toolResponseError(ToolResponseFailureDuplicateTerminal)},
		{name: "oversized output", err: toolResponseError(ToolResponseFailureTooLarge)},
		{name: "evidence access", err: evidenceError(EvidenceFailureAccessRefused)},
		{name: "evidence preparation", err: evidenceError(EvidenceFailurePreparation)},
		{name: "evidence bounds", err: evidenceError(EvidenceFailureRoundsExceeded)},
		{name: "controller poison", err: &WorkerPoisonError{}},
		{name: "validator", err: &OutputError{Cause: errors.New("needs_human")}},
	}
	for _, testCase := range tests {
		testCase := testCase
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			if got := classifyRetryFailure(testCase.err); got != testCase.want {
				t.Fatalf("classifyRetryFailure(%T) = %v, want %v", testCase.err, got, testCase.want)
			}
		})
	}
}

func TestShouldRetryRequiresExplicitPolicyFirstAttemptAndLiveBudget(t *testing.T) {
	t.Parallel()

	transient := &failure.APIError{Status: 503, Message: "secret"}
	transientRun := &RunError{Stage: hustle.StageInference, ReasonCode: hustle.ReasonInference, Cause: transient}
	if shouldRetry(hustle.RetryPolicyNone, 0, nil, transientRun, false) {
		t.Fatal("disabled policy retried")
	}
	if !shouldRetry(hustle.RetryPolicyClassifiedOnce, 0, nil, transientRun, false) {
		t.Fatal("classified first failure did not retry")
	}
	if shouldRetry(hustle.RetryPolicyClassifiedOnce, 1, nil, transientRun, false) {
		t.Fatal("second failure retried")
	}
	if shouldRetry(hustle.RetryPolicyClassifiedOnce, 0, context.DeadlineExceeded, transientRun, false) {
		t.Fatal("exhausted deadline retried")
	}
	if shouldRetry(hustle.RetryPolicyClassifiedOnce, 0, nil, transientRun, true) {
		t.Fatal("poisoned controller retried")
	}
	validatorRun := &RunError{
		Stage: hustle.StageOutput, ReasonCode: hustle.ReasonInvalidOutput,
		Cause: &OutputError{Cause: transient},
	}
	if shouldRetry(hustle.RetryPolicyClassifiedOnce, 0, nil, validatorRun, false) {
		t.Fatal("transient-looking validator failure retried")
	}
	malformedValidatorRun := &RunError{
		Stage: hustle.StageInference, ReasonCode: hustle.ReasonInference,
		Cause: &OutputError{Cause: hustle.NewRecoverableTerminalValidationError()},
	}
	if shouldRetry(hustle.RetryPolicyClassifiedOnce, 0, nil, malformedValidatorRun, false) {
		t.Fatal("terminal-validation marker at inference stage retried")
	}
	malformedInternalRun := &RunError{
		Stage: hustle.StageOutput, ReasonCode: hustle.ReasonInternal,
		Cause: &OutputError{Cause: hustle.NewRecoverableTerminalValidationError()},
	}
	if shouldRetry(hustle.RetryPolicyClassifiedOnce, 0, nil, malformedInternalRun, false) {
		t.Fatal("terminal-validation marker with internal reason retried")
	}
}

func TestStrictClassifierValidatorRetriesRecoverableWireShapeFailures(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		malformed string
	}{
		{name: "duplicate field", malformed: `{"decision":"approve","decision":"approve"}`},
		{name: "unknown field", malformed: `{"decision":"approve","extra":true}`},
		{name: "missing field", malformed: `{"reason":"safe"}`},
	}
	for _, testCase := range tests {
		testCase := testCase
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			sessionID, loopID := mustRuntimeTestID(t), mustRuntimeTestID(t)
			responses := []string{testCase.malformed, `{"decision":"approve"}`}
			client := &runtimeTestClient{invoke: func(context.Context, inference.Request) (*inference.Response, error) {
				response := responses[0]
				responses = responses[1:]
				return terminalEvidenceResponse(response, nil), nil
			}}
			definition := runtimeRetryEvidenceDefinition(t, client, time.Second, func(context.Context, tool.EvidenceFactoryBindings) ([]tool.InvokableTool, error) {
				return []tool.InvokableTool{newPreparedEvidenceTool("workspace_read", "ok")}, nil
			})
			controller := runtimeEvidenceController(t, sessionID, definition)
			err := controller.RunAndFinalize(
				context.Background(),
				runtimeEvidenceRequest(t, definition.Name(), sessionID, loopID),
				strictClassifierWireValidator,
				noOpFinalizer,
			)
			if err != nil {
				t.Fatal(err)
			}
			if client.invocations.Load() != 2 {
				t.Fatalf("invocations = %d, want one retry", client.invocations.Load())
			}
		})
	}
}

func TestClassifierValidatorRetryBoundaryIsClosed(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		output    string
		validator ValidateResult
	}{
		{name: "domain needs human", output: `{"decision":"needs_human"}`, validator: strictClassifierWireValidator},
		{name: "domain deny", output: `{"decision":"deny"}`, validator: strictClassifierWireValidator},
		{name: "basis mismatch", output: `{"decision":"approve"}`, validator: func(context.Context, hustle.Result) error {
			return errors.New("basis_mismatch")
		}},
		{name: "arbitrary validator error", output: `{"decision":"approve"}`, validator: func(context.Context, hustle.Result) error {
			return errors.New("decoder says retry")
		}},
		{name: "validator panic", output: `{"decision":"approve"}`, validator: func(context.Context, hustle.Result) error {
			panic("provider output secret")
		}},
	}
	for _, testCase := range tests {
		testCase := testCase
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			sessionID, loopID := mustRuntimeTestID(t), mustRuntimeTestID(t)
			client := &runtimeTestClient{invoke: func(context.Context, inference.Request) (*inference.Response, error) {
				return terminalEvidenceResponse(testCase.output, nil), nil
			}}
			definition := runtimeRetryEvidenceDefinition(t, client, time.Second, func(context.Context, tool.EvidenceFactoryBindings) ([]tool.InvokableTool, error) {
				return []tool.InvokableTool{newPreparedEvidenceTool("workspace_read", "ok")}, nil
			})
			controller := runtimeEvidenceController(t, sessionID, definition)
			_ = controller.RunAndFinalize(
				context.Background(),
				runtimeEvidenceRequest(t, definition.Name(), sessionID, loopID),
				testCase.validator,
				noOpFinalizer,
			)
			if client.invocations.Load() != 1 {
				t.Fatalf("invocations = %d, want no retry", client.invocations.Load())
			}
		})
	}
}

func TestSecondRecoverableValidatorFailureStopsWithBoundedCause(t *testing.T) {
	t.Parallel()

	sessionID, loopID := mustRuntimeTestID(t), mustRuntimeTestID(t)
	client := &runtimeTestClient{invoke: func(context.Context, inference.Request) (*inference.Response, error) {
		return terminalEvidenceResponse(`{"decision":"approve","decoder_secret":"sensitive"}`, nil), nil
	}}
	definition := runtimeRetryEvidenceDefinition(t, client, time.Second, func(context.Context, tool.EvidenceFactoryBindings) ([]tool.InvokableTool, error) {
		return []tool.InvokableTool{newPreparedEvidenceTool("workspace_read", "ok")}, nil
	})
	controller := runtimeEvidenceController(t, sessionID, definition)
	err := controller.RunAndFinalize(
		context.Background(),
		runtimeEvidenceRequest(t, definition.Name(), sessionID, loopID),
		func(context.Context, hustle.Result) error {
			return hustle.NewRecoverableTerminalValidationError()
		},
		noOpFinalizer,
	)
	if client.invocations.Load() != 2 {
		t.Fatalf("invocations = %d, want exactly two", client.invocations.Load())
	}
	if err == nil || strings.Contains(err.Error(), "sensitive") {
		t.Fatalf("error = %v, want bounded classification without decoder/output text", err)
	}
	var runErr *RunError
	if !errors.As(err, &runErr) {
		t.Fatalf("error = %T %v, want RunError", err, err)
	}
	outputErr, ok := runErr.Cause.(*OutputError)
	if !ok || !hustle.IsRecoverableTerminalValidationError(outputErr.Cause) {
		t.Fatalf("cause = %T %v, want fixed recoverable terminal marker", runErr.Cause, runErr.Cause)
	}
}

func TestRecoverableValidatorMarkerDoesNotEnableZeroRetryPolicy(t *testing.T) {
	t.Parallel()

	sessionID, loopID := mustRuntimeTestID(t), mustRuntimeTestID(t)
	client := &runtimeTestClient{invoke: func(context.Context, inference.Request) (*inference.Response, error) {
		return terminalEvidenceResponse(`{"unexpected":true}`, nil), nil
	}}
	definition := runtimeEvidenceDefinition(
		t,
		client,
		runtimeEvidenceModel(),
		func(context.Context, tool.EvidenceFactoryBindings) ([]tool.InvokableTool, error) {
			return []tool.InvokableTool{newPreparedEvidenceTool("workspace_read", "ok")}, nil
		},
		hustle.ToolLoopLimits{
			MaxRounds: 2, MaxCalls: 2, MaxCallsPerRound: 1,
			MaxResultBytes: 1024, MaxEvidenceBytes: 2048,
		},
	)
	controller := runtimeEvidenceController(t, sessionID, definition)
	_ = controller.RunAndFinalize(
		context.Background(),
		runtimeEvidenceRequest(t, definition.Name(), sessionID, loopID),
		func(context.Context, hustle.Result) error {
			return hustle.NewRecoverableTerminalValidationError()
		},
		noOpFinalizer,
	)
	if client.invocations.Load() != 1 {
		t.Fatalf("invocations = %d, want zero-policy single attempt", client.invocations.Load())
	}
}

func TestRetryEnabledRunDoesNotRetrySessionShutdown(t *testing.T) {
	t.Parallel()

	sessionID, loopID := mustRuntimeTestID(t), mustRuntimeTestID(t)
	invoked := make(chan struct{})
	client := &runtimeTestClient{invoke: func(ctx context.Context, _ inference.Request) (*inference.Response, error) {
		close(invoked)
		<-ctx.Done()
		return nil, &failure.NetworkError{Err: ctx.Err()}
	}}
	definition := runtimeRetryEvidenceDefinition(t, client, time.Second, func(context.Context, tool.EvidenceFactoryBindings) ([]tool.InvokableTool, error) {
		return []tool.InvokableTool{newPreparedEvidenceTool("workspace_read", "ok")}, nil
	})
	controller := runtimeEvidenceController(t, sessionID, definition)
	request := runtimeEvidenceRequest(t, definition.Name(), sessionID, loopID)
	result := make(chan error, 1)
	go func() {
		result <- controller.RunAndFinalize(context.Background(), request, acceptResult, noOpFinalizer)
	}()
	<-invoked
	if err := controller.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	err := <-result
	var runErr *RunError
	if !errors.As(err, &runErr) || runErr.ReasonCode != hustle.ReasonCanceled {
		t.Fatalf("error = %T %v, want canceled run", err, err)
	}
	if client.invocations.Load() != 1 {
		t.Fatalf("invocations = %d, want no shutdown retry", client.invocations.Load())
	}
	<-controller.Drained()
}

func TestRetryEnabledRunDoesNotRetryFinalizerFailure(t *testing.T) {
	t.Parallel()

	sessionID, loopID := mustRuntimeTestID(t), mustRuntimeTestID(t)
	finalizerCause := errors.New("finalizer failure")
	client := &runtimeTestClient{invoke: func(context.Context, inference.Request) (*inference.Response, error) {
		return terminalEvidenceResponse(`{"decision":"approve"}`, nil), nil
	}}
	definition := runtimeRetryEvidenceDefinition(t, client, time.Second, func(context.Context, tool.EvidenceFactoryBindings) ([]tool.InvokableTool, error) {
		return []tool.InvokableTool{newPreparedEvidenceTool("workspace_read", "ok")}, nil
	})
	controller := runtimeEvidenceController(t, sessionID, definition)
	err := controller.RunAndFinalize(
		context.Background(),
		runtimeEvidenceRequest(t, definition.Name(), sessionID, loopID),
		strictClassifierWireValidator,
		func(context.Context, hustle.Outcome) error { return finalizerCause },
	)
	var finalizerErr *FinalizerError
	if !errors.As(err, &finalizerErr) || !errors.Is(err, finalizerCause) {
		t.Fatalf("error = %T %v, want finalizer failure", err, err)
	}
	if client.invocations.Load() != 1 {
		t.Fatalf("invocations = %d, want no finalizer retry", client.invocations.Load())
	}
}

func strictClassifierWireValidator(_ context.Context, result hustle.Result) error {
	decoder := json.NewDecoder(strings.NewReader(string(result.Output)))
	seenDecision := false
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return hustle.NewRecoverableTerminalValidationError()
	}
	for decoder.More() {
		key, err := decoder.Token()
		if err != nil {
			return hustle.NewRecoverableTerminalValidationError()
		}
		if key != "decision" || seenDecision {
			return hustle.NewRecoverableTerminalValidationError()
		}
		seenDecision = true
		var decision string
		if err := decoder.Decode(&decision); err != nil {
			return hustle.NewRecoverableTerminalValidationError()
		}
	}
	if _, err := decoder.Token(); err != nil || !seenDecision {
		return hustle.NewRecoverableTerminalValidationError()
	}
	return nil
}

func TestClassifiedRetryRestartsFromImmutableInputAndFreshEvidenceCatalog(t *testing.T) {
	t.Parallel()

	sessionID, loopID := mustRuntimeTestID(t), mustRuntimeTestID(t)
	var requests []inference.Request
	builds := 0
	runs := 0
	client := &runtimeTestClient{invoke: func(_ context.Context, request inference.Request) (*inference.Response, error) {
		requests = append(requests, request)
		switch len(requests) {
		case 1:
			request.Messages[0].(*content.UserMessage).Blocks[0].(*content.TextBlock).Text = "provider mutation"
			request.Tools[0].Schema[0] = '['
			return oneEvidenceCallResponse("first-evidence"), nil
		case 2:
			return &inference.Response{Usage: &content.Usage{InputTokens: 5, OutputTokens: 1}},
				&failure.APIError{Status: 503, Message: "provider secret"}
		case 3:
			if len(request.Messages) != 1 {
				t.Fatalf("retry messages = %d, want pristine single input", len(request.Messages))
			}
			user := request.Messages[0].(*content.UserMessage)
			if text := user.Blocks[0].(*content.TextBlock).Text; text != `{"version":1}` {
				t.Fatalf("retry input = %q, want immutable original", text)
			}
			if !json.Valid(request.Tools[0].Schema) {
				t.Fatal("retry tool schema retained provider mutation")
			}
			return terminalEvidenceResponse(`{"summary":"allow"}`, &content.Usage{InputTokens: 7, OutputTokens: 2}), nil
		default:
			t.Fatalf("unexpected retry loop invocation %d", len(requests))
			return nil, nil
		}
	}}
	definition := runtimeRetryEvidenceDefinition(t, client, time.Second, func(context.Context, tool.EvidenceFactoryBindings) ([]tool.InvokableTool, error) {
		builds++
		evidence := newPreparedEvidenceTool("workspace_read", "ok")
		evidence.result = tool.TextResult("private evidence")
		return []tool.InvokableTool{&countingEvidenceTool{preparedEvidenceTool: evidence, runs: &runs}}, nil
	})
	controller := runtimeEvidenceController(t, sessionID, definition)
	var got hustle.Result
	err := controller.RunAndFinalize(
		context.Background(),
		runtimeEvidenceRequest(t, definition.Name(), sessionID, loopID),
		acceptResult,
		func(_ context.Context, outcome hustle.Outcome) error {
			if outcome.Result == nil {
				t.Fatal("finalizer result = nil")
			}
			got = *outcome.Result
			return nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(requests) != 3 || builds != 2 || runs != 1 {
		t.Fatalf("calls = inference:%d builds:%d evidence:%d, want 3,2,1", len(requests), builds, runs)
	}
	wantUsage := &content.Usage{InputTokens: 12, OutputTokens: 3}
	if !reflect.DeepEqual(got.Usage, wantUsage) {
		t.Fatalf("usage = %#v, want all billable attempt usage %#v", got.Usage, wantUsage)
	}
}

func TestRecoverableTerminalParseRetriesOnceWithinOriginalDeadline(t *testing.T) {
	t.Parallel()

	sessionID, loopID := mustRuntimeTestID(t), mustRuntimeTestID(t)
	var deadlines []time.Time
	client := &runtimeTestClient{invoke: func(ctx context.Context, _ inference.Request) (*inference.Response, error) {
		deadline, ok := ctx.Deadline()
		if !ok {
			t.Fatal("inference context has no deadline")
		}
		deadlines = append(deadlines, deadline)
		if len(deadlines) == 1 {
			return toolResponse(stream.FinishReasonStop, toolResponseText(`not-json`)), nil
		}
		if len(deadlines) > 2 {
			t.Fatalf("unexpected retry loop invocation %d", len(deadlines))
		}
		return terminalEvidenceResponse(`{"summary":"allow"}`, nil), nil
	}}
	definition := runtimeRetryEvidenceDefinition(t, client, time.Second, func(context.Context, tool.EvidenceFactoryBindings) ([]tool.InvokableTool, error) {
		return []tool.InvokableTool{newPreparedEvidenceTool("workspace_read", "ok")}, nil
	})
	controller := runtimeEvidenceController(t, sessionID, definition)
	if err := controller.RunAndFinalize(context.Background(), runtimeEvidenceRequest(t, definition.Name(), sessionID, loopID), acceptResult, noOpFinalizer); err != nil {
		t.Fatal(err)
	}
	if len(deadlines) != 2 || !deadlines[0].Equal(deadlines[1]) {
		t.Fatalf("deadlines = %v, want identical original deadline", deadlines)
	}
}

func TestRetryDoesNotResetExhaustedDeadlineOrChangeToollessBehavior(t *testing.T) {
	t.Parallel()

	t.Run("deadline exhausted before retry", func(t *testing.T) {
		sessionID, loopID := mustRuntimeTestID(t), mustRuntimeTestID(t)
		var executionCtx *runtimeManualContext
		client := &runtimeTestClient{invoke: func(ctx context.Context, _ inference.Request) (*inference.Response, error) {
			executionCtx.trigger(context.DeadlineExceeded)
			<-ctx.Done()
			return nil, &failure.NetworkError{Err: ctx.Err()}
		}}
		definition := runtimeRetryEvidenceDefinition(t, client, time.Second, func(context.Context, tool.EvidenceFactoryBindings) ([]tool.InvokableTool, error) {
			return []tool.InvokableTool{newPreparedEvidenceTool("workspace_read", "ok")}, nil
		})
		controller := runtimeEvidenceController(t, sessionID, definition)
		controller.runtime.newExecutionContext = func(parent context.Context, _ time.Duration) (context.Context, context.CancelFunc) {
			executionCtx = newRuntimeManualContext(parent)
			return executionCtx, func() {}
		}
		err := controller.RunAndFinalize(context.Background(), runtimeEvidenceRequest(t, definition.Name(), sessionID, loopID), acceptResult, noOpFinalizer)
		var runErr *RunError
		if !errors.As(err, &runErr) || runErr.ReasonCode != hustle.ReasonTimeout {
			t.Fatalf("error = %#v, want timeout", err)
		}
		if client.invocations.Load() != 1 {
			t.Fatalf("invocations = %d, want no deadline retry", client.invocations.Load())
		}
	})

	t.Run("tool-less remains one attempt", func(t *testing.T) {
		client := &runtimeTestClient{invoke: func(context.Context, inference.Request) (*inference.Response, error) {
			return nil, &failure.APIError{Status: 503}
		}}
		definition := runtimeTestBoundDefinition(t, "test.no-retry", hustle.ParticipationBlocking, client, hustle.ModelSourceNamed, nil)
		controller := runtimeTestController(t, definition, &runtimeTestAudit{}, &runtimeTestFaults{}, &runtimeTestActivity{})
		if err := controller.RunAndFinalize(context.Background(), runtimeRequest(t, definition.Name()), acceptResult, noOpFinalizer); err == nil {
			t.Fatal("RunAndFinalize() error = nil")
		}
		if client.invocations.Load() != 1 {
			t.Fatalf("invocations = %d, want legacy one attempt", client.invocations.Load())
		}
	})
}

type countingEvidenceTool struct {
	*preparedEvidenceTool
	runs *int
}

func (t *countingEvidenceTool) InvokableRun(ctx context.Context, args string) (*tool.ToolResult, error) {
	(*t.runs)++
	return t.preparedEvidenceTool.InvokableRun(ctx, args)
}

func runtimeRetryEvidenceDefinition(
	t *testing.T,
	client inference.Client,
	timeout time.Duration,
	factory tool.EvidenceFactory,
) hustle.BoundDefinition {
	t.Helper()
	info := evidenceToolInfo("workspace_read")
	candidate := runtimeEvidenceModel()
	definition, err := hustle.Define(
		hustle.WithName("test.retry-evidence"),
		hustle.WithParticipation(hustle.ParticipationBlocking),
		hustle.WithTimeout(timeout),
		hustle.WithLimits(hustle.Limits{InputBytes: 1024, OutputBytes: 1024}),
		hustle.WithSystemPrompt("Review only.", "prompt-v1"),
		hustle.WithPolicyRevision("policy-v1"),
		hustle.WithNamedInference(client, candidate),
		hustle.WithOutputSchema(runtimeTestOutputSchema()),
		hustle.WithEvidenceTools(hustle.EvidenceToolPolicy{
			Revision: "evidence-v1",
			Limits: hustle.ToolLoopLimits{
				MaxRounds: 2, MaxCalls: 2, MaxCallsPerRound: 1,
				MaxResultBytes: 1024, MaxEvidenceBytes: 2048,
			},
			Definitions: []tool.Definition{tool.NewEvidenceDefinition(
				"workspace-read", tool.RequiresWorkspaceRead, []tool.ToolInfo{info}, factory,
			)},
		}),
		hustle.WithRetryPolicy(hustle.RetryPolicyClassifiedOnce),
	)
	if err != nil {
		t.Fatal(err)
	}
	bound, err := definition.Bind(context.Background(), hustle.Bindings{})
	if err != nil {
		t.Fatal(err)
	}
	return bound
}
