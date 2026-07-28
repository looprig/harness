package hustleruntime

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/looprig/core/content"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/gate"
	"github.com/looprig/harness/pkg/hustle"
	"github.com/looprig/harness/pkg/identity"
	"github.com/looprig/harness/pkg/tool"
	"github.com/looprig/inference"
	model "github.com/looprig/inference/model"
	"github.com/looprig/inference/stream"
)

func TestRunAndFinalizeExecutesOnePrivateEvidenceRoundThenTerminal(t *testing.T) {
	t.Parallel()

	sessionID := mustRuntimeTestID(t)
	loopID := mustRuntimeTestID(t)
	evidence := newPreparedEvidenceTool("workspace_read", "ok")
	evidence.result = tool.TextResult("safe")
	builds := 0
	var requests []inference.Request
	client := &runtimeTestClient{invoke: func(_ context.Context, request inference.Request) (*inference.Response, error) {
		requests = append(requests, request)
		if len(requests) == 1 {
			return &inference.Response{
				Message: &content.AIMessage{Message: content.Message{
					Role: content.RoleAssistant,
					Blocks: []content.Block{&content.ToolUseBlock{
						ID: "call-1", Name: "workspace_read", Input: json.RawMessage(`{"path":"file"}`),
					}},
				}},
				Usage:        &content.Usage{InputTokens: 3, OutputTokens: 2},
				FinishReason: stream.FinishReasonToolUse,
			}, nil
		}
		return &inference.Response{
			Message: &content.AIMessage{Message: content.Message{
				Role:   content.RoleAssistant,
				Blocks: []content.Block{&content.TextBlock{Text: `{"summary":"allow"}`}},
			}},
			Usage:        &content.Usage{InputTokens: 5, OutputTokens: 4},
			FinishReason: stream.FinishReasonStop,
		}, nil
	}}
	definition := runtimeEvidenceDefinition(t, client, runtimeEvidenceModel(), func(_ context.Context, bindings tool.EvidenceFactoryBindings) ([]tool.InvokableTool, error) {
		builds++
		if bindings.SessionID != sessionID || bindings.LoopID != loopID ||
			bindings.ReadWorkspace == nil || bindings.ReadWorkspace.Root != "/workspace" {
			t.Fatalf("evidence bindings = %#v", bindings)
		}
		return []tool.InvokableTool{evidence}, nil
	}, hustle.ToolLoopLimits{
		MaxRounds: 2, MaxCalls: 2, MaxCallsPerRound: 1,
		MaxResultBytes: 1024, MaxEvidenceBytes: 2048,
	})
	controller := runtimeEvidenceController(t, sessionID, definition)
	request := runtimeEvidenceRequest(t, definition.Name(), sessionID, loopID)

	var got hustle.Result
	err := controller.RunAndFinalize(context.Background(), request, func(_ context.Context, result hustle.Result) error {
		got = result
		return nil
	}, noOpFinalizer)
	if err != nil {
		t.Fatal(err)
	}
	if builds != 1 || client.invocations.Load() != 2 {
		t.Fatalf("calls = builds:%d inference:%d, want 1,2", builds, client.invocations.Load())
	}
	if string(got.Output) != `{"summary":"allow"}` ||
		!reflect.DeepEqual(got.Usage, &content.Usage{InputTokens: 8, OutputTokens: 6}) {
		t.Fatalf("result = %#v", got)
	}
	if len(requests) != 2 || len(requests[0].Messages) != 1 || len(requests[1].Messages) != 3 {
		t.Fatalf("message counts = %d/%d, want 1/3", len(requests[0].Messages), len(requests[1].Messages))
	}
	assistant, ok := requests[1].Messages[1].(*content.AIMessage)
	if !ok || len(assistant.Blocks) != 1 {
		t.Fatalf("assistant evidence message = %#v", requests[1].Messages[1])
	}
	call, ok := assistant.Blocks[0].(*content.ToolUseBlock)
	if !ok || call.ID != "call-1" || call.Name != "workspace_read" {
		t.Fatalf("assistant call = %#v", assistant.Blocks[0])
	}
	result, ok := requests[1].Messages[2].(*content.ToolResultMessage)
	if !ok || result.ToolUseID != call.ID || result.Role != content.RoleTool ||
		len(result.Blocks) != 1 {
		t.Fatalf("paired result = %#v", requests[1].Messages[2])
	}
}

func TestEvidenceExecutionPreservesSequentialCallAndPrivatePairOrder(t *testing.T) {
	t.Parallel()
	sessionID, loopID := mustRuntimeTestID(t), mustRuntimeTestID(t)
	evidence := newPreparedEvidenceTool("workspace_read", "ok")
	evidence.result = tool.TextResult("safe")
	invocation := 0
	client := &runtimeTestClient{invoke: func(_ context.Context, request inference.Request) (*inference.Response, error) {
		invocation++
		if invocation == 1 {
			return &inference.Response{
				Message: &content.AIMessage{Message: content.Message{
					Role: content.RoleAssistant,
					Blocks: []content.Block{
						&content.ToolUseBlock{ID: "call-a", Name: "workspace_read", Input: json.RawMessage(`{"path":"a"}`)},
						&content.ToolUseBlock{ID: "call-b", Name: "workspace_read", Input: json.RawMessage(`{"path":"b"}`)},
					},
				}},
				FinishReason: stream.FinishReasonToolUse,
			}, nil
		}
		if len(request.Messages) != 4 {
			t.Fatalf("messages = %d, want user+assistant+two results", len(request.Messages))
		}
		for index, id := range []string{"call-a", "call-b"} {
			result, ok := request.Messages[index+2].(*content.ToolResultMessage)
			if !ok || result.ToolUseID != id {
				t.Fatalf("result[%d] = %#v, want pair %q", index, request.Messages[index+2], id)
			}
		}
		return terminalEvidenceResponse(`{"summary":"allow"}`, nil), nil
	}}
	definition := runtimeEvidenceDefinition(t, client, runtimeEvidenceModel(), func(context.Context, tool.EvidenceFactoryBindings) ([]tool.InvokableTool, error) {
		return []tool.InvokableTool{evidence}, nil
	}, hustle.ToolLoopLimits{
		MaxRounds: 2, MaxCalls: 2, MaxCallsPerRound: 2,
		MaxResultBytes: 1024, MaxEvidenceBytes: 2048,
	})
	controller := runtimeEvidenceController(t, sessionID, definition)
	if err := controller.RunAndFinalize(context.Background(), runtimeEvidenceRequest(t, definition.Name(), sessionID, loopID), acceptResult, noOpFinalizer); err != nil {
		t.Fatal(err)
	}
	evidence.mu.Lock()
	gotArgs := append([]string(nil), evidence.args...)
	evidence.mu.Unlock()
	wantArgs := []string{`{"path":"a"}`, `{"path":"a"}`, `{"path":"b"}`, `{"path":"b"}`}
	if !reflect.DeepEqual(gotArgs, wantArgs) {
		t.Fatalf("prepare/run order = %v, want %v", gotArgs, wantArgs)
	}
}

func TestEvidenceExecutionEnforcesRoundAndCallBoundsBeforeWork(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name           string
		limits         hustle.ToolLoopLimits
		calls          int
		evidenceRounds int
		wantReason     EvidenceFailureReason
		wantRuns       int
	}{
		{
			name: "one over rounds", calls: 1,
			limits:         hustle.ToolLoopLimits{MaxRounds: 1, MaxCalls: 1, MaxCallsPerRound: 1, MaxResultBytes: 1024, MaxEvidenceBytes: 2048},
			evidenceRounds: 1, wantReason: EvidenceFailureRoundsExceeded, wantRuns: 1,
		},
		{
			name: "one over total calls", calls: 1,
			limits:         hustle.ToolLoopLimits{MaxRounds: 3, MaxCalls: 1, MaxCallsPerRound: 1, MaxResultBytes: 1024, MaxEvidenceBytes: 2048},
			evidenceRounds: 2, wantReason: EvidenceFailureCallsExceeded, wantRuns: 1,
		},
		{
			name: "one over per-round calls", calls: 2,
			limits:         hustle.ToolLoopLimits{MaxRounds: 2, MaxCalls: 2, MaxCallsPerRound: 1, MaxResultBytes: 1024, MaxEvidenceBytes: 2048},
			evidenceRounds: 1, wantReason: EvidenceFailureCallsPerRoundExceeded,
		},
	}
	for _, testCase := range tests {
		testCase := testCase
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			sessionID, loopID := mustRuntimeTestID(t), mustRuntimeTestID(t)
			evidence := newPreparedEvidenceTool("workspace_read", "ok")
			evidence.result = tool.TextResult("safe")
			invocations := 0
			client := &runtimeTestClient{invoke: func(_ context.Context, request inference.Request) (*inference.Response, error) {
				invocations++
				if invocations > testCase.evidenceRounds {
					return terminalEvidenceResponse(`{"summary":"allow"}`, nil), nil
				}
				blocks := make([]content.Block, testCase.calls)
				for i := range blocks {
					input := json.RawMessage(`{"path":"file"}`)
					if testCase.wantReason == EvidenceFailureCallsPerRoundExceeded && i == len(blocks)-1 {
						// The call-count bound must win before argument parsing,
						// cloning, preparation, or execution.
						input = json.RawMessage(`{"malformed":"provider-secret"`)
					}
					blocks[i] = &content.ToolUseBlock{
						ID: "call-" + string(rune('a'+i)), Name: "workspace_read",
						Input: input,
					}
				}
				return &inference.Response{
					Message:      &content.AIMessage{Message: content.Message{Role: content.RoleAssistant, Blocks: blocks}},
					FinishReason: stream.FinishReasonToolUse,
				}, nil
			}}
			definition := runtimeEvidenceDefinition(t, client, runtimeEvidenceModel(), func(context.Context, tool.EvidenceFactoryBindings) ([]tool.InvokableTool, error) {
				return []tool.InvokableTool{evidence}, nil
			}, testCase.limits)
			controller := runtimeEvidenceController(t, sessionID, definition)
			err := controller.RunAndFinalize(context.Background(), runtimeEvidenceRequest(t, definition.Name(), sessionID, loopID), acceptResult, noOpFinalizer)
			var evidenceErr *EvidenceError
			if !errors.As(err, &evidenceErr) || evidenceErr.Reason != testCase.wantReason {
				t.Fatalf("error = %T %v, want evidence reason %q", err, err, testCase.wantReason)
			}
			_, runs := evidence.counts()
			if runs != testCase.wantRuns {
				t.Fatalf("tool runs = %d, want %d", runs, testCase.wantRuns)
			}
		})
	}
}

func TestEvidenceExecutionEnforcesExactResultAndAggregateByteBounds(t *testing.T) {
	t.Parallel()
	result := tool.TextResult("safe")
	encoded, err := content.MarshalBlocks(result.Content)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name        string
		resultLimit int
		totalLimit  int
		rounds      int
		wantReason  EvidenceFailureReason
	}{
		{name: "exact result and aggregate", resultLimit: len(encoded), totalLimit: len(encoded), rounds: 1},
		{name: "result one over", resultLimit: len(encoded) - 1, totalLimit: len(encoded) * 2, rounds: 1, wantReason: EvidenceFailureResultTooLarge},
		{name: "aggregate exact", resultLimit: len(encoded), totalLimit: len(encoded) * 2, rounds: 2},
		{name: "aggregate one over", resultLimit: len(encoded), totalLimit: len(encoded)*2 - 1, rounds: 2, wantReason: EvidenceFailureEvidenceTooLarge},
	}
	for _, testCase := range tests {
		testCase := testCase
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			sessionID, loopID := mustRuntimeTestID(t), mustRuntimeTestID(t)
			evidence := newPreparedEvidenceTool("workspace_read", "ok")
			evidence.result = result
			invocation := 0
			client := &runtimeTestClient{invoke: func(context.Context, inference.Request) (*inference.Response, error) {
				invocation++
				if invocation <= testCase.rounds {
					return oneEvidenceCallResponse("call-" + string(rune('a'+invocation))), nil
				}
				return terminalEvidenceResponse(`{"summary":"allow"}`, nil), nil
			}}
			limits := hustle.ToolLoopLimits{
				MaxRounds: testCase.rounds + 1, MaxCalls: testCase.rounds, MaxCallsPerRound: 1,
				MaxResultBytes: testCase.resultLimit, MaxEvidenceBytes: testCase.totalLimit,
			}
			definition := runtimeEvidenceDefinition(t, client, runtimeEvidenceModel(), func(context.Context, tool.EvidenceFactoryBindings) ([]tool.InvokableTool, error) {
				return []tool.InvokableTool{evidence}, nil
			}, limits)
			controller := runtimeEvidenceController(t, sessionID, definition)
			err := controller.RunAndFinalize(context.Background(), runtimeEvidenceRequest(t, definition.Name(), sessionID, loopID), acceptResult, noOpFinalizer)
			if testCase.wantReason == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			var evidenceErr *EvidenceError
			if !errors.As(err, &evidenceErr) || evidenceErr.Reason != testCase.wantReason {
				t.Fatalf("error = %T %v, want evidence reason %q", err, err, testCase.wantReason)
			}
		})
	}
}

func TestEvidenceExecutionRejectsMissingModelCapabilitiesBeforeFactoryOrInference(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		model model.Model
	}{
		{name: "tools", model: func() model.Model {
			candidate := runtimeStructuredTestModel()
			return candidate
		}()},
		{name: "structured output", model: func() model.Model {
			candidate := runtimeTestModel()
			candidate.Caps.Tools = true
			return candidate
		}()},
		{name: "structured output with tools", model: func() model.Model {
			candidate := runtimeStructuredTestModel()
			candidate.Caps.Tools = true
			return candidate
		}()},
	}
	for _, testCase := range tests {
		testCase := testCase
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			sessionID, loopID := mustRuntimeTestID(t), mustRuntimeTestID(t)
			client := successfulRuntimeClient(nil)
			builds := 0
			definition := runtimeEvidenceDefinition(t, client, testCase.model, func(context.Context, tool.EvidenceFactoryBindings) ([]tool.InvokableTool, error) {
				builds++
				return []tool.InvokableTool{newPreparedEvidenceTool("workspace_read", "ok")}, nil
			}, hustle.ToolLoopLimits{
				MaxRounds: 2, MaxCalls: 1, MaxCallsPerRound: 1,
				MaxResultBytes: 1024, MaxEvidenceBytes: 2048,
			})
			controller := runtimeEvidenceController(t, sessionID, definition)
			err := controller.RunAndFinalize(context.Background(), runtimeEvidenceRequest(t, definition.Name(), sessionID, loopID), acceptResult, noOpFinalizer)
			var unsupported *inference.StructuredOutputWithToolsUnsupportedError
			if !errors.As(err, &unsupported) {
				t.Fatalf("error = %T %v, want capability error", err, err)
			}
			if builds != 0 || client.invocations.Load() != 0 {
				t.Fatalf("work = builds:%d invokes:%d, want 0,0", builds, client.invocations.Load())
			}
		})
	}
}

func TestEvidenceExecutionUsageOverflowRetainsPriorKnownUsage(t *testing.T) {
	t.Parallel()
	sessionID, loopID := mustRuntimeTestID(t), mustRuntimeTestID(t)
	evidence := newPreparedEvidenceTool("workspace_read", "ok")
	evidence.result = tool.TextResult("safe")
	invocation := 0
	firstUsage := &content.Usage{InputTokens: ^content.TokenCount(0), OutputTokens: 1}
	client := &runtimeTestClient{invoke: func(context.Context, inference.Request) (*inference.Response, error) {
		invocation++
		if invocation == 1 {
			response := oneEvidenceCallResponse("call-a")
			response.Usage = firstUsage
			return response, nil
		}
		return terminalEvidenceResponse(`{"summary":"allow"}`, &content.Usage{InputTokens: 1, OutputTokens: 1}), nil
	}}
	definition := runtimeEvidenceDefinition(t, client, runtimeEvidenceModel(), func(context.Context, tool.EvidenceFactoryBindings) ([]tool.InvokableTool, error) {
		return []tool.InvokableTool{evidence}, nil
	}, hustle.ToolLoopLimits{
		MaxRounds: 2, MaxCalls: 1, MaxCallsPerRound: 1,
		MaxResultBytes: 1024, MaxEvidenceBytes: 2048,
	})
	audit := &runtimeTestAudit{}
	controller := runtimeEvidenceControllerWith(t, sessionID, definition, audit, time.Second, time.Second)
	err := controller.RunAndFinalize(context.Background(), runtimeEvidenceRequest(t, definition.Name(), sessionID, loopID), acceptResult, noOpFinalizer)
	var overflow *content.UsageOverflowError
	if !errors.As(err, &overflow) {
		t.Fatalf("error = %T %v, want usage overflow", err, err)
	}
	events := audit.snapshot()
	if len(events) != 2 {
		t.Fatalf("events = %d, want started+failed", len(events))
	}
	failed, ok := events[1].(event.HustleFailed)
	if !ok || !reflect.DeepEqual(failed.Usage, firstUsage) || failed.Usage == firstUsage {
		t.Fatalf("failed usage = %#v, want owned prior usage %#v", failed.Usage, firstUsage)
	}
}

func TestEvidenceExecutionTimeoutPoisonsControllerWhenToolIgnoresCancellation(t *testing.T) {
	t.Parallel()
	sessionID, loopID := mustRuntimeTestID(t), mustRuntimeTestID(t)
	blocking := &blockingEvidenceTool{
		preparedEvidenceTool: newPreparedEvidenceTool("workspace_read", "ok"),
		started:              make(chan struct{}),
		release:              make(chan struct{}),
	}
	client := &runtimeTestClient{invoke: func(context.Context, inference.Request) (*inference.Response, error) {
		return oneEvidenceCallResponse("call-a"), nil
	}}
	definition := runtimeEvidenceDefinitionWithTimeout(t, client, runtimeEvidenceModel(), func(context.Context, tool.EvidenceFactoryBindings) ([]tool.InvokableTool, error) {
		return []tool.InvokableTool{blocking}, nil
	}, hustle.ToolLoopLimits{
		MaxRounds: 2, MaxCalls: 1, MaxCallsPerRound: 1,
		MaxResultBytes: 1024, MaxEvidenceBytes: 2048,
	}, 20*time.Millisecond)
	controller := runtimeEvidenceControllerWith(t, sessionID, definition, &runtimeTestAudit{}, time.Second, 10*time.Millisecond)

	err := controller.RunAndFinalize(context.Background(), runtimeEvidenceRequest(t, definition.Name(), sessionID, loopID), acceptResult, noOpFinalizer)
	var runErr *RunError
	if !errors.As(err, &runErr) || runErr.ReasonCode != hustle.ReasonTimeout {
		t.Fatalf("error = %T %v, want run timeout", err, err)
	}
	select {
	case <-blocking.started:
	default:
		t.Fatal("blocking tool did not start")
	}
	secondErr := controller.RunAndFinalize(context.Background(), runtimeEvidenceRequest(t, definition.Name(), sessionID, loopID), acceptResult, noOpFinalizer)
	var admission *AdmissionError
	if !errors.As(secondErr, &admission) || admission.Reason != AdmissionPoisoned {
		t.Fatalf("second error = %T %v, want poisoned admission", secondErr, secondErr)
	}
	close(blocking.release)
}

func TestEvidenceExecutionTimeoutPoisonsControllerWhenInferenceIgnoresCancellation(t *testing.T) {
	t.Parallel()
	sessionID, loopID := mustRuntimeTestID(t), mustRuntimeTestID(t)
	started, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	client := &runtimeTestClient{invoke: func(context.Context, inference.Request) (*inference.Response, error) {
		once.Do(func() { close(started) })
		<-release
		return terminalEvidenceResponse(`{"summary":"late"}`, nil), nil
	}}
	definition := runtimeEvidenceDefinitionWithTimeout(t, client, runtimeEvidenceModel(), func(context.Context, tool.EvidenceFactoryBindings) ([]tool.InvokableTool, error) {
		return []tool.InvokableTool{newPreparedEvidenceTool("workspace_read", "ok")}, nil
	}, hustle.ToolLoopLimits{
		MaxRounds: 2, MaxCalls: 1, MaxCallsPerRound: 1,
		MaxResultBytes: 1024, MaxEvidenceBytes: 2048,
	}, 20*time.Millisecond)
	controller := runtimeEvidenceControllerWith(t, sessionID, definition, &runtimeTestAudit{}, time.Second, 10*time.Millisecond)

	err := controller.RunAndFinalize(context.Background(), runtimeEvidenceRequest(t, definition.Name(), sessionID, loopID), acceptResult, noOpFinalizer)
	var runErr *RunError
	if !errors.As(err, &runErr) || runErr.ReasonCode != hustle.ReasonTimeout {
		t.Fatalf("error = %T %v, want run timeout", err, err)
	}
	select {
	case <-started:
	default:
		t.Fatal("blocking inference did not start")
	}
	secondErr := controller.RunAndFinalize(context.Background(), runtimeEvidenceRequest(t, definition.Name(), sessionID, loopID), acceptResult, noOpFinalizer)
	var admission *AdmissionError
	if !errors.As(secondErr, &admission) || admission.Reason != AdmissionPoisoned {
		t.Fatalf("second error = %T %v, want poisoned admission", secondErr, secondErr)
	}
	close(release)
}

func TestEvidenceExecutionCallerCancellationIsHonored(t *testing.T) {
	t.Parallel()
	sessionID, loopID := mustRuntimeTestID(t), mustRuntimeTestID(t)
	cancelAware := &cancelAwareEvidenceTool{
		preparedEvidenceTool: newPreparedEvidenceTool("workspace_read", "ok"),
		started:              make(chan struct{}),
	}
	client := &runtimeTestClient{invoke: func(context.Context, inference.Request) (*inference.Response, error) {
		return oneEvidenceCallResponse("call-a"), nil
	}}
	definition := runtimeEvidenceDefinition(t, client, runtimeEvidenceModel(), func(context.Context, tool.EvidenceFactoryBindings) ([]tool.InvokableTool, error) {
		return []tool.InvokableTool{cancelAware}, nil
	}, hustle.ToolLoopLimits{
		MaxRounds: 2, MaxCalls: 1, MaxCallsPerRound: 1,
		MaxResultBytes: 1024, MaxEvidenceBytes: 2048,
	})
	controller := runtimeEvidenceController(t, sessionID, definition)
	ctx, cancel := context.WithCancel(context.Background())
	errs := make(chan error, 1)
	go func() {
		errs <- controller.RunAndFinalize(ctx, runtimeEvidenceRequest(t, definition.Name(), sessionID, loopID), acceptResult, noOpFinalizer)
	}()
	<-cancelAware.started
	cancel()
	err := <-errs
	var runErr *RunError
	if !errors.As(err, &runErr) || runErr.ReasonCode != hustle.ReasonCanceled {
		t.Fatalf("error = %T %v, want canceled run", err, err)
	}
}

func TestEvidenceExecutionOwnsProviderRequestsResponsesAndToolResults(t *testing.T) {
	t.Parallel()
	sessionID, loopID := mustRuntimeTestID(t), mustRuntimeTestID(t)
	evidence := newPreparedEvidenceTool("workspace_read", "ok")
	evidence.result = tool.TextResult("safe")
	var firstResponse *inference.Response
	invocation := 0
	client := &runtimeTestClient{invoke: func(_ context.Context, request inference.Request) (*inference.Response, error) {
		invocation++
		if invocation == 1 {
			request.Messages[0].(*content.UserMessage).Blocks[0].(*content.TextBlock).Text = "mutated-input"
			request.Tools[0].Schema[0] = '['
			request.Output.Schema[0] = '['
			firstResponse = oneEvidenceCallResponse("call-a")
			return firstResponse, nil
		}
		firstResponse.Message.Blocks[0].(*content.ToolUseBlock).ID = "mutated-call"
		evidence.result.Content[0].(*content.TextBlock).Text = "mutated-result"
		user := request.Messages[0].(*content.UserMessage).Blocks[0].(*content.TextBlock)
		call := request.Messages[1].(*content.AIMessage).Blocks[0].(*content.ToolUseBlock)
		result := request.Messages[2].(*content.ToolResultMessage)
		text := result.Blocks[0].(*content.TextBlock)
		if user.Text != `{"version":1}` || call.ID != "call-a" || result.ToolUseID != "call-a" ||
			text.Text != "safe" || request.Tools[0].Schema[0] != '{' || request.Output.Schema[0] != '{' {
			t.Fatalf("private basis aliased provider/tool memory: user=%q call=%q pair=%q result=%q", user.Text, call.ID, result.ToolUseID, text.Text)
		}
		return terminalEvidenceResponse(`{"summary":"allow"}`, nil), nil
	}}
	definition := runtimeEvidenceDefinition(t, client, runtimeEvidenceModel(), func(context.Context, tool.EvidenceFactoryBindings) ([]tool.InvokableTool, error) {
		return []tool.InvokableTool{evidence}, nil
	}, hustle.ToolLoopLimits{
		MaxRounds: 2, MaxCalls: 1, MaxCallsPerRound: 1,
		MaxResultBytes: 1024, MaxEvidenceBytes: 2048,
	})
	controller := runtimeEvidenceController(t, sessionID, definition)
	if err := controller.RunAndFinalize(context.Background(), runtimeEvidenceRequest(t, definition.Name(), sessionID, loopID), acceptResult, noOpFinalizer); err != nil {
		t.Fatal(err)
	}
}

func TestEvidenceExecutionValidatorFailurePublishesNoIntermediateEvidence(t *testing.T) {
	t.Parallel()
	sessionID, loopID := mustRuntimeTestID(t), mustRuntimeTestID(t)
	evidence := newPreparedEvidenceTool("workspace_read", "ok")
	evidence.result = tool.TextResult("safe")
	invocation := 0
	client := &runtimeTestClient{invoke: func(context.Context, inference.Request) (*inference.Response, error) {
		invocation++
		if invocation == 1 {
			return oneEvidenceCallResponse("call-a"), nil
		}
		return terminalEvidenceResponse(`{"summary":"deny"}`, nil), nil
	}}
	definition := runtimeEvidenceDefinition(t, client, runtimeEvidenceModel(), func(context.Context, tool.EvidenceFactoryBindings) ([]tool.InvokableTool, error) {
		return []tool.InvokableTool{evidence}, nil
	}, hustle.ToolLoopLimits{
		MaxRounds: 2, MaxCalls: 1, MaxCallsPerRound: 1,
		MaxResultBytes: 1024, MaxEvidenceBytes: 2048,
	})
	audit := &runtimeTestAudit{}
	controller := runtimeEvidenceControllerWith(t, sessionID, definition, audit, time.Second, time.Second)
	validationErr := errors.New("domain rejected")
	err := controller.RunAndFinalize(context.Background(), runtimeEvidenceRequest(t, definition.Name(), sessionID, loopID), func(context.Context, hustle.Result) error {
		return validationErr
	}, noOpFinalizer)
	if !errors.Is(err, validationErr) {
		t.Fatalf("error = %T %v, want validator failure", err, err)
	}
	events := audit.snapshot()
	if len(events) != 2 {
		t.Fatalf("events = %d, want only started+failed", len(events))
	}
	if _, ok := events[0].(event.HustleStarted); !ok {
		t.Fatalf("event[0] = %T, want started", events[0])
	}
	if _, ok := events[1].(event.HustleFailed); !ok {
		t.Fatalf("event[1] = %T, want failed", events[1])
	}
}

type blockingEvidenceTool struct {
	*preparedEvidenceTool
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

type cancelAwareEvidenceTool struct {
	*preparedEvidenceTool
	started chan struct{}
	once    sync.Once
}

func (t *cancelAwareEvidenceTool) InvokableRun(ctx context.Context, _ string) (*tool.ToolResult, error) {
	t.once.Do(func() { close(t.started) })
	<-ctx.Done()
	return nil, ctx.Err()
}

func (t *blockingEvidenceTool) InvokableRun(context.Context, string) (*tool.ToolResult, error) {
	t.once.Do(func() { close(t.started) })
	<-t.release
	return tool.TextResult("late"), nil
}

func acceptResult(context.Context, hustle.Result) error { return nil }

func oneEvidenceCallResponse(id string) *inference.Response {
	return &inference.Response{
		Message: &content.AIMessage{Message: content.Message{
			Role: content.RoleAssistant,
			Blocks: []content.Block{&content.ToolUseBlock{
				ID: id, Name: "workspace_read", Input: json.RawMessage(`{"path":"file"}`),
			}},
		}},
		FinishReason: stream.FinishReasonToolUse,
	}
}

func terminalEvidenceResponse(output string, usage *content.Usage) *inference.Response {
	return &inference.Response{
		Message: &content.AIMessage{Message: content.Message{
			Role: content.RoleAssistant, Blocks: []content.Block{&content.TextBlock{Text: output}},
		}},
		Usage: usage, FinishReason: stream.FinishReasonStop,
	}
}

func runtimeEvidenceModel() model.Model {
	candidate := runtimeStructuredTestModel()
	candidate.Caps.Tools = true
	candidate.Caps.StructuredOutputWithTools = true
	return candidate
}

func runtimeEvidenceDefinition(
	t *testing.T,
	client inference.Client,
	candidate model.Model,
	factory tool.EvidenceFactory,
	limits hustle.ToolLoopLimits,
) hustle.BoundDefinition {
	t.Helper()
	return runtimeEvidenceDefinitionWithTimeout(t, client, candidate, factory, limits, time.Second)
}

func runtimeEvidenceDefinitionWithTimeout(
	t *testing.T,
	client inference.Client,
	candidate model.Model,
	factory tool.EvidenceFactory,
	limits hustle.ToolLoopLimits,
	timeout time.Duration,
) hustle.BoundDefinition {
	t.Helper()
	info := evidenceToolInfo("workspace_read")
	definition, err := hustle.Define(
		hustle.WithName("test.evidence-runtime"),
		hustle.WithParticipation(hustle.ParticipationBlocking),
		hustle.WithTimeout(timeout),
		hustle.WithLimits(hustle.Limits{InputBytes: 1024, OutputBytes: 1024}),
		hustle.WithSystemPrompt("Review only.", "prompt-v1"),
		hustle.WithPolicyRevision("policy-v1"),
		hustle.WithNamedInference(client, candidate),
		hustle.WithOutputSchema(runtimeTestOutputSchema()),
		hustle.WithEvidenceTools(hustle.EvidenceToolPolicy{
			Revision: "evidence-v1", Limits: limits,
			Definitions: []tool.Definition{tool.NewEvidenceDefinition(
				"workspace-read", tool.RequiresWorkspaceRead, []tool.ToolInfo{info}, factory,
			)},
		}),
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

func runtimeEvidenceController(t *testing.T, sessionID uuid.UUID, definition hustle.BoundDefinition) *Controller {
	t.Helper()
	return runtimeEvidenceControllerWith(t, sessionID, definition, &runtimeTestAudit{}, time.Second, time.Second)
}

func runtimeEvidenceControllerWith(
	t *testing.T,
	sessionID uuid.UUID,
	definition hustle.BoundDefinition,
	audit *runtimeTestAudit,
	timeout time.Duration,
	workerDrain time.Duration,
) *Controller {
	t.Helper()
	factory := event.NewFactory(uuid.New, func() time.Time { return time.Unix(123, 0).UTC() })
	controller, err := New(context.Background(), Config{
		Blocking: LaneLimits{Concurrent: 1, Queued: 2}, Background: LaneLimits{Concurrent: 1, Queued: 2},
		Runtime: &RuntimeConfig{
			SessionID: sessionID, Definitions: []hustle.BoundDefinition{definition},
			AuditTimeout: timeout, FinalizationTimeout: time.Second, WorkerDrainTimeout: workerDrain,
			Stamper: factory, Audit: audit, Faults: &runtimeTestFaults{}, Activity: &runtimeTestActivity{},
			Evidence: &EvidenceRuntimeConfig{
				Access:         &evidenceAccessStub{access: gate.AccessAllow},
				AllowedKinds:   []string{evidenceReadKind},
				ReadWorkspace:  &tool.ReadWorkspaceBinding{Root: "/workspace"},
				NewExecutionID: uuid.New,
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return controller
}

func runtimeEvidenceRequest(t *testing.T, name hustle.Name, sessionID, loopID uuid.UUID) hustle.Request {
	t.Helper()
	return hustle.Request{
		Name: name,
		Cause: identity.Cause{
			Coordinates: identity.Coordinates{SessionID: sessionID, LoopID: loopID},
			CommandID:   mustRuntimeTestID(t),
		},
		Input: json.RawMessage(`{"version":1}`),
	}
}
