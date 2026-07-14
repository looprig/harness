package loopruntime

import (
	"context"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/looprig/core/content"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/command"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/inference"
)

type loopContextCounter struct {
	mu         sync.Mutex
	capability inference.CounterCapability
	counts     []content.TokenCount
	err        error
	requests   []inference.Request
}

func (c *loopContextCounter) CountContext(_ context.Context, request inference.Request) (inference.ContextCount, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.requests = append(c.requests, request)
	if c.err != nil {
		return inference.ContextCount{}, c.err
	}
	index := len(c.requests) - 1
	if index >= len(c.counts) {
		index = len(c.counts) - 1
	}
	return inference.ContextCount{Model: request.Model.Key(), InputTokens: c.counts[index], Quality: c.capability.Quality}, nil
}

func (c *loopContextCounter) CounterCapability() inference.CounterCapability { return c.capability }

func (c *loopContextCounter) requestModels() []inference.ModelKey {
	c.mu.Lock()
	defer c.mu.Unlock()
	models := make([]inference.ModelKey, len(c.requests))
	for index, request := range c.requests {
		models[index] = request.Model.Key()
	}
	return models
}

type contextOrderClient struct {
	mu           sync.Mutex
	recorder     *recordingPublisher
	calls        int
	eventsAtCall []event.Event
}

func (*contextOrderClient) Invoke(context.Context, inference.Request) (*inference.Response, error) {
	return nil, errors.New("contextOrderClient.Invoke not used")
}

func (c *contextOrderClient) Stream(context.Context, inference.Request) (*inference.StreamReader[content.Chunk], error) {
	c.mu.Lock()
	c.calls++
	c.eventsAtCall = c.recorder.events()
	c.mu.Unlock()
	emitted := false
	return inference.NewStreamReader(func() (content.Chunk, error) {
		if !emitted {
			emitted = true
			return textChunk("done"), nil
		}
		return nil, io.EOF
	}, nil), nil
}

func (c *contextOrderClient) snapshot() (int, []event.Event) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls, append([]event.Event(nil), c.eventsAtCall...)
}

type contextAwaitSink struct {
	started chan compactionDisposition
	release chan contextCompactionAwaitResult
}

func newContextAwaitSink() *contextAwaitSink {
	return &contextAwaitSink{started: make(chan compactionDisposition, 1), release: make(chan contextCompactionAwaitResult, 1)}
}

func (s *contextAwaitSink) CoordinateCompaction(_ context.Context, disposition compactionDisposition) error {
	s.started <- disposition
	return nil
}

func (s *contextAwaitSink) AwaitCompaction(ctx context.Context, _ event.CompactAttemptID) (contextCompactionAwaitResult, error) {
	select {
	case result := <-s.release:
		return result, nil
	case <-ctx.Done():
		return contextCompactionAwaitUnknown, ctx.Err()
	}
}

func TestLoopContextAdmissionBeforePrimaryInference(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name            string
		count           content.TokenCount
		countErr        error
		observation     *loop.ContextObservationPolicy
		compaction      *loop.CompactionPolicy
		withAwaiter     bool
		release         contextCompactionAwaitResult
		wantInference   int
		wantMeasured    bool
		wantPressure    event.PressureLevel
		wantTerminalErr func(error) bool
	}{
		{
			name: "observe below limit measures before inference", count: 40,
			observation:   &loop.ContextObservationPolicy{ReservedOutput: 20, CountTimeout: 31 * time.Millisecond},
			wantInference: 1, wantMeasured: true, wantPressure: event.PressureNormal,
		},
		{
			name: "observe hard limit blocks inference", count: 80,
			observation:  &loop.ContextObservationPolicy{ReservedOutput: 20, CountTimeout: 31 * time.Millisecond},
			wantMeasured: true, wantPressure: event.PressureHardLimit,
			wantTerminalErr: func(err error) bool { var target *loop.ContextLimitError; return errors.As(err, &target) },
		},
		{
			name: "count failure blocks inference without measurement", countErr: errors.New("count failed"),
			observation:     &loop.ContextObservationPolicy{ReservedOutput: 20, CountTimeout: 31 * time.Millisecond},
			wantTerminalErr: func(err error) bool { var target *inference.ContextCountError; return errors.As(err, &target) },
		},
		{
			name: "automatic soft rejection continues", count: 65,
			compaction:  &loop.CompactionPolicy{Automatic: true, CounterPolicy: loop.CounterPolicyRequireExact, CompactAt: 8_000, RearmBelow: 6_000, ReservedOutput: 20, MaxSummaryTokens: 10, CountTimeout: 31 * time.Millisecond, Hustle: "context.compact"},
			withAwaiter: true, release: contextCompactionAwaitRejected,
			wantInference: 1, wantMeasured: true, wantPressure: event.PressureCompact,
		},
		{
			name: "automatic hard rejection blocks after real attempt", count: 80,
			compaction:  &loop.CompactionPolicy{Automatic: true, CounterPolicy: loop.CounterPolicyRequireExact, CompactAt: 8_000, RearmBelow: 6_000, ReservedOutput: 20, MaxSummaryTokens: 10, CountTimeout: 31 * time.Millisecond, Hustle: "context.compact"},
			withAwaiter: true, release: contextCompactionAwaitRejected,
			wantMeasured: true, wantPressure: event.PressureHardLimit,
			wantTerminalErr: func(err error) bool { var target *loop.ContextLimitError; return errors.As(err, &target) },
		},
		{
			name: "automatic hard without real attempt blocks immediately", count: 80,
			compaction:   &loop.CompactionPolicy{Automatic: true, CounterPolicy: loop.CounterPolicyRequireExact, CompactAt: 8_000, RearmBelow: 6_000, ReservedOutput: 20, MaxSummaryTokens: 10, CountTimeout: 31 * time.Millisecond, Hustle: "context.compact"},
			wantMeasured: true, wantPressure: event.PressureHardLimit,
			wantTerminalErr: func(err error) bool { var target *loop.ContextLimitError; return errors.As(err, &target) },
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)
			recorder := &recordingPublisher{}
			client := &contextOrderClient{recorder: recorder}
			counter := &loopContextCounter{capability: contextTestCapability(inference.CountQualityExactLocal), counts: []content.TokenCount{tt.count}, err: tt.countErr}
			model := testModel()
			model.Limits = inference.ContextLimits{WindowTokens: 100, MaxInputTokens: 80, MaxOutputTokens: 20}
			config := runtimeConfig{
				Client: client, Model: model, System: "system", DrainTimeout: 200 * time.Millisecond,
				ContextCounter: counter, CounterCapability: counter.capability, InferenceCapability: contextTestInferenceCapability(),
				ContextObservation: tt.observation, Compaction: tt.compaction,
			}
			var sink *contextAwaitSink
			if tt.withAwaiter {
				sink = newContextAwaitSink()
				config.compactionSink = sink
			}
			actor, err := newWithConfig(ctx, uuid.UUID{1}, uuid.UUID{2}, Provenance{}, recorder, config)
			if err != nil {
				t.Fatalf("newWithConfig() error = %v", err)
			}
			startTurn(t, actor, recorder, []content.Block{&content.TextBlock{Text: "hello"}})
			if sink != nil {
				select {
				case disposition := <-sink.started:
					if disposition.Kind != compactionDispositionStart || disposition.Attempt == nil {
						t.Fatalf("automatic disposition = %+v, want real start", disposition)
					}
					if calls, _ := client.snapshot(); calls != 0 {
						t.Fatalf("primary inference calls before automatic disposition = %d, want 0", calls)
					}
					sink.release <- tt.release
				case <-time.After(2 * time.Second):
					t.Fatal("automatic compaction was not coordinated")
				}
			}
			terminal := drainToTerminal(t, recorder)
			calls, atCall := client.snapshot()
			if calls != tt.wantInference {
				t.Fatalf("primary inference calls = %d, want %d", calls, tt.wantInference)
			}
			failed, failedTurn := terminal.(event.TurnFailed)
			if tt.wantTerminalErr != nil {
				if !failedTurn || !tt.wantTerminalErr(failed.Err) {
					t.Fatalf("terminal = %T %+v, want typed TurnFailed", terminal, terminal)
				}
			} else if _, ok := terminal.(event.TurnDone); !ok {
				t.Fatalf("terminal = %T, want TurnDone", terminal)
			}
			measured, pressure := contextEvents(recorder.events())
			if (measured != nil) != tt.wantMeasured {
				t.Fatalf("ContextMeasured present = %v, want %v", measured != nil, tt.wantMeasured)
			}
			if tt.wantPressure != event.PressureUnknown && (pressure == nil || pressure.Current != tt.wantPressure) {
				t.Fatalf("ContextPressure = %+v, want current %v", pressure, tt.wantPressure)
			}
			if calls > 0 {
				atMeasured, atPressure := contextEvents(atCall)
				if atMeasured == nil || atPressure == nil {
					t.Fatal("primary inference began before ContextMeasured and ContextPressure publication")
				}
			}
		})
	}
}

func TestLoopContextAdmissionRecountsAfterSmallerModelChange(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
	}{
		{name: "smaller model is recounted and blocked"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)
			recorder := &recordingPublisher{}
			client := &contextOrderClient{recorder: recorder}
			counter := &loopContextCounter{
				capability: contextTestCapability(inference.CountQualityExactLocal),
				counts:     []content.TokenCount{100, 100},
			}
			large := testModel()
			large.Name = "large"
			large.Limits = inference.ContextLimits{WindowTokens: 200, MaxInputTokens: 180, MaxOutputTokens: 20}
			config := runtimeConfig{
				Client: client, Model: large, System: "system", DrainTimeout: 200 * time.Millisecond,
				ContextCounter: counter, CounterCapability: counter.capability, InferenceCapability: contextTestInferenceCapability(),
				ContextObservation: &loop.ContextObservationPolicy{ReservedOutput: 20, CountTimeout: 31 * time.Millisecond},
			}
			actor, err := newWithConfig(ctx, uuid.UUID{3}, uuid.UUID{4}, Provenance{}, recorder, config)
			if err != nil {
				t.Fatalf("newWithConfig() error = %v", err)
			}
			startTurn(t, actor, recorder, []content.Block{&content.TextBlock{Text: "first"}})
			if terminal := drainToTerminal(t, recorder); terminal == nil {
				t.Fatal("first turn produced no terminal")
			}
			smaller := large
			smaller.Name = "small"
			smaller.Limits = inference.ContextLimits{WindowTokens: 100, MaxInputTokens: 80, MaxOutputTokens: 20}
			if result := sendChange(t, actor, command.ChangeLoopInference{Model: smaller, SetModel: true}); result.Err != nil {
				t.Fatalf("ChangeLoopInference error = %v", result.Err)
			}
			from := len(recorder.events())
			startTurn(t, actor, recorder, []content.Block{&content.TextBlock{Text: "second"}})
			terminal := awaitTerminalAfter(t, recorder, from)
			failed, ok := terminal.(event.TurnFailed)
			var limitErr *loop.ContextLimitError
			if !ok || !errors.As(failed.Err, &limitErr) || limitErr.Measurement.Model != smaller.Key() {
				t.Fatalf("second terminal = %T %+v, want smaller-model ContextLimitError", terminal, terminal)
			}
			if calls, _ := client.snapshot(); calls != 1 {
				t.Fatalf("primary inference calls = %d, want only first turn", calls)
			}
			models := counter.requestModels()
			if len(models) != 2 || models[0] != large.Key() || models[1] != smaller.Key() {
				t.Fatalf("counted models = %+v, want [%+v %+v]", models, large.Key(), smaller.Key())
			}
		})
	}
}

func contextEvents(events []event.Event) (*event.ContextMeasured, *event.ContextPressure) {
	var measured *event.ContextMeasured
	var pressure *event.ContextPressure
	for _, value := range events {
		switch typed := value.(type) {
		case event.ContextMeasured:
			copy := typed
			measured = &copy
		case event.ContextPressure:
			copy := typed
			pressure = &copy
		}
	}
	return measured, pressure
}
