package sessionstore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/looprig/core/content"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/command"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/gate"
	"github.com/looprig/harness/pkg/identity"
	"github.com/looprig/harness/pkg/journal"
	"github.com/looprig/storage"
	"github.com/looprig/storage/memstore"
)

// leaseFor returns a journal.Lease (the real *sessionLease adapter over a
// controllable fakeLease double, defined in lease_test.go) at the given epoch,
// plus its Lost channel so a test can close it to simulate a lost lease.
func leaseFor(epoch uint64, id uuid.UUID) (*sessionLease, chan struct{}) {
	lost := make(chan struct{})
	return &sessionLease{inner: &fakeLease{epoch: epoch, lost: lost}, sessionID: id}, lost
}

// newTestUUID mints a random session id or fails the test.
func newTestUUID(t *testing.T) uuid.UUID {
	t.Helper()
	id, err := uuid.New()
	if err != nil {
		t.Fatalf("uuid.New() err = %v", err)
	}
	return id
}

// smallCommand builds a header-only Interrupt record with a distinct id: a
// tiny, always-inline record for the ordering/concurrency tests.
func smallCommand(id, cmdID uuid.UUID) journal.JournalRecord {
	return journal.NewCommandRecord(id, id, command.Interrupt{Header: command.Header{CommandID: cmdID}})
}

// largeCommand builds a UserInput carrying a padded text block whose marshaled
// envelope is far above any small offload threshold — the offload-path fixture.
func largeCommand(id, cmdID uuid.UUID) (journal.JournalRecord, command.UserInput) {
	cmd := command.UserInput{
		Header: command.Header{CommandID: cmdID},
		Blocks: []content.Block{&content.TextBlock{Text: strings.Repeat("x", 1024)}},
	}
	return journal.NewCommandRecord(id, id, cmd), cmd
}

// TestAppendBeforeReady covers the fast-path guard: an Append on a journal whose
// opening fence was never written (ready == false) fails closed with a typed
// *journal.JournalNotReadyError and never touches the backend.
func TestAppendBeforeReady(t *testing.T) {
	t.Parallel()
	id := newTestUUID(t)
	j := &sessionJournal{id: id} // ready defaults to false

	seq, err := j.Append(context.Background(), journal.NewFenceRecord(id, journal.LeaseFence{Epoch: 1}))
	if seq != 0 {
		t.Errorf("Append() seq = %d, want 0 on not-ready", seq)
	}
	var notReady *journal.JournalNotReadyError
	if !errors.As(err, &notReady) {
		t.Fatalf("Append() err = %v, want *journal.JournalNotReadyError", err)
	}
	if notReady.SessionID != id {
		t.Errorf("JournalNotReadyError.SessionID = %v, want %v", notReady.SessionID, id)
	}
}

// TestOpenJournalWritesOpeningFence covers the ownership handshake: OpenJournal
// writes, as the first ledger record, a fence-kind envelope carrying the lease
// epoch, advancing the tip to 1.
func TestOpenJournalWritesOpeningFence(t *testing.T) {
	t.Parallel()
	const epoch uint64 = 7
	st, err := Open(memstore.New())
	if err != nil {
		t.Fatalf("Open() err = %v", err)
	}
	id := newTestUUID(t)
	lease, _ := leaseFor(epoch, id)

	j, err := st.OpenJournal(context.Background(), id, lease)
	if err != nil {
		t.Fatalf("OpenJournal() err = %v", err)
	}
	if j == nil {
		t.Fatal("OpenJournal() journal = nil, want non-nil")
	}

	// The tip advanced to exactly the opening fence.
	tip, err := st.backend.Ledger.Tip(context.Background(), ledgerName(id))
	if err != nil {
		t.Fatalf("Tip() err = %v", err)
	}
	if tip != 1 {
		t.Fatalf("Tip() = %d, want 1 (opening fence only)", tip)
	}

	env := readEnvelope(t, st, id, 1)
	if env.Kind != string(kindFence) {
		t.Errorf("record 1 kind = %q, want %q", env.Kind, kindFence)
	}
	if env.ID != "7" {
		t.Errorf("record 1 id = %q, want %q (epoch)", env.ID, "7")
	}
	fence, err := journal.UnmarshalLeaseFence(env.Body)
	if err != nil {
		t.Fatalf("UnmarshalLeaseFence() err = %v", err)
	}
	if fence.Epoch != epoch {
		t.Errorf("opening fence epoch = %d, want %d", fence.Epoch, epoch)
	}
}

func TestAppendRejectsGatePreparedEventRecord(t *testing.T) {
	t.Parallel()
	st, err := Open(memstore.New())
	if err != nil {
		t.Fatalf("Open() err = %v", err)
	}
	id := newTestUUID(t)
	lease, _ := leaseFor(1, id)
	j, err := st.OpenJournal(context.Background(), id, lease)
	if err != nil {
		t.Fatalf("OpenJournal() err = %v", err)
	}
	ev := event.GatePrepared{
		Header: event.Header{
			Coordinates: identity.Coordinates{SessionID: id, LoopID: id, TurnID: id, StepID: id},
			EventID:     newTestUUID(t),
		},
		Gate: gate.Gate{
			ID:       gate.ID(newTestUUID(t)),
			Kind:     gate.KindPermission,
			Resolver: gate.ResolverLoop,
			Subject:  gate.Subject{TurnID: gate.ID(id), StepID: gate.ID(id)},
		},
	}

	seq, err := j.Append(context.Background(), journal.NewEventRecord(ev))
	if err == nil {
		t.Fatal("Append(GatePrepared EventRecord) err = nil, want private-event rejection")
	}
	if seq != 0 {
		t.Errorf("Append(GatePrepared EventRecord) seq = %d, want 0", seq)
	}
}

// TestAppendConcurrentStrictOrder covers the mutex-serialized writer under
// concurrent callers: N goroutines Append; the returned sequences are strictly
// contiguous with no gaps or dups, and the tip lands at 1+N (fence + N records).
func TestAppendConcurrentStrictOrder(t *testing.T) {
	t.Parallel()
	const n = 32
	st, err := Open(memstore.New())
	if err != nil {
		t.Fatalf("Open() err = %v", err)
	}
	id := newTestUUID(t)
	lease, _ := leaseFor(1, id)
	j, err := st.OpenJournal(context.Background(), id, lease)
	if err != nil {
		t.Fatalf("OpenJournal() err = %v", err)
	}

	// Pre-mint distinct command ids on the test goroutine (no shared mutation in
	// the workers beyond their own result slot).
	cmdIDs := make([]uuid.UUID, n)
	for i := range cmdIDs {
		cmdIDs[i] = newTestUUID(t)
	}
	seqs := make([]uint64, n)
	errs := make([]error, n)

	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			seqs[i], errs[i] = j.Append(context.Background(), smallCommand(id, cmdIDs[i]))
		}(i)
	}
	wg.Wait()

	// Assertions run on the test goroutine after the workers have joined.
	seen := make(map[uint64]bool, n)
	for i := 0; i < n; i++ {
		if errs[i] != nil {
			t.Fatalf("worker %d Append() err = %v", i, errs[i])
		}
		if seqs[i] < 2 || seqs[i] > 1+n {
			t.Errorf("worker %d seq = %d, want within [2,%d]", i, seqs[i], 1+n)
		}
		if seen[seqs[i]] {
			t.Errorf("duplicate seq %d", seqs[i])
		}
		seen[seqs[i]] = true
	}
	// Every sequence 2..1+n appeared exactly once: contiguous, gap-free.
	for want := uint64(2); want <= 1+n; want++ {
		if !seen[want] {
			t.Errorf("missing seq %d (gap in the log)", want)
		}
	}
	tip, err := st.backend.Ledger.Tip(context.Background(), ledgerName(id))
	if err != nil {
		t.Fatalf("Tip() err = %v", err)
	}
	if tip != 1+n {
		t.Errorf("Tip() = %d, want %d", tip, 1+n)
	}
}

// TestAppendAfterLeaseLost covers the fast-path lease guard: once the lease's
// Lost channel fires, Append refuses with a typed *journal.JournalLeaseLostError
// and appends nothing (the tip stays at the opening fence).
func TestAppendAfterLeaseLost(t *testing.T) {
	t.Parallel()
	const epoch uint64 = 3
	st, err := Open(memstore.New())
	if err != nil {
		t.Fatalf("Open() err = %v", err)
	}
	id := newTestUUID(t)
	lease, lost := leaseFor(epoch, id)
	j, err := st.OpenJournal(context.Background(), id, lease)
	if err != nil {
		t.Fatalf("OpenJournal() err = %v", err)
	}

	close(lost) // the lease is lost (released or overtaken)

	cmdID := newTestUUID(t)
	seq, err := j.Append(context.Background(), smallCommand(id, cmdID))
	if seq != 0 {
		t.Errorf("Append() seq = %d, want 0 after lease lost", seq)
	}
	var lostErr *journal.JournalLeaseLostError
	if !errors.As(err, &lostErr) {
		t.Fatalf("Append() err = %v, want *journal.JournalLeaseLostError", err)
	}
	if lostErr.Epoch != epoch {
		t.Errorf("JournalLeaseLostError.Epoch = %d, want %d", lostErr.Epoch, epoch)
	}
	tip, err := st.backend.Ledger.Tip(context.Background(), ledgerName(id))
	if err != nil {
		t.Fatalf("Tip() err = %v", err)
	}
	if tip != 1 {
		t.Errorf("Tip() = %d, want 1 (nothing appended after loss)", tip)
	}
}

// TestAppendOverThresholdOffloads covers the offload discipline: an over-threshold
// record's full envelope is written to Blobs BEFORE the pointer record is appended
// (blob-durable-before-pointer), and the appended ledger record is a blobptr
// envelope referencing the content-addressed blob.
func TestAppendOverThresholdOffloads(t *testing.T) {
	t.Parallel()
	mem := memstore.New()
	log := &opLog{}
	comp := &storage.Composite{
		Ledger: &recordingLedger{inner: mem.Ledger, log: log},
		Leaser: mem.Leaser,
		KV:     mem.KV,
		Blobs:  &recordingBlobs{inner: mem.Blobs, log: log},
	}
	st, err := Open(comp, WithOffloadThreshold(64))
	if err != nil {
		t.Fatalf("Open() err = %v", err)
	}
	id := newTestUUID(t)
	lease, _ := leaseFor(1, id)
	j, err := st.OpenJournal(context.Background(), id, lease)
	if err != nil {
		t.Fatalf("OpenJournal() err = %v", err)
	}

	// Isolate the over-threshold append from the opening fence.
	log.reset()

	cmdID := newTestUUID(t)
	rec, cmd := largeCommand(id, cmdID)
	seq, err := j.Append(context.Background(), rec)
	if err != nil {
		t.Fatalf("Append() err = %v", err)
	}
	if seq != 2 {
		t.Errorf("Append() seq = %d, want 2", seq)
	}

	// Ordering: the blob Put precedes the ledger Append (blob durable first).
	ops := log.snapshot()
	if len(ops) != 2 || ops[0] != "put" || ops[1] != "append" {
		t.Fatalf("op order = %v, want [put append] (blob before pointer)", ops)
	}

	// The appended record is a blobptr envelope pointing at the offloaded blob.
	env := readEnvelope(t, st, id, 2)
	if env.Kind != string(kindBlobPtr) {
		t.Fatalf("record 2 kind = %q, want %q", env.Kind, kindBlobPtr)
	}
	if env.ID != cmdID.String() {
		t.Errorf("record 2 id = %q, want %q", env.ID, cmdID.String())
	}
	ptr, err := decodeBlobPointer(env.Body)
	if err != nil {
		t.Fatalf("decodeBlobPointer() err = %v", err)
	}

	// Recompute the offloaded envelope the same way the writer does, and confirm
	// the pointer addresses it and the blob holds exactly those bytes.
	body, err := command.MarshalCommand(cmd)
	if err != nil {
		t.Fatalf("MarshalCommand() err = %v", err)
	}
	inner, err := encodeEnvelope(envelope{V: envelopeVersion, Kind: string(kindCommand), ID: cmdID.String(), Body: body})
	if err != nil {
		t.Fatalf("encodeEnvelope() err = %v", err)
	}
	sum := sha256.Sum256(inner)
	wantHex := hex.EncodeToString(sum[:])
	wantKey := "sessions/" + id.String() + "/blobs/" + wantHex
	if ptr.SHA256 != wantHex {
		t.Errorf("blobPointer.SHA256 = %q, want %q", ptr.SHA256, wantHex)
	}
	if ptr.Key != wantKey {
		t.Errorf("blobPointer.Key = %q, want %q", ptr.Key, wantKey)
	}
	if ptr.Size != int64(len(inner)) {
		t.Errorf("blobPointer.Size = %d, want %d", ptr.Size, len(inner))
	}
	rc, err := st.backend.Blobs.Get(context.Background(), ptr.Key)
	if err != nil {
		t.Fatalf("Blobs.Get() err = %v", err)
	}
	defer rc.Close()
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read blob err = %v", err)
	}
	if !bytes.Equal(got, inner) {
		t.Errorf("blob bytes (%d) != offloaded envelope (%d)", len(got), len(inner))
	}
}

// TestAppendStaleWriterConflict covers the CAS fence: a writer whose tracked tip
// is behind (another writer advanced the ledger after it opened) has its Append
// rejected with a typed *journal.AppendError, and nothing of its own lands.
func TestAppendStaleWriterConflict(t *testing.T) {
	t.Parallel()
	st, err := Open(memstore.New())
	if err != nil {
		t.Fatalf("Open() err = %v", err)
	}
	id := newTestUUID(t)
	lease, _ := leaseFor(1, id)
	j, err := st.OpenJournal(context.Background(), id, lease)
	if err != nil {
		t.Fatalf("OpenJournal() err = %v", err)
	}

	// A foreign writer advances the tip past the journal's tracked tip (1 -> 2).
	if err := st.backend.Ledger.Append(context.Background(), ledgerName(id), 1, []byte("stale-writer-advance")); err != nil {
		t.Fatalf("foreign Append() err = %v", err)
	}

	cmdID := newTestUUID(t)
	seq, err := j.Append(context.Background(), smallCommand(id, cmdID))
	if seq != 0 {
		t.Errorf("Append() seq = %d, want 0 on conflict", seq)
	}
	var appendErr *journal.AppendError
	if !errors.As(err, &appendErr) {
		t.Fatalf("Append() err = %v, want *journal.AppendError", err)
	}
	if appendErr.Expected != 1 {
		t.Errorf("AppendError.Expected = %d, want 1 (stale tip)", appendErr.Expected)
	}
	// The foreign record still holds seq 2; the stale writer added nothing.
	tip, err := st.backend.Ledger.Tip(context.Background(), ledgerName(id))
	if err != nil {
		t.Fatalf("Tip() err = %v", err)
	}
	if tip != 2 {
		t.Errorf("Tip() = %d, want 2 (only the foreign record beyond the fence)", tip)
	}
}

// TestAppendPerAppendDeadline covers the bounded append: a wedged ledger Append
// does not hang the serialized writer — the derived deadline fires and Append
// returns a context-deadline-derived error, tip unadvanced.
func TestAppendPerAppendDeadline(t *testing.T) {
	t.Parallel()
	mem := memstore.New()
	bl := &blockingLedger{inner: mem.Ledger}
	comp := &storage.Composite{Ledger: bl, Leaser: mem.Leaser, KV: mem.KV, Blobs: mem.Blobs}
	st, err := Open(comp)
	if err != nil {
		t.Fatalf("Open() err = %v", err)
	}
	id := newTestUUID(t)
	lease, _ := leaseFor(1, id)
	j, err := st.OpenJournal(context.Background(), id, lease) // fence writes while unblocked
	if err != nil {
		t.Fatalf("OpenJournal() err = %v", err)
	}

	bl.block.Store(true) // wedge every subsequent ledger Append

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	cmdID := newTestUUID(t)

	done := make(chan struct{})
	var seq uint64
	var appendErr error
	go func() {
		seq, appendErr = j.Append(ctx, smallCommand(id, cmdID))
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Append() hung past the per-append deadline")
	}

	if seq != 0 {
		t.Errorf("Append() seq = %d, want 0 on deadline", seq)
	}
	if !errors.Is(appendErr, context.DeadlineExceeded) {
		t.Fatalf("Append() err = %v, want context.DeadlineExceeded-derived", appendErr)
	}
	tip, err := st.backend.Ledger.Tip(context.Background(), ledgerName(id))
	if err != nil {
		t.Fatalf("Tip() err = %v", err)
	}
	if tip != 1 {
		t.Errorf("Tip() = %d, want 1 (nothing landed on deadline)", tip)
	}
}

// TestAppendVerifyErrorMapsToAmbiguous covers the fail-closed classification of an
// unresolved conflict: when AppendDefinite hits a CAS conflict whose resolving Read
// fails, it returns a *storage.AppendVerifyError (outcome genuinely unknown). The
// journal must surface that as a *journal.AmbiguousAckError (the "unresolved, decide
// fail-or-retry" case), not leak storage's type, with the verify error still
// reachable via errors.As/Unwrap.
func TestAppendVerifyErrorMapsToAmbiguous(t *testing.T) {
	t.Parallel()
	id := newTestUUID(t)
	lease, _ := leaseFor(1, id)
	fl := &verifyFailLedger{readErr: errVerifyRead}
	j := &sessionJournal{
		id:         id,
		lease:      lease,
		ledger:     fl,
		blobs:      memstore.New().Blobs,
		name:       ledgerName(id),
		threshold:  defaultOffloadThreshold,
		ready:      true,
		trackedTip: 5,
	}

	cmdID := newTestUUID(t)
	seq, err := j.Append(context.Background(), smallCommand(id, cmdID))
	if seq != 0 {
		t.Errorf("Append() seq = %d, want 0 on unresolved conflict", seq)
	}
	var ambErr *journal.AmbiguousAckError
	if !errors.As(err, &ambErr) {
		t.Fatalf("Append() err = %v, want *journal.AmbiguousAckError", err)
	}
	if ambErr.Expected != 5 {
		t.Errorf("AmbiguousAckError.Expected = %d, want 5 (tracked tip)", ambErr.Expected)
	}
	var verifyErr *storage.AppendVerifyError
	if !errors.As(err, &verifyErr) {
		t.Fatalf("err %v does not carry a *storage.AppendVerifyError", err)
	}
	if !errors.Is(err, errVerifyRead) {
		t.Errorf("err %v does not unwrap to the underlying verify-read failure", err)
	}
	// The tracked tip stays unadvanced (nothing definitely landed).
	if j.trackedTip != 5 {
		t.Errorf("trackedTip = %d, want 5 (unadvanced on ambiguous outcome)", j.trackedTip)
	}
}

// --- test doubles ---------------------------------------------------------

// errVerifyRead is the leaf read failure verifyFailLedger injects so a conflict's
// resolving Read fails, driving AppendDefinite's *storage.AppendVerifyError path.
var errVerifyRead = errors.New("verify read boom")

// verifyFailLedger is a Ledger double whose Append always conflicts and whose Read
// always fails: it drives AppendDefinite into verifyAppend and then a read failure,
// yielding a *storage.AppendVerifyError (unresolved outcome).
type verifyFailLedger struct {
	readErr error
}

func (l *verifyFailLedger) Append(ctx context.Context, name string, expected uint64, payload []byte) error {
	return &storage.ConflictError{Name: name, Expected: expected}
}
func (l *verifyFailLedger) Read(ctx context.Context, name string, from uint64) (storage.Cursor, error) {
	return nil, l.readErr
}
func (l *verifyFailLedger) Tip(ctx context.Context, name string) (uint64, error) { return 0, nil }
func (l *verifyFailLedger) Delete(ctx context.Context, name string) error        { return nil }

// readEnvelope reads the ledger record at seq for session id and decodes its
// envelope frame, failing the test on any error.
func readEnvelope(t *testing.T, st *Store, id uuid.UUID, seq uint64) envelope {
	t.Helper()
	cur, err := st.backend.Ledger.Read(context.Background(), ledgerName(id), seq)
	if err != nil {
		t.Fatalf("Read(seq=%d) err = %v", seq, err)
	}
	defer cur.Close()
	rec, err := cur.Next(context.Background())
	if err != nil {
		t.Fatalf("Next(seq=%d) err = %v", seq, err)
	}
	env, err := decodeEnvelope(rec.Payload)
	if err != nil {
		t.Fatalf("decodeEnvelope(seq=%d) err = %v", seq, err)
	}
	return env
}

// opLog records the interleaving of blob and ledger operations across the two
// wrapped primitives so a test can assert blob-before-pointer ordering.
type opLog struct {
	mu  sync.Mutex
	ops []string
}

func (l *opLog) record(op string) {
	l.mu.Lock()
	l.ops = append(l.ops, op)
	l.mu.Unlock()
}

func (l *opLog) reset() {
	l.mu.Lock()
	l.ops = nil
	l.mu.Unlock()
}

func (l *opLog) snapshot() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]string, len(l.ops))
	copy(out, l.ops)
	return out
}

// recordingLedger records each Append against a shared opLog, then delegates.
type recordingLedger struct {
	inner storage.Ledger
	log   *opLog
}

func (l *recordingLedger) Append(ctx context.Context, name string, expected uint64, payload []byte) error {
	l.log.record("append")
	return l.inner.Append(ctx, name, expected, payload)
}
func (l *recordingLedger) Read(ctx context.Context, name string, from uint64) (storage.Cursor, error) {
	return l.inner.Read(ctx, name, from)
}
func (l *recordingLedger) Tip(ctx context.Context, name string) (uint64, error) {
	return l.inner.Tip(ctx, name)
}
func (l *recordingLedger) Delete(ctx context.Context, name string) error {
	return l.inner.Delete(ctx, name)
}

// recordingBlobs records each Put against a shared opLog, then delegates.
type recordingBlobs struct {
	inner storage.Blobs
	log   *opLog
}

func (b *recordingBlobs) Put(ctx context.Context, key string, r io.Reader) error {
	b.log.record("put")
	return b.inner.Put(ctx, key, r)
}
func (b *recordingBlobs) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	return b.inner.Get(ctx, key)
}
func (b *recordingBlobs) Delete(ctx context.Context, key string) error {
	return b.inner.Delete(ctx, key)
}
func (b *recordingBlobs) List(ctx context.Context, prefix string) ([]string, error) {
	return b.inner.List(ctx, prefix)
}

// blockingLedger wedges Append (blocking on ctx) once block is set, so a test can
// prove the per-append deadline unblocks the serialized writer. Other methods
// delegate so the opening fence writes while unblocked.
type blockingLedger struct {
	inner storage.Ledger
	block atomic.Bool
}

func (l *blockingLedger) Append(ctx context.Context, name string, expected uint64, payload []byte) error {
	if l.block.Load() {
		<-ctx.Done()
		return ctx.Err()
	}
	return l.inner.Append(ctx, name, expected, payload)
}
func (l *blockingLedger) Read(ctx context.Context, name string, from uint64) (storage.Cursor, error) {
	return l.inner.Read(ctx, name, from)
}
func (l *blockingLedger) Tip(ctx context.Context, name string) (uint64, error) {
	return l.inner.Tip(ctx, name)
}
func (l *blockingLedger) Delete(ctx context.Context, name string) error {
	return l.inner.Delete(ctx, name)
}

// Compile-time proofs that the test doubles honor the storage contracts they
// stand in for; a drift in the interfaces breaks here rather than at a call site.
var (
	_ storage.Ledger = (*recordingLedger)(nil)
	_ storage.Blobs  = (*recordingBlobs)(nil)
	_ storage.Ledger = (*blockingLedger)(nil)
	_ storage.Ledger = (*verifyFailLedger)(nil)
)
