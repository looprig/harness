package hustleruntime

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/looprig/harness/pkg/hustle"
	"github.com/looprig/inference"
)

type runtimeCleanupTracker struct {
	lease ActivityLease
	err   error
	order *runtimeTestOrder
}

func (t *runtimeCleanupTracker) AcquireHustleActivity(context.Context, hustle.RunID) (ActivityLease, error) {
	if t.order != nil {
		t.order.add("acquire")
	}
	return t.lease, t.err
}

type runtimeCleanupLease struct {
	releases atomic.Int32
	err      error
	order    *runtimeTestOrder
}

func (l *runtimeCleanupLease) Release(context.Context) error {
	l.releases.Add(1)
	if l.order != nil {
		l.order.add("release")
	}
	return l.err
}

type runtimeOrderedFaults struct {
	mu     sync.Mutex
	faults []error
	order  *runtimeTestOrder
}

type runtimeUncomparableError struct{ values []byte }

func (runtimeUncomparableError) Error() string { return "uncomparable activity error" }

func TestSameErrorValueIsPanicSafe(t *testing.T) {
	t.Parallel()
	shared := &runtimeFailureCause{label: "shared"}
	uncomparable := runtimeUncomparableError{values: []byte{1}}
	tests := []struct {
		name  string
		left  error
		right error
		want  bool
	}{
		{name: "same comparable pointer", left: shared, right: shared, want: true},
		{name: "distinct comparable pointers", left: &runtimeFailureCause{label: "same"}, right: &runtimeFailureCause{label: "same"}},
		{name: "same uncomparable value is not identity comparable", left: uncomparable, right: uncomparable},
		{name: "nil is not cached identity", left: nil, right: shared},
	}
	for _, tt := range tests {
		testCase := tt
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			if got := sameErrorValue(testCase.left, testCase.right); got != testCase.want {
				t.Fatalf("sameErrorValue(%T,%T) = %v, want %v", testCase.left, testCase.right, got, testCase.want)
			}
		})
	}
}

func (f *runtimeOrderedFaults) ReportFault(_ context.Context, err error) {
	operation := "other"
	var activityErr *ActivityError
	if errors.As(err, &activityErr) {
		operation = string(activityErr.Operation)
	}
	f.order.add("fault:" + operation)
	f.mu.Lock()
	f.faults = append(f.faults, err)
	f.mu.Unlock()
}

func (f *runtimeOrderedFaults) snapshot() []error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]error(nil), f.faults...)
}

func TestActivityAcquisitionFailuresFaultBeforeFinalization(t *testing.T) {
	t.Parallel()
	acquireCause := &runtimeFailureCause{label: "activity acquisition failed"}
	releaseCause := &runtimeFailureCause{label: "activity release failed"}
	tests := []struct {
		name            string
		acquireErr      error
		lease           bool
		releaseErr      error
		wantOrder       []string
		wantFaultOps    []ActivityOperation
		wantReleaseErr  bool
		wantReleaseCall int32
	}{
		{name: "nil lease with acquisition error", acquireErr: acquireCause, wantOrder: []string{"acquire", "fault:acquire", "finalize"}, wantFaultOps: []ActivityOperation{ActivityAcquire}},
		{name: "partial lease returns cached acquisition error", acquireErr: acquireCause, lease: true, releaseErr: acquireCause, wantOrder: []string{"acquire", "fault:acquire", "finalize", "release"}, wantFaultOps: []ActivityOperation{ActivityAcquire}, wantReleaseCall: 1},
		{name: "missing lease on successful acquisition", wantOrder: []string{"acquire", "fault:acquire", "finalize"}, wantFaultOps: []ActivityOperation{ActivityAcquire}},
		{name: "partial lease retains distinct release failure", acquireErr: acquireCause, lease: true, releaseErr: releaseCause, wantOrder: []string{"acquire", "fault:acquire", "finalize", "release", "fault:release"}, wantFaultOps: []ActivityOperation{ActivityAcquire, ActivityRelease}, wantReleaseErr: true, wantReleaseCall: 1},
	}
	for _, tt := range tests {
		testCase := tt
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			order := &runtimeTestOrder{}
			lease := &runtimeCleanupLease{err: testCase.releaseErr, order: order}
			tracker := &runtimeCleanupTracker{err: testCase.acquireErr, order: order}
			if testCase.lease {
				tracker.lease = lease
			}
			faults := &runtimeOrderedFaults{order: order}
			client := successfulRuntimeClient(nil)
			definition := runtimeTestBoundDefinition(t, "test.acquire-fault", hustle.ParticipationBlocking, client, hustle.ModelSourceNamed, nil)
			controller := runtimeTestControllerWithAudit(t, definition, &runtimeTestAudit{}, faults, tracker)
			var finalOutcome hustle.Outcome
			err := controller.RunAndFinalize(context.Background(), runtimeRequest(t, "test.acquire-fault"), func(context.Context, hustle.Result) error { return nil }, func(_ context.Context, outcome hustle.Outcome) error {
				order.add("finalize")
				finalOutcome = outcome
				return nil
			})
			var runErr *RunError
			var activityErr *ActivityError
			if !errors.As(err, &runErr) || !errors.As(err, &activityErr) || activityErr.Operation != ActivityAcquire || finalOutcome.Err != runErr || finalOutcome.Result != nil {
				t.Fatalf("result = error:%T %v outcome:%#v, want acquisition RunError", err, err, finalOutcome)
			}
			if testCase.acquireErr != nil && !errors.Is(err, testCase.acquireErr) {
				t.Fatalf("error = %v, want acquisition cause %v", err, testCase.acquireErr)
			}
			if testCase.wantReleaseErr != errors.Is(err, releaseCause) {
				t.Fatalf("release error retained = %v, want %v", errors.Is(err, releaseCause), testCase.wantReleaseErr)
			}
			if lease.releases.Load() != testCase.wantReleaseCall {
				t.Fatalf("release calls = %d, want %d", lease.releases.Load(), testCase.wantReleaseCall)
			}
			if got := order.snapshot(); !reflect.DeepEqual(got, testCase.wantOrder) {
				t.Fatalf("order = %v, want %v", got, testCase.wantOrder)
			}
			gotFaults := faults.snapshot()
			if len(gotFaults) != len(testCase.wantFaultOps) {
				t.Fatalf("faults = %#v, want operations %v", gotFaults, testCase.wantFaultOps)
			}
			for index, wantOperation := range testCase.wantFaultOps {
				var gotActivity *ActivityError
				if !errors.As(gotFaults[index], &gotActivity) || gotActivity.Operation != wantOperation {
					t.Fatalf("fault[%d] = %T %v, want activity %v", index, gotFaults[index], gotFaults[index], wantOperation)
				}
			}
			if client.invocations.Load() != 0 {
				t.Fatalf("invoke calls = %d, want 0", client.invocations.Load())
			}
		})
	}
}

func TestActivityAndFinalizerFailuresRemainTypedAndOwned(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name          string
		invalidOutput bool
		finalizerErr  error
		releaseErr    error
		wantFaults    int
	}{
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
			controller := runtimeTestController(t, definition, audit, faults, tracker)
			var finalizers atomic.Int32
			err := controller.RunAndFinalize(context.Background(), runtimeRequest(t, "test.cleanup"), func(context.Context, hustle.Result) error { return nil }, func(context.Context, hustle.Outcome) error {
				finalizers.Add(1)
				return testCase.finalizerErr
			})
			if finalizers.Load() != 1 {
				t.Fatalf("finalizer calls = %d, want 1", finalizers.Load())
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
