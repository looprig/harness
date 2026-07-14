package loopruntime

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/looprig/core/content"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/identity"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/inference"
)

type contextTestCounter struct {
	capability inference.CounterCapability
	count      inference.ContextCount
	err        error
	block      bool
	request    inference.Request
	deadline   time.Time
	calledAt   time.Time
}

func (c *contextTestCounter) CountContext(ctx context.Context, request inference.Request) (inference.ContextCount, error) {
	c.calledAt = time.Now()
	c.request = request
	c.deadline, _ = ctx.Deadline()
	if c.block {
		<-ctx.Done()
		return inference.ContextCount{}, ctx.Err()
	}
	if c.err != nil {
		return inference.ContextCount{}, c.err
	}
	return c.count, nil
}

func (c *contextTestCounter) CounterCapability() inference.CounterCapability { return c.capability }

func contextTestCapability(quality inference.CountQuality) inference.CounterCapability {
	return inference.CounterCapability{Transport: inference.CounterTransportLocal, Retention: inference.RetentionNone, TokenizerRev: "context-test-v1", Quality: quality}
}

func contextTestInferenceCapability() inference.InferenceCapability {
	return inference.InferenceCapability{Transport: inference.InferenceTransportLocal, Retention: inference.RetentionNone}
}

func contextTestRequest() inference.Request {
	return inference.Request{
		Model:    inference.Model{Provider: "test", APIFormat: inference.APIFormatOpenAI, BaseURL: "http://localhost:1234", Name: "primary", Limits: inference.ContextLimits{WindowTokens: 120, MaxInputTokens: 100, MaxOutputTokens: 30}},
		System:   "system",
		Messages: content.AgenticMessages{&content.UserMessage{Message: content.Message{Role: content.RoleUser, Blocks: []content.Block{&content.TextBlock{Text: "hello"}}}}},
		Tools:    []inference.Tool{{Name: "read", Description: "read a file", Schema: []byte(`{"type":"object"}`)}},
	}
}

func TestMeasureRequestContextCountsCompleteCandidate(t *testing.T) {
	t.Parallel()
	request := contextTestRequest()
	basis := event.ContextBasis{Revision: 2, ThroughEventID: uuid.UUID{2}}
	timeout := 37*time.Millisecond + time.Nanosecond
	settings := contextAdmissionSettings{ReservedOutput: 20, SafetyMargin: 5, CountTimeout: timeout}
	tests := []struct {
		name       string
		counter    func() *contextTestCounter
		ctx        func() (context.Context, context.CancelFunc)
		request    inference.Request
		wantErr    func(error) bool
		wantLimit  content.TokenCount
		wantTokens content.TokenCount
	}{
		{
			name: "complete request and exact limit",
			counter: func() *contextTestCounter {
				return &contextTestCounter{capability: contextTestCapability(inference.CountQualityExactLocal), count: inference.ContextCount{Model: request.Model.Key(), InputTokens: 61, Quality: inference.CountQualityExactLocal}}
			},
			ctx:     func() (context.Context, context.CancelFunc) { return context.WithCancel(context.Background()) },
			request: request, wantLimit: 95, wantTokens: 61,
		},
		{
			name: "unknown limit fails closed before count",
			counter: func() *contextTestCounter {
				return &contextTestCounter{capability: contextTestCapability(inference.CountQualityExactLocal), count: inference.ContextCount{Model: request.Model.Key(), InputTokens: 1, Quality: inference.CountQualityExactLocal}}
			},
			ctx: func() (context.Context, context.CancelFunc) { return context.WithCancel(context.Background()) },
			request: func() inference.Request {
				value := request
				value.Model.Limits = inference.ContextLimits{}
				return value
			}(),
			wantErr: func(err error) bool { var target *loop.ContextLimitUnknownError; return errors.As(err, &target) },
		},
		{
			name: "counter failure is typed",
			counter: func() *contextTestCounter {
				return &contextTestCounter{capability: contextTestCapability(inference.CountQualityExactLocal), err: errors.New("counter unavailable")}
			},
			ctx:     func() (context.Context, context.CancelFunc) { return context.WithCancel(context.Background()) },
			request: request,
			wantErr: func(err error) bool { var target *inference.ContextCountError; return errors.As(err, &target) },
		},
		{
			name: "timeout is typed",
			counter: func() *contextTestCounter {
				return &contextTestCounter{capability: contextTestCapability(inference.CountQualityExactLocal), block: true}
			},
			ctx:     func() (context.Context, context.CancelFunc) { return context.WithCancel(context.Background()) },
			request: request,
			wantErr: func(err error) bool {
				var target *inference.ContextCountError
				return errors.As(err, &target) && errors.Is(err, context.DeadlineExceeded)
			},
		},
		{
			name: "parent cancellation is typed",
			counter: func() *contextTestCounter {
				return &contextTestCounter{capability: contextTestCapability(inference.CountQualityExactLocal), block: true}
			},
			ctx: func() (context.Context, context.CancelFunc) {
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				return ctx, func() {}
			},
			request: request,
			wantErr: func(err error) bool {
				var target *inference.ContextCountError
				return errors.As(err, &target) && errors.Is(err, context.Canceled)
			},
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := tt.ctx()
			defer cancel()
			counter := tt.counter()
			measurement, err := measureRequestContext(ctx, counter, counter.capability, contextTestInferenceCapability(), settings, basis, tt.request, "runtime-v1")
			if tt.wantErr != nil {
				if !tt.wantErr(err) {
					t.Fatalf("measureRequestContext() error = %T %v", err, err)
				}
				if _, ok := err.(*loop.ContextLimitUnknownError); ok && counter.request.Model.Name != "" {
					t.Fatal("counter called despite unknown hard limit")
				}
				return
			}
			if err != nil {
				t.Fatalf("measureRequestContext() error = %v", err)
			}
			if !reflect.DeepEqual(counter.request, tt.request) {
				t.Fatalf("counted request = %#v, want complete candidate %#v", counter.request, tt.request)
			}
			if got := counter.deadline.Sub(counter.calledAt); got <= 0 || got > timeout {
				t.Fatalf("counter deadline offset = %v, want within exact timeout %v", got, timeout)
			}
			if measurement.Basis != basis || measurement.Model != tt.request.Model.Key() || measurement.InputTokens != tt.wantTokens || measurement.InputLimit != tt.wantLimit || measurement.Quality != counter.capability.Quality || measurement.RequestFingerprint == ([32]byte{}) {
				t.Fatalf("measurement = %+v, want basis/model/tokens/limit/quality fidelity", measurement)
			}
		})
	}
}

func TestContextTrackerPressureRearmAndAutomaticLatch(t *testing.T) {
	t.Parallel()
	type sample struct {
		basis       byte
		used        content.TokenCount
		wantLevel   event.PressureLevel
		wantChanged bool
		wantAuto    bool
		wantHardErr bool
	}
	tests := []struct {
		name     string
		settings contextAdmissionSettings
		samples  []sample
	}{
		{
			name:     "observe only never enters compact",
			settings: contextAdmissionSettings{ReservedOutput: 1, CountTimeout: time.Second},
			samples: []sample{
				{basis: 1, used: 50, wantLevel: event.PressureNormal, wantChanged: true},
				{basis: 1, used: 50, wantLevel: event.PressureNormal},
				{basis: 2, used: 100, wantLevel: event.PressureHardLimit, wantChanged: true, wantHardErr: true},
				{basis: 3, used: 50, wantLevel: event.PressureNormal, wantChanged: true},
			},
		},
		{
			name:     "automatic hysteresis and one attempt per basis",
			settings: contextAdmissionSettings{ReservedOutput: 1, CountTimeout: time.Second, Automatic: true, CompactAt: 8_000, RearmBelow: 6_000},
			samples: []sample{
				{basis: 1, used: 79, wantLevel: event.PressureNormal, wantChanged: true},
				{basis: 2, used: 80, wantLevel: event.PressureCompact, wantChanged: true, wantAuto: true},
				{basis: 2, used: 80, wantLevel: event.PressureCompact, wantAuto: true},
				{basis: 3, used: 70, wantLevel: event.PressureCompact, wantChanged: true, wantAuto: true},
				{basis: 4, used: 59, wantLevel: event.PressureNormal, wantChanged: true},
				{basis: 5, used: 100, wantLevel: event.PressureHardLimit, wantChanged: true, wantAuto: true},
			},
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			tracker := contextTracker{}
			for i, sample := range tt.samples {
				measurement := event.ContextMeasurement{
					Basis: event.ContextBasis{Revision: event.ContextRevision(sample.basis), ThroughEventID: uuid.UUID{sample.basis}}, Model: inference.ModelKey{Provider: "test", Model: "primary"},
					RequestFingerprint: [32]byte{sample.basis}, InputTokens: sample.used, InputLimit: 100, Quality: inference.CountQualityExactLocal,
				}
				result, err := tracker.apply(measurement, tt.settings)
				if err != nil {
					t.Fatalf("sample %d apply() error = %v", i, err)
				}
				if result.Current != sample.wantLevel || result.MeasurementChanged != sample.wantChanged || result.TriggerAutomatic != sample.wantAuto {
					t.Fatalf("sample %d result = %+v, want level=%v changed=%v auto=%v", i, result, sample.wantLevel, sample.wantChanged, sample.wantAuto)
				}
				if sample.wantHardErr {
					var target *loop.ContextLimitError
					if !errors.As(result.AdmissionError, &target) || target.Measurement != measurement {
						t.Fatalf("sample %d admission error = %T %v, want ContextLimitError", i, result.AdmissionError, result.AdmissionError)
					}
				} else if result.AdmissionError != nil {
					t.Fatalf("sample %d admission error = %v", i, result.AdmissionError)
				}
			}
		})
	}
}

func TestContextTrackerExhaustsOnlyDurableAutomaticRejection(t *testing.T) {
	t.Parallel()
	basis := event.ContextBasis{Revision: 5, ThroughEventID: uuid.UUID{5}}
	measurement := event.ContextMeasurement{
		Basis: basis, Model: inference.ModelKey{Provider: "test", Model: "primary"},
		RequestFingerprint: [32]byte{5}, InputTokens: 80, InputLimit: 100, Quality: inference.CountQualityExactLocal,
	}
	settings := contextAdmissionSettings{Automatic: true, CompactAt: 8_000, RearmBelow: 6_000}
	tests := []struct {
		name      string
		configure func(*contextTracker)
		wantAuto  bool
	}{
		{name: "pending opener alone remains eligible", wantAuto: true},
		{name: "manual canonical rejection remains eligible", configure: func(tracker *contextTracker) { tracker.exhaustAutomatic(basis, event.CompactionReasonManual, true) }, wantAuto: true},
		{name: "pre-start automatic waiter rejection remains eligible", configure: func(tracker *contextTracker) { tracker.exhaustAutomatic(basis, event.CompactionReasonAutomatic, false) }, wantAuto: true},
		{name: "durable automatic canonical rejection exhausts basis", configure: func(tracker *contextTracker) { tracker.exhaustAutomatic(basis, event.CompactionReasonAutomatic, true) }},
		{name: "later basis remains eligible", configure: func(tracker *contextTracker) {
			tracker.exhaustAutomatic(event.ContextBasis{Revision: 4, ThroughEventID: uuid.UUID{4}}, event.CompactionReasonAutomatic, true)
		}, wantAuto: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tracker := contextTracker{}
			if tt.configure != nil {
				tt.configure(&tracker)
			}
			result, err := tracker.apply(measurement, settings)
			if err != nil {
				t.Fatalf("apply() error = %v", err)
			}
			if result.TriggerAutomatic != tt.wantAuto {
				t.Fatalf("TriggerAutomatic = %v, want %v", result.TriggerAutomatic, tt.wantAuto)
			}
		})
	}
}

func TestLiveAutomaticPreStartControlRejectionLeavesBasisEligible(t *testing.T) {
	t.Parallel()
	basis := event.ContextBasis{Revision: 5, ThroughEventID: uuid.UUID{5}}
	measurement := event.ContextMeasurement{
		Basis: basis, Model: inference.ModelKey{Provider: "test", Model: "primary"},
		RequestFingerprint: [32]byte{5}, InputTokens: 80, InputLimit: 100, Quality: inference.CountQualityExactLocal,
	}
	settings := contextAdmissionSettings{Automatic: true, CompactAt: 8_000, RearmBelow: 6_000}
	tests := []struct {
		name   string
		cancel func(*compactionControl)
		reason event.CompactRejectReason
	}{
		{name: "interrupt", cancel: func(control *compactionControl) { control.interrupt() }, reason: event.CompactRejectInterrupted},
		{name: "shutdown", cancel: func(control *compactionControl) { control.shutdown() }, reason: event.CompactRejectShuttingDown},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tracker := contextTracker{}
			first, err := tracker.apply(measurement, settings)
			if err != nil || !first.TriggerAutomatic {
				t.Fatalf("first apply = %+v, %v; want automatic trigger", first, err)
			}
			control := newCompactionControl(2)
			if _, err := control.admit(compactCommand(uuid.UUID{1}, time.Now(), identity.AgencyMachine), fixedCompactionID); err != nil {
				t.Fatalf("admit() error = %v", err)
			}
			tt.cancel(control)
			disposition := control.atBoundary(compactionBoundaryStep)
			if disposition.Kind != compactionDispositionReject || disposition.RejectReason != tt.reason || control.pending != nil {
				t.Fatalf("control disposition = %+v pending=%+v, want rejection and cleared slot", disposition, control.pending)
			}
			second, err := tracker.apply(measurement, settings)
			if err != nil || !second.TriggerAutomatic {
				t.Fatalf("second apply = %+v, %v; unchanged basis should remain eligible", second, err)
			}
		})
	}
}

func TestContextTrackerRestoreSuppressesOnlyRecordedAutomaticBasis(t *testing.T) {
	t.Parallel()
	settings := contextAdmissionSettings{Automatic: true, CompactAt: 8_000, RearmBelow: 6_000}
	basis := event.ContextBasis{Revision: 5, ThroughEventID: uuid.UUID{5}}
	measurement := event.ContextMeasurement{
		Basis: basis, Model: inference.ModelKey{Provider: "test", Model: "primary"},
		RequestFingerprint: [32]byte{5}, InputTokens: 80, InputLimit: 100, Quality: inference.CountQualityExactLocal,
	}
	tests := []struct {
		name         string
		automatic    event.ContextBasis
		hasAutomatic bool
		wantTrigger  bool
	}{
		{name: "same durable automatic rejection suppresses", automatic: basis, hasAutomatic: true},
		{name: "manual or absent rejection remains eligible", wantTrigger: true},
		{name: "older automatic basis remains eligible", automatic: event.ContextBasis{Revision: 4, ThroughEventID: uuid.UUID{4}}, hasAutomatic: true, wantTrigger: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tracker := contextTracker{}
			if err := tracker.restore(basis, true, event.ContextMeasurement{}, false, tt.automatic, tt.hasAutomatic, settings); err != nil {
				t.Fatalf("restore() error = %v", err)
			}
			result, err := tracker.apply(measurement, settings)
			if err != nil {
				t.Fatalf("apply() error = %v", err)
			}
			if result.TriggerAutomatic != tt.wantTrigger {
				t.Fatalf("TriggerAutomatic = %v, want %v", result.TriggerAutomatic, tt.wantTrigger)
			}
		})
	}
}

func TestValidateContextCompactionProposal(t *testing.T) {
	t.Parallel()
	attemptID := event.CompactAttemptID(uuid.UUID{1})
	basis := event.ContextBasis{Revision: 3, ThroughEventID: uuid.UUID{3}}
	automatic := &compactionAttempt{
		AttemptID: attemptID, WaiterCommandIDs: []uuid.UUID{{2}}, Reason: event.CompactionReasonAutomatic,
		Basis: basis, StartedAt: time.Now(),
	}
	success := &compactionPreparedSuccess{Summary: validFinalizationSummary(), PostContext: validFinalizationMeasurement(8)}
	tests := []struct {
		name    string
		attempt *compactionAttempt
		result  contextCompactionAwaitResult
		wantErr bool
	}{
		{name: "valid rejection proposal", attempt: automatic, result: contextCompactionAwaitResult{Disposition: contextCompactionAwaitRejected, Proposal: compactionFinalizationProposal{RejectReason: event.CompactRejectExecutionFailed}}},
		{name: "valid prepared success proposal", attempt: automatic, result: contextCompactionAwaitResult{Disposition: contextCompactionAwaitCommitted, Proposal: compactionFinalizationProposal{Success: success}}},
		{name: "missing actor attempt", result: contextCompactionAwaitResult{Disposition: contextCompactionAwaitRejected, Proposal: compactionFinalizationProposal{RejectReason: event.CompactRejectExecutionFailed}}, wantErr: true},
		{name: "unknown disposition", attempt: automatic, result: contextCompactionAwaitResult{Proposal: compactionFinalizationProposal{RejectReason: event.CompactRejectExecutionFailed}}, wantErr: true},
		{name: "rejected disposition cannot carry success", attempt: automatic, result: contextCompactionAwaitResult{Disposition: contextCompactionAwaitRejected, Proposal: compactionFinalizationProposal{Success: success}}, wantErr: true},
		{name: "committed disposition cannot carry rejection", attempt: automatic, result: contextCompactionAwaitResult{Disposition: contextCompactionAwaitCommitted, Proposal: compactionFinalizationProposal{RejectReason: event.CompactRejectExecutionFailed}}, wantErr: true},
		{name: "proposal must carry exactly one outcome", attempt: automatic, result: contextCompactionAwaitResult{Disposition: contextCompactionAwaitRejected}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateContextCompactionProposal(tt.attempt, tt.result)
			if (err != nil) != tt.wantErr {
				t.Fatalf("validateContextCompactionProposal() error = %T %v, wantErr=%v", err, err, tt.wantErr)
			}
			if tt.wantErr {
				var typed *contextCompactionOutcomeError
				if !errors.As(err, &typed) {
					t.Fatalf("error = %T %v, want *contextCompactionOutcomeError", err, err)
				}
			}
		})
	}
}

func TestPreflightContextMutationRejectsOverflowWithoutMutation(t *testing.T) {
	t.Parallel()
	maxRevision := ^event.ContextRevision(0)
	maxGeneration := ^uint64(0)
	tests := []struct {
		name              string
		basis             event.ContextBasis
		generation        uint64
		kind              contextMutationKind
		wantRevisionError bool
		wantGenerationErr bool
	}{
		{name: "TurnStarted revision overflow", basis: event.ContextBasis{Revision: maxRevision, ThroughEventID: uuid.UUID{1}}},
		{name: "StepDone revision overflow", basis: event.ContextBasis{Revision: maxRevision, ThroughEventID: uuid.UUID{1}}},
		{name: "TurnFoldedInto revision overflow", basis: event.ContextBasis{Revision: maxRevision, ThroughEventID: uuid.UUID{1}}},
		{name: "LoopModeChanged revision overflow", basis: event.ContextBasis{Revision: maxRevision, ThroughEventID: uuid.UUID{1}}, kind: contextMutationRequestShape},
		{name: "LoopInferenceChanged revision overflow", basis: event.ContextBasis{Revision: maxRevision, ThroughEventID: uuid.UUID{1}}, kind: contextMutationRequestShape},
		{name: "LoopModeChanged generation overflow", basis: event.ContextBasis{Revision: 1, ThroughEventID: uuid.UUID{1}}, generation: maxGeneration, kind: contextMutationRequestShape, wantGenerationErr: true},
		{name: "LoopInferenceChanged generation overflow", basis: event.ContextBasis{Revision: 1, ThroughEventID: uuid.UUID{1}}, generation: maxGeneration, kind: contextMutationRequestShape, wantGenerationErr: true},
	}
	for index := range tests {
		tests[index].wantRevisionError = tests[index].basis.Revision == maxRevision
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tracker := contextTracker{basis: tt.basis}
			before := tracker
			_, err := preflightContextMutation(tracker, tt.generation, uuid.UUID{2}, tt.kind)
			if err == nil {
				t.Fatal("preflightContextMutation() error = nil, want overflow")
			}
			var revisionErr *contextRevisionOverflowError
			var generationErr *contextGenerationOverflowError
			if errors.As(err, &revisionErr) != tt.wantRevisionError {
				t.Fatalf("revision error = %v, want %v: %T %v", errors.As(err, &revisionErr), tt.wantRevisionError, err, err)
			}
			if errors.As(err, &generationErr) != tt.wantGenerationErr {
				t.Fatalf("generation error = %v, want %v: %T %v", errors.As(err, &generationErr), tt.wantGenerationErr, err, err)
			}
			if tracker != before {
				t.Fatalf("tracker mutated on overflow: got %+v want %+v", tracker, before)
			}
		})
	}
}
