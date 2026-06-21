package session

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/inventivepotter/urvi/internal/agent/loop"
	"github.com/inventivepotter/urvi/internal/agent/loop/event"
	"github.com/inventivepotter/urvi/internal/agent/session/hub"
	"github.com/inventivepotter/urvi/internal/content"
)

// errFault is a leaf cause used to populate the SessionPersistenceFault in tests.
var errFault = errors.New("durable append failed")

// TestReportFaultRejectsNewWork proves a faulted session refuses new Submit and
// NewLoop with the typed SessionFaulted error: once the hub reports a persistence
// fault, the session must not accept any new work (fail-secure).
func TestReportFaultRejectsNewWork(t *testing.T) {
	t.Parallel()
	s, err := New(context.Background(), cfg(&stubLLM{chunks: []content.Chunk{textChunk("x")}}))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = s.Shutdown(context.Background()) })

	// Before the fault, Submit and NewLoop work (sanity).
	if _, err := s.Submit(context.Background(), []content.Block{&content.TextBlock{Text: "hi"}}); err != nil {
		t.Fatalf("pre-fault Submit = %v, want nil", err)
	}

	// Inject a fault as the hub would.
	fault := &hub.SessionPersistenceFault{Event: event.SessionActive{}, Cause: errFault}
	s.ReportFault(context.Background(), fault)

	// Submit is refused with the typed faulted error wrapping the fault.
	_, subErr := s.Submit(context.Background(), []content.Block{&content.TextBlock{Text: "again"}})
	var se *SessionError
	if !errors.As(subErr, &se) || se.Kind != SessionFaulted {
		t.Fatalf("post-fault Submit = %v, want *SessionError{SessionFaulted}", subErr)
	}
	if !errors.Is(subErr, errFault) {
		t.Errorf("post-fault Submit error does not chain the fault cause: %v", subErr)
	}

	// NewLoop is refused with the same typed faulted error.
	_, nlErr := s.NewLoop(loop.Provenance{}, cfg(&stubLLM{}))
	if !errors.As(nlErr, &se) || se.Kind != SessionFaulted {
		t.Fatalf("post-fault NewLoop = %v, want *SessionError{SessionFaulted}", nlErr)
	}
}

// TestReportFaultWakesWaitIdle proves a blocked WaitIdle waiter is woken with the
// fault when a persistence fault is reported (not left hanging, not falsely idle).
func TestReportFaultWakesWaitIdle(t *testing.T) {
	t.Parallel()
	// blockUntilCancel (NOT ignoreCtx): the turn stays running — keeping the session
	// Active so WaitIdle blocks — until the loop's ctx is cancelled, so the cleanup
	// Shutdown drains cleanly.
	s, err := New(context.Background(), cfg(&stubLLM{blockUntilCancel: true}))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = s.Shutdown(context.Background()) })

	// Drive the session Active so WaitIdle blocks: submit input the primary loop
	// runs until its ctx is cancelled.
	if _, err := s.Submit(context.Background(), []content.Block{&content.TextBlock{Text: "go"}}); err != nil {
		t.Fatalf("Submit = %v", err)
	}

	// Wait until the session is Active (the loop started its turn) so a WaitIdle
	// would actually block, then start the waiter.
	if !waitForActive(t, s) {
		t.Fatal("session never went Active")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	waitErr := make(chan error, 1)
	go func() { waitErr <- s.WaitIdle(ctx) }()

	// The waiter must be blocked (Active), not returning prematurely.
	select {
	case err := <-waitErr:
		t.Fatalf("WaitIdle returned %v before fault; expected it to block (Active)", err)
	case <-time.After(50 * time.Millisecond):
	}

	fault := &hub.SessionPersistenceFault{Event: event.SessionIdle{}, Cause: errFault}
	s.ReportFault(context.Background(), fault)

	select {
	case err := <-waitErr:
		if !errors.Is(err, errFault) {
			t.Fatalf("WaitIdle woke with %v, want the fault (errFault chained)", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("WaitIdle was not woken by ReportFault")
	}
}

// waitForActive polls WaitIdle with an immediately-cancelled context: while the
// session is Active, WaitIdle blocks (so the cancelled ctx wins → context.Canceled);
// once it would be idle it returns nil. It returns true as soon as the session is
// observed Active within the deadline.
func waitForActive(t *testing.T, s *Session) bool {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		err := s.WaitIdle(ctx)
		if errors.Is(err, context.Canceled) {
			return true // blocked => Active
		}
		time.Sleep(10 * time.Millisecond)
	}
	return false
}
