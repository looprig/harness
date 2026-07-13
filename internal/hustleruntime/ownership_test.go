package hustleruntime

import (
	"context"
	"errors"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/looprig/core/content"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/hustle"
	"github.com/looprig/harness/pkg/identity"
	"github.com/looprig/inference"
)

type gatedStartedAudit struct {
	base    *runtimeTestAudit
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (p *gatedStartedAudit) PublishInternalEventChecked(ctx context.Context, ev event.Event) error {
	if _, started := ev.(event.HustleStarted); started {
		p.once.Do(func() {
			close(p.entered)
			<-p.release
		})
	}
	return p.base.PublishInternalEventChecked(ctx, ev)
}

type partialActivityTracker struct {
	lease    *partialActivityLease
	acquires atomic.Int32
	err      error
}

func (t *partialActivityTracker) AcquireHustleActivity(context.Context, hustle.RunID) (ActivityLease, error) {
	t.acquires.Add(1)
	return t.lease, t.err
}

type partialActivityLease struct{ releases atomic.Int32 }

func (l *partialActivityLease) Release(context.Context) error {
	l.releases.Add(1)
	return nil
}

func successfulRuntimeClient(invocations *atomic.Int32) *runtimeTestClient {
	client := &runtimeTestClient{}
	client.invoke = func(context.Context, inference.Request) (*inference.Response, error) {
		if invocations != nil {
			invocations.Add(1)
		}
		return &inference.Response{Message: &content.AIMessage{Message: content.Message{
			Role: content.RoleAssistant, Blocks: []content.Block{&content.TextBlock{Text: `{"ok":true}`}},
		}}}, nil
	}
	return client
}

func runtimeRequest(t *testing.T, name hustle.Name) hustle.Request {
	t.Helper()
	return hustle.Request{
		Name: name,
		Cause: identity.Cause{
			Coordinates: identity.Coordinates{LoopID: mustRuntimeTestID(t)},
			CommandID:   mustRuntimeTestID(t),
		},
		Input: []byte(`{"version":1}`),
	}
}

func TestInitializingOwnedRunIsSoleLifecycleDriver(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		signal     func(context.CancelFunc, *Controller) <-chan error
		waitClosed bool
	}{
		{
			name: "caller cancellation signals initializer",
			signal: func(cancel context.CancelFunc, _ *Controller) <-chan error {
				cancel()
				return nil
			},
		},
		{
			name:       "close signals initializer and joins it",
			waitClosed: true,
			signal: func(_ context.CancelFunc, controller *Controller) <-chan error {
				closed := make(chan error, 1)
				go func() { closed <- controller.Close(context.Background()) }()
				return closed
			},
		},
	}
	for _, tt := range tests {
		testCase := tt
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			var invokes atomic.Int32
			client := successfulRuntimeClient(&invokes)
			definition := runtimeTestBoundDefinition(t, "test.initialize", hustle.ParticipationBlocking, client, hustle.ModelSourceNamed, nil)
			baseAudit := &runtimeTestAudit{}
			gatedAudit := &gatedStartedAudit{base: baseAudit, entered: make(chan struct{}), release: make(chan struct{})}
			faults := &runtimeTestFaults{}
			activity := &runtimeTestActivity{}
			controller := runtimeTestControllerWithAudit(t, definition, gatedAudit, faults, activity)
			runCtx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var finalizers atomic.Int32
			var finalOutcome hustle.Outcome
			request := runtimeRequest(t, "test.initialize")
			runResult := make(chan error, 1)
			go func() {
				runResult <- controller.RunAndFinalize(runCtx, request, func(context.Context, hustle.Result) error { return nil }, func(_ context.Context, outcome hustle.Outcome) error {
					finalOutcome = outcome
					finalizers.Add(1)
					return nil
				})
			}()
			<-gatedAudit.entered
			controller.blocking.mu.Lock()
			executing := controller.blocking.executing
			controller.blocking.mu.Unlock()
			if executing != 0 || invokes.Load() != 0 {
				t.Fatalf("initializing run = executing:%d invokes:%d, want 0,0", executing, invokes.Load())
			}
			closeResult := testCase.signal(cancel, controller)
			if testCase.waitClosed {
				awaitControllerClosed(t, controller)
			}
			if finalizers.Load() != 0 {
				t.Fatalf("finalizer ran concurrently with initializer: %d", finalizers.Load())
			}
			if closeResult != nil {
				select {
				case err := <-closeResult:
					t.Fatalf("Close returned before initializer resolved: %v", err)
				default:
				}
			}
			close(gatedAudit.release)
			err := receiveRuntimeError(t, runResult)
			if err == nil || finalOutcome.Err == nil || finalOutcome.Result != nil {
				t.Fatalf("run/finalizer = (%v,%#v), want owned queue failure", err, finalOutcome)
			}
			if closeResult != nil {
				if err := receiveRuntimeError(t, closeResult); err != nil {
					t.Fatal(err)
				}
			}
			if invokes.Load() != 0 || finalizers.Load() != 1 || activity.releases.Load() != 1 {
				t.Fatalf("terminal calls = invokes:%d finalizers:%d releases:%d, want 0,1,1", invokes.Load(), finalizers.Load(), activity.releases.Load())
			}
			events := baseAudit.snapshot()
			if len(events) != 2 {
				t.Fatalf("audit events = %#v, want Started,Failed", events)
			}
			if _, ok := events[0].(event.HustleStarted); !ok {
				t.Fatalf("audit[0] = %T, want HustleStarted", events[0])
			}
			failed, ok := events[1].(event.HustleFailed)
			if !ok || failed.Stage != hustle.StageQueue {
				t.Fatalf("audit[1] = %#v, want queue HustleFailed", events[1])
			}
		})
	}
}

func awaitControllerClosed(t *testing.T, controller *Controller) {
	t.Helper()
	deadline := time.After(time.Second)
	for {
		controller.admissionMu.Lock()
		controller.blocking.mu.Lock()
		closed := controller.blocking.closed
		controller.blocking.mu.Unlock()
		controller.admissionMu.Unlock()
		if closed {
			return
		}
		select {
		case <-deadline:
			t.Fatal("controller admission did not close")
		default:
			runtime.Gosched()
		}
	}
}

func TestPartialActivityFailureRetainsOwnershipThroughFinalizer(t *testing.T) {
	t.Parallel()
	tests := []struct{ name string }{{name: "partial lease releases after owned failure finalizer"}}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var invokes atomic.Int32
			client := successfulRuntimeClient(&invokes)
			definition := runtimeTestBoundDefinition(t, "test.partial", hustle.ParticipationBlocking, client, hustle.ModelSourceNamed, nil)
			audit := &runtimeTestAudit{}
			lease := &partialActivityLease{}
			acquireErr := &runtimeFailureCause{label: "activity edge append failed"}
			activity := &partialActivityTracker{lease: lease, err: acquireErr}
			controller := runtimeTestController(t, definition, audit, &runtimeTestFaults{}, activity)
			var finalizers atomic.Int32
			var releasedDuringFinalizer bool
			err := controller.RunAndFinalize(context.Background(), runtimeRequest(t, "test.partial"), func(context.Context, hustle.Result) error { return nil }, func(_ context.Context, outcome hustle.Outcome) error {
				finalizers.Add(1)
				releasedDuringFinalizer = lease.releases.Load() != 0
				if outcome.Err == nil || outcome.Result != nil {
					t.Errorf("finalizer outcome = %#v, want activity failure", outcome)
				}
				return nil
			})
			if err == nil || !errors.Is(err, acquireErr) {
				t.Fatalf("RunAndFinalize error = %T %v, want wrapped activity failure", err, err)
			}
			if invokes.Load() != 0 || finalizers.Load() != 1 || releasedDuringFinalizer || lease.releases.Load() != 1 {
				t.Fatalf("calls = invoke:%d finalize:%d released-during:%v releases:%d", invokes.Load(), finalizers.Load(), releasedDuringFinalizer, lease.releases.Load())
			}
			if events := audit.snapshot(); len(events) != 0 {
				t.Fatalf("audit events = %#v, want none before Started", events)
			}
		})
	}
}

func runtimeTestControllerWithAudit(t *testing.T, definition hustle.BoundDefinition, audit AuditPublisher, faults FaultReporter, activity ActivityTracker) *Controller {
	t.Helper()
	factory := event.NewFactory(uuid.New, func() time.Time { return time.Unix(123, 0).UTC() })
	controller, err := New(context.Background(), Config{
		Blocking:   LaneLimits{Concurrent: 1, Queued: 2},
		Background: LaneLimits{Concurrent: 1, Queued: 2},
		Runtime: &RuntimeConfig{
			SessionID: mustRuntimeTestID(t), Definitions: []hustle.BoundDefinition{definition},
			AuditTimeout: time.Second, FinalizationTimeout: time.Second, WorkerDrainTimeout: time.Second,
			Stamper: factory, Audit: audit, Faults: faults, Activity: activity,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return controller
}

func receiveRuntimeError(t *testing.T, result <-chan error) error {
	t.Helper()
	select {
	case err := <-result:
		return err
	case <-time.After(time.Second):
		t.Fatal("runtime operation did not complete")
		return nil
	}
}
