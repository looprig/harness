package hustleruntime

import (
	"context"
	"testing"
	"time"
)

func TestExecutionContextZeroTimeoutHasNoDeadline(t *testing.T) {
	t.Parallel()
	session, stopSession := context.WithCancel(context.Background())
	defer stopSession()
	r := &runtimeController{executionCtx: session}

	caller, cancelCaller := context.WithCancel(context.Background())
	ctx, done := r.executionContextWithTimeout(caller, 0)
	defer done()
	if _, ok := ctx.Deadline(); ok {
		t.Fatal("zero timeout execution context has a deadline")
	}
	cancelCaller()
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("caller cancellation did not end execution")
	}

	ctx, done = r.executionContextWithTimeout(context.Background(), 0)
	defer done()
	stopSession()
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("session cancellation did not end execution")
	}

	bounded := &runtimeController{executionCtx: context.Background()}
	ctx, done = bounded.executionContextWithTimeout(context.Background(), time.Hour)
	defer done()
	if _, ok := ctx.Deadline(); !ok {
		t.Fatal("positive timeout execution context has no deadline")
	}
}
