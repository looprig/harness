package hustleruntime

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/looprig/harness/pkg/hustle"
	"github.com/looprig/inference"
)

type runtimeCleanupTracker struct {
	lease ActivityLease
	err   error
}

func (t *runtimeCleanupTracker) AcquireHustleActivity(context.Context, hustle.RunID) (ActivityLease, error) {
	return t.lease, t.err
}

type runtimeCleanupLease struct {
	releases atomic.Int32
	err      error
}

func (l *runtimeCleanupLease) Release(context.Context) error {
	l.releases.Add(1)
	return l.err
}

func TestActivityAndFinalizerFailuresRemainTypedAndOwned(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name          string
		missingLease  bool
		invalidOutput bool
		finalizerErr  error
		releaseErr    error
		wantFaults    int
	}{
		{name: "missing successful acquisition lease fails before Started", missingLease: true},
		{name: "release failure after success is returned and faulted", releaseErr: &runtimeFailureCause{label: "release failed"}, wantFaults: 1},
		{name: "execution finalizer and release failures are all retained", invalidOutput: true, finalizerErr: &runtimeFailureCause{label: "finalizer failed"}, releaseErr: &runtimeFailureCause{label: "release failed"}, wantFaults: 2},
	}
	for _, tt := range tests {
		testCase := tt
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			client := successfulRuntimeClient(nil)
			if testCase.invalidOutput {
				client = &runtimeTestClient{invoke: func(context.Context, inference.Request) (*inference.Response, error) {
					return runtimeResponse(`{"broken"`, nil), nil
				}}
			}
			definition := runtimeTestBoundDefinition(t, "test.cleanup", hustle.ParticipationBlocking, client, hustle.ModelSourceNamed, nil)
			audit := &runtimeTestAudit{}
			faults := &runtimeTestFaults{}
			lease := &runtimeCleanupLease{err: testCase.releaseErr}
			tracker := &runtimeCleanupTracker{lease: lease}
			if testCase.missingLease {
				tracker.lease = nil
			}
			controller := runtimeTestController(t, definition, audit, faults, tracker)
			var finalizers atomic.Int32
			err := controller.RunAndFinalize(context.Background(), runtimeRequest(t, "test.cleanup"), func(context.Context, hustle.Result) error { return nil }, func(context.Context, hustle.Outcome) error {
				finalizers.Add(1)
				return testCase.finalizerErr
			})
			if finalizers.Load() != 1 {
				t.Fatalf("finalizer calls = %d, want 1", finalizers.Load())
			}
			if testCase.missingLease {
				var activityErr *ActivityError
				var runErr *RunError
				if !errors.As(err, &runErr) || !errors.As(err, &activityErr) || activityErr.Operation != ActivityAcquire || len(audit.snapshot()) != 0 || client.invocations.Load() != 0 {
					t.Fatalf("missing lease result = error:%T %v events:%#v invokes:%d", err, err, audit.snapshot(), client.invocations.Load())
				}
				return
			}
			if lease.releases.Load() != 1 {
				t.Fatalf("release calls = %d, want 1", lease.releases.Load())
			}
			if testCase.invalidOutput {
				var runErr *RunError
				if !errors.As(err, &runErr) || runErr.FinalizerErr == nil || runErr.CleanupErr == nil || !errors.Is(err, testCase.finalizerErr) || !errors.Is(err, testCase.releaseErr) {
					t.Fatalf("combined failure = %#v, want primary with finalizer and cleanup", err)
				}
			} else {
				var activityErr *ActivityError
				if !errors.As(err, &activityErr) || activityErr.Operation != ActivityRelease || !errors.Is(err, testCase.releaseErr) {
					t.Fatalf("release error = %T %v, want ActivityError", err, err)
				}
			}
			faults.mu.Lock()
			faultCount := len(faults.faults)
			faults.mu.Unlock()
			if faultCount != testCase.wantFaults {
				t.Fatalf("fault count = %d, want %d", faultCount, testCase.wantFaults)
			}
		})
	}
}
