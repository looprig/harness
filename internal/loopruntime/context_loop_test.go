package loopruntime

import (
	"context"
	"errors"
	"fmt"
	"io"
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

type loopContextCounter struct {
	mu         sync.Mutex
	capability inference.CounterCapability
	counts     []content.TokenCount
	err        error
	requests   []inference.Request
}

type gatedLoopContextCounter struct {
	capability inference.CounterCapability
	counts     []content.TokenCount
	started    chan inference.Request
	release    chan struct{}
	mu         sync.Mutex
	requests   []inference.Request
}

func (c *gatedLoopContextCounter) CountContext(ctx context.Context, request inference.Request) (inference.ContextCount, error) {
	c.mu.Lock()
	call := len(c.requests)
	c.requests = append(c.requests, request)
	c.mu.Unlock()
	if call == 0 {
		c.started <- request
		select {
		case <-c.release:
		case <-ctx.Done():
			return inference.ContextCount{}, ctx.Err()
		}
	}
	count := content.TokenCount(40)
	if len(c.counts) > 0 {
		index := call
		if index >= len(c.counts) {
			index = len(c.counts) - 1
		}
		count = c.counts[index]
	}
	return inference.ContextCount{Model: request.Model.Key(), InputTokens: count, Quality: c.capability.Quality}, nil
}

func (c *gatedLoopContextCounter) CounterCapability() inference.CounterCapability {
	return c.capability
}

func (c *gatedLoopContextCounter) models() []inference.ModelKey {
	c.mu.Lock()
	defer c.mu.Unlock()
	models := make([]inference.ModelKey, len(c.requests))
	for index, request := range c.requests {
		models[index] = request.Model.Key()
	}
	return models
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
	requests     []inference.Request
}

func (*contextOrderClient) Invoke(context.Context, inference.Request) (*inference.Response, error) {
	return nil, errors.New("contextOrderClient.Invoke not used")
}

func (c *contextOrderClient) Stream(_ context.Context, request inference.Request) (*inference.StreamReader[content.Chunk], error) {
	c.mu.Lock()
	c.calls++
	c.eventsAtCall = c.recorder.events()
	request.Messages = cloneMessages(request.Messages)
	c.requests = append(c.requests, request)
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

func (c *contextOrderClient) requestSnapshot() []inference.Request {
	c.mu.Lock()
	defer c.mu.Unlock()
	requests := append([]inference.Request(nil), c.requests...)
	for index := range requests {
		requests[index].Messages = cloneMessages(requests[index].Messages)
	}
	return requests
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
		return contextCompactionAwaitResult{Disposition: contextCompactionAwaitUnknown}, ctx.Err()
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
			withAwaiter: true, release: contextCompactionAwaitResult{
				Disposition: contextCompactionAwaitRejected,
				Proposal:    compactionFinalizationProposal{RejectReason: event.CompactRejectExecutionFailed},
			},
			wantInference: 1, wantMeasured: true, wantPressure: event.PressureCompact,
		},
		{
			name: "automatic hard rejection blocks after real attempt", count: 80,
			compaction:  &loop.CompactionPolicy{Automatic: true, CounterPolicy: loop.CounterPolicyRequireExact, CompactAt: 8_000, RearmBelow: 6_000, ReservedOutput: 20, MaxSummaryTokens: 10, CountTimeout: 31 * time.Millisecond, Hustle: "context.compact"},
			withAwaiter: true, release: contextCompactionAwaitResult{
				Disposition: contextCompactionAwaitRejected,
				Proposal:    compactionFinalizationProposal{RejectReason: event.CompactRejectExecutionFailed},
			},
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
			counts := []content.TokenCount{tt.count}
			if tt.name == "automatic soft rejection continues" {
				counts = append(counts, 40)
			}
			counter := &loopContextCounter{capability: contextTestCapability(inference.CountQualityExactLocal), counts: counts, err: tt.countErr}
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
			if tt.wantPressure != event.PressureUnknown && !hasContextPressure(recorder.events(), tt.wantPressure) {
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
			if len(models) != 3 || models[0] != large.Key() || models[1] != large.Key() || models[2] != smaller.Key() {
				t.Fatalf("counted models = %+v, want [%+v %+v %+v]", models, large.Key(), large.Key(), smaller.Key())
			}
		})
	}
}

func TestLoopContextAdmissionRejectsStaleCountAcrossRuntimeChange(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		apply func(*testing.T, *Loop, inference.Model) error
	}{
		{
			name: "inference change",
			apply: func(t *testing.T, actor *Loop, changed inference.Model) error {
				t.Helper()
				return sendChange(t, actor, command.ChangeLoopInference{Model: changed, SetModel: true}).Err
			},
		},
		{
			name: "mode change",
			apply: func(t *testing.T, actor *Loop, _ inference.Model) error {
				t.Helper()
				return sendSetMode(t, actor, "changed").Err
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			capability := contextTestCapability(inference.CountQualityExactLocal)
			counter := &gatedLoopContextCounter{capability: capability, started: make(chan inference.Request, 1), release: make(chan struct{})}
			client := &contextOrderClient{}
			base := testModel()
			base.Name = "base"
			base.Limits = inference.ContextLimits{WindowTokens: 100, MaxInputTokens: 80, MaxOutputTokens: 20}
			changed := base
			changed.Name = "changed"
			definition, err := loop.Define(
				loop.WithName("agent"), loop.WithInference(client, base),
				loop.WithModes(loop.Mode{Name: "base"}, loop.Mode{Name: "changed", Model: changed}),
				loop.WithInitialMode("base"),
				loop.WithContextCounter(counter),
				loop.WithInferenceCapability(contextTestInferenceCapability()),
				loop.WithContextObservation(loop.ContextObservationPolicy{ReservedOutput: 20, CountTimeout: time.Second}),
			)
			if err != nil {
				t.Fatalf("Define() error = %v", err)
			}
			bound, err := definition.Bind(context.Background(), tool.Bindings{SessionID: mustID(t), LoopID: mustID(t)})
			if err != nil {
				t.Fatalf("Bind() error = %v", err)
			}
			actor, recorder := newBoundLoop(t, client, bound)
			client.recorder = recorder
			startTurn(t, actor, recorder, []content.Block{&content.TextBlock{Text: "first"}})
			select {
			case request := <-counter.started:
				if request.Model.Key() != base.Key() {
					t.Fatalf("first counted model = %+v, want %+v", request.Model.Key(), base.Key())
				}
			case <-time.After(2 * time.Second):
				t.Fatal("first count did not start")
			}
			if err := tt.apply(t, actor, changed); err != nil {
				t.Fatalf("runtime change error = %v", err)
			}
			close(counter.release)
			terminal := drainToTerminal(t, recorder)
			if _, ok := terminal.(event.TurnFailed); !ok {
				t.Fatalf("stale-count terminal = %T, want TurnFailed", terminal)
			}
			for _, value := range recorder.events() {
				if measured, ok := value.(event.ContextMeasured); ok && measured.Measurement.Model == base.Key() {
					t.Fatal("old-model ContextMeasured published after runtime change")
				}
			}
			from := len(recorder.events())
			startTurn(t, actor, recorder, []content.Block{&content.TextBlock{Text: "second"}})
			if terminal := awaitTerminalAfter(t, recorder, from); terminal == nil {
				t.Fatal("second turn produced no terminal")
			}
			models := counter.models()
			if len(models) != 3 || models[0] != base.Key() || models[1] != changed.Key() || models[2] != changed.Key() {
				t.Fatalf("counted models = %+v, want [%+v %+v %+v]", models, base.Key(), changed.Key(), changed.Key())
			}
		})
	}
}

func TestRestoredLoopAdvancesBasisWithoutCurrentMeasurement(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
	}{
		{name: "next live turn advances restored independent basis"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)
			recorder := &recordingPublisher{}
			client := &contextOrderClient{recorder: recorder}
			counter := &loopContextCounter{capability: contextTestCapability(inference.CountQualityExactLocal), counts: []content.TokenCount{40}}
			model := testModel()
			model.Limits = inference.ContextLimits{WindowTokens: 100, MaxInputTokens: 80, MaxOutputTokens: 20}
			config := runtimeConfig{
				Client: client, Model: model, System: "system", DrainTimeout: 200 * time.Millisecond,
				ContextCounter: counter, CounterCapability: counter.capability, InferenceCapability: contextTestInferenceCapability(),
				ContextObservation: &loop.ContextObservationPolicy{ReservedOutput: 20, CountTimeout: time.Second},
			}
			restoredBasis := event.ContextBasis{Revision: 10, ThroughEventID: uuid.UUID{10}}
			actor, err := newRestoredWithConfig(ctx, uuid.UUID{5}, uuid.UUID{6}, recorder, config, RestoredState{Basis: restoredBasis, HasBasis: true})
			if err != nil {
				t.Fatalf("newRestoredWithConfig() error = %v", err)
			}
			startTurn(t, actor, recorder, []content.Block{&content.TextBlock{Text: "resume"}})
			if terminal := drainToTerminal(t, recorder); terminal == nil {
				t.Fatal("restored turn produced no terminal")
			}
			measured, _ := contextEvents(recorder.events())
			var committedStep event.StepDone
			for _, value := range recorder.events() {
				if step, ok := value.(event.StepDone); ok {
					committedStep = step
				}
			}
			if measured == nil || measured.Measurement.Basis.Revision != restoredBasis.Revision+2 ||
				measured.Measurement.Basis.ThroughEventID != committedStep.EventID {
				t.Fatalf("restored measurement = %+v, want revision %d through terminal StepDone %s", measured, restoredBasis.Revision+2, committedStep.EventID)
			}
		})
	}
}

func TestRestoredLoopFirstRequestUsesCompactedContextState(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
	}{
		{name: "restored pressure and exhausted basis rearm after first history mutation"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)
			recorder := &recordingPublisher{}
			client := &contextOrderClient{recorder: recorder}
			capability := contextTestCapability(inference.CountQualityExactLocal)
			counter := &loopContextCounter{capability: capability, counts: []content.TokenCount{40, 20}}
			model := testModel()
			model.Limits = inference.ContextLimits{WindowTokens: 100, MaxInputTokens: 80, MaxOutputTokens: 20}
			sink := newContextAwaitSink()
			basis := event.ContextBasis{Revision: 10, ThroughEventID: uuid.UUID{0xd0}}
			measurement := event.ContextMeasurement{
				Basis: basis, Model: model.Key(), RequestFingerprint: [32]byte{0xd1},
				InputTokens: 40, InputLimit: 60, Quality: inference.CountQualityExactLocal,
			}
			summary := seededUser("restored summary")
			restoredHistory := content.AgenticMessages{summary, seededAI("later committed answer")}
			actor, err := newRestoredWithConfig(ctx, uuid.UUID{0xd2}, uuid.UUID{0xd3}, recorder, runtimeConfig{
				Client: client, Model: model, System: "system", DrainTimeout: 200 * time.Millisecond,
				ContextCounter: counter, CounterCapability: capability, InferenceCapability: contextTestInferenceCapability(),
				Compaction: &loop.CompactionPolicy{
					Automatic: true, CounterPolicy: loop.CounterPolicyRequireExact,
					CompactAt: 5_000, RearmBelow: 4_000, ReservedOutput: 20,
					MaxSummaryTokens: 10, CountTimeout: time.Second, Hustle: "context.compact",
				},
				compactionSink: sink,
			}, RestoredState{
				Msgs: restoredHistory, TurnIndex: 3,
				Basis: basis, HasBasis: true, Context: measurement, HasContext: true,
				AutomaticBasis: basis, HasAutomaticBasis: true,
			})
			if err != nil {
				t.Fatal(err)
			}
			startTurn(t, actor, recorder, []content.Block{&content.TextBlock{Text: "new input"}})
			var disposition compactionDisposition
			select {
			case disposition = <-sink.started:
			case <-time.After(2 * time.Second):
				t.Fatal("restored exhausted basis did not rearm after the new TurnStarted basis")
			}
			if disposition.Attempt == nil || disposition.Attempt.Reason != event.CompactionReasonAutomatic || disposition.Attempt.Basis.Revision != basis.Revision+1 {
				t.Fatalf("automatic disposition = %+v, want rearmed revision %d", disposition, basis.Revision+1)
			}
			measured, _ := contextEvents(recorder.events())
			if measured == nil || measured.Measurement.Basis != disposition.Attempt.Basis || measured.Measurement.Model != model.Key() || measured.Measurement.RequestFingerprint == ([32]byte{}) {
				t.Fatalf("first measurement = %+v, want attempt basis, restored model, and derived fingerprint", measured)
			}
			for _, published := range recorder.events() {
				if pressure, ok := published.(event.ContextPressure); ok {
					t.Fatalf("first same-pressure count emitted %+v; restored current measurement pressure was not seeded", pressure)
				}
			}
			sink.release <- contextCompactionAwaitResult{
				Disposition: contextCompactionAwaitRejected,
				Proposal:    compactionFinalizationProposal{RejectReason: event.CompactRejectExecutionFailed},
			}
			deadline := time.Now().Add(2 * time.Second)
			foundTerminal := false
			for !foundTerminal && time.Now().Before(deadline) {
				for _, published := range recorder.events() {
					switch published.(type) {
					case event.TurnDone, event.TurnFailed, event.TurnInterrupted:
						foundTerminal = true
					}
				}
				if !foundTerminal {
					time.Sleep(2 * time.Millisecond)
				}
			}
			if !foundTerminal {
				events := recorder.events()
				types := make([]string, len(events))
				for index, published := range events {
					types[index] = fmt.Sprintf("%T", published)
				}
				t.Fatalf("first restored turn produced no terminal; events=%v", types)
			}
			counter.mu.Lock()
			counted := append([]inference.Request(nil), counter.requests...)
			counter.mu.Unlock()
			if len(counted) == 0 || len(counted[0].Messages) != 3 || !reflect.DeepEqual(content.AgenticMessages(counted[0].Messages[:2]), restoredHistory) {
				t.Fatalf("first counted request messages = %#v, want restored summary plus later history then new user", counted)
			}
			primary := client.requestSnapshot()
			if len(primary) != 1 || len(primary[0].Messages) != 3 || !reflect.DeepEqual(content.AgenticMessages(primary[0].Messages[:2]), restoredHistory) {
				t.Fatalf("first primary request messages = %#v, want restored summary plus later history then new user", primary)
			}
		})
	}
}

func TestLoopAutomaticCompactionRetriesAfterManualOpenedRejection(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
	}{
		{name: "machine join preserves manual opener then opens automatic attempt"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)
			recorder := &recordingPublisher{}
			client := &contextOrderClient{recorder: recorder}
			capability := contextTestCapability(inference.CountQualityExactLocal)
			counter := &gatedLoopContextCounter{
				capability: capability, counts: []content.TokenCount{40, 40, 20},
				started: make(chan inference.Request, 1), release: make(chan struct{}),
			}
			model := testModel()
			model.Limits = inference.ContextLimits{WindowTokens: 100, MaxInputTokens: 80, MaxOutputTokens: 20}
			sink := newContextAwaitSink()
			config := runtimeConfig{
				Client: client, Model: model, System: "system", DrainTimeout: 200 * time.Millisecond,
				ContextCounter: counter, CounterCapability: capability, InferenceCapability: contextTestInferenceCapability(),
				Compaction:     &loop.CompactionPolicy{Automatic: true, CounterPolicy: loop.CounterPolicyRequireExact, CompactAt: 5_000, RearmBelow: 4_000, ReservedOutput: 20, MaxSummaryTokens: 10, CountTimeout: 5 * time.Second, Hustle: "context.compact"},
				compactionSink: sink,
			}
			sessionID, loopID := uuid.UUID{21}, uuid.UUID{22}
			actor, err := newWithConfig(ctx, sessionID, loopID, Provenance{}, recorder, config)
			if err != nil {
				t.Fatalf("newWithConfig() error = %v", err)
			}
			startTurn(t, actor, recorder, []content.Block{&content.TextBlock{Text: "mixed"}})
			select {
			case <-counter.started:
			case <-time.After(2 * time.Second):
				t.Fatal("count did not start")
			}
			manual := command.Compact{
				Header:      command.Header{CommandID: mustID(t), Agency: identity.AgencyUser, CreatedAt: time.Now()},
				Coordinates: identity.Coordinates{SessionID: sessionID, LoopID: loopID},
			}
			if !sendCmd(t, actor, manual) {
				t.Fatal("manual Compact did not land")
			}
			close(counter.release)
			first := <-sink.started
			if first.Kind != compactionDispositionStart || first.Attempt == nil || first.Attempt.Reason != event.CompactionReasonManual {
				t.Fatalf("first disposition = %+v, want Manual opener", first)
			}
			measured, _ := contextEvents(recorder.events())
			if measured == nil {
				t.Fatal("missing measured basis before manual attempt")
			}
			if first.Attempt.Basis != measured.Measurement.Basis {
				t.Fatalf("first attempted basis = %+v, want %+v", first.Attempt.Basis, measured.Measurement.Basis)
			}
			sink.release <- contextCompactionAwaitResult{
				Disposition: contextCompactionAwaitRejected,
				Proposal:    compactionFinalizationProposal{RejectReason: event.CompactRejectExecutionFailed},
			}
			select {
			case second := <-sink.started:
				if second.Kind != compactionDispositionStart || second.Attempt == nil || second.Attempt.Reason != event.CompactionReasonAutomatic || second.Attempt.AttemptID == first.Attempt.AttemptID {
					t.Fatalf("second disposition = %+v, want distinct Automatic opener", second)
				}
				if second.Attempt.Basis != measured.Measurement.Basis {
					t.Fatalf("second attempted basis = %+v, want %+v", second.Attempt.Basis, measured.Measurement.Basis)
				}
				sink.release <- contextCompactionAwaitResult{
					Disposition: contextCompactionAwaitRejected,
					Proposal:    compactionFinalizationProposal{RejectReason: event.CompactRejectExecutionFailed},
				}
			case <-time.After(2 * time.Second):
				t.Fatal("automatic attempt did not open after manual rejection")
			}
			if terminal := drainToTerminal(t, recorder); terminal == nil {
				t.Fatal("mixed-origin turn produced no terminal")
			}
			if calls, _ := client.snapshot(); calls != 1 {
				t.Fatalf("primary inference calls = %d, want 1 after both soft rejections", calls)
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

func hasContextPressure(events []event.Event, want event.PressureLevel) bool {
	for _, value := range events {
		if pressure, ok := value.(event.ContextPressure); ok && pressure.Current == want {
			return true
		}
	}
	return false
}
