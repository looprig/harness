package sessionruntime

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/looprig/core/content"
	"github.com/looprig/harness/pkg/event"
	sessionapi "github.com/looprig/harness/pkg/session"
	"github.com/looprig/harness/pkg/sessionstore"
	"github.com/looprig/storage"
	"github.com/looprig/storage/memstore"
)

// outageLedger is a storage.Ledger whose Append fails while an outage is on and
// succeeds again once it is off: a PostgreSQL or S3 blip, not a lost database. It
// counts every append that reached the backend so a test can prove what was (and
// was not) written after a given point.
type outageLedger struct {
	storage.Ledger
	down     atomic.Bool
	mu       sync.Mutex
	appended int
}

var errOutage = errors.New("injected storage outage")

func (l *outageLedger) Append(ctx context.Context, name string, expected uint64, payload []byte) error {
	if l.down.Load() {
		return errOutage
	}
	if err := l.Ledger.Append(ctx, name, expected, payload); err != nil {
		return err
	}
	l.mu.Lock()
	l.appended++
	l.mu.Unlock()
	return nil
}

func (l *outageLedger) appends() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.appended
}

func newOutageStore(t *testing.T) (*sessionstore.Store, *outageLedger) {
	t.Helper()
	base := memstore.New()
	ledger := &outageLedger{Ledger: base.Ledger}
	backend, err := storage.NewCompositeWithOrderedIndex(ledger, base.Leaser, base.KV, base.Blobs, base.OrderedIndex)
	if err != nil {
		t.Fatal(err)
	}
	store, err := sessionstore.Open(backend)
	if err != nil {
		t.Fatal(err)
	}
	return store, ledger
}

// submitAndSettle submits one input and waits for it to settle: its turn's terminal
// (then whole-session idle) on a healthy session, or the latched persistence fault.
// It never trusts WaitIdle alone, which answers nil before the turn has started.
func submitAndSettle(t *testing.T, s *Session, text string) error {
	t.Helper()
	sub, err := s.SubscribeEvents(event.EventFilter{Enduring: event.LoopScope{All: true}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sub.Close() }()
	if _, err := s.Submit(context.Background(), []content.Block{&content.TextBlock{Text: text}}); err != nil {
		return err
	}
	deadline := time.After(5 * time.Second)
	for {
		select {
		case delivery, open := <-sub.Events():
			if !open {
				return s.PersistenceFault()
			}
			switch delivery.Event.(type) {
			case event.TurnDone, event.TurnFailed, event.TurnRejected:
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				return s.WaitIdle(ctx)
			}
		case <-s.PersistenceFaulted():
			return s.PersistenceFault()
		case <-deadline:
			t.Fatalf("input %q neither settled nor faulted", text)
		}
	}
}

// turnsCarrying counts the durable TurnStarted records whose user message is text.
func turnsCarrying(events []event.Event, text string) int {
	n := 0
	for _, ev := range events {
		started, ok := ev.(event.TurnStarted)
		if !ok || started.Message == nil {
			continue
		}
		for _, block := range started.Message.Blocks {
			if tb, ok := block.(*content.TextBlock); ok && tb.Text == text {
				n++
			}
		}
	}
	return n
}

// awaitLedgerQuiet waits until no append has reached the ledger for a quiet period
// and returns the settled count.
func awaitLedgerQuiet(t *testing.T, ledger *outageLedger) int {
	t.Helper()
	const quiet = 200 * time.Millisecond
	deadline := time.Now().Add(5 * time.Second)
	last := ledger.appends()
	for time.Now().Before(deadline) {
		time.Sleep(quiet)
		now := ledger.appends()
		if now == last {
			return now
		}
		last = now
	}
	t.Fatal("the ledger never went quiet")
	return 0
}

// faultThroughOutage drives one session into a persistence fault the way the P3.1
// cloud lane did: a turn runs while the ledger refuses every append, and the ledger
// then comes back.
func faultThroughOutage(t *testing.T, s *Session, ledger *outageLedger) {
	t.Helper()
	if err := submitAndSettle(t, s, "before"); err != nil {
		t.Fatalf("pre-outage turn: %v", err)
	}
	ledger.down.Store(true)
	if err := submitAndSettle(t, s, "during"); err == nil {
		t.Fatal("a turn run through a storage outage settled idle; the outage never reached the durable tap")
	}
	ledger.down.Store(false)
}

// TestTransientOutageLatchesTheSessionForGood is the D3 reproduction, kept as a
// characterization: one failed required append latches the session faulted, and it
// stays faulted after storage recovers. That is deliberate fail-closed design (the
// in-memory state and the durable log may now disagree), which is why recovery is a
// RESTORE, never an in-place reset.
func TestTransientOutageLatchesTheSessionForGood(t *testing.T) {
	t.Parallel()
	store, ledger := newOutageStore(t)
	lifecycle, err := newTestLifecycle(cfg(&stubLLM{chunks: []content.Chunk{textChunk("ok")}}), store)
	if err != nil {
		t.Fatal(err)
	}
	s, err := lifecycle.NewSession(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.AbandonResidency(context.Background()) })
	faultThroughOutage(t, s, ledger)

	_, err = s.Submit(context.Background(), []content.Block{&content.TextBlock{Text: "after"}})
	var sessionErr *SessionError
	if !errors.As(err, &sessionErr) || sessionErr.Kind != SessionFaulted {
		t.Fatalf("Submit after the outage = %v, want the latched SessionFaulted", err)
	}
	if !errors.Is(err, errOutage) {
		t.Errorf("the latched fault does not chain the storage failure: %v", err)
	}
	// A supervisor that first asks AFTER the fault latched must see it at once.
	select {
	case <-s.PersistenceFaulted():
	default:
		t.Error("PersistenceFaulted first requested after the fault is not closed")
	}
}

// TestPersistenceFaultIsObservable pins the health signal a supervisor selects on:
// the channel closes exactly when the terminal fault latches, and the accessor then
// names the storage failure.
func TestPersistenceFaultIsObservable(t *testing.T) {
	t.Parallel()
	store, ledger := newOutageStore(t)
	lifecycle, err := newTestLifecycle(cfg(&stubLLM{chunks: []content.Chunk{textChunk("ok")}}), store)
	if err != nil {
		t.Fatal(err)
	}
	s, err := lifecycle.NewSession(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.AbandonResidency(context.Background()) })
	var reporter sessionapi.PersistenceFaultReporter = s
	faulted := reporter.PersistenceFaulted()

	if err := submitAndSettle(t, s, "healthy"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-faulted:
		t.Fatal("PersistenceFaulted closed on a healthy session")
	default:
	}
	if err := reporter.PersistenceFault(); err != nil {
		t.Fatalf("PersistenceFault on a healthy session = %v, want nil", err)
	}

	faultThroughOutage(t, s, ledger)
	select {
	case <-faulted:
	case <-time.After(5 * time.Second):
		t.Fatal("PersistenceFaulted did not close after the fault latched")
	}
	if err := reporter.PersistenceFault(); !errors.Is(err, errOutage) {
		t.Fatalf("PersistenceFault = %v, want the storage failure", err)
	}
	if again := reporter.PersistenceFaulted(); again != faulted {
		t.Error("PersistenceFaulted returned a different channel on a second call")
	}
}

// TestAbandonedFaultedSessionRestoresAndAppliesNewInput is the recovery contract: a
// faulted session abandoned crash-equivalently writes NOTHING more (no SessionStopped,
// no release record), gives its journal lease back, and a successor restores it from
// the durable journal and applies new input exactly once.
func TestAbandonedFaultedSessionRestoresAndAppliesNewInput(t *testing.T) {
	t.Parallel()
	store, ledger := newOutageStore(t)
	lifecycle, err := newTestLifecycle(cfg(&stubLLM{chunks: []content.Chunk{textChunk("ok")}}), store)
	if err != nil {
		t.Fatal(err)
	}
	s, err := lifecycle.NewSession(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	sessionID := s.SessionID()
	faultThroughOutage(t, s, ledger)
	// The loop still resolves the input whose append failed with a durable terminal
	// (TurnRejected, then LoopIdle) once storage is back; that is designed, not the
	// abandon's doing. Let it land before measuring what the abandon itself writes.
	before := awaitLedgerQuiet(t, ledger)
	var abandoner sessionapi.ResidencyAbandoner = s
	if err := abandoner.AbandonResidency(context.Background()); err != nil {
		t.Fatalf("AbandonResidency: %v", err)
	}
	select {
	case <-s.Done():
	default:
		t.Error("Done still open after AbandonResidency")
	}
	if got := ledger.appends(); got != before {
		t.Fatalf("AbandonResidency appended %d journal record(s); a crash-equivalent release must append nothing", got-before)
	}
	for _, ev := range replayAllSessionEvents(t, store, sessionID) {
		switch ev.(type) {
		case event.SessionStopped:
			t.Fatal("the abandoned session journaled SessionStopped: it is terminal and can never be restored")
		case event.SessionResidencyReleased:
			t.Fatal("the abandoned session journaled a release record anchored to a log it cannot trust")
		}
	}

	restored, err := lifecycle.RestoreSession(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("RestoreSession after AbandonResidency: %v", err)
	}
	t.Cleanup(func() { _ = restored.Shutdown(context.Background()) })
	if err := submitAndSettle(t, restored, "after"); err != nil {
		t.Fatalf("the restored session could not apply new input: %v", err)
	}
	events := replayAllSessionEvents(t, store, sessionID)
	for text, want := range map[string]int{"before": 1, "after": 1} {
		if got := turnsCarrying(events, text); got != want {
			t.Errorf("durable turns carrying %q = %d, want %d", text, got, want)
		}
	}
	if got := turnsCarrying(events, "during"); got > 1 {
		t.Errorf("durable turns carrying %q = %d, want at most 1", "during", got)
	}
}

// TestAbandonResidencyIsNonterminalOnAHealthySession: the release is crash-equivalent
// whatever the session's health, so a supervisor may use it on any runtime it can no
// longer trust. It still writes nothing and still leaves the session restorable.
func TestAbandonResidencyIsNonterminalOnAHealthySession(t *testing.T) {
	t.Parallel()
	store, ledger := newOutageStore(t)
	lifecycle, err := newTestLifecycle(cfg(&stubLLM{chunks: []content.Chunk{textChunk("ok")}}), store)
	if err != nil {
		t.Fatal(err)
	}
	s, err := lifecycle.NewSession(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if err := submitAndSettle(t, s, "one"); err != nil {
		t.Fatal(err)
	}
	before := ledger.appends()
	if err := s.AbandonResidency(context.Background()); err != nil {
		t.Fatalf("AbandonResidency: %v", err)
	}
	if got := ledger.appends(); got != before {
		t.Fatalf("AbandonResidency appended %d record(s), want 0", got-before)
	}
	// A second call joins the finished teardown rather than failing.
	if err := s.AbandonResidency(context.Background()); err != nil {
		t.Fatalf("repeated AbandonResidency: %v", err)
	}
	restored, err := lifecycle.RestoreSession(context.Background(), s.SessionID())
	if err != nil {
		t.Fatalf("RestoreSession: %v", err)
	}
	t.Cleanup(func() { _ = restored.Shutdown(context.Background()) })
}

// TestPersistentOutageFailsTheRestoreVisibly: when storage does NOT come back, the
// successor's restore is refused with an error rather than yielding a live session
// that cannot persist.
func TestPersistentOutageFailsTheRestoreVisibly(t *testing.T) {
	t.Parallel()
	store, ledger := newOutageStore(t)
	lifecycle, err := newTestLifecycle(cfg(&stubLLM{chunks: []content.Chunk{textChunk("ok")}}), store)
	if err != nil {
		t.Fatal(err)
	}
	s, err := lifecycle.NewSession(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if err := submitAndSettle(t, s, "before"); err != nil {
		t.Fatal(err)
	}
	ledger.down.Store(true)
	if err := submitAndSettle(t, s, "during"); err == nil {
		t.Fatal("turn settled through an outage")
	}
	if err := s.AbandonResidency(context.Background()); err != nil {
		t.Fatalf("AbandonResidency during the outage: %v", err)
	}
	restored, err := lifecycle.RestoreSession(context.Background(), s.SessionID())
	if err == nil {
		_ = restored.Shutdown(context.Background())
		t.Fatal("RestoreSession succeeded while the journal refuses every append")
	}
}

// TestPersistenceFaultedFirstAskedAfterTheLatchIsClosed: a supervisor that attaches
// to a session which faulted before anyone asked must still see the fault at once.
func TestPersistenceFaultedFirstAskedAfterTheLatchIsClosed(t *testing.T) {
	t.Parallel()
	s, err := newTestSession(context.Background(), cfg(&stubLLM{}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.AbandonResidency(context.Background()) })
	boom := errors.New("durable log lost")
	s.latchSessionFault(boom)
	select {
	case <-s.PersistenceFaulted():
	default:
		t.Fatal("PersistenceFaulted first requested after the latch is open")
	}
	if err := s.PersistenceFault(); !errors.Is(err, boom) {
		t.Fatalf("PersistenceFault = %v, want the latched cause", err)
	}
}

// TestAbandonResidencyLeavesAnInFlightTurnAsCrashDebt: a turn running when the
// runtime is abandoned gets NO durable terminal — no TurnInterrupted from the loop's
// shutdown — exactly as if the process had died; the successor's restore owns it.
func TestAbandonResidencyLeavesAnInFlightTurnAsCrashDebt(t *testing.T) {
	t.Parallel()
	store, ledger := newOutageStore(t)
	lifecycle, err := newTestLifecycle(cfg(&stubLLM{blockUntilCancel: true}), store)
	if err != nil {
		t.Fatal(err)
	}
	s, err := lifecycle.NewSession(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	sub, err := s.SubscribeEvents(event.EventFilter{Enduring: event.LoopScope{All: true}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Submit(context.Background(), []content.Block{&content.TextBlock{Text: "long"}}); err != nil {
		t.Fatal(err)
	}
	awaitTurnStarted(t, sub)
	_ = sub.Close()
	before := awaitLedgerQuiet(t, ledger)
	if err := s.AbandonResidency(context.Background()); err != nil {
		t.Fatalf("AbandonResidency: %v", err)
	}
	if got := ledger.appends(); got != before {
		t.Fatalf("abandoning a busy runtime appended %d record(s), want 0", got-before)
	}
	for _, ev := range replayAllSessionEvents(t, store, s.SessionID()) {
		switch ev.(type) {
		case event.TurnInterrupted, event.TurnDone, event.TurnFailed, event.SessionStopped:
			t.Fatalf("abandon journaled %T for the in-flight turn", ev)
		}
	}
	restored, err := lifecycle.RestoreSession(context.Background(), s.SessionID())
	if err != nil {
		t.Fatalf("RestoreSession: %v", err)
	}
	t.Cleanup(func() { _ = restored.Shutdown(context.Background()) })
}
