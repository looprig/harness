package sessionruntime

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestCheckpointAdmissionWriterBlocksNewExecutionAcrossLoops(t *testing.T) {
	t.Parallel()
	gate := newCheckpointAdmissionGate()
	releaseFirst, err := gate.enterExecution(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	writerEntered := make(chan struct{})
	releaseWriter := make(chan func(), 1)
	go func() {
		release, err := gate.enterCheckpoint(context.Background())
		if err != nil {
			return
		}
		close(writerEntered)
		releaseWriter <- release
	}()
	select {
	case <-writerEntered:
		t.Fatal("checkpoint entered while another loop execution was active")
	case <-time.After(20 * time.Millisecond):
	}
	releaseFirst()
	select {
	case <-writerEntered:
	case <-time.After(time.Second):
		t.Fatal("checkpoint did not enter after active execution released")
	}
	secondCtx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := gate.enterExecution(secondCtx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("new loop execution during checkpoint = %v, want deadline", err)
	}
	(<-releaseWriter)()
	if release, err := gate.enterExecution(context.Background()); err != nil {
		t.Fatalf("execution after checkpoint: %v", err)
	} else {
		release()
	}
}
