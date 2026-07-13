package sessionruntime

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/gate"
	"github.com/looprig/harness/pkg/journal"
	"github.com/looprig/harness/pkg/sessionstore"
)

// blockingJournal is a fake journal.SessionJournal whose Append signals it is in flight
// and then blocks until released — standing in for the writer mid-offload (the blob Put
// has landed but the blobptr AppendDefinite has not). It records, atomically, whether the
// Append has returned, so a barrier test can assert GC only scanned AFTER the append
// completed.
type blockingJournal struct {
	started  chan struct{}
	release  chan struct{}
	returned atomic.Bool
}

func (b *blockingJournal) Append(_ context.Context, _ journal.JournalRecord) (uint64, error) {
	b.started <- struct{}{}
	<-b.release
	b.returned.Store(true)
	return 1, nil
}

// recordingScanner is a fake offloadScanner. Its GC runs check (an at-scan-time assertion)
// then closes called exactly once, so a test can both assert ordering at the moment of the
// scan and wait for the pass to happen.
type recordingScanner struct {
	check    func()
	calls    atomic.Int64
	called   chan struct{}
	closeOne sync.Once
}

func (s *recordingScanner) GC(_ context.Context) (sessionstore.GCResult, error) {
	if s.check != nil {
		s.check()
	}
	s.calls.Add(1)
	s.closeOne.Do(func() { close(s.called) })
	return sessionstore.GCResult{}, nil
}

// manualTicker is the deterministic tick seam: a test calls tick() to drive exactly one GC
// opportunity, with no wall-clock timer.
type manualTicker struct{ c chan time.Time }

func newManualTicker() *manualTicker        { return &manualTicker{c: make(chan time.Time, 1)} }
func (m *manualTicker) C() <-chan time.Time { return m.c }
func (m *manualTicker) Stop()               {}
func (m *manualTicker) tick()               { m.c <- time.Now() }

func gateRecordFor(kind string) journal.JournalRecord {
	if kind == "gate-record" {
		return journal.NewGatePreparedRecord(event.GatePrepared{}, gate.OpenPayload{})
	}
	return journal.NewEventRecord(event.LoopIdle{})
}

// TestOffloadGCBarrierBlocksScanUntilAppendCompletes is the forced-barrier proof: while an
// append/offload holds the admission gate as a reader (blocked before its pointer append),
// a GC tick cannot scan/delete until that append completes and releases the reader. It runs
// for both a plain event record and a gate record — the single decorator is record-kind
// agnostic, so the same barrier must hold for gate records.
func TestOffloadGCBarrierBlocksScanUntilAppendCompletes(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"event-record", "gate-record"} {
		kind := kind
		t.Run(kind, func(t *testing.T) {
			t.Parallel()
			gateAdmission := newJournalAdmissionGate()
			inner := &blockingJournal{started: make(chan struct{}), release: make(chan struct{})}
			gj := newGatedJournal(inner, gateAdmission)

			// Start an append that holds the reader while blocked mid-offload.
			appendDone := make(chan struct{})
			go func() {
				defer close(appendDone)
				if _, err := gj.Append(context.Background(), gateRecordFor(kind)); err != nil {
					t.Errorf("gatedJournal.Append: %v", err)
				}
			}()
			<-inner.started // reader now held; inner Append is blocked

			scanner := &recordingScanner{called: make(chan struct{})}
			scanner.check = func() {
				if !inner.returned.Load() {
					t.Errorf("GC scanned while an offload append was still in flight (barrier breached)")
				}
			}
			ticker := newManualTicker()
			runner := newOffloadGCRunner(uuid.UUID{}, scanner, gateAdmission, func() offloadGCTicker { return ticker }, nil, time.Minute)
			runner.start(func() bool { return true })
			defer runner.Stop()

			ticker.tick() // runner tries to take the writer; blocks behind the held reader

			// Nothing should have scanned yet.
			if scanner.calls.Load() != 0 {
				t.Fatalf("GC ran before the in-flight append completed")
			}

			close(inner.release) // append completes, releases the reader
			<-appendDone

			select {
			case <-scanner.called:
			case <-time.After(2 * time.Second):
				t.Fatal("GC never ran after the append completed")
			}
		})
	}
}

// TestOffloadGCRunsOnlyWhenIdle proves the runner skips a tick while the session is not at
// native SessionIdle and runs a pass once it is.
func TestOffloadGCRunsOnlyWhenIdle(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		idle     bool
		wantCall bool
	}{
		{name: "idle runs GC", idle: true, wantCall: true},
		{name: "not idle skips GC", idle: false, wantCall: false},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			gateAdmission := newJournalAdmissionGate()
			scanner := &recordingScanner{called: make(chan struct{})}
			ticker := newManualTicker()
			passed := make(chan struct{}, 1)
			runner := newOffloadGCRunner(uuid.UUID{}, scanner, gateAdmission, func() offloadGCTicker { return ticker }, nil, time.Minute)
			runner.onPass = func(sessionstore.GCResult, error) { passed <- struct{}{} }
			runner.start(func() bool { return tt.idle })
			defer runner.Stop()

			ticker.tick()

			// Every tick drives exactly one loop iteration; onPass fires for a tick whether or
			// not the idle gate admitted a scan, so we can wait deterministically.
			select {
			case <-passed:
			case <-time.After(2 * time.Second):
				t.Fatal("tick was never processed")
			}
			if got := scanner.calls.Load() > 0; got != tt.wantCall {
				t.Fatalf("GC called = %v, want %v", got, tt.wantCall)
			}
		})
	}
}

// TestOffloadGCCancelledOnLeaseLoss proves lease loss stops the runner: it returns and stops
// processing ticks.
func TestOffloadGCCancelledOnLeaseLoss(t *testing.T) {
	t.Parallel()
	gateAdmission := newJournalAdmissionGate()
	scanner := &recordingScanner{called: make(chan struct{})}
	ticker := newManualTicker()
	lost := make(chan struct{})
	runner := newOffloadGCRunner(uuid.UUID{}, scanner, gateAdmission, func() offloadGCTicker { return ticker }, lost, time.Minute)
	runner.start(func() bool { return true })

	close(lost) // lease lost -> runner must return
	runner.Stop()

	// A tick after stop must not run GC.
	ticker.tick()
	if scanner.calls.Load() != 0 {
		t.Fatalf("GC ran after lease loss")
	}
}

// TestOffloadGCStopJoins proves Stop is idempotent, joins the goroutine, and prevents any
// further pass.
func TestOffloadGCStopJoins(t *testing.T) {
	t.Parallel()
	gateAdmission := newJournalAdmissionGate()
	scanner := &recordingScanner{called: make(chan struct{})}
	ticker := newManualTicker()
	runner := newOffloadGCRunner(uuid.UUID{}, scanner, gateAdmission, func() offloadGCTicker { return ticker }, nil, time.Minute)
	runner.start(func() bool { return true })

	runner.Stop()
	runner.Stop() // idempotent

	ticker.tick()
	if scanner.calls.Load() != 0 {
		t.Fatalf("GC ran after Stop")
	}
}

// ctxCapturingScanner is a fake offloadScanner that hands the per-pass ctx back to the test
// and blocks inside GC until either release is closed or the ctx is cancelled — letting a
// test inspect the ctx of an IN-FLIGHT pass (its deadline and its cancellation).
type ctxCapturingScanner struct {
	entered chan context.Context
	release chan struct{}
}

func (s *ctxCapturingScanner) GC(ctx context.Context) (sessionstore.GCResult, error) {
	s.entered <- ctx
	select {
	case <-s.release:
	case <-ctx.Done():
	}
	return sessionstore.GCResult{}, ctx.Err()
}

// TestOffloadGCPassContextCancellation covers passContext's two safety behaviors on an
// IN-FLIGHT pass (not just the run-loop return): the per-pass ctx carries a deadline within
// the policy Timeout, and it is cancelled when Stop() or lease loss fires mid-pass.
func TestOffloadGCPassContextCancellation(t *testing.T) {
	t.Parallel()
	const timeout = 30 * time.Second
	tests := []struct {
		name    string
		trigger string // "stop" | "lost"
	}{
		{name: "stop cancels in-flight pass", trigger: "stop"},
		{name: "lease loss cancels in-flight pass", trigger: "lost"},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			gateAdmission := newJournalAdmissionGate()
			scanner := &ctxCapturingScanner{entered: make(chan context.Context, 1), release: make(chan struct{})}
			ticker := newManualTicker()
			lost := make(chan struct{})
			runner := newOffloadGCRunner(uuid.UUID{}, scanner, gateAdmission, func() offloadGCTicker { return ticker }, lost, timeout)
			runner.start(func() bool { return true })

			ticker.tick()

			var passCtx context.Context
			select {
			case passCtx = <-scanner.entered:
			case <-time.After(2 * time.Second):
				t.Fatal("GC pass never started")
			}

			// (a) The per-pass ctx carries a deadline within the policy Timeout.
			deadline, ok := passCtx.Deadline()
			if !ok {
				t.Fatal("pass ctx has no deadline; Timeout not applied")
			}
			remaining := time.Until(deadline)
			if remaining <= 0 || remaining > timeout {
				t.Fatalf("pass ctx deadline %v out of (0, %v]", remaining, timeout)
			}
			// Not cancelled while the pass is still in flight and nothing has fired.
			select {
			case <-passCtx.Done():
				t.Fatal("pass ctx cancelled before any trigger")
			default:
			}

			// (b)/(c) Fire the trigger mid-pass and prove the ctx is cancelled.
			switch tt.trigger {
			case "stop":
				go runner.Stop() // Stop blocks until the (now-cancelled) pass returns
			case "lost":
				close(lost)
			}
			select {
			case <-passCtx.Done():
			case <-time.After(2 * time.Second):
				t.Fatalf("pass ctx not cancelled after %s fired", tt.trigger)
			}

			close(scanner.release) // let GC return so the runner can drain
			runner.Stop()
		})
	}
}
