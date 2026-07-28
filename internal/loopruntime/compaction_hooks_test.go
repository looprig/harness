package loopruntime

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/looprig/core/content"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/command"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/hook"
	"github.com/looprig/harness/pkg/identity"
	"github.com/looprig/harness/pkg/loop"
	contextcount "github.com/looprig/inference/contextcount"
)

type compactionHookContextKey struct{}

type compactionHookCompactor struct {
	mu             sync.Mutex
	calls          int
	sawHookContext bool
	summary        *content.UserMessage
	recorder       *recordingPublisher
}

type compactionHookPublisher struct {
	*recordingPublisher
	mu              sync.Mutex
	startedDerived  bool
	terminalDerived bool
}

func (p *compactionHookPublisher) PublishEventChecked(ctx context.Context, value event.Event) error {
	p.mu.Lock()
	switch value.(type) {
	case event.CompactionStarted:
		p.startedDerived = ctx.Value(compactionHookContextKey{}) == "derived"
	case event.CompactionCommitted, event.CompactionRejected:
		p.terminalDerived = ctx.Value(compactionHookContextKey{}) == "derived"
	}
	p.mu.Unlock()
	return p.recordingPublisher.PublishEventChecked(ctx, value)
}

func (p *compactionHookPublisher) derivedSnapshot() (bool, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.startedDerived, p.terminalDerived
}

type failingCompactionHookCompactor struct {
	panicValue any
	invalid    bool
}

func (c failingCompactionHookCompactor) CompactAndFinalize(
	ctx context.Context,
	input loop.CompactionInput,
	finalize func(context.Context, CompactionOutcome) error,
) error {
	if c.panicValue != nil {
		panic(c.panicValue)
	}
	output := &loop.CompactionOutput{
		Basis: input.Basis, Model: input.Model, RequestFingerprint: input.RequestFingerprint,
		Summary: validFinalizationSummary(),
	}
	if c.invalid {
		output.RequestFingerprint = [32]byte{0xff}
	}
	return finalize(ctx, CompactionOutcome{Value: output})
}

func (c *compactionHookCompactor) CompactAndFinalize(
	ctx context.Context,
	input loop.CompactionInput,
	finalize func(context.Context, CompactionOutcome) error,
) error {
	c.mu.Lock()
	c.calls++
	c.sawHookContext = ctx.Value(compactionHookContextKey{}) == "derived"
	c.mu.Unlock()
	if len(input.Transcript) != 0 {
		input.Transcript[0] = replacementTestMessage("compactor-local mutation")
	}
	if !publishedCompactionEvent(c.recorder.events(), func(value event.Event) bool {
		_, ok := value.(event.CompactionStarted)
		return ok
	}) {
		panic("compactor invoked before CompactionStarted")
	}
	return finalize(ctx, CompactionOutcome{Value: &loop.CompactionOutput{
		Basis: input.Basis, Model: input.Model, RequestFingerprint: input.RequestFingerprint,
		Summary: cloneUserMessage(c.summary),
	}})
}

func (c *compactionHookCompactor) snapshot() (int, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls, c.sawHookContext
}

func TestCompactionHooksSuccessFreezesCallPropagatesContextAndFinishesAfterCommit(t *testing.T) {
	var started hook.Call
	finished := make(chan hook.Result, 1)
	recorder := &recordingPublisher{}
	publisher := &compactionHookPublisher{recordingPublisher: recorder}
	hooks := compileRuntimeHooks(t, hook.Set{Around: []hook.Around{{
		Operation: hook.OperationCompaction,
		Begin: func(ctx context.Context, call hook.Call) (context.Context, hook.FinishFunc) {
			started = call
			if call.Compaction == nil || call.Compaction.AttemptID == (event.CompactAttemptID{}) ||
				call.Compaction.Input == nil || call.Compaction.Input.Basis == (event.ContextBasis{}) ||
				call.StartedAt.IsZero() {
				t.Fatalf("unfrozen compaction call = %#v", call)
			}
			if publishedCompactionEvent(recorder.events(), func(value event.Event) bool {
				_, ok := value.(event.CompactionStarted)
				return ok
			}) {
				t.Fatal("CompactionStarted published before hook")
			}
			return context.WithValue(ctx, compactionHookContextKey{}, "derived"), func(result hook.Result) {
				if !publishedCompactionEvent(recorder.events(), func(value event.Event) bool {
					_, ok := value.(event.CompactionCommitted)
					return ok
				}) {
					t.Error("hook finished before canonical CompactionCommitted append")
				}
				finished <- result
			}
		},
	}}})
	compactor := &compactionHookCompactor{summary: validFinalizationSummary(), recorder: recorder}
	actor := newCompactionHooksActor(t, publisher, hooks, compactor, nil)

	commandID := uuid.UUID{0xd3}
	sendCompact(t, actor, uuid.UUID{0xd1}, uuid.UUID{0xd2}, commandID, identity.AgencyUser)
	waitForCompactionHookTerminal(t, recorder, commandID)

	result := awaitCompactionHookFinish(t, finished)
	if result.Outcome != hook.OutcomeCompleted || result.Err != nil {
		t.Fatalf("finish = %#v, want completed", result)
	}
	if result.Compaction == nil || result.Compaction.Output == nil {
		t.Fatalf("finish compaction output = %#v, want validated output", result.Compaction)
	}
	if result.Compaction.AttemptID != started.Compaction.AttemptID ||
		result.Coordinates != (identity.Coordinates{SessionID: uuid.UUID{0xd1}, LoopID: uuid.UUID{0xd2}}) ||
		result.AgentName != identity.AgentName("compactor") ||
		result.Cause.CommandID != commandID {
		t.Fatalf("finish identity = %#v; start = %#v", result.Call, started)
	}
	if len(started.Compaction.Input.Transcript) != 1 ||
		!reflect.DeepEqual(started.Compaction.Input.Transcript[0], replacementTestMessage("committed history")) {
		t.Fatalf("hook input aliased compactor input: %#v", started.Compaction.Input.Transcript)
	}
	if calls, derived := compactor.snapshot(); calls != 1 || !derived {
		t.Fatalf("compactor = (%d calls, derived=%v), want (1,true)", calls, derived)
	}
	if startedDerived, terminalDerived := publisher.derivedSnapshot(); !startedDerived || !terminalDerived {
		t.Fatalf("publication contexts = (started:%v terminal:%v), want both derived", startedDerived, terminalDerived)
	}
}

func TestCompactionHooksFailuresFinishOnceWithBoundedOutcomes(t *testing.T) {
	countErr := errors.New("counter failed after summary")
	tests := []struct {
		name       string
		compactor  Compactor
		counterErr error
	}{
		{name: "invalid output", compactor: failingCompactionHookCompactor{invalid: true}},
		{name: "counter failure", compactor: failingCompactionHookCompactor{}, counterErr: countErr},
		{name: "compactor panic", compactor: failingCompactionHookCompactor{panicValue: "secret panic detail"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			finished := make(chan hook.Result, 2)
			recorder := &recordingPublisher{}
			hooks := compileRuntimeHooks(t, hook.Set{Around: []hook.Around{{
				Operation: hook.OperationCompaction,
				Begin: func(ctx context.Context, _ hook.Call) (context.Context, hook.FinishFunc) {
					return ctx, func(result hook.Result) { finished <- result }
				},
			}}})
			actor := newCompactionHooksActor(t, recorder, hooks, tt.compactor, tt.counterErr)
			commandID := uuid.UUID{0xf3}
			sendCompact(t, actor, uuid.UUID{0xd1}, uuid.UUID{0xd2}, commandID, identity.AgencyUser)
			waitForCompactionHookTerminal(t, recorder, commandID)
			result := awaitCompactionHookFinish(t, finished)
			if result.Outcome != hook.OutcomeFailed || result.Compaction.Output != nil {
				t.Fatalf("finish = %#v, want failed without output", result)
			}
			select {
			case duplicate := <-finished:
				t.Fatalf("duplicate finish = %#v", duplicate)
			case <-time.After(25 * time.Millisecond):
			}
		})
	}
}

func TestCompactionHooksCancellationFinishesCanceledOnce(t *testing.T) {
	finished := make(chan hook.Result, 2)
	recorder := &recordingPublisher{}
	hooks := compileRuntimeHooks(t, hook.Set{Around: []hook.Around{{
		Operation: hook.OperationCompaction,
		Begin: func(ctx context.Context, _ hook.Call) (context.Context, hook.FinishFunc) {
			return ctx, func(result hook.Result) { finished <- result }
		},
	}}})
	compactor := &gatedExecutorCompactor{
		summary: validFinalizationSummary(), started: make(chan struct{}), release: make(chan struct{}),
	}
	actor := newCompactionHooksActor(t, recorder, hooks, compactor, nil)
	commandID := uuid.UUID{0xa3}
	sendCompact(t, actor, uuid.UUID{0xd1}, uuid.UUID{0xd2}, commandID, identity.AgencyUser)
	select {
	case <-compactor.started:
	case <-time.After(2 * time.Second):
		t.Fatal("compactor did not start")
	}
	ack := make(chan bool, 1)
	actor.PriorityCommandSink() <- command.Interrupt{Header: command.Header{CommandID: uuid.UUID{0xa4}}, Ack: ack}
	if <-ack {
		t.Fatal("idle compaction interrupt reported an active turn")
	}
	close(compactor.release)
	waitForCompactionHookTerminal(t, recorder, commandID)
	result := awaitCompactionHookFinish(t, finished)
	if result.Outcome != hook.OutcomeCanceled || !errors.Is(result.Err, context.Canceled) {
		t.Fatalf("finish = %#v, want canceled", result)
	}
	select {
	case duplicate := <-finished:
		t.Fatalf("duplicate finish = %#v", duplicate)
	case <-time.After(25 * time.Millisecond):
	}
}

func TestCompactionHooksFinalizerFailureAndRetryFinishOnlyOnce(t *testing.T) {
	call := hook.Call{
		Operation: hook.OperationCompaction, StartedAt: time.Now(),
		Coordinates: identity.Coordinates{SessionID: uuid.UUID{0x31}, LoopID: uuid.UUID{0x32}},
		Compaction: &hook.CompactionData{
			AttemptID: event.CompactAttemptID(uuid.UUID{0x33}),
			Input: &loop.CompactionInput{
				Basis: event.ContextBasis{Revision: 4, ThroughEventID: uuid.UUID{0x34}},
			},
		},
	}
	finished := make(chan hook.Result, 2)
	hooks := compileRuntimeHooks(t, hook.Set{Around: []hook.Around{{
		Operation: hook.OperationCompaction,
		Begin: func(ctx context.Context, _ hook.Call) (context.Context, hook.FinishFunc) {
			return ctx, func(result hook.Result) { finished <- result }
		},
	}}})
	hookCtx, finish, err := hooks.Start(context.Background(), call)
	if err != nil {
		t.Fatalf("hooks.Start() error = %v", err)
	}
	scope := &compactionHookScope{ctx: hookCtx, call: call, finish: finish}
	scope.setTerminal(hook.OutcomeCompleted, nil, &loop.CompactionOutput{Summary: validFinalizationSummary()})
	publisher := &compactionFinalizationPublisher{}
	journalErr := &compactionFinalizationJournalError{}
	publisher.setFailure("CompactionCommitted", journalErr)
	attempt := validFinalizationAttempt()
	attempt.AttemptID = call.Compaction.AttemptID
	finalizer := newCompactionFinalizer(compactionFinalizerConfig{
		Publisher: publisher, Factory: finalizationFactory(),
		SessionID: call.Coordinates.SessionID, LoopID: call.Coordinates.LoopID,
		Now: func() time.Time { return attempt.StartedAt.Add(time.Second) },
	})
	proposal := compactionFinalizationProposal{Success: validPreparedFinalizationSuccess(6), hookScope: scope}
	if _, err := finalizer.Finalize(context.Background(), attempt, proposal); err == nil {
		t.Fatal("Finalize() error = nil, want canonical append failure")
	}
	if result := awaitCompactionHookFinish(t, finished); result.Outcome != hook.OutcomeFailed {
		t.Fatalf("failure finish = %#v, want failed", result)
	}
	publisher.setFailure("", nil)
	if _, err := finalizer.Finalize(context.Background(), attempt, proposal); err != nil {
		t.Fatalf("retry Finalize() error = %v", err)
	}
	if _, err := finalizer.Finalize(context.Background(), attempt, proposal); err != nil {
		t.Fatalf("idempotent Finalize() error = %v", err)
	}
	select {
	case duplicate := <-finished:
		t.Fatalf("duplicate retry finish = %#v", duplicate)
	case <-time.After(25 * time.Millisecond):
	}
}

func TestCompactionHooksSuccessfulFinalizerReplayDoesNotDoubleFinish(t *testing.T) {
	call := hook.Call{
		Operation: hook.OperationCompaction, StartedAt: time.Now(),
		Coordinates: identity.Coordinates{SessionID: uuid.UUID{0x41}, LoopID: uuid.UUID{0x42}},
		Compaction: &hook.CompactionData{
			AttemptID: event.CompactAttemptID(uuid.UUID{0x43}),
			Input: &loop.CompactionInput{
				Basis: event.ContextBasis{Revision: 4, ThroughEventID: uuid.UUID{0x44}},
			},
		},
	}
	finished := make(chan hook.Result, 2)
	hooks := compileRuntimeHooks(t, hook.Set{Around: []hook.Around{{
		Operation: hook.OperationCompaction,
		Begin: func(ctx context.Context, _ hook.Call) (context.Context, hook.FinishFunc) {
			return ctx, func(result hook.Result) { finished <- result }
		},
	}}})
	hookCtx, finish, err := hooks.Start(context.Background(), call)
	if err != nil {
		t.Fatalf("hooks.Start() error = %v", err)
	}
	scope := &compactionHookScope{ctx: hookCtx, call: call, finish: finish}
	scope.setTerminal(hook.OutcomeCompleted, nil, &loop.CompactionOutput{Summary: validFinalizationSummary()})
	attempt := validFinalizationAttempt()
	attempt.AttemptID = call.Compaction.AttemptID
	finalizer := newCompactionFinalizer(compactionFinalizerConfig{
		Publisher: &compactionFinalizationPublisher{}, Factory: finalizationFactory(),
		SessionID: call.Coordinates.SessionID, LoopID: call.Coordinates.LoopID,
		Now: func() time.Time { return attempt.StartedAt.Add(time.Second) },
	})
	proposal := compactionFinalizationProposal{Success: validPreparedFinalizationSuccess(7), hookScope: scope}
	for index := 0; index < 2; index++ {
		if _, err := finalizer.Finalize(context.Background(), attempt, proposal); err != nil {
			t.Fatalf("Finalize() call %d error = %v", index+1, err)
		}
	}
	if result := awaitCompactionHookFinish(t, finished); result.Outcome != hook.OutcomeCompleted {
		t.Fatalf("finish = %#v, want completed", result)
	}
	select {
	case duplicate := <-finished:
		t.Fatalf("duplicate replay finish = %#v", duplicate)
	case <-time.After(25 * time.Millisecond):
	}
}

func TestCompactionHooksDenialUsesExistingRedactedRejectionWithoutCompactor(t *testing.T) {
	const secret = "do-not-export-hook-policy-detail"
	finished := make(chan hook.Result, 1)
	recorder := &recordingPublisher{}
	hooks := compileRuntimeHooks(t, hook.Set{
		PolicyRevision: "deny-v1",
		Guards: []hook.Guard{{
			Operation: hook.OperationCompaction,
			Check: func(context.Context, hook.Call) error {
				return hook.Deny("policy.compaction", secret)
			},
		}},
		Around: []hook.Around{{
			Operation: hook.OperationCompaction,
			Begin: func(ctx context.Context, _ hook.Call) (context.Context, hook.FinishFunc) {
				return ctx, func(result hook.Result) { finished <- result }
			},
		}},
	})
	compactor := &compactionHookCompactor{summary: validFinalizationSummary(), recorder: recorder}
	actor := newCompactionHooksActor(t, recorder, hooks, compactor, nil)

	commandID := uuid.UUID{0xe3}
	sendCompact(t, actor, uuid.UUID{0xd1}, uuid.UUID{0xd2}, commandID, identity.AgencyUser)
	waitForCompactionHookTerminal(t, recorder, commandID)

	result := awaitCompactionHookFinish(t, finished)
	if result.Outcome != hook.OutcomeDenied || result.Err == nil ||
		strings.Contains(result.Err.Error(), secret) {
		t.Fatalf("finish = %#v, want redacted denial", result)
	}
	if calls, _ := compactor.snapshot(); calls != 0 {
		t.Fatalf("compactor calls = %d, want 0", calls)
	}
	var rejected, waiter bool
	for _, published := range recorder.events() {
		switch value := published.(type) {
		case event.CompactionStarted:
			t.Fatalf("denied compaction published %T", value)
		case event.CompactionRejected:
			rejected = value.RejectReason == event.CompactRejectUnavailable
		case event.CompactWaiterRejected:
			waiter = value.Cause.CommandID == commandID && value.Reason == event.CompactRejectUnavailable
		}
	}
	if !rejected || !waiter {
		t.Fatalf("denial protocol = rejected:%v waiter:%v", rejected, waiter)
	}
}

func TestCompactionHooksInternalGuardFailureFailsClosedAndRedactsDetails(t *testing.T) {
	const secret = "internal-hook-secret"
	finished := make(chan hook.Result, 1)
	recorder := &recordingPublisher{}
	hooks := compileRuntimeHooks(t, hook.Set{
		PolicyRevision: "fail-v1",
		Guards: []hook.Guard{{
			Operation: hook.OperationCompaction,
			Check:     func(context.Context, hook.Call) error { return errors.New(secret) },
		}},
		Around: []hook.Around{{
			Operation: hook.OperationCompaction,
			Begin: func(ctx context.Context, _ hook.Call) (context.Context, hook.FinishFunc) {
				return ctx, func(result hook.Result) { finished <- result }
			},
		}},
	})
	compactor := &compactionHookCompactor{summary: validFinalizationSummary(), recorder: recorder}
	actor := newCompactionHooksActor(t, recorder, hooks, compactor, nil)
	commandID := uuid.UUID{0xb3}
	sendCompact(t, actor, uuid.UUID{0xd1}, uuid.UUID{0xd2}, commandID, identity.AgencyUser)
	waitForCompactionHookTerminal(t, recorder, commandID)

	result := awaitCompactionHookFinish(t, finished)
	if result.Outcome != hook.OutcomeFailed || result.Err == nil ||
		strings.Contains(result.Err.Error(), secret) {
		t.Fatalf("finish = %#v, want redacted internal failure", result)
	}
	if calls, _ := compactor.snapshot(); calls != 0 {
		t.Fatalf("compactor calls = %d, want 0", calls)
	}
	for _, published := range recorder.events() {
		switch value := published.(type) {
		case event.CompactionStarted:
			t.Fatalf("blocked compaction published %T", value)
		case event.CompactionRejected:
			if value.RejectReason != event.CompactRejectInternal {
				t.Fatalf("rejection = %v, want internal", value.RejectReason)
			}
		case event.CompactWaiterRejected:
			if value.Cause.CommandID == commandID && value.Reason != event.CompactRejectInternal {
				t.Fatalf("waiter rejection = %v, want internal", value.Reason)
			}
		}
	}
}

func newCompactionHooksActor(
	t *testing.T,
	publisher eventPublisher,
	hooks *hook.Runner,
	compactor Compactor,
	counterErr error,
) *Loop {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	capability := contextTestCapability(contextcount.CountQualityExactLocal)
	counter := &sequenceContextCounter{
		capability: capability,
		counts:     []content.TokenCount{40, 10},
		errs:       []error{nil, counterErr},
	}
	settings := contextAdmissionSettings{ReservedOutput: 20, CountTimeout: 2 * time.Second}
	executor, err := newCompactionExecutor(ctx, compactionExecutorConfig{
		Compactor: compactor, Counter: counter, CounterCapability: capability,
		InferenceCapability: contextTestInferenceCapability(), Settings: settings, MaxSummaryTokens: 10,
	})
	if err != nil {
		t.Fatalf("newCompactionExecutor() error = %v", err)
	}
	runtimeModel := testModel()
	runtimeModel.Limits = testContextLimits{WindowTokens: 100, MaxInputTokens: 80, MaxOutputTokens: 20}
	actor, err := newRestoredWithConfig(ctx, uuid.UUID{0xd1}, uuid.UUID{0xd2}, publisher, runtimeConfig{
		Client: &scriptedLLM{}, Model: runtimeModel, System: "stable system", AgentName: "compactor",
		Hooks: hooks, ContextCounter: counter, CounterCapability: capability,
		InferenceCapability: contextTestInferenceCapability(), DrainTimeout: 200 * time.Millisecond,
		Compaction: &loop.CompactionPolicy{
			CounterPolicy: loop.CounterPolicyRequireExact, ReservedOutput: 20,
			MaxSummaryTokens: 10, CountTimeout: 2 * time.Second, Hustle: "context.compact",
		},
		compactionSink: executor,
	}, RestoredState{
		Msgs: content.AgenticMessages{replacementTestMessage("committed history")}, TurnIndex: 1,
		Basis: event.ContextBasis{Revision: 3, ThroughEventID: uuid.UUID{0xd0}}, HasBasis: true,
	})
	if err != nil {
		t.Fatalf("newRestoredWithConfig() error = %v", err)
	}
	return actor
}

func waitForCompactionHookTerminal(t *testing.T, recorder *recordingPublisher, commandID uuid.UUID) {
	t.Helper()
	blockUntilEvents(t, recorder, func(events []event.Event) bool {
		return publishedCompactionEvent(events, func(value event.Event) bool {
			waiter, ok := value.(event.CompactWaiterRejected)
			if ok {
				return waiter.Cause.CommandID == commandID
			}
			resolved, ok := value.(event.CompactWaiterResolved)
			return ok && resolved.Cause.CommandID == commandID
		})
	})
}

func publishedCompactionEvent(events []event.Event, match func(event.Event) bool) bool {
	for _, value := range events {
		if match(value) {
			return true
		}
	}
	return false
}

func awaitCompactionHookFinish(t *testing.T, finished <-chan hook.Result) hook.Result {
	t.Helper()
	select {
	case result := <-finished:
		return result
	case <-time.After(2 * time.Second):
		t.Fatal("compaction hook did not finish")
		return hook.Result{}
	}
}
