package loopruntime

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
	"github.com/looprig/harness/pkg/command"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/identity"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/tool"
	"github.com/looprig/inference"
)

type executorTestCompactor struct {
	outcome CompactionOutcome
	err     error
	input   loop.CompactionInput
}

type echoExecutorCompactor struct{ summary *content.UserMessage }

func (c *echoExecutorCompactor) CompactAndFinalize(ctx context.Context, input loop.CompactionInput, finalizer func(context.Context, CompactionOutcome) error) error {
	return finalizer(ctx, CompactionOutcome{Value: &loop.CompactionOutput{
		Basis: input.Basis, Model: input.Model, RequestFingerprint: input.RequestFingerprint,
		Summary: cloneUserMessage(c.summary),
	}})
}

type gatedExecutorCompactor struct {
	summary *content.UserMessage
	started chan struct{}
	release chan struct{}
}

type lateSuccessExecutorCompactor struct {
	summary  *content.UserMessage
	started  chan struct{}
	release  chan struct{}
	finished chan struct{}
}

func (c *lateSuccessExecutorCompactor) CompactAndFinalize(ctx context.Context, input loop.CompactionInput, finalizer func(context.Context, CompactionOutcome) error) error {
	close(c.started)
	<-c.release
	defer close(c.finished)
	return finalizer(ctx, CompactionOutcome{Value: &loop.CompactionOutput{
		Basis: input.Basis, Model: input.Model, RequestFingerprint: input.RequestFingerprint,
		Summary: cloneUserMessage(c.summary),
	}})
}

func (c *gatedExecutorCompactor) CompactAndFinalize(ctx context.Context, input loop.CompactionInput, finalizer func(context.Context, CompactionOutcome) error) error {
	close(c.started)
	select {
	case <-c.release:
	case <-ctx.Done():
		return ctx.Err()
	}
	return finalizer(ctx, CompactionOutcome{Value: &loop.CompactionOutput{
		Basis: input.Basis, Model: input.Model, RequestFingerprint: input.RequestFingerprint,
		Summary: cloneUserMessage(c.summary),
	}})
}

func (c *executorTestCompactor) CompactAndFinalize(ctx context.Context, input loop.CompactionInput, finalizer func(context.Context, CompactionOutcome) error) error {
	c.input = input
	if c.err != nil {
		return c.err
	}
	return finalizer(ctx, c.outcome)
}

type executorDeadlineCounter struct {
	capability  inference.CounterCapability
	calls       int
	sawDeadline bool
}

type sequenceContextCounter struct {
	mu         sync.Mutex
	capability inference.CounterCapability
	counts     []content.TokenCount
	errs       []error
	requests   []inference.Request
}

func (c *sequenceContextCounter) CountContext(_ context.Context, request inference.Request) (inference.ContextCount, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	index := len(c.requests)
	c.requests = append(c.requests, request)
	if index < len(c.errs) && c.errs[index] != nil {
		return inference.ContextCount{}, c.errs[index]
	}
	count := c.counts[len(c.counts)-1]
	if index < len(c.counts) {
		count = c.counts[index]
	}
	return inference.ContextCount{Model: request.Model.Key(), InputTokens: count, Quality: c.capability.Quality}, nil
}

func (c *sequenceContextCounter) CounterCapability() inference.CounterCapability { return c.capability }

type outcomeErrorCompactor struct{ err error }

func (c outcomeErrorCompactor) CompactAndFinalize(ctx context.Context, _ loop.CompactionInput, finalizer func(context.Context, CompactionOutcome) error) error {
	return finalizer(ctx, CompactionOutcome{Err: c.err})
}

func (c *executorDeadlineCounter) CountContext(ctx context.Context, request inference.Request) (inference.ContextCount, error) {
	c.calls++
	_, c.sawDeadline = ctx.Deadline()
	<-ctx.Done()
	return inference.ContextCount{Model: request.Model.Key(), Quality: c.capability.Quality}, ctx.Err()
}

func (c *executorDeadlineCounter) CounterCapability() inference.CounterCapability {
	return c.capability
}

func TestRunTurnConsumesCompactionDirectiveAtSafeBoundary(t *testing.T) {
	tests := []struct {
		name             string
		scripts          [][]content.Chunk
		tools            []tool.InvokableTool
		wantRequests     int
		wantMeasureCalls int
		wantOrder        []string
		wantSummaryNext  bool
	}{
		{
			name: "tool continuation resets before next primary inference",
			scripts: [][]content.Chunk{
				{toolUseChunk(0, "tool-1", "Echo", `{"value":1}`)},
				{textChunk("continued after compaction")},
			},
			tools:            []tool.InvokableTool{&echoTool{name: "Echo", output: "tool output"}},
			wantRequests:     2,
			wantMeasureCalls: 3,
			wantOrder:        []string{"measure-1", "stream-1", "step-done-1", "measure-2", "replacement", "stream-2", "step-done-2", "measure-3"},
			wantSummaryNext:  true,
		},
		{
			name:             "terminal response waits for compaction without another inference",
			scripts:          [][]content.Chunk{{textChunk("original terminal bytes")}},
			wantRequests:     1,
			wantMeasureCalls: 2,
			wantOrder:        []string{"measure-1", "stream-1", "step-done-1", "measure-2"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var order []string
			client := &scriptedLLM{scripts: tt.scripts, onStreamN: make(map[int]func(), len(tt.scripts))}
			for index := range tt.scripts {
				call := index + 1
				client.onStreamN[index] = func() { order = append(order, "stream-"+singleDigit(call)) }
			}
			toolSet := agenticToolSet(tt.tools, 25, 100)
			cfg, state, recorder := newTurnFixture(textBlocks("start"), nil, toolSet, client, noGateReg())
			summary := validFinalizationSummary()
			measureCalls := 0
			cfg.measure = func(_ context.Context, _ inference.Request, _ string, _ *content.UserMessage, _ bool) error {
				measureCalls++
				order = append(order, "measure-"+singleDigit(measureCalls))
				if measureCalls == 2 {
					return &contextReplacementDirective{
						AttemptID: event.CompactAttemptID{1},
						Replacement: turnContextReplacement{
							Summary: summary,
						},
					}
				}
				return nil
			}
			commit := cfg.commit
			commitCalls := 0
			cfg.commit = func(ctx context.Context, value turnCommit) error {
				if _, ok := value.Event.(event.StepDone); ok {
					commitCalls++
					order = append(order, "step-done-"+singleDigit(commitCalls))
				}
				return commit(ctx, value)
			}
			cfg.afterContextReplacement = func() { order = append(order, "replacement") }

			terminal := runTurn(context.Background(), cfg, state)
			done, ok := terminal.(event.TurnDone)
			if !ok {
				t.Fatalf("terminal = %T %+v, want TurnDone", terminal, terminal)
			}
			if measureCalls != tt.wantMeasureCalls {
				t.Fatalf("measure calls = %d, want %d", measureCalls, tt.wantMeasureCalls)
			}
			requests := client.requests()
			if len(requests) != tt.wantRequests {
				t.Fatalf("primary requests = %d, want %d", len(requests), tt.wantRequests)
			}
			if !reflect.DeepEqual(order, tt.wantOrder) {
				t.Fatalf("boundary order = %v, want %v", order, tt.wantOrder)
			}
			if tt.wantSummaryNext {
				if len(requests[1].Messages) != 1 || !reflect.DeepEqual(requests[1].Messages[0], summary) {
					t.Fatalf("post-compaction request messages = %#v, want summary only", requests[1].Messages)
				}
				return
			}
			steps := stepDones(recorder.events())
			if len(steps) != 1 {
				t.Fatalf("StepDone events = %d, want 1", len(steps))
			}
			committed := steps[0].Messages[0].(*content.AIMessage)
			wantBytes, marshalErr := json.Marshal(committed)
			if marshalErr != nil {
				t.Fatalf("json.Marshal(committed response) error = %v", marshalErr)
			}
			gotBytes, marshalErr := json.Marshal(done.Message)
			if marshalErr != nil {
				t.Fatalf("json.Marshal(returned response) error = %v", marshalErr)
			}
			if !reflect.DeepEqual(gotBytes, wantBytes) {
				t.Fatalf("returned response bytes = %s, want committed bytes %s", gotBytes, wantBytes)
			}
		})
	}
}

func singleDigit(value int) string {
	return string(rune('0' + value))
}

func TestCompactionExecutorCountsExactSummaryCandidateBeforeProposal(t *testing.T) {
	countErr := errors.New("count failed")
	executionErr := errors.New("hustle unavailable")
	tests := []struct {
		name         string
		inputTokens  content.TokenCount
		modelLimits  inference.ContextLimits
		countErr     error
		outcomeErr   error
		directErr    error
		timeoutCount bool
		wantReason   event.CompactRejectReason
		wantCause    string
		wantPrepared bool
		wantCounts   int
	}{
		{name: "success prepares counted summary", inputTokens: 40, modelLimits: inference.ContextLimits{WindowTokens: 100, MaxInputTokens: 80, MaxOutputTokens: 20}, wantPrepared: true, wantCounts: 1},
		{name: "summary at hard limit rejects too large", inputTokens: 80, modelLimits: inference.ContextLimits{WindowTokens: 100, MaxInputTokens: 80, MaxOutputTokens: 20}, wantReason: event.CompactRejectSummaryTooLarge, wantCause: "summary_too_large", wantCounts: 1},
		{name: "post summary count failure rejects count failed", modelLimits: inference.ContextLimits{WindowTokens: 100, MaxInputTokens: 80, MaxOutputTokens: 20}, countErr: countErr, wantReason: event.CompactRejectContextCountFailed, wantCause: "context_count", wantCounts: 1},
		{name: "post summary count timeout carries deadline and rejects count failed", modelLimits: inference.ContextLimits{WindowTokens: 100, MaxInputTokens: 80, MaxOutputTokens: 20}, timeoutCount: true, wantReason: event.CompactRejectContextCountFailed, wantCause: "context_count", wantCounts: 1},
		{name: "unknown input limit rejects limit unknown", inputTokens: 1, wantReason: event.CompactRejectContextLimitUnknown, wantCause: "limit_unknown"},
		{name: "invalid adapter summary rejects invalid summary", modelLimits: inference.ContextLimits{WindowTokens: 100, MaxInputTokens: 80, MaxOutputTokens: 20}, outcomeErr: &loop.InvalidSummaryError{Reason: loop.InvalidSummaryXMLContent}, wantReason: event.CompactRejectInvalidSummary, wantCause: "invalid_summary"},
		{name: "direct hustle failure rejects execution failed", modelLimits: inference.ContextLimits{WindowTokens: 100, MaxInputTokens: 80, MaxOutputTokens: 20}, directErr: executionErr, wantReason: event.CompactRejectExecutionFailed, wantCause: "execution"},
		{name: "caller cancellation rejects canceled", modelLimits: inference.ContextLimits{WindowTokens: 100, MaxInputTokens: 80, MaxOutputTokens: 20}, directErr: context.Canceled, wantReason: event.CompactRejectCanceled, wantCause: "canceled"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			model := testModel()
			model.Limits = tt.modelLimits
			basis := event.ContextBasis{Revision: 7, ThroughEventID: uuid.UUID{7}}
			fingerprint := [32]byte{8}
			summary := validFinalizationSummary()
			output := &loop.CompactionOutput{Basis: basis, Model: model.Key(), RequestFingerprint: fingerprint, Summary: summary}
			compactor := &executorTestCompactor{err: tt.directErr}
			if tt.outcomeErr != nil {
				compactor.outcome = CompactionOutcome{Err: tt.outcomeErr}
			} else {
				compactor.outcome = CompactionOutcome{Value: output}
			}
			basicCounter := &loopContextCounter{
				capability: contextTestCapability(inference.CountQualityExactLocal), counts: []content.TokenCount{tt.inputTokens}, err: tt.countErr,
			}
			var counter inference.ContextCounter = basicCounter
			var deadlineCounter *executorDeadlineCounter
			if tt.timeoutCount {
				deadlineCounter = &executorDeadlineCounter{capability: basicCounter.capability}
				counter = deadlineCounter
			}
			executor, err := newCompactionExecutor(context.Background(), compactionExecutorConfig{
				Compactor: compactor, Counter: counter, CounterCapability: basicCounter.capability,
				InferenceCapability: contextTestInferenceCapability(),
				Settings:            contextAdmissionSettings{ReservedOutput: 20, CountTimeout: 5 * time.Millisecond}, MaxSummaryTokens: 10,
			})
			if err != nil {
				t.Fatalf("newCompactionExecutor() error = %v", err)
			}
			attempt := validFinalizationAttempt()
			attempt.Basis = basis
			runtimeTail := replacementTestMessage("runtime tail")
			candidate := compactionExecutionCandidate{
				Measurement: event.ContextMeasurement{
					Basis: basis, Model: model.Key(), RequestFingerprint: fingerprint,
					InputTokens: 70, InputLimit: 80, Quality: inference.CountQualityExactLocal,
				},
				Request: inference.Request{
					Model: model, System: "system", Messages: content.AgenticMessages{replacementTestMessage("old transcript"), runtimeTail},
				},
				RuntimeTail: runtimeTail, RuntimeRevision: revisionDigest([]byte("runtime tail")),
				Transcript: content.AgenticMessages{replacementTestMessage("old transcript")},
			}
			if err := executor.CoordinateCompactionCandidate(context.Background(), compactionDisposition{
				Kind: compactionDispositionStart, Attempt: &attempt,
			}, candidate); err != nil {
				t.Fatalf("CoordinateCompactionCandidate() error = %v", err)
			}
			result, err := executor.AwaitCompaction(context.Background(), attempt.AttemptID)
			if err != nil {
				t.Fatalf("AwaitCompaction() error = %v", err)
			}
			if tt.wantPrepared {
				if result.Disposition != contextCompactionAwaitCommitted || result.Proposal.Success == nil {
					t.Fatalf("result = %+v, want prepared success", result)
				}
				post := result.Proposal.Success.PostCount
				if post.InputTokens != tt.inputTokens || post.InputLimit != 80 || post.Model != model.Key() {
					t.Fatalf("post count = %+v, want exact counted summary candidate", post)
				}
				requests := basicCounter.requests
				if len(requests) != 1 || len(requests[0].Messages) != 2 || !reflect.DeepEqual(requests[0].Messages[0], summary) || !reflect.DeepEqual(requests[0].Messages[1], runtimeTail) {
					t.Fatalf("counted requests = %#v, want summary plus runtime tail", requests)
				}
			} else if result.Disposition != contextCompactionAwaitRejected || result.Proposal.RejectReason != tt.wantReason {
				t.Fatalf("result = %+v, want rejection %v", result, tt.wantReason)
			}
			switch tt.wantCause {
			case "summary_too_large":
				var typed *loop.SummaryTooLargeError
				if !errors.As(result.ContinuationError, &typed) || typed.Measurement.InputTokens != tt.inputTokens {
					t.Fatalf("continuation error = %T %+v, want typed SummaryTooLargeError", result.ContinuationError, result.ContinuationError)
				}
			case "context_count":
				var typed *inference.ContextCountError
				if !errors.As(result.ContinuationError, &typed) {
					t.Fatalf("continuation error = %T %+v, want typed ContextCountError", result.ContinuationError, result.ContinuationError)
				}
			case "limit_unknown":
				var typed *loop.ContextLimitUnknownError
				if !errors.As(result.ContinuationError, &typed) {
					t.Fatalf("continuation error = %T %+v, want typed ContextLimitUnknownError", result.ContinuationError, result.ContinuationError)
				}
			case "invalid_summary":
				var typed *loop.InvalidSummaryError
				if !errors.As(result.ContinuationError, &typed) {
					t.Fatalf("continuation error = %T %+v, want typed InvalidSummaryError", result.ContinuationError, result.ContinuationError)
				}
			case "execution":
				if !errors.Is(result.ContinuationError, executionErr) {
					t.Fatalf("continuation error = %T %+v, want execution cause", result.ContinuationError, result.ContinuationError)
				}
			case "canceled":
				if !errors.Is(result.ContinuationError, context.Canceled) {
					t.Fatalf("continuation error = %T %+v, want context.Canceled", result.ContinuationError, result.ContinuationError)
				}
			}
			countCalls := len(basicCounter.requests)
			if deadlineCounter != nil {
				countCalls = deadlineCounter.calls
				if !deadlineCounter.sawDeadline {
					t.Fatal("post-summary counter did not receive configured deadline")
				}
			}
			if countCalls != tt.wantCounts {
				t.Fatalf("post-summary count calls = %d, want %d", countCalls, tt.wantCounts)
			}
			if !reflect.DeepEqual(compactor.input.Transcript, candidate.Transcript) || compactor.input.Basis != basis || compactor.input.RequestFingerprint != fingerprint {
				t.Fatalf("compactor input = %+v, want frozen candidate identity/transcript", compactor.input)
			}
		})
	}
}

func TestLoopCompactsToolContinuationAtPostStepBoundary(t *testing.T) {
	tests := []struct {
		name string
	}{
		{name: "post step attempt commits before summary only continuation"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)
			recorder := &recordingPublisher{}
			client := &scriptedLLM{scripts: [][]content.Chunk{
				{toolUseChunk(0, "tool-1", "Echo", `{"value":1}`)},
				{textChunk("continued")},
			}}
			counter := &loopContextCounter{
				capability: contextTestCapability(inference.CountQualityExactLocal),
				counts:     []content.TokenCount{40, 65, 20, 25},
			}
			model := testModel()
			model.Limits = inference.ContextLimits{WindowTokens: 100, MaxInputTokens: 80, MaxOutputTokens: 20}
			settings := contextAdmissionSettings{ReservedOutput: 20, CompactAt: 8_000, RearmBelow: 6_000, CountTimeout: time.Second, Automatic: true}
			executor, err := newCompactionExecutor(ctx, compactionExecutorConfig{
				Compactor: &echoExecutorCompactor{summary: validFinalizationSummary()}, Counter: counter,
				CounterCapability: counter.capability, InferenceCapability: contextTestInferenceCapability(),
				Settings: settings, MaxSummaryTokens: 10,
			})
			if err != nil {
				t.Fatalf("newCompactionExecutor() error = %v", err)
			}
			actor, err := newWithConfig(ctx, uuid.UUID{101}, uuid.UUID{102}, Provenance{}, recorder, runtimeConfig{
				Client: client, Model: model, System: "system", DrainTimeout: 200 * time.Millisecond,
				Tools:          agenticToolSet([]tool.InvokableTool{&echoTool{name: "Echo", output: "tool output"}}, 25, 100),
				ContextCounter: counter, CounterCapability: counter.capability, InferenceCapability: contextTestInferenceCapability(),
				Compaction: &loop.CompactionPolicy{
					Automatic: true, CounterPolicy: loop.CounterPolicyRequireExact, CompactAt: 8_000, RearmBelow: 6_000,
					ReservedOutput: 20, MaxSummaryTokens: 10, CountTimeout: time.Second, Hustle: "context.compact",
				},
				compactionSink: executor,
			})
			if err != nil {
				t.Fatalf("newWithConfig() error = %v", err)
			}
			startTurn(t, actor, recorder, textBlocks("start"))
			if terminal := drainToTerminal(t, recorder); reflect.TypeOf(terminal) != reflect.TypeOf(event.TurnDone{}) {
				t.Fatalf("terminal = %T %+v, want TurnDone", terminal, terminal)
			}
			requests := client.requests()
			if len(requests) != 2 || len(requests[1].Messages) != 1 || !reflect.DeepEqual(requests[1].Messages[0], validFinalizationSummary()) {
				t.Fatalf("primary requests = %#v, want second request with summary only", requests)
			}
			var names []string
			for _, published := range recorder.events() {
				switch published.(type) {
				case event.StepDone:
					names = append(names, "StepDone")
				case event.CompactionStarted:
					names = append(names, "CompactionStarted")
				case event.CompactionCommitted:
					names = append(names, "CompactionCommitted")
				case event.CompactWaiterResolved:
					names = append(names, "CompactWaiterResolved")
				}
			}
			wantOrder := []string{"StepDone", "CompactionStarted", "CompactionCommitted", "CompactWaiterResolved", "StepDone"}
			if !reflect.DeepEqual(names, wantOrder) {
				t.Fatalf("boundary event order = %v, want %v", names, wantOrder)
			}
		})
	}
}

func TestLoopCompactionRejectionAdmissionAtToolContinuation(t *testing.T) {
	countErr := errors.New("post-summary count failed")
	tests := []struct {
		name           string
		counts         []content.TokenCount
		countErrs      []error
		invalidSummary bool
		executorMargin content.TokenCount
		wantReason     event.CompactRejectReason
		wantPrimary    int
		wantError      string
	}{
		{name: "soft invalid summary continues original candidate", counts: []content.TokenCount{40, 65, 25}, invalidSummary: true, wantReason: event.CompactRejectInvalidSummary, wantPrimary: 2},
		{name: "soft count failure continues original candidate", counts: []content.TokenCount{40, 65, 0, 25}, countErrs: []error{nil, nil, countErr}, wantReason: event.CompactRejectContextCountFailed, wantPrimary: 2},
		{name: "hard original candidate blocks after invalid summary", counts: []content.TokenCount{40, 80}, invalidSummary: true, wantReason: event.CompactRejectInvalidSummary, wantPrimary: 1, wantError: "context_limit"},
		{name: "summary too large blocks even when original is soft", counts: []content.TokenCount{40, 65, 80}, wantReason: event.CompactRejectSummaryTooLarge, wantPrimary: 1, wantError: "summary_too_large"},
		{name: "unknown post-summary limit blocks continuation", counts: []content.TokenCount{40, 65}, executorMargin: 80, wantReason: event.CompactRejectContextLimitUnknown, wantPrimary: 1, wantError: "limit_unknown"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)
			recorder := &recordingPublisher{}
			client := &scriptedLLM{scripts: [][]content.Chunk{
				{toolUseChunk(0, "tool-1", "Echo", `{"value":1}`)}, {textChunk("continued")},
			}}
			counter := &sequenceContextCounter{
				capability: contextTestCapability(inference.CountQualityExactLocal), counts: tt.counts, errs: tt.countErrs,
			}
			model := testModel()
			model.Limits = inference.ContextLimits{WindowTokens: 100, MaxInputTokens: 80, MaxOutputTokens: 20}
			var compactor Compactor = &echoExecutorCompactor{summary: validFinalizationSummary()}
			if tt.invalidSummary {
				compactor = outcomeErrorCompactor{err: &loop.InvalidSummaryError{Reason: loop.InvalidSummaryXMLContent}}
			}
			executorSettings := contextAdmissionSettings{
				ReservedOutput: 20, SafetyMargin: tt.executorMargin, CompactAt: 8_000, RearmBelow: 6_000,
				CountTimeout: time.Second, Automatic: true,
			}
			executor, err := newCompactionExecutor(ctx, compactionExecutorConfig{
				Compactor: compactor, Counter: counter, CounterCapability: counter.capability,
				InferenceCapability: contextTestInferenceCapability(), Settings: executorSettings, MaxSummaryTokens: 10,
			})
			if err != nil {
				t.Fatalf("newCompactionExecutor() error = %v", err)
			}
			actor, err := newWithConfig(ctx, uuid.UUID{131}, uuid.UUID{132}, Provenance{}, recorder, runtimeConfig{
				Client: client, Model: model, DrainTimeout: 200 * time.Millisecond,
				Tools:          agenticToolSet([]tool.InvokableTool{&echoTool{name: "Echo", output: "tool output"}}, 25, 100),
				ContextCounter: counter, CounterCapability: counter.capability, InferenceCapability: contextTestInferenceCapability(),
				Compaction: &loop.CompactionPolicy{
					Automatic: true, CounterPolicy: loop.CounterPolicyRequireExact, CompactAt: 8_000, RearmBelow: 6_000,
					ReservedOutput: 20, MaxSummaryTokens: 10, CountTimeout: time.Second, Hustle: "context.compact",
				},
				compactionSink: executor,
			})
			if err != nil {
				t.Fatalf("newWithConfig() error = %v", err)
			}
			startTurn(t, actor, recorder, textBlocks("start"))
			terminal := drainToTerminal(t, recorder)
			switch tt.wantError {
			case "":
				if _, ok := terminal.(event.TurnDone); !ok {
					t.Fatalf("terminal = %T %+v, want TurnDone", terminal, terminal)
				}
			case "context_limit":
				failed, ok := terminal.(event.TurnFailed)
				var typed *loop.ContextLimitError
				if !ok || !errors.As(failed.Err, &typed) || typed.Measurement.InputTokens != 80 {
					t.Fatalf("terminal = %T %+v, want typed ContextLimitError at 80", terminal, terminal)
				}
			case "summary_too_large":
				failed, ok := terminal.(event.TurnFailed)
				var typed *loop.SummaryTooLargeError
				if !ok || !errors.As(failed.Err, &typed) || typed.Measurement.InputTokens != 80 {
					t.Fatalf("terminal = %T %+v, want typed SummaryTooLargeError at 80", terminal, terminal)
				}
			case "limit_unknown":
				failed, ok := terminal.(event.TurnFailed)
				var typed *loop.ContextLimitUnknownError
				if !ok || !errors.As(failed.Err, &typed) {
					t.Fatalf("terminal = %T %+v, want typed ContextLimitUnknownError", terminal, terminal)
				}
			}
			if got := len(client.requests()); got != tt.wantPrimary {
				t.Fatalf("primary calls = %d, want %d", got, tt.wantPrimary)
			}
			if tt.wantPrimary == 2 {
				for _, message := range client.requests()[1].Messages {
					if reflect.DeepEqual(message, validFinalizationSummary()) {
						t.Fatal("soft rejection replaced original continuation candidate")
					}
				}
			}
			var rejection *event.CompactionRejected
			for _, published := range recorder.events() {
				if value, ok := published.(event.CompactionRejected); ok {
					copyOfValue := value
					rejection = &copyOfValue
				}
			}
			if rejection == nil || rejection.RejectReason != tt.wantReason {
				t.Fatalf("canonical rejection = %+v, want %v", rejection, tt.wantReason)
			}
		})
	}
}

func TestLoopTerminalCompactionRejectionPreservesProducedResponse(t *testing.T) {
	countErr := errors.New("post-summary count failed")
	tests := []struct {
		name           string
		counts         []content.TokenCount
		countErrs      []error
		invalidSummary bool
		executorMargin content.TokenCount
		wantReason     event.CompactRejectReason
	}{
		{name: "invalid summary", counts: []content.TokenCount{40, 65}, invalidSummary: true, wantReason: event.CompactRejectInvalidSummary},
		{name: "post-summary count failure", counts: []content.TokenCount{40, 65, 0}, countErrs: []error{nil, nil, countErr}, wantReason: event.CompactRejectContextCountFailed},
		{name: "summary too large", counts: []content.TokenCount{40, 65, 80}, wantReason: event.CompactRejectSummaryTooLarge},
		{name: "post-summary limit unknown", counts: []content.TokenCount{40, 65}, executorMargin: 80, wantReason: event.CompactRejectContextLimitUnknown},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)
			recorder := &recordingPublisher{}
			client := &scriptedLLM{scripts: [][]content.Chunk{{textChunk("original terminal bytes")}}}
			counter := &sequenceContextCounter{
				capability: contextTestCapability(inference.CountQualityExactLocal), counts: tt.counts, errs: tt.countErrs,
			}
			model := testModel()
			model.Limits = inference.ContextLimits{WindowTokens: 100, MaxInputTokens: 80, MaxOutputTokens: 20}
			var compactor Compactor = &echoExecutorCompactor{summary: validFinalizationSummary()}
			if tt.invalidSummary {
				compactor = outcomeErrorCompactor{err: &loop.InvalidSummaryError{Reason: loop.InvalidSummaryXMLContent}}
			}
			executor, err := newCompactionExecutor(ctx, compactionExecutorConfig{
				Compactor: compactor, Counter: counter, CounterCapability: counter.capability,
				InferenceCapability: contextTestInferenceCapability(),
				Settings: contextAdmissionSettings{
					ReservedOutput: 20, SafetyMargin: tt.executorMargin, CompactAt: 8_000, RearmBelow: 6_000,
					CountTimeout: time.Second, Automatic: true,
				},
				MaxSummaryTokens: 10,
			})
			if err != nil {
				t.Fatalf("newCompactionExecutor() error = %v", err)
			}
			actor, err := newWithConfig(ctx, uuid.UUID{141}, uuid.UUID{142}, Provenance{}, recorder, runtimeConfig{
				Client: client, Model: model, DrainTimeout: 200 * time.Millisecond,
				ContextCounter: counter, CounterCapability: counter.capability, InferenceCapability: contextTestInferenceCapability(),
				Compaction: &loop.CompactionPolicy{
					Automatic: true, CounterPolicy: loop.CounterPolicyRequireExact, CompactAt: 8_000, RearmBelow: 6_000,
					ReservedOutput: 20, MaxSummaryTokens: 10, CountTimeout: time.Second, Hustle: "context.compact",
				},
				compactionSink: executor,
			})
			if err != nil {
				t.Fatalf("newWithConfig() error = %v", err)
			}
			startTurn(t, actor, recorder, textBlocks("start"))
			terminal := drainToTerminal(t, recorder)
			done, ok := terminal.(event.TurnDone)
			if !ok {
				t.Fatalf("terminal = %T %+v, want TurnDone", terminal, terminal)
			}
			if got := len(client.requests()); got != 1 {
				t.Fatalf("primary calls = %d, want original inference only", got)
			}
			gotBytes, err := json.Marshal(done.Message)
			if err != nil {
				t.Fatalf("marshal TurnDone message: %v", err)
			}
			wantBytes, err := json.Marshal(&content.AIMessage{Message: content.Message{
				Role: content.RoleAssistant, Blocks: []content.Block{&content.TextBlock{Text: "original terminal bytes"}},
			}})
			if err != nil {
				t.Fatalf("marshal expected message: %v", err)
			}
			if !reflect.DeepEqual(gotBytes, wantBytes) {
				t.Fatalf("terminal response bytes = %s, want %s", gotBytes, wantBytes)
			}
			var rejection *event.CompactionRejected
			for _, published := range recorder.events() {
				if value, ok := published.(event.CompactionRejected); ok {
					copyOfValue := value
					rejection = &copyOfValue
				}
			}
			if rejection == nil || rejection.RejectReason != tt.wantReason {
				t.Fatalf("canonical rejection = %+v, want %v", rejection, tt.wantReason)
			}
		})
	}
}

func TestLoopTerminalResponseWaitsForCompactionAndRemainsUnchanged(t *testing.T) {
	tests := []struct {
		name string
	}{
		{name: "terminal StepDone remains active and actor responsive until replacement commits"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)
			recorder := &recordingPublisher{}
			client := &scriptedLLM{scripts: [][]content.Chunk{{textChunk("original terminal bytes")}}}
			counter := &loopContextCounter{
				capability: contextTestCapability(inference.CountQualityExactLocal), counts: []content.TokenCount{40, 65, 20},
			}
			model := testModel()
			model.Limits = inference.ContextLimits{WindowTokens: 100, MaxInputTokens: 80, MaxOutputTokens: 20}
			settings := contextAdmissionSettings{ReservedOutput: 20, CompactAt: 8_000, RearmBelow: 6_000, CountTimeout: time.Second, Automatic: true}
			compactor := &gatedExecutorCompactor{summary: validFinalizationSummary(), started: make(chan struct{}), release: make(chan struct{})}
			executor, err := newCompactionExecutor(ctx, compactionExecutorConfig{
				Compactor: compactor, Counter: counter, CounterCapability: counter.capability,
				InferenceCapability: contextTestInferenceCapability(), Settings: settings, MaxSummaryTokens: 10,
			})
			if err != nil {
				t.Fatalf("newCompactionExecutor() error = %v", err)
			}
			actor, err := newWithConfig(ctx, uuid.UUID{111}, uuid.UUID{112}, Provenance{}, recorder, runtimeConfig{
				Client: client, Model: model, System: "system", DrainTimeout: 200 * time.Millisecond,
				ContextCounter: counter, CounterCapability: counter.capability, InferenceCapability: contextTestInferenceCapability(),
				Compaction: &loop.CompactionPolicy{
					Automatic: true, CounterPolicy: loop.CounterPolicyRequireExact, CompactAt: 8_000, RearmBelow: 6_000,
					ReservedOutput: 20, MaxSummaryTokens: 10, CountTimeout: time.Second, Hustle: "context.compact",
				},
				compactionSink: executor,
			})
			if err != nil {
				t.Fatalf("newWithConfig() error = %v", err)
			}
			startTurn(t, actor, recorder, textBlocks("start"))
			select {
			case <-compactor.started:
			case <-time.After(2 * time.Second):
				t.Fatal("terminal compaction did not start")
			}
			for _, published := range recorder.events() {
				if _, done := published.(event.TurnDone); done {
					t.Fatal("TurnDone published before terminal compaction completed")
				}
			}
			if len(client.requests()) != 1 {
				t.Fatalf("primary calls while terminal compaction paused = %d, want 1", len(client.requests()))
			}
			queuedID := uuid.UUID{113}
			actor.Commands <- command.UserInput{Header: command.Header{CommandID: queuedID}, Blocks: textBlocks("queued")}
			if _, ok := awaitReply(t, recorder, queuedID).(event.InputQueued); !ok {
				t.Fatal("actor did not keep input queued while terminal compaction paused")
			}
			actor.Commands <- command.CancelQueuedInput{Header: command.Header{CommandID: uuid.UUID{114}}, TargetCommandID: queuedID}
			blockUntilEvents(t, recorder, func(events []event.Event) bool {
				for _, published := range events {
					if canceled, ok := published.(event.InputCancelled); ok && canceled.Cause.CommandID == queuedID {
						return true
					}
				}
				return false
			})
			close(compactor.release)
			terminal := drainToTerminal(t, recorder)
			done, ok := terminal.(event.TurnDone)
			if !ok {
				t.Fatalf("terminal = %T %+v, want TurnDone", terminal, terminal)
			}
			var committedAI *content.AIMessage
			for _, published := range recorder.events() {
				if step, ok := published.(event.StepDone); ok && len(step.Messages) == 1 {
					committedAI, _ = step.Messages[0].(*content.AIMessage)
					break
				}
			}
			gotBytes, _ := json.Marshal(done.Message)
			wantBytes, _ := json.Marshal(committedAI)
			if !reflect.DeepEqual(gotBytes, wantBytes) {
				t.Fatalf("terminal response bytes = %s, want committed bytes %s", gotBytes, wantBytes)
			}
			if len(client.requests()) != 1 {
				t.Fatalf("primary calls after terminal compaction = %d, want no extra call", len(client.requests()))
			}
		})
	}
}

func TestLoopTerminalCompactionCancellationPreemptsProducedResponse(t *testing.T) {
	tests := []struct {
		name       string
		cancel     string
		wantReason event.CompactRejectReason
	}{
		{name: "interrupt", cancel: "interrupt", wantReason: event.CompactRejectInterrupted},
		{name: "shutdown", cancel: "shutdown", wantReason: event.CompactRejectShuttingDown},
		{name: "parent request cancel", cancel: "parent", wantReason: event.CompactRejectCanceled},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)
			recorder := &recordingPublisher{}
			client := &scriptedLLM{scripts: [][]content.Chunk{{textChunk("produced but canceled")}}}
			counter := &loopContextCounter{
				capability: contextTestCapability(inference.CountQualityExactLocal), counts: []content.TokenCount{40, 65},
			}
			model := testModel()
			model.Limits = inference.ContextLimits{WindowTokens: 100, MaxInputTokens: 80, MaxOutputTokens: 20}
			settings := contextAdmissionSettings{
				ReservedOutput: 20, CompactAt: 8_000, RearmBelow: 6_000, CountTimeout: time.Second, Automatic: true,
			}
			compactor := &lateSuccessExecutorCompactor{
				summary: validFinalizationSummary(), started: make(chan struct{}), release: make(chan struct{}), finished: make(chan struct{}),
			}
			executor, err := newCompactionExecutor(ctx, compactionExecutorConfig{
				Compactor: compactor, Counter: counter, CounterCapability: counter.capability,
				InferenceCapability: contextTestInferenceCapability(), Settings: settings, MaxSummaryTokens: 10,
			})
			if err != nil {
				t.Fatalf("newCompactionExecutor() error = %v", err)
			}
			sessionID, loopID := uuid.UUID{151}, uuid.UUID{152}
			actor, err := newWithConfig(ctx, sessionID, loopID, Provenance{}, recorder, runtimeConfig{
				Client: client, Model: model, DrainTimeout: 200 * time.Millisecond,
				ContextCounter: counter, CounterCapability: counter.capability, InferenceCapability: contextTestInferenceCapability(),
				Compaction: &loop.CompactionPolicy{
					Automatic: true, CounterPolicy: loop.CounterPolicyRequireExact, CompactAt: 8_000, RearmBelow: 6_000,
					ReservedOutput: 20, MaxSummaryTokens: 10, CountTimeout: time.Second, Hustle: "context.compact",
				},
				compactionSink: executor,
			})
			if err != nil {
				t.Fatalf("newWithConfig() error = %v", err)
			}
			inputID, _ := startTurn(t, actor, recorder, textBlocks("start"))
			select {
			case <-compactor.started:
			case <-time.After(2 * time.Second):
				t.Fatal("terminal compaction did not start")
			}
			var shutdownAck <-chan error
			switch tt.cancel {
			case "interrupt":
				ack := make(chan bool, 1)
				actor.Commands <- command.Interrupt{Header: command.Header{CommandID: uuid.UUID{153}}, Ack: ack}
				if !<-ack {
					t.Fatal("Interrupt did not cancel terminal compaction wait")
				}
			case "shutdown":
				ack := make(chan error, 1)
				shutdownAck = ack
				actor.Commands <- command.Shutdown{Header: command.Header{CommandID: uuid.UUID{153}}, Ack: ack}
			case "parent":
				if got := cancelDelegateRequest(t, actor, inputID); got != command.DelegateCancelActive {
					t.Fatalf("parent cancellation = %v, want active", got)
				}
			}
			terminal := drainToTerminal(t, recorder)
			if _, ok := terminal.(event.TurnInterrupted); !ok {
				t.Fatalf("terminal = %T %+v, want TurnInterrupted", terminal, terminal)
			}
			if shutdownAck != nil {
				if err := <-shutdownAck; err != nil {
					t.Fatalf("Shutdown error = %v", err)
				}
			}
			var rejected *event.CompactionRejected
			waiterRejected := false
			for _, published := range recorder.events() {
				switch value := published.(type) {
				case event.CompactionRejected:
					copyOfValue := value
					rejected = &copyOfValue
				case event.CompactWaiterRejected:
					if value.Reason == tt.wantReason {
						waiterRejected = true
					}
				case event.TurnDone:
					t.Fatal("canceled terminal response published TurnDone")
				}
			}
			if rejected == nil || rejected.RejectReason != tt.wantReason || !waiterRejected {
				t.Fatalf("cancellation outcome = rejection %+v waiter=%v, want %v", rejected, waiterRejected, tt.wantReason)
			}
			if got := len(client.requests()); got != 1 {
				t.Fatalf("primary calls = %d, want no inference after cancellation", got)
			}
			close(compactor.release)
			select {
			case <-compactor.finished:
			case <-time.After(2 * time.Second):
				t.Fatal("late compactor did not finish")
			}
			for _, published := range recorder.events() {
				if _, committed := published.(event.CompactionCommitted); committed {
					t.Fatal("late success committed after cancellation")
				}
			}
		})
	}
}

func TestLoopStartedCompactionCancellationIsActorOwned(t *testing.T) {
	tests := []struct {
		name       string
		shutdown   bool
		wantReason event.CompactRejectReason
	}{
		{name: "interrupt rejects in progress attempt and permits lane reuse", wantReason: event.CompactRejectInterrupted},
		{name: "shutdown rejects in progress attempt before loop exits", shutdown: true, wantReason: event.CompactRejectShuttingDown},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)
			recorder := &recordingPublisher{}
			counter := &loopContextCounter{
				capability: contextTestCapability(inference.CountQualityExactLocal), counts: []content.TokenCount{65},
			}
			model := testModel()
			model.Limits = inference.ContextLimits{WindowTokens: 100, MaxInputTokens: 80, MaxOutputTokens: 20}
			settings := contextAdmissionSettings{ReservedOutput: 20, CompactAt: 8_000, RearmBelow: 6_000, CountTimeout: time.Second, Automatic: true}
			compactor := &lateSuccessExecutorCompactor{
				summary: validFinalizationSummary(), started: make(chan struct{}), release: make(chan struct{}), finished: make(chan struct{}),
			}
			executor, err := newCompactionExecutor(ctx, compactionExecutorConfig{
				Compactor: compactor, Counter: counter, CounterCapability: counter.capability,
				InferenceCapability: contextTestInferenceCapability(), Settings: settings, MaxSummaryTokens: 10,
			})
			if err != nil {
				t.Fatalf("newCompactionExecutor() error = %v", err)
			}
			sessionID, loopID := uuid.UUID{121}, uuid.UUID{122}
			actor, err := newWithConfig(ctx, sessionID, loopID, Provenance{}, recorder, runtimeConfig{
				Client: &scriptedLLM{scripts: [][]content.Chunk{{textChunk("must not run")}}}, Model: model, DrainTimeout: 200 * time.Millisecond,
				ContextCounter: counter, CounterCapability: counter.capability, InferenceCapability: contextTestInferenceCapability(),
				Compaction: &loop.CompactionPolicy{
					Automatic: true, CounterPolicy: loop.CounterPolicyRequireExact, CompactAt: 8_000, RearmBelow: 6_000,
					ReservedOutput: 20, MaxSummaryTokens: 10, CountTimeout: time.Second, Hustle: "context.compact",
				},
				compactionSink: executor,
			})
			if err != nil {
				t.Fatalf("newWithConfig() error = %v", err)
			}
			startTurn(t, actor, recorder, textBlocks("start"))
			select {
			case <-compactor.started:
			case <-time.After(2 * time.Second):
				t.Fatal("compaction did not start")
			}
			if tt.shutdown {
				ack := make(chan error, 1)
				actor.Commands <- command.Shutdown{Header: command.Header{CommandID: uuid.UUID{123}}, Ack: ack}
				if err := <-ack; err != nil {
					t.Fatalf("Shutdown error = %v", err)
				}
			} else {
				ack := make(chan bool, 1)
				actor.Commands <- command.Interrupt{Header: command.Header{CommandID: uuid.UUID{123}}, Ack: ack}
				if !<-ack {
					t.Fatal("Interrupt did not cancel paused turn")
				}
			}
			blockUntilEvents(t, recorder, func(events []event.Event) bool {
				for _, published := range events {
					if rejected, ok := published.(event.CompactionRejected); ok && rejected.RejectReason == tt.wantReason {
						return true
					}
				}
				return false
			})
			close(compactor.release)
			select {
			case <-compactor.finished:
			case <-time.After(2 * time.Second):
				t.Fatal("late compactor did not return")
			}
			for _, published := range recorder.events() {
				if _, committed := published.(event.CompactionCommitted); committed {
					t.Fatal("late compactor success committed after cancellation terminal")
				}
			}
			if tt.shutdown {
				return
			}
			if terminal := drainToTerminal(t, recorder); reflect.TypeOf(terminal) != reflect.TypeOf(event.TurnInterrupted{}) {
				t.Fatalf("terminal = %T %+v, want TurnInterrupted", terminal, terminal)
			}
			sendCompact(t, actor, sessionID, loopID, uuid.UUID{124}, identity.AgencyUser)
			blockUntilEvents(t, recorder, func(events []event.Event) bool {
				attempts := make(map[event.CompactAttemptID]struct{})
				for _, published := range events {
					if started, ok := published.(event.CompactionStarted); ok {
						attempts[started.AttemptID] = struct{}{}
					}
				}
				return len(attempts) == 2
			})
		})
	}
}
