package sessionruntime

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/looprig/core/content"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/command"
	"github.com/looprig/harness/pkg/hook"
	"github.com/looprig/harness/pkg/journal"
	"github.com/looprig/harness/pkg/runtimecommand"
	"github.com/looprig/harness/pkg/sessionstore"
	"github.com/looprig/harness/pkg/workspacestore"
	"github.com/looprig/storage/memstore"
)

// --- fixture ---------------------------------------------------------------

// runtimeCommandFixture is one durable session over an in-memory backend with a
// FAKE loop, so the only observable effect of an applied runtime command is the
// exact command value the session pushed onto the loop's command channel. The
// journal, lease, and application-prefix log are all REAL: the prefix is written
// through the same lease-fenced serialized writer production uses.
type runtimeCommandFixture struct {
	session *Session
	cmds    chan command.Command
	done    chan struct{}
	store   *sessionstore.Store
	lease   journal.Lease
	journal journal.SessionJournal
	sid     uuid.UUID
}

func newRuntimeCommandFixture(t *testing.T) *runtimeCommandFixture {
	t.Helper()
	return newRuntimeCommandFixtureOver(t, sessionstoreOverMemstore(t))
}

func sessionstoreOverMemstore(t *testing.T) *sessionstore.Store {
	t.Helper()
	store, err := sessionstore.Open(memstore.New())
	if err != nil {
		t.Fatalf("sessionstore.Open: %v", err)
	}
	return store
}

func newRuntimeCommandFixtureOver(t *testing.T, store *sessionstore.Store) *runtimeCommandFixture {
	t.Helper()
	sid := mustUUID()
	return newRuntimeCommandFixtureForSession(t, store, sid)
}

func newRuntimeCommandFixtureForSession(t *testing.T, store *sessionstore.Store, sid uuid.UUID) *runtimeCommandFixture {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	lease, err := store.AcquireLease(ctx, sid)
	if err != nil {
		t.Fatalf("AcquireLease: %v", err)
	}
	j, err := store.OpenJournal(ctx, sid, lease)
	if err != nil {
		t.Fatalf("OpenJournal: %v", err)
	}
	log, err := store.OpenRuntimeCommandLog(sid, j)
	if err != nil {
		t.Fatalf("OpenRuntimeCommandLog: %v", err)
	}

	cmds := make(chan command.Command, 4)
	done := make(chan struct{})
	sessionCtx, sessionCancel := context.WithCancel(context.Background())
	rootLoopID := mustUUID()
	s := &Session{
		sessionID:     sid,
		sessionCtx:    sessionCtx,
		sessionCancel: sessionCancel,
		loops:         map[uuid.UUID]*loopHandle{rootLoopID: {backend: &channelBackend{Commands: cmds, Done: done}}},
		activeLoopID:  rootLoopID,
		newID:         uuid.New,
	}
	WithRuntimeCommands(log, lease)(s)
	t.Cleanup(sessionCancel)
	return &runtimeCommandFixture{session: s, cmds: cmds, done: done, store: store, lease: lease, journal: j, sid: sid}
}

func (f *runtimeCommandFixture) admittedInput(id runtimecommand.CommandID, runtimeID uuid.UUID, text string) runtimecommand.Admitted {
	return runtimecommand.Admitted{
		CommandID:        id,
		RuntimeCommandID: runtimeID,
		Kind:             runtimecommand.KindInput,
		LeaseEpoch:       f.lease.Epoch(),
		Blocks:           []content.Block{&content.TextBlock{Text: text}},
	}
}

// drainOne returns the single command the session dispatched, failing if none or
// more than one arrived.
func (f *runtimeCommandFixture) drainOne(t *testing.T) command.Command {
	t.Helper()
	select {
	case cmd := <-f.cmds:
		select {
		case extra := <-f.cmds:
			t.Fatalf("a second command %T was dispatched", extra)
		default:
		}
		return cmd
	case <-time.After(2 * time.Second):
		t.Fatalf("no command dispatched")
		return nil
	}
}

func (f *runtimeCommandFixture) requireNoCommand(t *testing.T, why string) {
	t.Helper()
	select {
	case cmd := <-f.cmds:
		t.Fatalf("%s: dispatched %T, want no runtime-visible effect", why, cmd)
	case <-time.After(50 * time.Millisecond):
	}
}

// --- capability segregation ------------------------------------------------

// TestRuntimeCommandsCapabilityIsSegregated proves the apply API is an OPTIONAL
// capability: a session built without the durable prefix log does not advertise
// it, and a bare *Session is still a full session.Session. The point of the
// two-result form is that a caller learns the answer BEFORE it hands Harness an
// admitted record it cannot durably correlate.
func TestRuntimeCommandsCapabilityIsSegregated(t *testing.T) {
	t.Parallel()
	bare, _, _ := sessionWithFakeLoop()
	applier, ok := bare.RuntimeCommands()
	if ok {
		t.Errorf("RuntimeCommands() ok = true on a session with no application-prefix log, want false")
	}
	if applier != nil {
		t.Errorf("RuntimeCommands() applier = %v with ok=false, want nil", applier)
	}

	f := newRuntimeCommandFixture(t)
	applier, ok = f.session.RuntimeCommands()
	if !ok || applier == nil {
		t.Fatalf("RuntimeCommands() = (%v, %v) on a durable session, want a live applier", applier, ok)
	}
	// The capability is reachable through the interface, not only the concrete type.
	var provider runtimecommand.Provider = f.session
	if _, ok := provider.RuntimeCommands(); !ok {
		t.Errorf("*Session does not satisfy runtimecommand.Provider's positive case")
	}
}

// TestRuntimeCommandsRequiresIdempotentJournal proves the capability is refused
// over a journal that cannot deduplicate a redelivered append. Without dedup the
// duplicate-delivery contract cannot be honored, so the log constructor fails
// closed rather than advertising a guarantee it cannot keep.
func TestRuntimeCommandsRequiresIdempotentJournal(t *testing.T) {
	t.Parallel()
	store := sessionstoreOverMemstore(t)
	sid := mustUUID()
	ctx := context.Background()
	lease, err := store.AcquireLease(ctx, sid)
	if err != nil {
		t.Fatalf("AcquireLease: %v", err)
	}
	j, err := store.OpenJournal(ctx, sid, lease)
	if err != nil {
		t.Fatalf("OpenJournal: %v", err)
	}
	if _, isIdempotent := j.(journal.IdempotentJournal); !isIdempotent {
		t.Fatalf("fixture precondition: the real journal must be idempotent")
	}
	// plainOnly hides every optional contract, exactly as a demoting decorator would.
	plain := plainJournalOnly{j}
	if _, err := store.OpenRuntimeCommandLog(sid, plain); err == nil {
		t.Errorf("OpenRuntimeCommandLog over a non-idempotent journal = nil error, want a typed refusal")
	}
	// The DECORATED journal must keep the capability: this is the contract the
	// runtime-command seam rides on.
	decorated := journal.Decorate(j, func(ctx context.Context, rec journal.JournalRecord, next func(context.Context) error) error {
		return next(ctx)
	})
	if _, err := store.OpenRuntimeCommandLog(sid, decorated); err != nil {
		t.Errorf("OpenRuntimeCommandLog over a DECORATED journal = %v, want nil (decoration must preserve idempotency)", err)
	}
}

type plainJournalOnly struct{ inner journal.SessionJournal }

func (p plainJournalOnly) Append(ctx context.Context, rec journal.JournalRecord) (uint64, error) {
	return p.inner.Append(ctx, rec)
}

// --- input -----------------------------------------------------------------

// TestApplyInputUsesAdmittedRuntimeIDAndPersistsPrefix is the central contract:
// Harness does not allocate a replacement UUID. The dispatched UserInput carries
// the RuntimeCommandID Host admitted, and the durable prefix correlates the
// opaque public id with that same UUID and the lease epoch.
func TestApplyInputUsesAdmittedRuntimeIDAndPersistsPrefix(t *testing.T) {
	t.Parallel()
	f := newRuntimeCommandFixture(t)
	runtimeID := mustUUID()
	adm := f.admittedInput("v1:opaque-public-id", runtimeID, "hello")

	disp, err := f.session.ApplyRuntimeCommand(context.Background(), adm)
	if err != nil {
		t.Fatalf("ApplyRuntimeCommand: %v", err)
	}
	if disp.Duplicate {
		t.Errorf("Disposition.Duplicate = true on first delivery")
	}
	if disp.RuntimeCommandID != runtimeID {
		t.Errorf("Disposition.RuntimeCommandID = %v, want the admitted %v", disp.RuntimeCommandID, runtimeID)
	}
	if disp.PrefixSequence == 0 {
		t.Errorf("Disposition.PrefixSequence = 0, want the durable sequence of the application prefix")
	}

	cmd := f.drainOne(t)
	input, isInput := cmd.(command.UserInput)
	if !isInput {
		t.Fatalf("dispatched %T, want command.UserInput", cmd)
	}
	if input.CommandHeader().CommandID != runtimeID {
		t.Errorf("dispatched UserInput CommandID = %v, want the admitted RuntimeCommandID %v (Harness must not mint a replacement)",
			input.CommandHeader().CommandID, runtimeID)
	}

	app, err := f.store.ReadCommandApplicationAt(context.Background(), f.sid, disp.PrefixSequence)
	if err != nil {
		t.Fatalf("ReadCommandApplicationAt(%d): %v", disp.PrefixSequence, err)
	}
	if app.CommandID != adm.CommandID || app.RuntimeCommandID != runtimeID || app.LeaseEpoch != f.lease.Epoch() {
		t.Errorf("durable prefix = %+v, want {%q %v %d}", app, adm.CommandID, runtimeID, f.lease.Epoch())
	}
}

// TestApplyRefusesWhenThePrefixCannotBePersisted is the persist-before-effect
// guard stated as a conditional: if the prefix append fails, NOTHING
// runtime-visible may happen. A crash after the effect but before the prefix
// would otherwise let a redelivery apply the command twice.
func TestApplyRefusesWhenThePrefixCannotBePersisted(t *testing.T) {
	t.Parallel()
	f := newRuntimeCommandFixture(t)
	boom := errors.New("prefix append refused")
	WithRuntimeCommands(failingRuntimeCommandLog{err: boom}, f.lease)(f.session)

	_, err := f.session.ApplyRuntimeCommand(context.Background(), f.admittedInput("c1", mustUUID(), "hello"))
	if !errors.Is(err, boom) {
		t.Fatalf("ApplyRuntimeCommand err = %v, want the append failure %v", err, boom)
	}
	f.requireNoCommand(t, "prefix append failed")
}

type failingRuntimeCommandLog struct{ err error }

func (l failingRuntimeCommandLog) AppendCommandApplication(context.Context, runtimecommand.Application) (journal.AppendResult, error) {
	return journal.AppendResult{}, l.err
}

func (l failingRuntimeCommandLog) ReadCommandApplicationAt(context.Context, uint64) (runtimecommand.Application, error) {
	return runtimecommand.Application{}, l.err
}

// TestPrefixIsDurableBeforeTheEffect observes the ORDER directly, without a
// goroutine: at the instant the prefix append returns, the loop's command channel
// must still be empty, and after the application returns it must hold exactly the
// one dispatched command. An implementation that dispatched first and persisted
// afterwards would see a non-empty channel here.
func TestPrefixIsDurableBeforeTheEffect(t *testing.T) {
	t.Parallel()
	f := newRuntimeCommandFixture(t)
	pending := -1
	f.session.runtimeCommands = orderRecordingLog{
		inner: f.session.runtimeCommands,
		observe: func() {
			pending = len(f.cmds)
		},
	}
	if _, err := f.session.ApplyRuntimeCommand(context.Background(), f.admittedInput("c1", mustUUID(), "hi")); err != nil {
		t.Fatalf("ApplyRuntimeCommand: %v", err)
	}
	if pending == -1 {
		t.Fatalf("the prefix append never ran; the ordering assertion is vacuous")
	}
	if pending != 0 {
		t.Errorf("%d commands were already dispatched when the prefix was made durable, want 0", pending)
	}
	if got := len(f.cmds); got != 1 {
		t.Errorf("after the application the loop holds %d commands, want 1", got)
	}
}

// orderRecordingLog observes the moment the durable prefix append completes.
type orderRecordingLog struct {
	inner   runtimeCommandLog
	observe func()
}

func (l orderRecordingLog) AppendCommandApplication(ctx context.Context, app runtimecommand.Application) (journal.AppendResult, error) {
	res, err := l.inner.AppendCommandApplication(ctx, app)
	if err == nil {
		l.observe()
	}
	return res, err
}

func (l orderRecordingLog) ReadCommandApplicationAt(ctx context.Context, seq uint64) (runtimecommand.Application, error) {
	return l.inner.ReadCommandApplicationAt(ctx, seq)
}

// --- duplicate delivery ----------------------------------------------------

// TestDuplicateDeliveryReturnsOriginalDispositionAndAppliesOnce is the guard the
// whole seam exists for. The second delivery must report the ORIGINAL disposition
// — same runtime id, same prefix sequence — and must not dispatch a second
// command. Asserting only "no error" would pass against an implementation that
// applied twice.
func TestDuplicateDeliveryReturnsOriginalDispositionAndAppliesOnce(t *testing.T) {
	t.Parallel()
	f := newRuntimeCommandFixture(t)
	runtimeID := mustUUID()
	adm := f.admittedInput("v1:retry-stable", runtimeID, "hello")

	first, err := f.session.ApplyRuntimeCommand(context.Background(), adm)
	if err != nil {
		t.Fatalf("first ApplyRuntimeCommand: %v", err)
	}
	firstCmd := f.drainOne(t)

	second, err := f.session.ApplyRuntimeCommand(context.Background(), adm)
	if err != nil {
		t.Fatalf("duplicate ApplyRuntimeCommand: %v", err)
	}
	if !second.Duplicate {
		t.Errorf("duplicate Disposition.Duplicate = false, want true")
	}
	if second.RuntimeCommandID != first.RuntimeCommandID {
		t.Errorf("duplicate RuntimeCommandID = %v, want the original %v", second.RuntimeCommandID, first.RuntimeCommandID)
	}
	if second.PrefixSequence != first.PrefixSequence {
		t.Errorf("duplicate PrefixSequence = %d, want the ORIGINAL %d", second.PrefixSequence, first.PrefixSequence)
	}
	f.requireNoCommand(t, "duplicate delivery")
	if firstCmd.CommandHeader().CommandID != runtimeID {
		t.Errorf("first dispatch CommandID = %v, want %v", firstCmd.CommandHeader().CommandID, runtimeID)
	}
}

// TestDuplicateDeliveryUnderANewLeaseEpochStillDeduplicates covers the crash-recovery
// shape: the original application prefix was written under epoch N, the session
// crashed, a successor acquired epoch N+1, and Host redelivers. The redelivery is
// the SAME command — same public id, same runtime id — so it must resolve to the
// original disposition by READING the prefix, not be mistaken for a conflict.
func TestDuplicateDeliveryUnderANewLeaseEpochStillDeduplicates(t *testing.T) {
	t.Parallel()
	store := sessionstoreOverMemstore(t)
	sid := mustUUID()
	first := newRuntimeCommandFixtureForSession(t, store, sid)
	runtimeID := mustUUID()
	adm := first.admittedInput("v1:survives-a-crash", runtimeID, "hello")
	original, err := first.session.ApplyRuntimeCommand(context.Background(), adm)
	if err != nil {
		t.Fatalf("original ApplyRuntimeCommand: %v", err)
	}
	first.drainOne(t)
	if err := first.lease.Release(context.Background()); err != nil {
		t.Fatalf("release lease: %v", err)
	}

	successor := newRuntimeCommandFixtureForSession(t, store, sid)
	if successor.lease.Epoch() <= first.lease.Epoch() {
		t.Fatalf("fixture precondition: epoch must advance, got %d then %d", first.lease.Epoch(), successor.lease.Epoch())
	}
	redelivered := adm
	redelivered.LeaseEpoch = successor.lease.Epoch()

	disp, err := successor.session.ApplyRuntimeCommand(context.Background(), redelivered)
	if err != nil {
		t.Fatalf("redelivered ApplyRuntimeCommand: %v", err)
	}
	if !disp.Duplicate {
		t.Errorf("redelivered Disposition.Duplicate = false, want true")
	}
	if disp.PrefixSequence != original.PrefixSequence {
		t.Errorf("redelivered PrefixSequence = %d, want the ORIGINAL %d", disp.PrefixSequence, original.PrefixSequence)
	}
	if disp.RuntimeCommandID != runtimeID {
		t.Errorf("redelivered RuntimeCommandID = %v, want %v", disp.RuntimeCommandID, runtimeID)
	}
	successor.requireNoCommand(t, "redelivery under a new epoch")
}

// --- mismatched mapping ----------------------------------------------------

// TestMismatchedPublicRuntimeMappingFailsClosed proves the check is real: the
// payload is one a Host bug (or a forged retry) would produce — the same public
// CommandID bound to a DIFFERENT RuntimeCommandID — and it would be applied a
// second time under a new runtime identity if the check were removed.
func TestMismatchedPublicRuntimeMappingFailsClosed(t *testing.T) {
	t.Parallel()
	f := newRuntimeCommandFixture(t)
	originalRuntimeID := mustUUID()
	adm := f.admittedInput("v1:one-public-id", originalRuntimeID, "hello")
	original, err := f.session.ApplyRuntimeCommand(context.Background(), adm)
	if err != nil {
		t.Fatalf("original ApplyRuntimeCommand: %v", err)
	}
	f.drainOne(t)

	conflicting := adm
	conflicting.RuntimeCommandID = mustUUID()
	_, err = f.session.ApplyRuntimeCommand(context.Background(), conflicting)
	var mismatch *runtimecommand.MappingConflictError
	if !errors.As(err, &mismatch) {
		t.Fatalf("conflicting ApplyRuntimeCommand err = %v, want *runtimecommand.MappingConflictError", err)
	}
	if mismatch.DurableRuntimeID != originalRuntimeID {
		t.Errorf("MappingConflictError.DurableRuntimeID = %v, want the durable %v", mismatch.DurableRuntimeID, originalRuntimeID)
	}
	if mismatch.RuntimeCommandID != conflicting.RuntimeCommandID {
		t.Errorf("MappingConflictError.RuntimeCommandID = %v, want the offered %v", mismatch.RuntimeCommandID, conflicting.RuntimeCommandID)
	}
	if mismatch.Sequence != original.PrefixSequence {
		t.Errorf("MappingConflictError.Sequence = %d, want %d", mismatch.Sequence, original.PrefixSequence)
	}
	f.requireNoCommand(t, "mismatched public/runtime mapping")
}

// --- stale lease epoch -----------------------------------------------------

// TestStaleLeaseEpochRefusedAgainstARealEpochAdvance drives the epoch forward with a
// real lease handover rather than a hand-built struct: the successor holds a strictly
// higher epoch, and a record admitted under the predecessor's epoch is refused before
// any prefix is written and before any effect.
func TestStaleLeaseEpochRefusedAgainstARealEpochAdvance(t *testing.T) {
	t.Parallel()
	store := sessionstoreOverMemstore(t)
	sid := mustUUID()
	predecessor := newRuntimeCommandFixtureForSession(t, store, sid)
	staleEpoch := predecessor.lease.Epoch()
	if err := predecessor.lease.Release(context.Background()); err != nil {
		t.Fatalf("release predecessor lease: %v", err)
	}
	successor := newRuntimeCommandFixtureForSession(t, store, sid)
	if successor.lease.Epoch() <= staleEpoch {
		t.Fatalf("fixture precondition: epoch must advance, got %d then %d", staleEpoch, successor.lease.Epoch())
	}

	adm := successor.admittedInput("v1:stale", mustUUID(), "hello")
	adm.LeaseEpoch = staleEpoch

	disp, err := successor.session.ApplyRuntimeCommand(context.Background(), adm)
	var stale *runtimecommand.StaleLeaseEpochError
	if !errors.As(err, &stale) {
		t.Fatalf("ApplyRuntimeCommand err = %v, want *runtimecommand.StaleLeaseEpochError", err)
	}
	if stale.Admitted != staleEpoch || stale.Current != successor.lease.Epoch() {
		t.Errorf("StaleLeaseEpochError = {%d %d}, want {%d %d}", stale.Admitted, stale.Current, staleEpoch, successor.lease.Epoch())
	}
	if disp != (runtimecommand.Disposition{}) {
		t.Errorf("Disposition = %+v on refusal, want the zero value", disp)
	}
	successor.requireNoCommand(t, "stale lease epoch")
	// Nothing durable was written for the refused command, so a LATER delivery at the
	// live epoch is a FIRST delivery, not a duplicate.
	fresh := adm
	fresh.LeaseEpoch = successor.lease.Epoch()
	later, err := successor.session.ApplyRuntimeCommand(context.Background(), fresh)
	if err != nil {
		t.Fatalf("re-delivery at the live epoch: %v", err)
	}
	if later.Duplicate {
		t.Errorf("re-delivery Duplicate = true; the refused delivery must have written no prefix")
	}
}

// --- interrupt -------------------------------------------------------------

// TestApplyInterruptDispatchesAnInterruptAndCorrelatesTheCommand proves the
// interrupt kind reaches the loop and is correlated by the same durable prefix.
func TestApplyInterruptDispatchesAnInterruptAndCorrelatesTheCommand(t *testing.T) {
	t.Parallel()
	f := newRuntimeCommandFixture(t)
	runtimeID := mustUUID()
	adm := runtimecommand.Admitted{
		CommandID:        "v1:interrupt",
		RuntimeCommandID: runtimeID,
		Kind:             runtimecommand.KindInterrupt,
		LeaseEpoch:       f.lease.Epoch(),
	}
	go func() {
		cmd := <-f.cmds
		if interrupt, ok := cmd.(command.Interrupt); ok {
			interrupt.Ack <- true
		}
	}()
	disp, err := f.session.ApplyRuntimeCommand(context.Background(), adm)
	if err != nil {
		t.Fatalf("ApplyRuntimeCommand(interrupt): %v", err)
	}
	if !disp.Interrupted {
		t.Errorf("Disposition.Interrupted = false, want true when a loop reported a cancelled turn")
	}
	app, err := f.store.ReadCommandApplicationAt(context.Background(), f.sid, disp.PrefixSequence)
	if err != nil {
		t.Fatalf("ReadCommandApplicationAt: %v", err)
	}
	if app.RuntimeCommandID != runtimeID {
		t.Errorf("interrupt prefix RuntimeCommandID = %v, want %v", app.RuntimeCommandID, runtimeID)
	}
}

// TestDuplicateInterruptDeliveryDoesNotInterruptTwice pins the duplicate rule for
// the interrupt kind too: an interrupt applied twice must reach the loop exactly
// once. The counter is the load-bearing assertion — a disposition check alone would
// pass against an implementation that re-interrupted the session.
func TestDuplicateInterruptDeliveryDoesNotInterruptTwice(t *testing.T) {
	t.Parallel()
	f := newRuntimeCommandFixture(t)
	adm := runtimecommand.Admitted{
		CommandID:        "v1:interrupt-once",
		RuntimeCommandID: mustUUID(),
		Kind:             runtimecommand.KindInterrupt,
		LeaseEpoch:       f.lease.Epoch(),
	}
	var mu sync.Mutex
	delivered := 0
	go func() {
		for cmd := range f.cmds {
			if interrupt, ok := cmd.(command.Interrupt); ok {
				mu.Lock()
				delivered++
				mu.Unlock()
				interrupt.Ack <- true
			}
		}
	}()
	first, err := f.session.ApplyRuntimeCommand(context.Background(), adm)
	if err != nil {
		t.Fatalf("first interrupt: %v", err)
	}
	second, err := f.session.ApplyRuntimeCommand(context.Background(), adm)
	if err != nil {
		t.Fatalf("duplicate interrupt: %v", err)
	}
	if !second.Duplicate || second.PrefixSequence != first.PrefixSequence {
		t.Errorf("duplicate interrupt disposition = %+v, want the original prefix %d", second, first.PrefixSequence)
	}
	// Interrupted is the TRANSIENT outcome of an application, not part of the durable
	// prefix, so a duplicate cannot report it. What a duplicate must guarantee is that
	// no second interrupt reached the loop.
	if second.Interrupted {
		t.Errorf("duplicate interrupt Interrupted = true; a duplicate applies nothing")
	}
	time.Sleep(50 * time.Millisecond)
	mu.Lock()
	got := delivered
	mu.Unlock()
	if got != 1 {
		t.Errorf("interrupt delivered %d times, want exactly 1", got)
	}
}

// --- legacy callers --------------------------------------------------------

// TestLegacyUUIDOnlyCallersAreUnchanged proves the seam is additive: Submit still
// mints its own RuntimeCommandID and still works on a session that also advertises
// the apply capability, and no application prefix is written for it.
func TestLegacyUUIDOnlyCallersAreUnchanged(t *testing.T) {
	t.Parallel()
	f := newRuntimeCommandFixture(t)
	id, err := f.session.Submit(context.Background(), []content.Block{&content.TextBlock{Text: "legacy"}})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if id.IsZero() {
		t.Fatalf("Submit id is zero")
	}
	cmd := f.drainOne(t)
	if cmd.CommandHeader().CommandID != id {
		t.Errorf("legacy Submit dispatched CommandID = %v, want %v", cmd.CommandHeader().CommandID, id)
	}
	// A legacy submit is not an admitted runtime command: applying one under a public
	// id that happens to render the same UUID must still be a FIRST delivery.
	disp, err := f.session.ApplyRuntimeCommand(context.Background(), f.admittedInput(runtimecommand.CommandID(id.String()), id, "admitted"))
	if err != nil {
		t.Fatalf("ApplyRuntimeCommand after a legacy submit: %v", err)
	}
	if disp.Duplicate {
		t.Errorf("Duplicate = true; a legacy Submit must not write an application prefix")
	}
}

// TestApplyValidatesBeforeAnyDurableWrite proves a malformed admitted record is
// refused with no prefix and no effect.
func TestApplyValidatesBeforeAnyDurableWrite(t *testing.T) {
	t.Parallel()
	f := newRuntimeCommandFixture(t)
	bad := f.admittedInput("", mustUUID(), "hello")
	if _, err := f.session.ApplyRuntimeCommand(context.Background(), bad); err == nil {
		t.Fatalf("ApplyRuntimeCommand with an empty public id = nil error, want a refusal")
	}
	f.requireNoCommand(t, "invalid admitted record")
	zeroRuntime := f.admittedInput("c1", uuid.UUID{}, "hello")
	if _, err := f.session.ApplyRuntimeCommand(context.Background(), zeroRuntime); err == nil {
		t.Fatalf("ApplyRuntimeCommand with a zero RuntimeCommandID = nil error, want a refusal")
	}
	f.requireNoCommand(t, "zero runtime command id")
	// The two rows above are ALSO caught by the record codec, which validates the
	// correlation before it is made durable. These are not: their correlation is
	// perfectly valid, so only Admitted.Validate stands between them and a dispatched
	// command with no content (or a silently dropped payload).
	emptyInput := f.admittedInput("c2", mustUUID(), "hello")
	emptyInput.Blocks = nil
	if _, err := f.session.ApplyRuntimeCommand(context.Background(), emptyInput); err == nil {
		t.Fatalf("ApplyRuntimeCommand with an input carrying no content = nil error, want a refusal")
	}
	f.requireNoCommand(t, "input with no content")
	interruptWithPayload := f.admittedInput("c3", mustUUID(), "hello")
	interruptWithPayload.Kind = runtimecommand.KindInterrupt
	if _, err := f.session.ApplyRuntimeCommand(context.Background(), interruptWithPayload); err == nil {
		t.Fatalf("ApplyRuntimeCommand with an interrupt carrying input blocks = nil error, want a refusal")
	}
	f.requireNoCommand(t, "interrupt carrying a payload")
}

// TestApplyDoesNotParseThePublicIDAsAUUID is the oracle for the opacity rule: two
// public ids that differ ONLY in whether they are parseable as UUIDs behave
// identically, and a public id that renders a valid UUID is NOT interchangeable
// with the runtime id it maps to.
func TestApplyDoesNotParseThePublicIDAsAUUID(t *testing.T) {
	t.Parallel()
	f := newRuntimeCommandFixture(t)
	uuidShaped := mustUUID()
	runtimeID := mustUUID()
	if uuidShaped == runtimeID {
		t.Fatalf("fixture precondition: the two identities must differ")
	}
	if _, err := f.session.ApplyRuntimeCommand(context.Background(),
		f.admittedInput(runtimecommand.CommandID(uuidShaped.String()), runtimeID, "a")); err != nil {
		t.Fatalf("uuid-shaped public id: %v", err)
	}
	if got := f.drainOne(t).CommandHeader().CommandID; got != runtimeID {
		t.Errorf("dispatched CommandID = %v, want the RuntimeCommandID %v (never the public id)", got, runtimeID)
	}
	if _, err := f.session.ApplyRuntimeCommand(context.Background(),
		f.admittedInput("not-a-uuid-at-all", mustUUID(), "b")); err != nil {
		t.Fatalf("opaque public id: %v", err)
	}
	f.drainOne(t)
}

// --- create / restore compatibility ---------------------------------------

// TestRuntimeCommandsSurviveCreateAndRestore is the create/restore compatibility
// guard. A session built through the ordinary lifecycle advertises the capability;
// an applied command's prefix is durable; and after a full restore over the SAME
// ledger the redelivery of that command deduplicates against the durable prefix
// rather than applying a second time. A restore that could not decode the private
// prefix record would fail this at the restore, not at the redelivery.
func TestRuntimeCommandsSurviveCreateAndRestore(t *testing.T) {
	t.Parallel()
	store := sessionstoreOverMemstore(t)
	definition := cfg(&stubLLM{chunks: []content.Chunk{textChunk("x")}})
	lifecycle, err := newTestLifecycle(definition, store)
	if err != nil {
		t.Fatalf("newTestLifecycle: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	original, err := lifecycle.NewSession(ctx, workspacestore.Ref(""))
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	applier, ok := original.RuntimeCommands()
	if !ok {
		t.Fatalf("a lifecycle-built session does not advertise the runtime-command capability")
	}

	runtimeID := mustUUID()
	adm := runtimecommand.Admitted{
		CommandID:        "v1:survives-restore",
		RuntimeCommandID: runtimeID,
		Kind:             runtimecommand.KindInput,
		LeaseEpoch:       original.runtimeCommandLease.Epoch(),
		Blocks:           []content.Block{&content.TextBlock{Text: "hello"}},
	}
	first, err := applier.ApplyRuntimeCommand(ctx, adm)
	if err != nil {
		t.Fatalf("ApplyRuntimeCommand: %v", err)
	}
	if first.Duplicate || first.PrefixSequence == 0 {
		t.Fatalf("first disposition = %+v, want a fresh application", first)
	}
	if err := original.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}

	restored, err := lifecycle.RestoreSession(ctx, original.SessionID())
	if err != nil {
		t.Fatalf("RestoreSession: %v", err)
	}
	t.Cleanup(func() { _ = restored.Shutdown(context.Background()) })
	restoredApplier, ok := restored.RuntimeCommands()
	if !ok {
		t.Fatalf("a restored session does not advertise the runtime-command capability")
	}
	redelivered := adm
	redelivered.LeaseEpoch = restored.runtimeCommandLease.Epoch()
	if redelivered.LeaseEpoch == adm.LeaseEpoch {
		t.Fatalf("precondition: restore must hold a new lease epoch, both are %d", adm.LeaseEpoch)
	}
	second, err := restoredApplier.ApplyRuntimeCommand(ctx, redelivered)
	if err != nil {
		t.Fatalf("redelivered ApplyRuntimeCommand: %v", err)
	}
	if !second.Duplicate {
		t.Errorf("redelivered after restore Duplicate = false, want true")
	}
	if second.PrefixSequence != first.PrefixSequence {
		t.Errorf("redelivered PrefixSequence = %d, want the ORIGINAL %d", second.PrefixSequence, first.PrefixSequence)
	}
	if second.RuntimeCommandID != runtimeID {
		t.Errorf("redelivered RuntimeCommandID = %v, want %v", second.RuntimeCommandID, runtimeID)
	}
}

// TestOpaquePublicIDsTheAdmissionAuthorityAcceptsAreApplicable is the end-to-end
// consequence of the opacity rule. Each id here is one Core's sessionwire/v1
// CommandID accepts, so Factory can durably admit it — and an applier that refused
// it would leave the command unapplicable until its apply deadline. Each must apply
// on first delivery and DEDUPLICATE on the second, which is the part that proves the
// id survived into the durable idempotency key intact rather than being mangled.
func TestOpaquePublicIDsTheAdmissionAuthorityAcceptsAreApplicable(t *testing.T) {
	t.Parallel()
	ids := []runtimecommand.CommandID{
		" leading-space",
		"trailing-space ",
		"embedded\ttab",
		"embedded\nnewline",
		"embedded\x00nul",
		"embedded\x7fdel",
		"zero\u200bwidth",
	}
	if len(ids) < 7 {
		t.Fatalf("guard consumes too few identities: %d", len(ids))
	}
	f := newRuntimeCommandFixture(t)
	seen := map[uint64]runtimecommand.CommandID{}
	for _, id := range ids {
		runtimeID := mustUUID()
		adm := f.admittedInput(id, runtimeID, "hello")
		first, err := f.session.ApplyRuntimeCommand(context.Background(), adm)
		if err != nil {
			t.Errorf("ApplyRuntimeCommand(%q) = %v; Core admits this id, so Harness must apply it", id, err)
			continue
		}
		if first.Duplicate {
			t.Errorf("ApplyRuntimeCommand(%q) reported a duplicate on FIRST delivery", id)
		}
		if prior, collided := seen[first.PrefixSequence]; collided {
			t.Errorf("%q and %q share prefix sequence %d", id, prior, first.PrefixSequence)
		}
		seen[first.PrefixSequence] = id
		if got := f.drainOne(t).CommandHeader().CommandID; got != runtimeID {
			t.Errorf("ApplyRuntimeCommand(%q) dispatched CommandID = %v, want %v", id, got, runtimeID)
		}
		second, err := f.session.ApplyRuntimeCommand(context.Background(), adm)
		if err != nil {
			t.Errorf("duplicate ApplyRuntimeCommand(%q): %v", id, err)
			continue
		}
		if !second.Duplicate || second.PrefixSequence != first.PrefixSequence {
			t.Errorf("duplicate of %q = %+v, want the original prefix %d", id, second, first.PrefixSequence)
		}
		f.requireNoCommand(t, "duplicate of an opaque id")
	}
	if len(seen) != len(ids) {
		t.Fatalf("only %d of %d identities reached a durable prefix", len(seen), len(ids))
	}
}

// TestEffectFailureAfterTheDurablePrefixStrandsTheCommand DOCUMENTS the residual of
// persist-before-effect, in the direction the rest of the suite does not cover.
//
// TestApplyRefusesWhenThePrefixCannotBePersisted covers "append failed, so no
// effect". This is the reverse: the prefix COMMITS and the effect then fails. The
// caller sees the effect's error, but the prefix is durable, so every redelivery is
// a duplicate that applies nothing — the command is silently lost to Harness, and
// only Host's apply deadline will notice.
//
// This is inherent to writing the correlation before the effect and is consistent
// with §10.4 (recovery "marks applied" from the correlation; it does not replay the
// outcome). It is asserted here rather than left implicit so a Host adapter author
// meets it in a test instead of in production: a non-nil error from
// ApplyRuntimeCommand does NOT mean the command may be re-offered.
func TestEffectFailureAfterTheDurablePrefixStrandsTheCommand(t *testing.T) {
	t.Parallel()
	f := newRuntimeCommandFixture(t)
	adm := f.admittedInput("v1:effect-fails", mustUUID(), "hello")
	// The loop is gone, so the dispatch fails AFTER the prefix has been appended.
	// The sink must be UNBUFFERED and unread: with a buffered sink the dispatch's
	// select has two ready cases and Go picks between them at random, which is a
	// flake, not a test. An unbuffered sink with no reader leaves exactly one ready
	// case — the closed Done — so the failure is deterministic.
	unread := make(chan command.Command)
	f.cmds = unread
	close(f.done)
	f.session.loops[f.session.activeLoopID].backend = &channelBackend{Commands: unread, Done: f.done}

	_, err := f.session.ApplyRuntimeCommand(context.Background(), adm)
	var sessionErr *SessionError
	if !errors.As(err, &sessionErr) || sessionErr.Kind != SessionLoopExited {
		t.Fatalf("ApplyRuntimeCommand err = %v, want *SessionError{SessionLoopExited}", err)
	}
	f.requireNoCommand(t, "the loop had already exited")

	// The prefix is durable regardless, and that is the documented consequence.
	disp, err := f.session.ApplyRuntimeCommand(context.Background(), adm)
	if err != nil {
		t.Fatalf("redelivery after a failed effect: %v", err)
	}
	if !disp.Duplicate {
		t.Fatalf("redelivery after a failed effect = %+v, want Duplicate=true", disp)
	}
	app, err := f.store.ReadCommandApplicationAt(context.Background(), f.sid, disp.PrefixSequence)
	if err != nil {
		t.Fatalf("ReadCommandApplicationAt: %v", err)
	}
	if app.CommandID != adm.CommandID || app.RuntimeCommandID != adm.RuntimeCommandID {
		t.Errorf("durable prefix = %+v, want the admitted correlation", app)
	}
}

// TestRuntimeCommandsSurvivesCompositionRootDecorators is the composition-root guard
// for the runtime-command capability, and the counterpart to
// TestCommittedPublicEventsSurvivesCompositionRootDecorators.
//
// The capability is discovered by type-asserting the composition root's journal to
// IdempotentJournal, and the root wraps that journal in DECORATORS — the offload-GC
// admission gate, then the operation-hook observer. Both lifecycle.go and
// restore_constructor.go SWALLOW OpenRuntimeCommandLog's error, so a decorator that
// exposed only Append would leave the capability silently unadvertised: no error
// anywhere, and RuntimeCommands() simply answers (nil, false). That is fail-closed —
// Host learns it before acknowledging anything — but it is a deployment that cannot
// apply a single command, and nothing would say why.
//
// The control row is the point of the table: every row builds the same session over
// the same store, and the ONLY variable is which decorators are armed. A row that
// fails while the control passes names the decorator that ate the capability.
func TestRuntimeCommandsSurvivesCompositionRootDecorators(t *testing.T) {
	t.Parallel()
	policy := OffloadGCPolicy{Interval: time.Minute, Timeout: 10 * time.Second}
	journalHooks, err := hook.Compile(hook.Set{Around: []hook.Around{{
		Operation: hook.OperationJournalAppend,
		Begin: func(ctx context.Context, _ hook.Call) (context.Context, hook.FinishFunc) {
			return ctx, func(hook.Result) {}
		},
	}}})
	if err != nil {
		t.Fatalf("hook.Compile: %v", err)
	}
	tests := []struct {
		name    string
		options []LifecycleOption
	}{
		{name: "control: no decorators"},
		{name: "offload GC armed", options: []LifecycleOption{WithLifecycleOffloadGC(policy)}},
		{name: "journal hooks armed", options: []LifecycleOption{WithLifecycleHooks(journalHooks)}},
		{
			name: "offload GC and journal hooks armed",
			options: []LifecycleOption{
				WithLifecycleOffloadGC(policy),
				WithLifecycleHooks(journalHooks),
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			store := newRestoreStore(t)
			r, err := newTestLifecycle(cfg(&stubLLM{chunks: []content.Chunk{textChunk("x")}}), store, tt.options...)
			if err != nil {
				t.Fatalf("NewTopologyLifecycle: %v", err)
			}
			s, err := r.NewSession(context.Background(), "")
			if err != nil {
				t.Fatalf("NewSession: %v", err)
			}
			t.Cleanup(func() { _ = s.Shutdown(context.Background()) })
			assertRuntimeCommandCapability(t, s, runtimecommand.CommandID("v1:new-"+tt.name))
		})
		t.Run(tt.name+" (restored)", func(t *testing.T) {
			t.Parallel()
			store := newRestoreStore(t)
			sid := runAndShutdown(t, store, restoreCfg(&stubLLM{chunks: []content.Chunk{textChunk("reply")}}, "model-x", "be helpful"))
			rr, err := newTestLifecycle(restoreCfg(&stubLLM{}, "model-x", "be helpful"), store, tt.options...)
			if err != nil {
				t.Fatalf("NewTopologyLifecycle (restore): %v", err)
			}
			s, err := rr.RestoreSession(context.Background(), sid)
			if err != nil {
				t.Fatalf("RestoreSession: %v", err)
			}
			t.Cleanup(func() { _ = s.Shutdown(context.Background()) })
			assertRuntimeCommandCapability(t, s, runtimecommand.CommandID("v1:restored-"+tt.name))
		})
	}
}

// assertRuntimeCommandCapability is the shared body of the composition-root rows: the
// session must advertise the capability AND actually apply a command through whatever
// decorators the root wrapped its journal in, then deduplicate a redelivery.
// Discovery alone would pass on a decorator that satisfies the interface and then
// fails to delegate, so the application and the redelivery are part of the assertion,
// not garnish.
func assertRuntimeCommandCapability(t *testing.T, s *Session, commandID runtimecommand.CommandID) {
	t.Helper()
	applier, ok := runtimecommand.Provider(s).RuntimeCommands()
	if !ok || applier == nil {
		t.Fatalf("RuntimeCommands() = (%v, %t), want an applier and true:"+
			" a composition-root decorator dropped the runtime-command contract", applier, ok)
	}
	adm := runtimecommand.Admitted{
		CommandID:        commandID,
		RuntimeCommandID: mustUUID(),
		Kind:             runtimecommand.KindInput,
		LeaseEpoch:       s.runtimeCommandLease.Epoch(),
		Blocks:           []content.Block{&content.TextBlock{Text: "hello"}},
	}
	first, err := applier.ApplyRuntimeCommand(context.Background(), adm)
	if err != nil {
		t.Fatalf("ApplyRuntimeCommand through the decorated journal: %v", err)
	}
	if first.Duplicate || first.PrefixSequence == 0 {
		t.Fatalf("first application = %+v, want a fresh durable prefix", first)
	}
	second, err := applier.ApplyRuntimeCommand(context.Background(), adm)
	if err != nil {
		t.Fatalf("duplicate ApplyRuntimeCommand through the decorated journal: %v", err)
	}
	if !second.Duplicate || second.PrefixSequence != first.PrefixSequence {
		t.Fatalf("duplicate through the decorated journal = %+v, want the original prefix %d:"+
			" the decorator did not delegate the idempotent append", second, first.PrefixSequence)
	}
}
