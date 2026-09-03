package sessionruntime

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/looprig/core/content"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/journal"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/runtimecommand"
	"github.com/looprig/harness/pkg/workspacestore"
	"github.com/looprig/storage"
	"github.com/looprig/storage/memstore"
)

// --- fixtures ---------------------------------------------------------------

// fixedEpochLease is a leaseEpochSource reporting one held epoch. The residency
// record must carry THAT number, so a hardcoded zero is distinguishable from it.
type fixedEpochLease struct{ epoch uint64 }

func (l fixedEpochLease) Epoch() uint64 { return l.epoch }
func (l fixedEpochLease) Valid() bool   { return true }

// unusedRuntimeCommandLog satisfies the log half of WithRuntimeCommands, which
// refuses to wire the lease at all unless both halves are present. No test here
// applies a runtime command, so every method is an error.
type unusedRuntimeCommandLog struct{}

func (unusedRuntimeCommandLog) AppendCommandApplication(context.Context, runtimecommand.Application) (journal.AppendResult, error) {
	return journal.AppendResult{}, errors.New("unused")
}

func (unusedRuntimeCommandLog) ReadCommandApplicationAt(context.Context, uint64) (runtimecommand.Application, error) {
	return runtimecommand.Application{}, errors.New("unused")
}

// countResidencyEvents returns the nonterminal residency records and the number of
// terminal stops in an appended stream. Every release assertion reads BOTH: "a
// residency record was written" and "the session was not ended" are separate claims,
// and one of them holding does not imply the other.
func countResidencyEvents(events []event.Event) (released []event.SessionResidencyReleased, stopped int) {
	for _, ev := range events {
		switch e := ev.(type) {
		case event.SessionResidencyReleased:
			released = append(released, e)
		case event.SessionStopped:
			stopped++
		}
	}
	return released, stopped
}

// lastCheckpointSequence returns the 1-based append position of the final
// WorkspaceCheckpointed, which is the sequence recordingEventAppender assigned it.
func lastCheckpointSequence(events []event.Event) uint64 {
	var seq uint64
	for i, ev := range events {
		if _, ok := ev.(event.WorkspaceCheckpointed); ok {
			seq = uint64(i + 1)
		}
	}
	return seq
}

// assertNoTerminalStop is the clause every release case shares, including the ones
// that expect NO residency record: a nonterminal path must never end the session.
func assertNoTerminalStop(t *testing.T, events []event.Event) {
	t.Helper()
	if _, stopped := countResidencyEvents(events); stopped != 0 {
		t.Errorf("SessionStopped count = %d, want 0: this path must not end the logical session", stopped)
	}
}

// assertReleasedExactlyOnce is the shared SUCCESS assertion: exactly one residency
// record, no terminal stop, the lease epoch the session held, and an anchor equal to
// the last workspace checkpoint the session actually appended.
//
// A case that disagrees with one clause calls assertNoTerminalStop (and the others it
// still wants) rather than opting out of this helper, which would silently drop every
// clause it would have passed.
func assertReleasedExactlyOnce(t *testing.T, events []event.Event, wantEpoch uint64) event.SessionResidencyReleased {
	t.Helper()
	released, _ := countResidencyEvents(events)
	if len(released) != 1 {
		t.Fatalf("SessionResidencyReleased count = %d, want exactly 1", len(released))
	}
	assertNoTerminalStop(t, events)
	if got := released[0].LeaseEpoch; got != wantEpoch {
		t.Errorf("LeaseEpoch = %d, want %d", got, wantEpoch)
	}
	if got, want := released[0].CheckpointSeq, lastCheckpointSequence(events); got != want {
		t.Errorf("CheckpointSeq = %d, want %d (the sequence of the last WorkspaceCheckpointed appended)", got, want)
	}
	return released[0]
}

const releaseFixtureEpoch = 11

type releaseFixture struct {
	session  *Session
	recorder *recordingEventAppender
	// leaseOrder is the labels of the lease releases, in the order they ran.
	leaseOrder func() []string
	// doneAtRelease reports whether Done was already closed when each lease released.
	doneAtRelease func() map[string]bool
}

func newReleaseFixture(t *testing.T, blobs storage.Blobs, definition loop.Definition, extra ...Option) *releaseFixture {
	t.Helper()
	ws, err := workspacestore.Open(blobs)
	if err != nil {
		t.Fatalf("workspacestore.Open: %v", err)
	}
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "work.txt"), []byte("work"), 0o600); err != nil {
		t.Fatal(err)
	}
	recorder := &recordingEventAppender{}
	var mu sync.Mutex
	var order []string
	doneSeen := map[string]bool{}
	f := &releaseFixture{recorder: recorder}
	record := func(label string) {
		mu.Lock()
		order = append(order, label)
		select {
		case <-f.session.Done():
			doneSeen[label] = true
		default:
			doneSeen[label] = false
		}
		mu.Unlock()
	}
	options := []Option{
		WithEventAppender(recorder),
		WithRuntimeCommands(unusedRuntimeCommandLog{}, fixedEpochLease{epoch: releaseFixtureEpoch}),
		WithLeaseRelease(func(context.Context) error { record("session"); return nil }),
		withResolvedPlacement(&resolvedPlacement{
			mode: PlacementSession, store: ws, root: root, coordinator: newWorkspaceCoordinator(nil),
			rootRelease: func(context.Context) error { record("root"); return nil },
		}),
		WithSnapshotPolicy(SnapshotPolicy{Trigger: SnapshotOnIdle, Priority: SnapshotRequired, Timeout: time.Second}),
	}
	options = append(options, extra...)
	s, err := newTestSession(context.Background(), definition, options...)
	if err != nil {
		t.Fatalf("newTestSession: %v", err)
	}
	f.session = s
	f.leaseOrder = func() []string { mu.Lock(); defer mu.Unlock(); return append([]string(nil), order...) }
	f.doneAtRelease = func() map[string]bool {
		mu.Lock()
		defer mu.Unlock()
		out := map[string]bool{}
		for k, v := range doneSeen {
			out[k] = v
		}
		return out
	}
	return f
}

func newIdleReleaseFixture(t *testing.T, extra ...Option) *releaseFixture {
	t.Helper()
	return newReleaseFixture(t, memstore.New().Blobs, cfg(&stubLLM{}), extra...)
}

// --- tests ------------------------------------------------------------------

// TestReleaseResidencyAppendsNonterminalRecordWithoutStopping is the core contract: a
// clean release writes ONE SessionResidencyReleased carrying the checkpoint it is
// anchored to and the lease epoch that produced it, and writes NO SessionStopped.
func TestReleaseResidencyAppendsNonterminalRecordWithoutStopping(t *testing.T) {
	t.Parallel()
	f := newIdleReleaseFixture(t)
	if err := f.session.ReleaseResidency(context.Background()); err != nil {
		t.Fatalf("ReleaseResidency: %v", err)
	}
	got := assertReleasedExactlyOnce(t, f.recorder.snapshot(), releaseFixtureEpoch)
	if got.CheckpointSeq == 0 {
		t.Error("CheckpointSeq = 0 on a workspace-backed session: the release anchored to no checkpoint")
	}
}

// TestReleaseResidencyStopsRuntimeAndReleasesLeases proves the release actually gives
// the runtime up: the checkpoint controller closes, the session context cancels, and
// BOTH leases release in the same LIFO order Shutdown uses.
func TestReleaseResidencyStopsRuntimeAndReleasesLeases(t *testing.T) {
	t.Parallel()
	f := newIdleReleaseFixture(t)
	if err := f.session.ReleaseResidency(context.Background()); err != nil {
		t.Fatalf("ReleaseResidency: %v", err)
	}
	select {
	case <-f.session.checkpoints.ctx.Done():
	default:
		t.Error("checkpoint controller still active after ReleaseResidency")
	}
	select {
	case <-f.session.sessionCtx.Done():
	default:
		t.Error("session context still live after ReleaseResidency")
	}
	if order := f.leaseOrder(); len(order) != 2 || order[0] != "root" || order[1] != "session" {
		t.Errorf("lease release order = %v, want [root session]", order)
	}
}

// TestReleaseResidencyClosesDoneAtTeardownStart pins that Done is a teardown-STARTED
// broadcast on this path too. The probe runs inside the lease release hooks, which are
// near the END of teardown: seeing Done already closed there proves it closed first.
func TestReleaseResidencyClosesDoneAtTeardownStart(t *testing.T) {
	t.Parallel()
	f := newIdleReleaseFixture(t)
	select {
	case <-f.session.Done():
		t.Fatal("Done closed before ReleaseResidency was called")
	default:
	}
	if err := f.session.ReleaseResidency(context.Background()); err != nil {
		t.Fatalf("ReleaseResidency: %v", err)
	}
	select {
	case <-f.session.Done():
	default:
		t.Fatal("Done not closed after ReleaseResidency")
	}
	for label, seen := range f.doneAtRelease() {
		if !seen {
			t.Errorf("Done was still open when the %s lease released: it must close at the START of teardown", label)
		}
	}
}

// TestReleaseResidencyRefusesWhileWorkIsInFlight proves the idle precondition. The
// refusal must be CLEAN: no residency record, no stop, and the session still resident
// (Done still open), because a host that cannot release must keep serving.
func TestReleaseResidencyRefusesWhileWorkIsInFlight(t *testing.T) {
	t.Parallel()
	f := newReleaseFixture(t, memstore.New().Blobs, cfg(&stubLLM{blockUntilCancel: true}))
	t.Cleanup(func() { _ = f.session.Shutdown(context.Background()) })
	sub, err := f.session.SubscribeEvents(event.EventFilter{Enduring: event.LoopScope{All: true}})
	if err != nil {
		t.Fatalf("SubscribeEvents: %v", err)
	}
	t.Cleanup(func() { _ = sub.Close() })
	if _, err := f.session.Submit(context.Background(), []content.Block{&content.TextBlock{Text: "hi"}}); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	awaitTurnStarted(t, sub)
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	err = f.session.ReleaseResidency(ctx)
	var refused *ResidencyReleaseRefusedError
	if !errors.As(err, &refused) {
		t.Fatalf("ReleaseResidency err = %v, want *ResidencyReleaseRefusedError while a turn was still running", err)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("refusal cause = %v, want the idle wait's own deadline", err)
	}
	released, _ := countResidencyEvents(f.recorder.snapshot())
	if len(released) != 0 {
		t.Errorf("SessionResidencyReleased count = %d, want 0 for a refused release", len(released))
	}
	assertNoTerminalStop(t, f.recorder.snapshot())
	select {
	case <-f.session.Done():
		t.Error("Done closed by a REFUSED release: the session must remain resident")
	default:
	}
	if order := f.leaseOrder(); len(order) != 0 {
		t.Errorf("lease releases after a refused release = %v, want none", order)
	}
}

// TestReleaseResidencyReportsCheckpointFailureAndWritesNoRecord proves the release is
// anchored to a checkpoint that actually committed: when the checkpoint cannot be
// written the caller learns, and no residency record claims an anchor that does not
// exist. It still writes no SessionStopped.
func TestReleaseResidencyReportsCheckpointFailureAndWritesNoRecord(t *testing.T) {
	t.Parallel()
	boom := errors.New("blob store down")
	f := newReleaseFixture(t, failingPutBlobs{Blobs: memstore.New().Blobs, err: boom}, cfg(&stubLLM{}))
	err := f.session.ReleaseResidency(context.Background())
	if !errors.Is(err, boom) {
		t.Fatalf("ReleaseResidency err = %v, want it to wrap %v", err, boom)
	}
	released, _ := countResidencyEvents(f.recorder.snapshot())
	if len(released) != 0 {
		t.Errorf("SessionResidencyReleased count = %d, want 0 when the required checkpoint failed", len(released))
	}
	assertNoTerminalStop(t, f.recorder.snapshot())
}

// TestRepeatedReleaseResidencyJoinsOneTeardownOwner proves concurrent and repeated
// callers share ONE teardown: exactly one residency record however many callers race,
// exactly two lease releases, and the same result for every caller.
func TestRepeatedReleaseResidencyJoinsOneTeardownOwner(t *testing.T) {
	t.Parallel()
	f := newIdleReleaseFixture(t)
	const callers = 8
	results := make(chan error, callers)
	var wg sync.WaitGroup
	for range callers {
		wg.Add(1)
		go func() { defer wg.Done(); results <- f.session.ReleaseResidency(context.Background()) }()
	}
	wg.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Errorf("ReleaseResidency (concurrent caller) = %v, want nil", err)
		}
	}
	if err := f.session.ReleaseResidency(context.Background()); err != nil {
		t.Errorf("ReleaseResidency (repeated, after completion) = %v, want nil", err)
	}
	assertReleasedExactlyOnce(t, f.recorder.snapshot(), releaseFixtureEpoch)
	if order := f.leaseOrder(); len(order) != 2 {
		t.Errorf("lease releases = %v, want exactly one root and one session release", order)
	}
}

// TestReleaseResidencyAndShutdownRaceElectOneOwner proves the two teardown entry
// points elect ONE owner rather than each running the sequence. Whichever wins, the
// stream carries EXACTLY ONE record — never both, which would say a session was
// released and also ended.
func TestReleaseResidencyAndShutdownRaceElectOneOwner(t *testing.T) {
	t.Parallel()
	for range 16 {
		f := newIdleReleaseFixture(t)
		start := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); <-start; _ = f.session.ReleaseResidency(context.Background()) }()
		go func() { defer wg.Done(); <-start; _ = f.session.Shutdown(context.Background()) }()
		close(start)
		wg.Wait()
		released, stopped := countResidencyEvents(f.recorder.snapshot())
		if len(released)+stopped != 1 {
			t.Fatalf("released=%d stopped=%d, want exactly one teardown owner to have written its record", len(released), stopped)
		}
		if order := f.leaseOrder(); len(order) != 2 {
			t.Fatalf("lease releases = %v, want exactly two (one owner)", order)
		}
	}
}

// TestReleaseResidencyTerminatesResourcesBeforeHubAndLeases proves the release runs
// the SAME ordered teardown Shutdown does: the session resource registry (and the
// supervised process it holds) fully terminates and confirms BEFORE the hub closes and
// BEFORE any lease releases. The probe runs from inside the resource's own Shutdown.
func TestReleaseResidencyTerminatesResourcesBeforeHubAndLeases(t *testing.T) {
	t.Parallel()
	resource := &testSessionResource{}
	s := newProcessShutdownSession(t, resource)

	sub, err := s.SubscribeEvents(event.EventFilter{Enduring: event.LoopScope{All: true}})
	if err != nil {
		t.Fatalf("SubscribeEvents: %v", err)
	}
	t.Cleanup(func() { _ = sub.Close() })

	var mu sync.Mutex
	var order []string
	record := func(label string) { mu.Lock(); order = append(order, label); mu.Unlock() }
	// Drain what the hub has already delivered, from inside the resource's own
	// Shutdown. Two INDEPENDENT observations, each with its own label so a failure
	// says which ordering broke: a closed stream means the hub closed first, and a
	// SessionResidencyReleased already delivered means the release was recorded
	// before the supervised processes stopped.
	resource.shutdownHook = func() {
		for drained := false; !drained; {
			select {
			case delivery, open := <-sub.Events():
				if !open {
					record("hub-closed-before-resources")
					drained = true
					continue
				}
				if _, recorded := delivery.Event.(event.SessionResidencyReleased); recorded {
					record("residency-recorded-before-resources")
				}
			default:
				drained = true
			}
		}
		record("resources")
	}

	if err := s.ReleaseResidency(context.Background()); err != nil {
		t.Fatalf("ReleaseResidency: %v", err)
	}
	if got := resource.shutdownCalls.Load(); got != 1 {
		t.Errorf("resource Shutdown calls = %d, want 1", got)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(order) != 1 || order[0] != "resources" {
		t.Fatalf("teardown order observed from inside the resource's own Shutdown = %v, want [resources]: the release must checkpoint and record only AFTER supervised processes have stopped, and while the hub is still open", order)
	}
}

// releaseCleanupProbeKey is a value a CALLER attached to the construction context. The
// teardown context must not carry it: teardown outlives the request that built the
// session, and a background cleanup goroutine holding an auth/request scope forever is
// exactly what a fresh cleanup root prevents.
type releaseCleanupProbeKey struct{}

// capturingCommandAppender records the context each command append ran under. The
// shutdown command append is the one teardown phase that receives the cleanup root
// directly, so it is where the cleanup root's values are observable.
type capturingCommandAppender struct {
	mu   sync.Mutex
	seen []context.Context
}

func (a *capturingCommandAppender) AppendCommand(ctx context.Context, _ journal.CommandRecord) error {
	a.mu.Lock()
	a.seen = append(a.seen, ctx)
	a.mu.Unlock()
	return nil
}

func (a *capturingCommandAppender) contexts() []context.Context {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]context.Context(nil), a.seen...)
}

// TestReleaseResidencyCleanupContextDropsConstructionValues proves teardown runs on a
// FRESH internal root rather than a value-preserving copy of the construction context.
func TestReleaseResidencyCleanupContextDropsConstructionValues(t *testing.T) {
	t.Parallel()
	appender := &capturingCommandAppender{}
	ws, err := workspacestore.Open(memstore.New().Blobs)
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	constructionCtx := context.WithValue(context.Background(), releaseCleanupProbeKey{}, "caller-secret")
	s, err := newTestSession(constructionCtx, cfg(&stubLLM{}),
		WithCommandAppender(appender),
		withResolvedPlacement(&resolvedPlacement{mode: PlacementSession, store: ws, root: root, coordinator: newWorkspaceCoordinator(nil)}),
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.ReleaseResidency(context.Background()); err != nil {
		t.Fatalf("ReleaseResidency: %v", err)
	}
	seen := appender.contexts()
	if len(seen) == 0 {
		t.Fatal("no command append observed during teardown: the probe is vacuous")
	}
	for i, ctx := range seen {
		if value := ctx.Value(releaseCleanupProbeKey{}); value != nil {
			t.Errorf("teardown context %d carries the construction caller's value %v", i, value)
		}
	}
}

// TestRestoreAfterReleaseResidency is the whole point of the nonterminal path: a
// released session is COLD, not ended, so a successor restores it from the same store.
// A Shutdown-style release would have made the logical session terminal and left the
// lease held.
func TestRestoreAfterReleaseResidency(t *testing.T) {
	t.Parallel()
	store := newRestoreStore(t)
	lifecycle, err := newTestLifecycle(cfg(&stubLLM{}), store)
	if err != nil {
		t.Fatalf("NewTopologyLifecycle: %v", err)
	}
	first, err := lifecycle.NewSession(context.Background(), "")
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	sessionID := first.SessionID()
	if err := first.ReleaseResidency(context.Background()); err != nil {
		t.Fatalf("ReleaseResidency: %v", err)
	}
	second, err := lifecycle.RestoreSession(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("RestoreSession after release (lease not released, or the journal read the release as terminal?): %v", err)
	}
	t.Cleanup(func() { _ = second.Shutdown(context.Background()) })
	if second.SessionID() != sessionID {
		t.Errorf("restored SessionID = %v, want %v", second.SessionID(), sessionID)
	}
}

// awaitTurnStarted blocks until the session publishes a TurnStarted, so an idle-
// precondition test is exercising a session that is genuinely busy rather than one
// whose Submit has not yet reached the actor.
func awaitTurnStarted(t *testing.T, sub event.Subscription) {
	t.Helper()
	for {
		select {
		case delivery := <-sub.Events():
			if _, ok := delivery.Event.(event.TurnStarted); ok {
				return
			}
		case <-time.After(5 * time.Second):
			t.Fatal("turn did not start")
		}
	}
}

// TestReleaseResidencyRefusesAFaultedSession pins the OTHER admission clause. A
// faulted session's durable log is no longer trustworthy, so there is nothing safe to
// anchor a release to; the refusal carries the typed SessionFaulted surface so a host
// can tell "this session is broken" from "this session is busy" — the fault reaches
// the idle wait too, but only as the raw cause.
func TestReleaseResidencyRefusesAFaultedSession(t *testing.T) {
	t.Parallel()
	f := newIdleReleaseFixture(t)
	t.Cleanup(func() { _ = f.session.Shutdown(context.Background()) })
	boom := errors.New("durable log lost")
	f.session.latchSessionFault(boom)

	err := f.session.ReleaseResidency(context.Background())
	var refused *ResidencyReleaseRefusedError
	if !errors.As(err, &refused) {
		t.Fatalf("ReleaseResidency err = %v, want *ResidencyReleaseRefusedError", err)
	}
	var sessionErr *SessionError
	if !errors.As(err, &sessionErr) || sessionErr.Kind != SessionFaulted {
		t.Errorf("refusal cause = %v, want a *SessionError with Kind SessionFaulted", err)
	}
	if !errors.Is(err, boom) {
		t.Errorf("refusal does not chain the latched fault %v: %v", boom, err)
	}
	released, _ := countResidencyEvents(f.recorder.snapshot())
	if len(released) != 0 {
		t.Errorf("SessionResidencyReleased count = %d, want 0 for a faulted session", len(released))
	}
	select {
	case <-f.session.Done():
		t.Error("Done closed by a refused release")
	default:
	}
}
