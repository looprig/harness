package hustleruntime

import (
	"context"
	"errors"
	"math"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/hustle"
)

func testConfig() Config {
	return Config{
		Blocking:   LaneLimits{Concurrent: 1, Queued: 2},
		Background: LaneLimits{Concurrent: 1, Queued: 2},
	}
}

func noOpFinalizer(context.Context, hustle.Outcome) error { return nil }

func TestNewControllerValidatesLaneLimits(t *testing.T) {
	t.Parallel()
	valid := testConfig()
	tests := []struct {
		name       string
		ctx        context.Context
		config     Config
		wantReason ConfigErrorReason
		wantField  string
	}{
		{name: "minimum valid including zero queues", ctx: context.Background(), config: Config{Blocking: LaneLimits{Concurrent: 1}, Background: LaneLimits{Concurrent: 1}}},
		{name: "nil session context", config: valid, wantReason: ConfigInvalidContext, wantField: "context"},
		{name: "canceled session context", ctx: func() context.Context { ctx, cancel := context.WithCancel(context.Background()); cancel(); return ctx }(), config: valid, wantReason: ConfigInvalidContext, wantField: "context"},
		{name: "zero blocking concurrency", ctx: context.Background(), config: func() Config { value := valid; value.Blocking.Concurrent = 0; return value }(), wantReason: ConfigInvalidConcurrent, wantField: "blocking.concurrent"},
		{name: "negative blocking queue", ctx: context.Background(), config: func() Config { value := valid; value.Blocking.Queued = -1; return value }(), wantReason: ConfigInvalidQueued, wantField: "blocking.queued"},
		{name: "blocking queue above bound", ctx: context.Background(), config: func() Config { value := valid; value.Blocking.Queued = maxLaneQueued + 1; return value }(), wantReason: ConfigInvalidQueued, wantField: "blocking.queued"},
		{name: "zero background concurrency", ctx: context.Background(), config: func() Config { value := valid; value.Background.Concurrent = 0; return value }(), wantReason: ConfigInvalidConcurrent, wantField: "background.concurrent"},
		{name: "negative background queue", ctx: context.Background(), config: func() Config { value := valid; value.Background.Queued = -1; return value }(), wantReason: ConfigInvalidQueued, wantField: "background.queued"},
		{name: "capacity overflow", ctx: context.Background(), config: func() Config {
			value := valid
			value.Background.Concurrent = math.MaxInt
			value.Background.Queued = 1
			return value
		}(), wantReason: ConfigCapacityOverflow, wantField: "background"},
	}
	for _, tt := range tests {
		testCase := tt
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			controller, err := New(testCase.ctx, testCase.config)
			if testCase.wantReason == "" {
				if err != nil || controller == nil {
					t.Fatalf("New() = (%v,%v), want controller,nil", controller, err)
				}
				return
			}
			var configErr *ConfigError
			if !errors.As(err, &configErr) || configErr.Reason != testCase.wantReason || configErr.Field != testCase.wantField {
				t.Fatalf("New() error = %T %v, want ConfigError reason=%q field=%q", err, err, testCase.wantReason, testCase.wantField)
			}
			if controller != nil {
				t.Fatalf("New() controller = %v, want nil", controller)
			}
		})
	}
}

func TestControllerRejectsBeforeOwnership(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		prepare    func(*testing.T, *Controller)
		ctx        func() context.Context
		part       hustle.Participation
		finalizer  Finalizer
		wantReason AdmissionErrorReason
	}{
		{name: "invalid participation", ctx: context.Background, part: hustle.ParticipationUnknown, finalizer: noOpFinalizer, wantReason: AdmissionInvalidParticipation},
		{name: "nil context", part: hustle.ParticipationBlocking, finalizer: noOpFinalizer, wantReason: AdmissionInvalidContext},
		{name: "nil finalizer", ctx: context.Background, part: hustle.ParticipationBlocking, wantReason: AdmissionNilFinalizer},
		{name: "full lane", ctx: context.Background, part: hustle.ParticipationBlocking, finalizer: noOpFinalizer, prepare: func(t *testing.T, controller *Controller) {
			config := controller.blocking
			for index := 0; index < config.capacity; index++ {
				if _, err := controller.own(context.Background(), hustle.ParticipationBlocking, noOpFinalizer); err != nil {
					t.Fatalf("fill own[%d] error = %v", index, err)
				}
			}
		}, wantReason: AdmissionFull},
		{name: "closed lane", ctx: context.Background, part: hustle.ParticipationBackground, finalizer: noOpFinalizer, prepare: func(t *testing.T, controller *Controller) {
			if err := controller.Close(context.Background()); err != nil {
				t.Fatalf("Close() error = %v", err)
			}
		}, wantReason: AdmissionClosed},
	}
	for _, tt := range tests {
		testCase := tt
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			config := testConfig()
			var finalized atomic.Int32
			if testCase.finalizer != nil {
				testCase.finalizer = func(context.Context, hustle.Outcome) error {
					finalized.Add(1)
					return nil
				}
			}
			controller, err := New(context.Background(), config)
			if err != nil {
				t.Fatal(err)
			}
			if testCase.prepare != nil {
				testCase.prepare(t, controller)
			}
			var ctx context.Context
			if testCase.ctx != nil {
				ctx = testCase.ctx()
			}
			run, err := controller.own(ctx, testCase.part, testCase.finalizer)
			var admissionErr *AdmissionError
			if !errors.As(err, &admissionErr) || admissionErr.Reason != testCase.wantReason {
				t.Fatalf("own() error = %T %v, want AdmissionError reason %q", err, err, testCase.wantReason)
			}
			if run != nil {
				t.Fatalf("own() run = %v, want nil", run)
			}
			if got := finalized.Load(); got != 0 {
				t.Fatalf("finalizer calls = %d, want 0 before ownership", got)
			}
		})
	}
}

func TestQueuedCancellationFinalizesExactlyOnce(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		part hustle.Participation
	}{
		{name: "blocking queued caller cancellation", part: hustle.ParticipationBlocking},
		{name: "background queued caller cancellation", part: hustle.ParticipationBackground},
	}
	for _, tt := range tests {
		testCase := tt
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			controller, err := New(context.Background(), testConfig())
			if err != nil {
				t.Fatal(err)
			}
			first, err := controller.own(context.Background(), testCase.part, noOpFinalizer)
			if err != nil {
				t.Fatal(err)
			}
			if err := first.awaitExecution(); err != nil {
				t.Fatal(err)
			}
			queuedCtx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var calls atomic.Int32
			var captured hustle.Outcome
			queued, err := controller.own(queuedCtx, testCase.part, func(_ context.Context, outcome hustle.Outcome) error {
				calls.Add(1)
				captured = outcome
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			waited := make(chan error, 1)
			go func() { waited <- queued.awaitExecution() }()
			cancel()
			waitErr := <-waited
			var queueErr *QueueFailureError
			if !errors.As(waitErr, &queueErr) || queueErr.Stage != hustle.StageQueue || queueErr.Reason != QueueFailureCanceled || queueErr.RunID != queued.id {
				t.Fatalf("awaitExecution() error = %T %v, want canceled queue failure for %v", waitErr, waitErr, queued.id)
			}
			if calls.Load() != 1 || captured.Err != queueErr || captured.Result != nil {
				t.Fatalf("finalizer = calls:%d outcome:%#v, want one queue failure", calls.Load(), captured)
			}
			if err := queued.finalize(context.Background(), hustle.Outcome{}); !errors.Is(err, waitErr) {
				t.Fatalf("second finalize error = %v, want cached %v", err, waitErr)
			}
			if calls.Load() != 1 {
				t.Fatalf("finalizer calls after retry = %d, want 1", calls.Load())
			}
			if err := first.finalize(context.Background(), hustle.Outcome{}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestControllerCloseRejectsAndResolvesQueuedOwnership(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		part hustle.Participation
	}{
		{name: "blocking close", part: hustle.ParticipationBlocking},
		{name: "background close", part: hustle.ParticipationBackground},
	}
	for _, tt := range tests {
		testCase := tt
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			controller, err := New(context.Background(), testConfig())
			if err != nil {
				t.Fatal(err)
			}
			active, err := controller.own(context.Background(), testCase.part, noOpFinalizer)
			if err != nil {
				t.Fatal(err)
			}
			if err := active.awaitExecution(); err != nil {
				t.Fatal(err)
			}
			var calls atomic.Int32
			queued := make([]*ownedRun, 0, 2)
			for range 2 {
				run, ownErr := controller.own(context.Background(), testCase.part, func(_ context.Context, outcome hustle.Outcome) error {
					var queueErr *QueueFailureError
					if !errors.As(outcome.Err, &queueErr) || queueErr.Reason != QueueFailureClosed {
						t.Errorf("finalizer outcome error = %T %v, want closed queue failure", outcome.Err, outcome.Err)
					}
					calls.Add(1)
					return nil
				})
				if ownErr != nil {
					t.Fatal(ownErr)
				}
				queued = append(queued, run)
			}
			if err := controller.Close(context.Background()); err != nil {
				t.Fatalf("Close() error = %v", err)
			}
			for index := range queued {
				err := queued[index].awaitExecution()
				var queueErr *QueueFailureError
				if !errors.As(err, &queueErr) || queueErr.Reason != QueueFailureClosed {
					t.Fatalf("queued[%d] error = %T %v, want closed queue failure", index, err, err)
				}
			}
			if calls.Load() != 2 {
				t.Fatalf("queued finalizer calls = %d, want 2", calls.Load())
			}
			if err := active.finalize(context.Background(), hustle.Outcome{}); err != nil {
				t.Fatal(err)
			}
			run, err := controller.own(context.Background(), testCase.part, noOpFinalizer)
			var admissionErr *AdmissionError
			if run != nil || !errors.As(err, &admissionErr) || admissionErr.Reason != AdmissionClosed {
				t.Fatalf("post-close own() = (%v,%T %v), want nil closed admission", run, err, err)
			}
		})
	}
}

func TestOwnedFinalizerIsConcurrentSafeAndCachesResult(t *testing.T) {
	t.Parallel()
	tests := []struct{ name string }{{name: "one finalizer for one owned run"}}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			controller, err := New(context.Background(), testConfig())
			if err != nil {
				t.Fatal(err)
			}
			var calls atomic.Int32
			run, err := controller.own(context.Background(), hustle.ParticipationBlocking, func(context.Context, hustle.Outcome) error {
				calls.Add(1)
				return errTestFinalizer
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := run.awaitExecution(); err != nil {
				t.Fatal(err)
			}
			const callers = 16
			results := make(chan error, callers)
			var group sync.WaitGroup
			for range callers {
				group.Add(1)
				go func() {
					defer group.Done()
					results <- run.finalize(context.Background(), hustle.Outcome{})
				}()
			}
			group.Wait()
			close(results)
			for result := range results {
				var finalizerErr *FinalizerError
				if !errors.As(result, &finalizerErr) || !errors.Is(result, errTestFinalizer) {
					t.Fatalf("finalize() error = %T %v, want typed cached finalizer error", result, result)
				}
			}
			if calls.Load() != 1 {
				t.Fatalf("finalizer calls = %d, want 1", calls.Load())
			}
		})
	}
}

var errTestFinalizer = &testFinalizerCause{}

type testFinalizerCause struct{}

func (*testFinalizerCause) Error() string { return "test finalizer failure" }

func TestRunIDFactoryFailureIsPreOwnership(t *testing.T) {
	t.Parallel()
	tests := []struct{ name string }{{name: "id generation fails before ownership"}}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			config := testConfig()
			config.NewRunID = func() (uuid.UUID, error) { return uuid.UUID{}, errTestRunID }
			controller, err := New(context.Background(), config)
			if err != nil {
				t.Fatal(err)
			}
			var finalized atomic.Int32
			run, err := controller.own(context.Background(), hustle.ParticipationBlocking, func(context.Context, hustle.Outcome) error {
				finalized.Add(1)
				return nil
			})
			var admissionErr *AdmissionError
			if run != nil || !errors.As(err, &admissionErr) || admissionErr.Reason != AdmissionRunID || !errors.Is(err, errTestRunID) {
				t.Fatalf("own() = (%v,%T %v), want typed run-id rejection", run, err, err)
			}
			if finalized.Load() != 0 {
				t.Fatalf("finalizer calls = %d, want 0", finalized.Load())
			}
		})
	}
}

var errTestRunID = &testRunIDCause{}

type testRunIDCause struct{}

func (*testRunIDCause) Error() string { return "test run id failure" }
