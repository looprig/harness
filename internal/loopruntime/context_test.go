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
				{basis: 2, used: 80, wantLevel: event.PressureCompact},
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
