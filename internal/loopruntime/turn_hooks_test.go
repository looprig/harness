package loopruntime

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/looprig/core/content"
	"github.com/looprig/harness/pkg/command"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/hook"
)

type orderedTurnPublisher struct {
	recordingPublisher
	mu    sync.Mutex
	order []string
}

type hookBoundaryContextKey struct{}

type hookBoundaryObservation struct {
	marker any
	err    error
}

type hookContextBoundary struct {
	recordingPublisher
	mu       sync.Mutex
	contexts map[string]hookBoundaryObservation
}

func newHookContextBoundary() *hookContextBoundary {
	return &hookContextBoundary{contexts: make(map[string]hookBoundaryObservation)}
}

func (b *hookContextBoundary) record(name string, ctx context.Context) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.contexts[name] = hookBoundaryObservation{
		marker: ctx.Value(hookBoundaryContextKey{}),
		err:    ctx.Err(),
	}
}

func (b *hookContextBoundary) observed(name string) (hookBoundaryObservation, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	value, ok := b.contexts[name]
	return value, ok
}

func (b *hookContextBoundary) PublishEventChecked(ctx context.Context, value event.Event) error {
	if _, ok := value.(event.TurnStarted); ok {
		b.record("turn_started", ctx)
	}
	return b.recordingPublisher.PublishEventChecked(ctx, value)
}

func (b *hookContextBoundary) CommitContextBoundary(ctx context.Context, value event.Event) (bool, error) {
	if _, ok := value.(event.StepDone); ok {
		b.record("step_done", ctx)
	}
	return true, b.recordingPublisher.PublishEventChecked(ctx, value)
}

func (b *hookContextBoundary) CommitBoundary(ctx context.Context, value event.Event) error {
	if value.EndsTurn() {
		b.record("turn_terminal", ctx)
	}
	return b.recordingPublisher.PublishEventChecked(ctx, value)
}

func (p *orderedTurnPublisher) appendOrder(value string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.order = append(p.order, value)
}

func (p *orderedTurnPublisher) PublishEvent(ctx context.Context, value event.Event) error {
	if _, ok := value.(event.TurnRejected); ok {
		p.appendOrder("turn.rejected")
	}
	return p.recordingPublisher.PublishEvent(ctx, value)
}

func (p *orderedTurnPublisher) PublishEventChecked(ctx context.Context, value event.Event) error {
	switch value.(type) {
	case event.TurnStarted:
		p.appendOrder("turn.started")
	case event.TurnRejected:
		p.appendOrder("turn.rejected")
	case event.TurnDone, event.TurnFailed, event.TurnInterrupted:
		p.appendOrder("turn.terminal")
	}
	return p.recordingPublisher.PublishEventChecked(ctx, value)
}

func (p *orderedTurnPublisher) orderSnapshot() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.order...)
}

func TestTurnHooksBeginBeforeStartedAndFinishAfterDurableTerminal(t *testing.T) {
	t.Parallel()
	publisher := &orderedTurnPublisher{}
	var finishCount atomic.Int32
	runner, err := hook.Compile(hook.Set{Around: []hook.Around{{
		Operation: hook.OperationTurn,
		Begin: func(ctx context.Context, call hook.Call) (context.Context, hook.FinishFunc) {
			if call.Coordinates.TurnID.IsZero() || call.Turn.Index != 1 || call.Turn.Input == nil {
				t.Errorf("turn call = %#v, want frozen id/index/input", call)
			}
			publisher.appendOrder("turn.begin")
			return ctx, func(result hook.Result) {
				finishCount.Add(1)
				publisher.appendOrder("turn.finish")
				if result.Outcome != hook.OutcomeCompleted {
					t.Errorf("turn outcome = %v, want completed", result.Outcome)
				}
			}
		},
	}}})
	if err != nil {
		t.Fatalf("hook.Compile: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	instance, err := newWithConfig(ctx, mustID(t), mustID(t), Provenance{}, publisher, runtimeConfig{
		Client: &fakeLLM{chunks: []content.Chunk{textChunk("done")}}, Model: testModel(),
		Hooks: runner, DrainTimeout: 200 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("newWithConfig: %v", err)
	}
	inputID := mustID(t)
	instance.Commands <- command.UserInput{Header: command.Header{CommandID: inputID}, Blocks: []content.Block{&content.TextBlock{Text: "go"}}}
	blockUntilEvents(t, &publisher.recordingPublisher, func(values []event.Event) bool {
		for _, value := range values {
			if value.EndsTurn() {
				return true
			}
		}
		return false
	})
	deadline := time.Now().Add(time.Second)
	for finishCount.Load() != 1 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := finishCount.Load(); got != 1 {
		t.Fatalf("finish count = %d, want 1", got)
	}
	want := []string{"turn.begin", "turn.started", "turn.terminal", "turn.finish"}
	got := publisher.orderSnapshot()
	if len(got) != len(want) {
		t.Fatalf("order = %v, want %v", got, want)
	}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("order = %v, want %v", got, want)
		}
	}
}

func TestTurnHooksFinishWaitsForDurableTerminalAcknowledgement(t *testing.T) {
	t.Parallel()
	boundary := &blockingExecutionBoundary{
		entered: make(chan event.Event, 2),
		release: make(chan struct{}, 2),
	}
	var finishCount atomic.Int32
	runner, err := hook.Compile(hook.Set{Around: []hook.Around{{
		Operation: hook.OperationTurn,
		Begin: func(ctx context.Context, _ hook.Call) (context.Context, hook.FinishFunc) {
			return ctx, func(hook.Result) { finishCount.Add(1) }
		},
	}}})
	if err != nil {
		t.Fatalf("hook.Compile: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	instance, err := newWithConfig(ctx, mustID(t), mustID(t), Provenance{}, boundary, runtimeConfig{
		Client: &fakeLLM{chunks: []content.Chunk{textChunk("done")}}, Model: testModel(),
		Hooks: runner, DrainTimeout: 200 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("newWithConfig: %v", err)
	}
	instance.Commands <- command.UserInput{
		Header: command.Header{CommandID: mustID(t)},
		Blocks: []content.Block{&content.TextBlock{Text: "go"}},
	}
	select {
	case value := <-boundary.entered:
		if _, ok := value.(event.StepDone); !ok {
			t.Fatalf("first boundary = %T, want StepDone", value)
		}
	case <-time.After(time.Second):
		t.Fatal("StepDone did not reach durable boundary")
	}
	boundary.release <- struct{}{}
	select {
	case value := <-boundary.entered:
		if _, ok := value.(event.TurnDone); !ok {
			t.Fatalf("second boundary = %T, want TurnDone", value)
		}
	case <-time.After(time.Second):
		t.Fatal("TurnDone did not reach durable boundary")
	}
	if got := finishCount.Load(); got != 0 {
		t.Fatalf("finish count before terminal ack = %d, want 0", got)
	}
	boundary.release <- struct{}{}
	deadline := time.Now().Add(time.Second)
	for finishCount.Load() != 1 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := finishCount.Load(); got != 1 {
		t.Fatalf("finish count after terminal ack = %d, want 1", got)
	}
}

func TestTurnHooksDenialPublishesNoStartedAndReleasesAdmissionOnce(t *testing.T) {
	t.Parallel()
	admission := &countingAdmission{}
	var finish hook.Result
	var finishCount atomic.Int32
	runner, err := hook.Compile(hook.Set{
		PolicyRevision: "turn-hook-test-v1",
		Guards: []hook.Guard{{
			Operation: hook.OperationTurn,
			Check: func(context.Context, hook.Call) error {
				return hook.Deny("turn_blocked", "private policy reason")
			},
		}},
		Around: []hook.Around{{
			Operation: hook.OperationTurn,
			Begin: func(ctx context.Context, _ hook.Call) (context.Context, hook.FinishFunc) {
				return ctx, func(result hook.Result) {
					finish = result
					finishCount.Add(1)
				}
			},
		}},
	})
	if err != nil {
		t.Fatalf("hook.Compile: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	client := &hookProbeClient{}
	instance, err := newWithConfig(ctx, mustID(t), mustID(t), Provenance{}, admission, runtimeConfig{
		Client: client, Model: testModel(), Hooks: runner, DrainTimeout: 200 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("newWithConfig: %v", err)
	}
	inputID := mustID(t)
	instance.Commands <- command.UserInput{Header: command.Header{CommandID: inputID}}
	blockUntilEvents(t, &admission.recordingPublisher, func(values []event.Event) bool {
		for _, value := range values {
			if rejected, ok := value.(event.TurnRejected); ok && rejected.Cause.CommandID == inputID {
				return true
			}
		}
		return false
	})
	if calls, _, _ := client.snapshot(); calls != 0 {
		t.Fatalf("Stream calls = %d, want 0", calls)
	}
	for _, value := range admission.events() {
		if started, ok := value.(event.TurnStarted); ok && started.Cause.CommandID == inputID {
			t.Fatalf("false TurnStarted published: %#v", started)
		}
	}
	if got := admission.releases.Load(); got != 1 {
		t.Fatalf("admission releases = %d, want 1", got)
	}
	if finishCount.Load() != 1 || finish.Outcome != hook.OutcomeDenied {
		t.Fatalf("finish = %#v count=%d, want one denied", finish, finishCount.Load())
	}
}

func TestTurnHooksDenialFinishesAfterInputResolution(t *testing.T) {
	t.Parallel()
	publisher := &orderedTurnPublisher{}
	runner, err := hook.Compile(hook.Set{
		PolicyRevision: "turn-order-hook-test-v1",
		Guards: []hook.Guard{{
			Operation: hook.OperationTurn,
			Check: func(context.Context, hook.Call) error {
				return hook.Deny("turn_blocked", "private policy reason")
			},
		}},
		Around: []hook.Around{{
			Operation: hook.OperationTurn,
			Begin: func(ctx context.Context, _ hook.Call) (context.Context, hook.FinishFunc) {
				publisher.appendOrder("turn.begin")
				return ctx, func(hook.Result) { publisher.appendOrder("turn.finish") }
			},
		}},
	})
	if err != nil {
		t.Fatalf("hook.Compile: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	instance, err := newWithConfig(ctx, mustID(t), mustID(t), Provenance{}, publisher, runtimeConfig{
		Client: &hookProbeClient{}, Model: testModel(), Hooks: runner,
		DrainTimeout: 200 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("newWithConfig: %v", err)
	}
	inputID := mustID(t)
	instance.Commands <- command.UserInput{Header: command.Header{CommandID: inputID}}
	blockUntilEvents(t, &publisher.recordingPublisher, func(values []event.Event) bool {
		for _, value := range values {
			if rejected, ok := value.(event.TurnRejected); ok && rejected.Cause.CommandID == inputID {
				return true
			}
		}
		return false
	})
	deadline := time.Now().Add(time.Second)
	for len(publisher.orderSnapshot()) < 3 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	got := publisher.orderSnapshot()
	want := []string{"turn.begin", "turn.rejected", "turn.finish"}
	if len(got) != len(want) {
		t.Fatalf("order = %v, want %v", got, want)
	}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("order = %v, want %v", got, want)
		}
	}
}

func TestTurnAndStepHooksPropagateDerivedContextsToDurableBoundaries(t *testing.T) {
	t.Parallel()
	boundary := newHookContextBoundary()
	runner, err := hook.Compile(hook.Set{Around: []hook.Around{
		{
			Operation: hook.OperationTurn,
			Begin: func(ctx context.Context, _ hook.Call) (context.Context, hook.FinishFunc) {
				return context.WithValue(ctx, hookBoundaryContextKey{}, "turn"), nil
			},
		},
		{
			Operation: hook.OperationStep,
			Begin: func(ctx context.Context, _ hook.Call) (context.Context, hook.FinishFunc) {
				return context.WithValue(ctx, hookBoundaryContextKey{}, "step"), nil
			},
		},
	}})
	if err != nil {
		t.Fatalf("hook.Compile: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	instance, err := newWithConfig(ctx, mustID(t), mustID(t), Provenance{}, boundary, runtimeConfig{
		Client: &fakeLLM{chunks: []content.Chunk{textChunk("done")}}, Model: testModel(),
		Hooks: runner, DrainTimeout: 200 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("newWithConfig: %v", err)
	}
	instance.Commands <- command.UserInput{
		Header: command.Header{CommandID: mustID(t)},
		Blocks: []content.Block{&content.TextBlock{Text: "go"}},
	}
	blockUntilEvents(t, &boundary.recordingPublisher, func(values []event.Event) bool {
		for _, value := range values {
			if value.EndsTurn() {
				return true
			}
		}
		return false
	})
	tests := []struct {
		name string
		want string
	}{
		{name: "turn_started", want: "turn"},
		{name: "step_done", want: "step"},
		{name: "turn_terminal", want: "turn"},
	}
	for _, test := range tests {
		observed, ok := boundary.observed(test.name)
		if !ok {
			t.Fatalf("%s context was not observed", test.name)
		}
		if got := observed.marker; got != test.want {
			t.Fatalf("%s marker = %v, want %q", test.name, got, test.want)
		}
		if err := observed.err; err != nil {
			t.Fatalf("%s context err = %v, want live observation context", test.name, err)
		}
	}
}

func TestTurnHooksMalformedDenialFailsClosedWithoutStarting(t *testing.T) {
	t.Parallel()
	admission := &countingAdmission{}
	var finish hook.Result
	runner, err := hook.Compile(hook.Set{
		PolicyRevision: "turn-malformed-hook-test-v1",
		Guards: []hook.Guard{{
			Operation: hook.OperationTurn,
			Check: func(context.Context, hook.Call) error {
				return &hook.Denial{Code: "INVALID CODE", Reason: "private malformed detail"}
			},
		}},
		Around: []hook.Around{{
			Operation: hook.OperationTurn,
			Begin: func(ctx context.Context, _ hook.Call) (context.Context, hook.FinishFunc) {
				return ctx, func(result hook.Result) { finish = result }
			},
		}},
	})
	if err != nil {
		t.Fatalf("hook.Compile: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	client := &hookProbeClient{}
	instance, err := newWithConfig(ctx, mustID(t), mustID(t), Provenance{}, admission, runtimeConfig{
		Client: client, Model: testModel(), Hooks: runner, DrainTimeout: 200 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("newWithConfig: %v", err)
	}
	inputID := mustID(t)
	instance.Commands <- command.UserInput{Header: command.Header{CommandID: inputID}}
	blockUntilEvents(t, &admission.recordingPublisher, func(values []event.Event) bool {
		for _, value := range values {
			if rejected, ok := value.(event.TurnRejected); ok && rejected.Cause.CommandID == inputID {
				return true
			}
		}
		return false
	})
	if calls, _, _ := client.snapshot(); calls != 0 {
		t.Fatalf("Stream calls = %d, want 0", calls)
	}
	for _, value := range admission.events() {
		if _, ok := value.(event.TurnStarted); ok {
			t.Fatalf("malformed denial published TurnStarted: %#v", value)
		}
	}
	if finish.Outcome != hook.OutcomeFailed {
		t.Fatalf("finish outcome = %v, want failed", finish.Outcome)
	}
}

func TestHookOutcomeRejectsMalformedDenialAndClassifiesCancellation(t *testing.T) {
	t.Parallel()
	if got := hookOutcome(context.Background(), &hook.Denial{Code: "BAD", Reason: "raw"}); got != hook.OutcomeFailed {
		t.Fatalf("malformed denial outcome = %v, want failed", got)
	}
	if got := hookOutcome(context.Background(), hook.Deny("blocked", "valid refusal")); got != hook.OutcomeDenied {
		t.Fatalf("valid denial outcome = %v, want denied", got)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got := hookOutcome(ctx, context.Canceled); got != hook.OutcomeCanceled {
		t.Fatalf("cancellation outcome = %v, want canceled", got)
	}
	if got := hookOutcome(ctx, nil); got != hook.OutcomeCompleted {
		t.Fatalf("normalized success under canceled ctx = %v, want completed", got)
	}
	guardFailure := &hook.GuardError{Operation: hook.OperationInference, Cause: context.Canceled}
	if got := hookOutcome(ctx, guardFailure); got != hook.OutcomeFailed {
		t.Fatalf("guard failure under canceled ctx = %v, want failed", got)
	}
	if got := hookOutcome(ctx, errors.New("provider failed before owner cleanup")); got != hook.OutcomeFailed {
		t.Fatalf("ordinary failure under canceled owner ctx = %v, want failed", got)
	}
}

func TestTurnHooksNestStepAndInferenceAndPropagateContext(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	var order []string
	appendOrder := func(value string) {
		mu.Lock()
		defer mu.Unlock()
		order = append(order, value)
	}
	around := func(operation hook.Operation, name string, derive bool) hook.Around {
		return hook.Around{
			Operation: operation,
			Begin: func(ctx context.Context, _ hook.Call) (context.Context, hook.FinishFunc) {
				appendOrder(name + ".begin")
				if derive {
					ctx = context.WithValue(ctx, inferenceHookContextKey{}, "turn-derived")
				}
				return ctx, func(hook.Result) { appendOrder(name + ".finish") }
			},
		}
	}
	runner, err := hook.Compile(hook.Set{Around: []hook.Around{
		around(hook.OperationTurn, "turn", true),
		around(hook.OperationStep, "step", false),
		around(hook.OperationInference, "inference", false),
	}})
	if err != nil {
		t.Fatalf("hook.Compile: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	client := &hookProbeClient{}
	publisher := &recordingPublisher{}
	instance, err := newWithConfig(ctx, mustID(t), mustID(t), Provenance{}, publisher, runtimeConfig{
		Client: client, Model: testModel(), Hooks: runner, DrainTimeout: 200 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("newWithConfig: %v", err)
	}
	instance.Commands <- command.UserInput{
		Header: command.Header{CommandID: mustID(t)},
		Blocks: []content.Block{&content.TextBlock{Text: "go"}},
	}
	blockUntilEvents(t, publisher, func(values []event.Event) bool {
		for _, value := range values {
			if value.EndsTurn() {
				return true
			}
		}
		return false
	})
	deadline := time.Now().Add(time.Second)
	for {
		mu.Lock()
		complete := len(order) == 6
		mu.Unlock()
		if complete || time.Now().After(deadline) {
			break
		}
		time.Sleep(time.Millisecond)
	}
	_, providerCtx, _ := client.snapshot()
	if got := providerCtx.Value(inferenceHookContextKey{}); got != "turn-derived" {
		t.Fatalf("provider context value = %v, want turn-derived", got)
	}
	mu.Lock()
	defer mu.Unlock()
	want := []string{
		"turn.begin",
		"step.begin",
		"inference.begin",
		"inference.finish",
		"step.finish",
		"turn.finish",
	}
	if len(order) != len(want) {
		t.Fatalf("order = %v, want %v", order, want)
	}
	for index := range want {
		if order[index] != want[index] {
			t.Fatalf("order = %v, want %v", order, want)
		}
	}
}
