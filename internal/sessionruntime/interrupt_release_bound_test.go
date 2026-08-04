package sessionruntime

import (
	"context"
	"testing"
	"time"

	"github.com/looprig/core/content"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/loop"
)

// TestInterruptBarrierFailsOpenWhenTheSessionNeverReachesIdle pins the fail-open guarantee
// InterruptReleasePolicy already documents: "the marks are cleared once AwaitRelease returns
// regardless of the error (fail-open on RELEASE, so a barrier can never wedge admission
// forever)."
//
// The default policy waited on WaitIdle with the SESSION lifetime as its only bound, so the
// guarantee did not hold: one loop that does not honor cancellation (a tool or provider
// ignoring ctx — here a stream that blocks on a channel) means SessionIdle never arrives, the
// barrier never releases, and the RETAINED human input on an already-idle loop can never be
// admitted. The user sees an interrupt that appears to work followed by messages that queue
// forever, on every loop in the session.
//
// The bound only ever governs this pathological case: when the session does reach idle, the
// SessionIdle edge still releases the barrier, which the surrounding tests cover.
func TestInterruptBarrierFailsOpenWhenTheSessionNeverReachesIdle(t *testing.T) {
	t.Parallel()

	recorder := &recordingEventAppender{}
	s, err := newTestSession(context.Background(), cfg(&stubLLM{chunks: []content.Chunk{textChunk("primer")}}),
		WithEventAppender(recorder))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Shutdown(context.Background()) })

	// Shorten the default policy's bound so the test exercises the real release path
	// (sessionIdleRelease) rather than a stand-in, without waiting out the production bound.
	s.interruptRelease = sessionIdleRelease{session: s, bound: 100 * time.Millisecond}

	// A child loop whose stream blocks on a channel and never observes cancellation: this
	// loop will not reach idle, so the session will not either.
	stuck := &releasedFailureLLM{started: make(chan struct{}), release: make(chan struct{}), err: context.Canceled}
	childID, err := s.NewLoop(loop.Provenance{}, cfg(stuck))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.SubmitToLoop(context.Background(), childID, nil); err != nil {
		t.Fatal(err)
	}
	select {
	case <-stuck.started:
	case <-time.After(2 * time.Second):
		t.Fatal("child turn did not start")
	}
	// The stuck loop must outlive the assertions; releasing it here would let the session
	// reach idle and release the barrier the ordinary way, testing nothing.
	t.Cleanup(func() { close(stuck.release) })

	if _, err := s.Interrupt(context.Background()); err != nil {
		t.Fatalf("Interrupt: %v", err)
	}

	inputID, err := s.Submit(context.Background(), []content.Block{&content.TextBlock{Text: "run me next"}})
	if err != nil {
		t.Fatalf("Submit after interrupt: %v", err)
	}

	deadline := time.After(5 * time.Second)
	for {
		for _, ev := range recorder.snapshot() {
			if started, ok := ev.(event.TurnStarted); ok && started.Cause.CommandID == inputID {
				return // the retained input was admitted: the barrier failed open
			}
		}
		select {
		case <-deadline:
			t.Fatal("input queued after interrupt never started: the admission barrier wedged " +
				"on a session that cannot reach idle")
		case <-time.After(10 * time.Millisecond):
		}
	}
}
