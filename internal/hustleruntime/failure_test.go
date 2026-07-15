package hustleruntime

import (
	"context"
	"errors"
	"reflect"
	"sync/atomic"
	"testing"

	"github.com/looprig/core/content"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/hustle"
	"github.com/looprig/inference"
)

type runtimeTestResolver struct {
	wantID  uuid.UUID
	binding hustle.InferenceBinding
	err     error
	calls   atomic.Int32
}

func (r *runtimeTestResolver) ResolveHustleModel(_ context.Context, loopID uuid.UUID) (hustle.InferenceBinding, error) {
	r.calls.Add(1)
	if loopID != r.wantID {
		return hustle.InferenceBinding{}, &runtimeUnexpectedLoopError{}
	}
	return r.binding, r.err
}

type runtimeUnexpectedLoopError struct{}

func (*runtimeUnexpectedLoopError) Error() string { return "unexpected loop id" }

type runtimeFailureCause struct{ label string }

func (e *runtimeFailureCause) Error() string { return e.label }

func TestRunAndFinalizeMapsExecutionFailuresAndAuditsBeforeFinalizer(t *testing.T) {
	t.Parallel()
	usage := &content.Usage{InputTokens: 13, OutputTokens: 5, ReasoningTokens: 1}
	resolveCause := &runtimeFailureCause{label: "resolve failed"}
	invokeCause := &runtimeFailureCause{label: "invoke failed"}
	validateCause := &runtimeFailureCause{label: "domain validation failed"}
	tests := []struct {
		name           string
		currentModel   bool
		resolveErr     error
		response       *inference.Response
		invokeErr      error
		validateErr    error
		wantStage      hustle.Stage
		wantReason     hustle.ReasonCode
		wantCause      error
		wantInvokes    int32
		wantUsage      *content.Usage
		wantRuntime    bool
		wantValidation int32
	}{
		{name: "current-loop model resolution", currentModel: true, resolveErr: resolveCause, wantStage: hustle.StageModelResolution, wantReason: hustle.ReasonModelResolution, wantCause: resolveCause},
		{name: "inference invoke", response: &inference.Response{Usage: usage}, invokeErr: invokeCause, wantStage: hustle.StageInference, wantReason: hustle.ReasonInference, wantCause: invokeCause, wantInvokes: 1, wantUsage: usage, wantRuntime: true},
		{name: "generic output", response: runtimeResponse(`{"broken"`, usage), wantStage: hustle.StageOutput, wantReason: hustle.ReasonInvalidOutput, wantInvokes: 1, wantUsage: usage, wantRuntime: true},
		{name: "domain validation", response: runtimeResponse(`{"ok":true}`, usage), validateErr: validateCause, wantStage: hustle.StageOutput, wantReason: hustle.ReasonInvalidOutput, wantCause: validateCause, wantInvokes: 1, wantUsage: usage, wantRuntime: true, wantValidation: 1},
	}
	for _, tt := range tests {
		testCase := tt
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			request := runtimeRequest(t, "test.failure")
			model := runtimeTestModel()
			client := &runtimeTestClient{invoke: func(context.Context, inference.Request) (*inference.Response, error) {
				return testCase.response, testCase.invokeErr
			}}
			var resolver *runtimeTestResolver
			var definition hustle.BoundDefinition
			if testCase.currentModel {
				resolver = &runtimeTestResolver{wantID: request.Cause.LoopID, binding: hustle.InferenceBinding{Client: client, Model: model}, err: testCase.resolveErr}
				definition = runtimeTestBoundDefinition(t, "test.failure", hustle.ParticipationBlocking, nil, hustle.ModelSourceCurrentLoop, resolver)
			} else {
				definition = runtimeTestBoundDefinition(t, "test.failure", hustle.ParticipationBlocking, client, hustle.ModelSourceNamed, nil)
			}
			audit := &runtimeTestAudit{}
			controller := runtimeTestController(t, definition, audit, &runtimeTestFaults{}, &runtimeTestActivity{})
			var validations atomic.Int32
			var finalizers atomic.Int32
			var finalOutcome hustle.Outcome
			err := controller.RunAndFinalize(context.Background(), request, func(context.Context, hustle.Result) error {
				validations.Add(1)
				return testCase.validateErr
			}, func(_ context.Context, outcome hustle.Outcome) error {
				finalizers.Add(1)
				finalOutcome = outcome
				if events := audit.snapshot(); len(events) != 2 {
					t.Errorf("audit count at finalizer = %d, want terminal already committed", len(events))
				}
				return nil
			})
			var runErr *RunError
			if !errors.As(err, &runErr) || runErr.Stage != testCase.wantStage || runErr.ReasonCode != testCase.wantReason {
				t.Fatalf("error = %T %v, want RunError stage=%v reason=%v", err, err, testCase.wantStage, testCase.wantReason)
			}
			if testCase.wantCause != nil && !errors.Is(err, testCase.wantCause) {
				t.Fatalf("error = %v, want cause %v", err, testCase.wantCause)
			}
			if finalizers.Load() != 1 || finalOutcome.Err != runErr || finalOutcome.Result != nil {
				t.Fatalf("finalizer = calls:%d outcome:%#v, want same RunError", finalizers.Load(), finalOutcome)
			}
			if client.invocations.Load() != testCase.wantInvokes || validations.Load() != testCase.wantValidation {
				t.Fatalf("calls = invoke:%d validate:%d, want %d,%d", client.invocations.Load(), validations.Load(), testCase.wantInvokes, testCase.wantValidation)
			}
			events := audit.snapshot()
			if len(events) != 2 {
				t.Fatalf("audit events = %#v, want Started,Failed", events)
			}
			failed, ok := events[1].(event.HustleFailed)
			if !ok || failed.Stage != testCase.wantStage || failed.ReasonCode != testCase.wantReason {
				t.Fatalf("terminal audit = %#v", events[1])
			}
			if !reflect.DeepEqual(failed.Usage, testCase.wantUsage) || (testCase.wantUsage != nil && failed.Usage == testCase.wantUsage) {
				t.Fatalf("failed usage = %#v, want copied %#v", failed.Usage, testCase.wantUsage)
			}
			if testCase.wantRuntime != (failed.Run.Runtime != (event.ModelRuntime{})) {
				t.Fatalf("failed runtime = %#v, want resolved=%v", failed.Run.Runtime, testCase.wantRuntime)
			}
			if resolver != nil && resolver.calls.Load() != 1 {
				t.Fatalf("resolver calls = %d, want 1", resolver.calls.Load())
			}
		})
	}
}

func runtimeResponse(output string, usage *content.Usage) *inference.Response {
	return &inference.Response{
		Message: &content.AIMessage{Message: content.Message{Role: content.RoleAssistant, Blocks: []content.Block{&content.TextBlock{Text: output}}}},
		Usage:   usage,
	}
}
