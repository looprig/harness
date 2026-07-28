package loopruntime

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/looprig/core/content"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/command"
	"github.com/looprig/harness/pkg/event"
	gatedomain "github.com/looprig/harness/pkg/gate"
	"github.com/looprig/harness/pkg/hook"
	"github.com/looprig/harness/pkg/identity"
	"github.com/looprig/harness/pkg/tool"
)

func TestGateHooks_RequestUserInputCoversOnlyReplyWaitAndCapturesAnswer(t *testing.T) {
	t.Parallel()
	callID := newCallID(t)
	gateID := newCallID(t)
	gateReg := make(chan gateRegistration)
	emitCh := make(chan event.Event, 1)
	began := make(chan hook.Call, 1)
	finished := make(chan hook.Result, 1)
	hooks := compileRuntimeHooks(t, hook.Set{Around: []hook.Around{{
		Operation: hook.OperationGateWait,
		Begin: func(ctx context.Context, call hook.Call) (context.Context, hook.FinishFunc) {
			began <- call
			return ctx, func(result hook.Result) { finished <- result }
		},
	}}})
	ctx := injectedCtx(t, callID, gateReg, func(ev event.Event) { emitCh <- ev })
	ctx = withOperationHookRuntime(ctx, operationHookRuntime{
		hooks:       hooks,
		coordinates: identity.Coordinates{StepID: uuid.UUID{0x31}},
		agentName:   "gate-test",
		cause:       identity.Cause{CommandID: uuid.UUID{0x32}},
	})

	done := make(chan error, 1)
	go func() {
		answer, err := RequestUserInput(ctx, "color?", []string{"blue"})
		if err == nil && answer != "blue" {
			err = errors.New("unexpected answer")
		}
		done <- err
	}()

	reg := <-gateReg
	select {
	case <-began:
		t.Fatal("GateWait began during registration, before durable ack and public emit")
	default:
	}
	reg.ack <- gateInstallAck{gateID: gateID}
	<-emitCh
	var started hook.Call
	select {
	case started = <-began:
	case <-time.After(2 * time.Second):
		t.Fatal("GateWait did not begin after ack and emit")
	}
	if started.GateWait.GateID != gateID ||
		started.GateWait.Kind != gatedomain.KindAskUser ||
		started.GateWait.Resolver != gatedomain.ResolverLoop ||
		started.GateWait.Blocks != gatedomain.BlocksToolCall ||
		started.GateWait.Effect != gatedomain.EffectResume {
		t.Fatalf("GateWait start = %+v", started.GateWait)
	}
	select {
	case <-finished:
		t.Fatal("GateWait finished before reply")
	default:
	}
	reg.reply <- command.ProvideUserInput{
		Header:    command.Header{Agency: identity.AgencyUser},
		GateRoute: command.GateRoute{GateID: gateID, ToolExecutionID: callID},
		Answer:    "blue",
	}
	if err := <-done; err != nil {
		t.Fatalf("RequestUserInput: %v", err)
	}
	got := <-finished
	if got.Outcome != hook.OutcomeCompleted || got.GateWait.Answer == nil {
		t.Fatalf("GateWait finish = %+v", got)
	}
	if got.GateWait.Answer.GateID != gateID ||
		got.GateWait.Answer.Values["answer"] != "blue" ||
		got.GateWait.Answer.Source.Kind != gatedomain.ResponseFromUser {
		t.Fatalf("GateWait answer = %+v", got.GateWait.Answer)
	}
}

func TestGateHooks_RequestUserInputUsesDerivedContextOnlyForWait(t *testing.T) {
	t.Parallel()
	callID := newCallID(t)
	gateID := newCallID(t)
	gateReg := make(chan gateRegistration)
	emitCh := make(chan event.Event, 1)
	finished := make(chan hook.Result, 1)
	hooks := compileRuntimeHooks(t, hook.Set{Around: []hook.Around{{
		Operation: hook.OperationGateWait,
		Begin: func(ctx context.Context, _ hook.Call) (context.Context, hook.FinishFunc) {
			waitCtx, cancel := context.WithCancel(ctx)
			cancel()
			return waitCtx, func(result hook.Result) { finished <- result }
		},
	}}})
	ctx := injectedCtx(t, callID, gateReg, func(ev event.Event) { emitCh <- ev })
	ctx = withOperationHookRuntime(ctx, operationHookRuntime{hooks: hooks})

	done := make(chan error, 1)
	go func() {
		_, err := RequestUserInput(ctx, "q", nil)
		done <- err
	}()
	reg := <-gateReg
	reg.ack <- gateInstallAck{gateID: gateID}
	<-emitCh
	var abandoned gateRegistration
	select {
	case abandoned = <-gateReg:
	case err := <-done:
		t.Fatalf("RequestUserInput returned %v before unregistering the canceled gate", err)
	case <-time.After(2 * time.Second):
		t.Fatal("RequestUserInput did not unregister the hook-canceled gate")
	}
	if abandoned.abandonID != gateID {
		t.Fatalf("abandoned gate = %v, want %v", abandoned.abandonID, gateID)
	}
	abandoned.ack <- gateInstallAck{gateID: gateID}
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("RequestUserInput error = %v, want hook-derived cancellation", err)
	}
	if got := <-finished; got.Outcome != hook.OutcomeCanceled {
		t.Fatalf("GateWait outcome = %v, want canceled", got.Outcome)
	}
}

func TestGateHooks_PermissionWaitCapturesApproval(t *testing.T) {
	t.Parallel()
	base := &fakeRunTool{name: "T", output: "ok"}
	base.prepareFn = func(executionID uuid.UUID, _ string) (tool.Request, tool.PreparedArtifact, error) {
		return commandRequest(executionID, "git status", false), nil, nil
	}
	finished := make(chan hook.Result, 1)
	hooks := compileRuntimeHooks(t, hook.Set{Around: []hook.Around{{
		Operation: hook.OperationGateWait,
		Begin: func(ctx context.Context, call hook.Call) (context.Context, hook.FinishFunc) {
			if call.GateWait.Kind != gatedomain.KindPermission {
				t.Errorf("kind = %q, want permission", call.GateWait.Kind)
			}
			return ctx, func(result hook.Result) { finished <- result }
		},
	}}})
	gateReg := make(chan gateRegistration)
	gateID := newCallID(t)
	go func() {
		reg := <-gateReg
		reg.ack <- gateInstallAck{gateID: gateID}
		reg.reply <- command.ApproveToolCall{
			Header:    command.Header{Agency: identity.AgencyUser},
			GateRoute: command.GateRoute{GateID: gateID, ToolExecutionID: reg.callID},
			Action:    gatedomain.ApprovalApprove,
		}
	}()
	runtime := hookBatchRuntime(hooks, func(event.Event) {})
	runtime.GateRegistrations = gateReg
	tools := ToolSet{
		Access:   interactiveEvaluator(t, gatedomain.AccessGated, &recordingRuleWriter{}, &recordingIssuer{}),
		Registry: []tool.InvokableTool{base},
	}

	results := RunBatch(context.Background(), []content.ToolUseBlock{
		{ID: "use", Name: "T", Input: []byte(`{}`)},
	}, tools, runtime)

	if len(results) != 1 || results[0].IsError {
		t.Fatalf("results = %+v, want success", results)
	}
	got := <-finished
	if got.Outcome != hook.OutcomeCompleted ||
		got.GateWait.GateID != gateID ||
		got.GateWait.Answer == nil ||
		got.GateWait.Answer.Action != string(gatedomain.ApprovalApprove) ||
		got.GateWait.Answer.Source.Kind != gatedomain.ResponseFromUser {
		t.Fatalf("permission GateWait finish = %+v", got)
	}
}

func TestGateHooks_PermissionWaitDerivedCancellationUnregistersGate(t *testing.T) {
	t.Parallel()
	base := &fakeRunTool{name: "T", output: "must not run"}
	base.prepareFn = func(executionID uuid.UUID, _ string) (tool.Request, tool.PreparedArtifact, error) {
		return commandRequest(executionID, "git status", false), nil, nil
	}
	finished := make(chan hook.Result, 1)
	hooks := compileRuntimeHooks(t, hook.Set{Around: []hook.Around{{
		Operation: hook.OperationGateWait,
		Begin: func(ctx context.Context, _ hook.Call) (context.Context, hook.FinishFunc) {
			waitCtx, cancel := context.WithCancel(ctx)
			cancel()
			return waitCtx, func(result hook.Result) { finished <- result }
		},
	}}})
	gateReg := make(chan gateRegistration)
	gateID := newCallID(t)
	abandoned := make(chan gatedomain.ID, 1)
	go func() {
		reg := <-gateReg
		reg.ack <- gateInstallAck{gateID: gateID}
		closeRequest := <-gateReg
		abandoned <- closeRequest.abandonID
		closeRequest.ack <- gateInstallAck{gateID: closeRequest.abandonID}
	}()
	runtime := hookBatchRuntime(hooks, func(event.Event) {})
	runtime.GateRegistrations = gateReg
	tools := ToolSet{
		Access:   interactiveEvaluator(t, gatedomain.AccessGated, &recordingRuleWriter{}, &recordingIssuer{}),
		Registry: []tool.InvokableTool{base},
	}

	results := RunBatch(context.Background(), []content.ToolUseBlock{
		{ID: "use", Name: "T", Input: []byte(`{}`)},
	}, tools, runtime)

	if len(results) != 1 || !results[0].IsError {
		t.Fatalf("results = %+v, want canceled permission failure", results)
	}
	if got := <-abandoned; got != gateID {
		t.Fatalf("abandoned gate = %v, want %v", got, gateID)
	}
	if got := <-finished; got.Outcome != hook.OutcomeCanceled {
		t.Fatalf("GateWait outcome = %v, want canceled", got.Outcome)
	}
}

func TestGateHooks_DetachedWaitContextStillHonorsParentCancellation(t *testing.T) {
	t.Parallel()
	parent, cancel := context.WithCancel(context.Background())
	callID := newCallID(t)
	gateID := newCallID(t)
	gateReg := make(chan gateRegistration)
	emitted := make(chan event.Event, 1)
	finished := make(chan hook.Result, 1)
	hooks := compileRuntimeHooks(t, hook.Set{Around: []hook.Around{{
		Operation: hook.OperationGateWait,
		Begin: func(context.Context, hook.Call) (context.Context, hook.FinishFunc) {
			return context.Background(), func(result hook.Result) { finished <- result }
		},
	}}})
	ctx := withGateReg(withCallID(withEmit(parent, func(ev event.Event) { emitted <- ev }), callID), gateReg)
	ctx = withOperationHookRuntime(ctx, operationHookRuntime{hooks: hooks})

	done := make(chan error, 1)
	go func() {
		_, err := RequestUserInput(ctx, "q", nil)
		done <- err
	}()
	reg := <-gateReg
	reg.ack <- gateInstallAck{gateID: gateID}
	<-emitted
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("RequestUserInput error = %v, want canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("detached GateWait context ignored parent cancellation")
	}
	if got := <-finished; got.Outcome != hook.OutcomeCanceled {
		t.Fatalf("GateWait outcome = %v, want canceled", got.Outcome)
	}
}
