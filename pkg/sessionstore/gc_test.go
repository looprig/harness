package sessionstore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/identity"
	"github.com/looprig/harness/pkg/journal"
	"github.com/looprig/harness/pkg/workspacestore"
	"github.com/looprig/storage"
	"github.com/looprig/storage/memstore"
)

// gcBlobPrefix is the content-addressed blob prefix GC sweeps for session id:
// "sessions/<uuid>/blobs/". It mirrors the writer's key derivation (ledgerName +
// blobsInfix), so a test never hard-codes the layout the writer owns.
func gcBlobPrefix(id uuid.UUID) string { return ledgerName(id) + blobsInfix }

// orphanKey builds a valid, canonical blob key under prefix that is NOT the hash of
// any real record — a fabricated 64-hex leaf (sha256 of a distinct marker) so a Put
// there is an ORPHAN: content the store holds with no in-ledger pointer referencing
// it. Distinct i yield distinct keys, and none collides with a real record's blob.
func orphanKey(prefix string, i int) string {
	h := sha256.Sum256([]byte("orphan-" + strconv.Itoa(i)))
	return prefix + hex.EncodeToString(h[:])
}

// writeSession opens a journal over st for a fresh session and appends n
// over-threshold records (each a distinct largeCommand, so each offloads a distinct
// content-addressed blob and appends a real blobptr pointer). It returns the session
// id, its held lease, and the sorted set of referenced blob keys the appends created.
func writeSession(t *testing.T, st *Store, n int) (uuid.UUID, journal.Lease, []string) {
	t.Helper()
	id := newTestUUID(t)
	lease, _ := leaseFor(1, id)
	j, err := st.OpenJournal(context.Background(), id, lease)
	if err != nil {
		t.Fatalf("OpenJournal() err = %v", err)
	}
	for i := 0; i < n; i++ {
		rec, _ := largeCommand(id, newTestUUID(t))
		if _, err := j.Append(context.Background(), rec); err != nil {
			t.Fatalf("Append() large record %d err = %v", i, err)
		}
	}
	refKeys, err := st.backend.Blobs.List(context.Background(), gcBlobPrefix(id))
	if err != nil {
		t.Fatalf("Blobs.List() err = %v", err)
	}
	if len(refKeys) != n {
		t.Fatalf("referenced blob count = %d, want %d (each large record offloads one)", len(refKeys), n)
	}
	return id, lease, refKeys
}

// putOrphans writes m orphan blobs directly under session id's blob prefix (no
// pointer) and returns their sorted keys.
func putOrphans(t *testing.T, st *Store, id uuid.UUID, m int) []string {
	t.Helper()
	prefix := gcBlobPrefix(id)
	var keys []string
	for i := 0; i < m; i++ {
		k := orphanKey(prefix, i)
		if err := st.backend.Blobs.Put(context.Background(), k, bytes.NewReader([]byte("orphan payload "+strconv.Itoa(i)))); err != nil {
			t.Fatalf("Blobs.Put(orphan) err = %v", err)
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// assertReplayResolves drains a full record replay of session id, failing if any
// record fails to decode or any pointer fails to resolve — the end-to-end proof that
// no live blob was reaped (a wrongly-deleted referenced blob surfaces here as a
// *BlobUnavailableError).
func assertReplayResolves(t *testing.T, st *Store, id uuid.UUID) {
	t.Helper()
	rr, err := st.OpenRecordReplayer(id, ReplayRequest{})
	if err != nil {
		t.Fatalf("OpenRecordReplayer() err = %v", err)
	}
	cur, err := rr.Open(context.Background(), journal.ReplayRequest{})
	if err != nil {
		t.Fatalf("RecordReplayer.Open() err = %v", err)
	}
	defer cur.Close()
	for {
		_, _, err := cur.Next(context.Background())
		if errors.Is(err, io.EOF) {
			return
		}
		if err != nil {
			t.Fatalf("replay Next() err = %v (a live blob was reaped or a record is corrupt)", err)
		}
	}
}

// TestGCReclaim is the live-set-sweep core: over a session holding real blobptr
// records (referenced blobs) plus fabricated orphan blobs, GC deletes EXACTLY the
// orphans, keeps every referenced blob, and leaves a replay fully resolvable.
func TestGCReclaim(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		numRef     int
		numOrphan  int
		wantRefCnt int
	}{
		{name: "orphans reclaimed, referenced kept", numRef: 3, numOrphan: 2, wantRefCnt: 3},
		{name: "no orphans deletes nothing", numRef: 3, numOrphan: 0, wantRefCnt: 3},
		{name: "only orphans, none referenced", numRef: 0, numOrphan: 2, wantRefCnt: 0},
		{name: "empty session is a no-op", numRef: 0, numOrphan: 0, wantRefCnt: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			st, err := Open(memstore.New(), WithOffloadThreshold(64))
			if err != nil {
				t.Fatalf("Open() err = %v", err)
			}
			id, lease, refKeys := writeSession(t, st, tt.numRef)
			orphanKeys := putOrphans(t, st, id, tt.numOrphan)

			gc, err := st.OpenObjectGC(id, lease)
			if err != nil {
				t.Fatalf("OpenObjectGC() err = %v", err)
			}
			res, err := gc.GC(context.Background())
			if err != nil {
				t.Fatalf("GC() err = %v", err)
			}

			if res.Scanned != tt.numRef+tt.numOrphan {
				t.Errorf("GCResult.Scanned = %d, want %d", res.Scanned, tt.numRef+tt.numOrphan)
			}
			if res.Referenced != tt.wantRefCnt {
				t.Errorf("GCResult.Referenced = %d, want %d", res.Referenced, tt.wantRefCnt)
			}
			if res.Deleted != tt.numOrphan {
				t.Errorf("GCResult.Deleted = %d, want %d", res.Deleted, tt.numOrphan)
			}
			if !reflect.DeepEqual(res.DeletedKeys, orphanKeys) {
				t.Errorf("GCResult.DeletedKeys = %v, want %v (only orphans)", res.DeletedKeys, orphanKeys)
			}

			// Every referenced blob still resolves.
			for _, k := range refKeys {
				rc, err := st.backend.Blobs.Get(context.Background(), k)
				if err != nil {
					t.Errorf("referenced blob %q was reaped: Get() err = %v", k, err)
					continue
				}
				_ = rc.Close()
			}
			// Every orphan is gone.
			remaining, err := st.backend.Blobs.List(context.Background(), gcBlobPrefix(id))
			if err != nil {
				t.Fatalf("Blobs.List() after GC err = %v", err)
			}
			if len(remaining) != tt.numRef {
				t.Errorf("remaining blobs = %d, want %d (only referenced survive)", len(remaining), tt.numRef)
			}
			// The whole session still replays and resolves every pointer.
			assertReplayResolves(t, st, id)
		})
	}
}

func TestWorkspaceLiveRefsScansEveryRetainedJournal(t *testing.T) {
	t.Parallel()
	st, err := Open(memstore.New())
	if err != nil {
		t.Fatalf("Open() err = %v", err)
	}
	refA := "v1:sha256:" + strings.Repeat("a", 64)
	refB := "v1:sha256:" + strings.Repeat("b", 64)
	refC := "v1:sha256:" + strings.Repeat("c", 64)
	ids := []uuid.UUID{newTestUUID(t), newTestUUID(t)}
	appendWorkspaceHistory(t, st, ids[0],
		event.WorkspaceCheckpointed{Ref: refA},
		event.WorkspaceCheckpointed{Ref: refB},
		event.WorkspaceRestored{Ref: refA},
	)
	appendWorkspaceHistory(t, st, ids[1],
		event.WorkspaceRestored{Ref: refC},
		event.WorkspaceCheckpointed{Ref: refB},
	)

	got, err := st.WorkspaceLiveRefs(context.Background(), ids)
	if err != nil {
		t.Fatalf("WorkspaceLiveRefs() err = %v", err)
	}
	want := map[workspacestore.Ref]struct{}{workspacestore.Ref(refA): {}, workspacestore.Ref(refB): {}, workspacestore.Ref(refC): {}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("WorkspaceLiveRefs() = %v, want %v", got, want)
	}
}

func TestWorkspaceCheckpointLookupBySeqAndTurnSelectsSameIdentity(t *testing.T) {
	t.Parallel()
	st, err := Open(memstore.New())
	if err != nil {
		t.Fatalf("Open() err = %v", err)
	}
	sid, loopID, turnID := newTestUUID(t), newTestUUID(t), newTestUUID(t)
	eventID := newTestUUID(t)
	ref := "v1:sha256:" + strings.Repeat("d", 64)
	checkpoint := event.WorkspaceCheckpointed{
		Header: event.Header{
			Coordinates: identity.Coordinates{SessionID: sid},
			EventID:     eventID,
			Cause: identity.Cause{
				Coordinates: identity.Coordinates{SessionID: sid, LoopID: loopID, TurnID: turnID},
				EventID:     newTestUUID(t),
			},
		},
		Ref: ref, Consistency: event.SnapshotQuiescent, Trigger: event.SnapshotTriggerTurnDone,
	}
	seqs := appendWorkspaceHistory(t, st, sid, checkpoint)

	bySeq, ok, err := st.WorkspaceCheckpointBySeq(context.Background(), sid, seqs[0])
	if err != nil || !ok {
		t.Fatalf("WorkspaceCheckpointBySeq() = (%+v, %v, %v), want found", bySeq, ok, err)
	}
	byTurn, ok, err := st.WorkspaceCheckpointByTurn(context.Background(), sid, turnID)
	if err != nil || !ok {
		t.Fatalf("WorkspaceCheckpointByTurn() = (%+v, %v, %v), want found", byTurn, ok, err)
	}
	if bySeq != byTurn {
		t.Errorf("lookup identity differs: by seq %+v, by turn %+v", bySeq, byTurn)
	}
	if bySeq.Seq != seqs[0] || bySeq.EventID != eventID || bySeq.Ref != workspacestore.Ref(ref) {
		t.Errorf("checkpoint identity = %+v, want seq=%d event=%v ref=%s", bySeq, seqs[0], eventID, ref)
	}
}

func appendWorkspaceHistory(t *testing.T, st *Store, sid uuid.UUID, events ...event.Event) []uint64 {
	t.Helper()
	lease, _ := leaseFor(1, sid)
	j, err := st.OpenJournal(context.Background(), sid, lease)
	if err != nil {
		t.Fatalf("OpenJournal() err = %v", err)
	}
	seqs := make([]uint64, 0, len(events))
	for _, ev := range events {
		switch e := ev.(type) {
		case event.WorkspaceCheckpointed:
			if e.SessionID.IsZero() {
				e.Header.Coordinates.SessionID = sid
			}
			if e.EventID.IsZero() {
				e.Header.EventID = newTestUUID(t)
			}
			if e.Consistency == event.SnapshotConsistencyUnknown {
				e.Consistency = event.SnapshotQuiescent
			}
			if e.Trigger == event.SnapshotTriggerKindUnknown {
				e.Trigger = event.SnapshotTriggerManual
			}
			ev = e
		case event.WorkspaceRestored:
			if e.SessionID.IsZero() {
				e.Header.Coordinates.SessionID = sid
			}
			if e.EventID.IsZero() {
				e.Header.EventID = newTestUUID(t)
			}
			ev = e
		}
		seq, err := j.Append(context.Background(), journal.NewEventRecord(ev))
		if err != nil {
			t.Fatalf("Append(%T) err = %v", ev, err)
		}
		seqs = append(seqs, seq)
	}
	return seqs
}

// TestGCFailsClosedOnScanError pins the fail-closed contract: a read or decode error
// mid live-set scan makes GC return a *GCScanError and delete NOTHING — a partial
// live set must never drive a delete (it could reap a still-referenced blob).
func TestGCFailsClosedOnScanError(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		setup func(t *testing.T) (gc *ObjectGC, blobs storage.Blobs, orphan string)
	}{
		{
			name: "corrupt ledger record",
			setup: func(t *testing.T) (*ObjectGC, storage.Blobs, string) {
				st, err := Open(memstore.New(), WithOffloadThreshold(64))
				if err != nil {
					t.Fatalf("Open() err = %v", err)
				}
				id := newTestUUID(t)
				lease, _ := leaseFor(1, id)
				if _, err := st.OpenJournal(context.Background(), id, lease); err != nil {
					t.Fatalf("OpenJournal() err = %v", err)
				}
				// Append a raw non-envelope record after the opening fence (seq 1 -> 2).
				if err := st.backend.Ledger.Append(context.Background(), ledgerName(id), 1, []byte("{ not json")); err != nil {
					t.Fatalf("corrupt Append() err = %v", err)
				}
				orphan := putOrphans(t, st, id, 1)[0]
				gc, err := st.OpenObjectGC(id, lease)
				if err != nil {
					t.Fatalf("OpenObjectGC() err = %v", err)
				}
				return gc, st.backend.Blobs, orphan
			},
		},
		{
			name: "ledger read failure",
			setup: func(t *testing.T) (*ObjectGC, storage.Blobs, string) {
				mem := memstore.New()
				comp := &storage.Composite{
					Ledger: &readFailLedger{inner: mem.Ledger, readErr: errScanBoom},
					Leaser: mem.Leaser,
					KV:     mem.KV,
					Blobs:  mem.Blobs,
				}
				st, err := Open(comp, WithOffloadThreshold(64))
				if err != nil {
					t.Fatalf("Open() err = %v", err)
				}
				id := newTestUUID(t)
				lease, _ := leaseFor(1, id)
				if _, err := st.OpenJournal(context.Background(), id, lease); err != nil {
					t.Fatalf("OpenJournal() err = %v", err)
				}
				orphan := putOrphans(t, st, id, 1)[0]
				gc, err := st.OpenObjectGC(id, lease)
				if err != nil {
					t.Fatalf("OpenObjectGC() err = %v", err)
				}
				return gc, mem.Blobs, orphan
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			gc, blobs, orphan := tt.setup(t)
			res, err := gc.GC(context.Background())
			var scanErr *GCScanError
			if !errors.As(err, &scanErr) {
				t.Fatalf("GC() err = %v, want *GCScanError", err)
			}
			if res.Deleted != 0 || len(res.DeletedKeys) != 0 {
				t.Errorf("GCResult = %+v, want zero deletes on a failed scan", res)
			}
			// The orphan (and any live blob) is untouched — nothing was reaped.
			rc, err := blobs.Get(context.Background(), orphan)
			if err != nil {
				t.Fatalf("orphan blob was reaped despite a failed scan: Get() err = %v", err)
			}
			_ = rc.Close()
		})
	}
}

// TestGCLeaseNotHeld covers the lease guard: GC deletes, so it must be the single
// writer. With the lease lost it refuses up front with a *GCLeaseNotHeldError and
// reaps nothing.
func TestGCLeaseNotHeld(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		loseLos  bool
		wantReap bool
	}{
		{name: "lost lease refuses", loseLos: true, wantReap: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			st, err := Open(memstore.New(), WithOffloadThreshold(64))
			if err != nil {
				t.Fatalf("Open() err = %v", err)
			}
			id := newTestUUID(t)
			lease, lost := leaseFor(4, id)
			if _, err := st.OpenJournal(context.Background(), id, lease); err != nil {
				t.Fatalf("OpenJournal() err = %v", err)
			}
			orphan := putOrphans(t, st, id, 1)[0]
			gc, err := st.OpenObjectGC(id, lease)
			if err != nil {
				t.Fatalf("OpenObjectGC() err = %v", err)
			}

			if tt.loseLos {
				close(lost) // the lease is lost before GC runs
			}
			res, err := gc.GC(context.Background())
			var notHeld *GCLeaseNotHeldError
			if !errors.As(err, &notHeld) {
				t.Fatalf("GC() err = %v, want *GCLeaseNotHeldError", err)
			}
			if notHeld.Epoch != 4 || notHeld.SessionID != id {
				t.Errorf("GCLeaseNotHeldError = %+v, want session %v epoch 4", notHeld, id)
			}
			if res.Deleted != 0 {
				t.Errorf("GCResult.Deleted = %d, want 0 (refused before any delete)", res.Deleted)
			}
			// The orphan survives — GC never ran.
			rc, err := st.backend.Blobs.Get(context.Background(), orphan)
			if err != nil {
				t.Fatalf("orphan blob reaped despite lost lease: Get() err = %v", err)
			}
			_ = rc.Close()
		})
	}
}

// TestGCSweepReguardsLease covers the per-delete lease re-guard (white-box on sweep,
// mirroring pkg/journal's reap test): a lease lost mid-pass stops further deletes at
// once with a *GCLeaseNotHeldError, and the would-be orphan survives untouched.
func TestGCSweepReguardsLease(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
	}{
		{name: "lost lease stops the sweep before delete"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			mem := memstore.New()
			id := newTestUUID(t)
			lease, lost := leaseFor(2, id)
			close(lost) // the lease is lost before the sweep reaches its delete

			prefix := gcBlobPrefix(id)
			key := orphanKey(prefix, 0)
			if err := mem.Blobs.Put(context.Background(), key, bytes.NewReader([]byte("x"))); err != nil {
				t.Fatalf("Blobs.Put() err = %v", err)
			}
			g := &ObjectGC{id: id, lease: lease, ledger: mem.Ledger, blobs: mem.Blobs, name: ledgerName(id)}

			res, err := g.sweep(context.Background(), map[string]struct{}{}, []string{key})
			var notHeld *GCLeaseNotHeldError
			if !errors.As(err, &notHeld) {
				t.Fatalf("sweep() err = %v, want *GCLeaseNotHeldError", err)
			}
			if res.Deleted != 0 {
				t.Errorf("GCResult.Deleted = %d, want 0 (re-guard stopped the delete)", res.Deleted)
			}
			rc, err := mem.Blobs.Get(context.Background(), key)
			if err != nil {
				t.Fatalf("blob reaped despite mid-sweep lease loss: Get() err = %v", err)
			}
			_ = rc.Close()
		})
	}
}

// TestGCDeleteFailsClosed covers the delete-failure surface: a Blobs.Delete failure
// mid-reap is surfaced as a typed *GCDeleteError rather than swallowed, so a caller
// learns the bucket was not fully reclaimed.
func TestGCDeleteFailsClosed(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		delErr error
	}{
		{name: "delete failure surfaces", delErr: errDeleteBoom},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			mem := memstore.New()
			fb := &deleteFailBlobs{inner: mem.Blobs, delErr: tt.delErr}
			comp := &storage.Composite{Ledger: mem.Ledger, Leaser: mem.Leaser, KV: mem.KV, Blobs: fb}
			st, err := Open(comp, WithOffloadThreshold(64))
			if err != nil {
				t.Fatalf("Open() err = %v", err)
			}
			id := newTestUUID(t)
			lease, _ := leaseFor(1, id)
			if _, err := st.OpenJournal(context.Background(), id, lease); err != nil {
				t.Fatalf("OpenJournal() err = %v", err)
			}
			orphan := putOrphans(t, st, id, 1)[0]

			gc, err := st.OpenObjectGC(id, lease)
			if err != nil {
				t.Fatalf("OpenObjectGC() err = %v", err)
			}
			res, err := gc.GC(context.Background())
			var delErr *GCDeleteError
			if !errors.As(err, &delErr) {
				t.Fatalf("GC() err = %v, want *GCDeleteError", err)
			}
			if delErr.Key != orphan {
				t.Errorf("GCDeleteError.Key = %q, want %q", delErr.Key, orphan)
			}
			if !errors.Is(err, tt.delErr) {
				t.Errorf("GC() err does not unwrap to the injected delete failure")
			}
			if res.Deleted != 0 {
				t.Errorf("GCResult.Deleted = %d, want 0 (delete failed)", res.Deleted)
			}
		})
	}
}

// TestGCLeaseNotHeldErrorUnwrap asserts the refusal error unwraps to a
// *journal.LeaseLostError (so a caller can errors.As the underlying loss cause) and
// carries the session/epoch — mirroring pkg/journal's ObjectGC.
func TestGCLeaseNotHeldErrorUnwrap(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		epoch uint64
	}{
		{name: "epoch 5", epoch: 5},
		{name: "epoch 0 boundary", epoch: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			sid := newTestUUID(t)
			err := &GCLeaseNotHeldError{SessionID: sid, Epoch: tt.epoch}
			var lost *journal.LeaseLostError
			if !errors.As(err, &lost) {
				t.Fatalf("GCLeaseNotHeldError does not unwrap to *journal.LeaseLostError")
			}
			if lost.SessionID != sid || lost.Epoch != tt.epoch {
				t.Errorf("unwrapped LeaseLostError = %+v, want session %v epoch %d", lost, sid, tt.epoch)
			}
		})
	}
}

// TestOpenObjectGCNilLease covers the DIP guard: a nil lease at construction fails
// closed with a *NilLeaseError rather than deferring a nil dereference to GC time.
func TestOpenObjectGCNilLease(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		nil     bool
		wantErr bool
	}{
		{name: "nil lease rejected", nil: true, wantErr: true},
		{name: "valid lease accepted", nil: false, wantErr: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			st, err := Open(memstore.New())
			if err != nil {
				t.Fatalf("Open() err = %v", err)
			}
			id := newTestUUID(t)
			var lease journal.Lease
			if !tt.nil {
				lease, _ = leaseFor(1, id)
			}
			gc, err := st.OpenObjectGC(id, lease)
			if (err != nil) != tt.wantErr {
				t.Fatalf("OpenObjectGC() err = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				var nilErr *NilLeaseError
				if !errors.As(err, &nilErr) {
					t.Fatalf("OpenObjectGC() err = %v, want *NilLeaseError", err)
				}
				if nilErr.SessionID != id {
					t.Errorf("NilLeaseError.SessionID = %v, want %v", nilErr.SessionID, id)
				}
				return
			}
			if gc == nil {
				t.Fatal("OpenObjectGC() gc = nil, want non-nil")
			}
		})
	}
}

// --- test doubles ---------------------------------------------------------

// errScanBoom is the leaf read failure readFailLedger injects so the GC live-set scan
// fails closed.
var errScanBoom = errors.New("scan read boom")

// errDeleteBoom is the leaf failure deleteFailBlobs injects so a reap delete fails.
var errDeleteBoom = errors.New("delete boom")

// readFailLedger wraps a Ledger but fails every Read, so a GC scan cannot build a
// complete live set. Append/Tip/Delete delegate, so OpenJournal's fence still writes.
type readFailLedger struct {
	inner   storage.Ledger
	readErr error
}

func (l *readFailLedger) Append(ctx context.Context, name string, expected uint64, payload []byte) error {
	return l.inner.Append(ctx, name, expected, payload)
}
func (l *readFailLedger) Read(ctx context.Context, name string, from uint64) (storage.Cursor, error) {
	return nil, l.readErr
}
func (l *readFailLedger) Tip(ctx context.Context, name string) (uint64, error) {
	return l.inner.Tip(ctx, name)
}
func (l *readFailLedger) Delete(ctx context.Context, name string) error {
	return l.inner.Delete(ctx, name)
}

// deleteFailBlobs wraps a Blobs but fails every Delete, driving the reap fail-closed
// path. Put/Get/List delegate so the orphan can be staged and listed.
type deleteFailBlobs struct {
	inner  storage.Blobs
	delErr error
}

func (b *deleteFailBlobs) Put(ctx context.Context, key string, r io.Reader) error {
	return b.inner.Put(ctx, key, r)
}
func (b *deleteFailBlobs) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	return b.inner.Get(ctx, key)
}
func (b *deleteFailBlobs) Delete(ctx context.Context, key string) error {
	return b.delErr
}
func (b *deleteFailBlobs) List(ctx context.Context, prefix string) ([]string, error) {
	return b.inner.List(ctx, prefix)
}

// Compile-time proofs that the GC test doubles honor the storage contracts.
var (
	_ storage.Ledger = (*readFailLedger)(nil)
	_ storage.Blobs  = (*deleteFailBlobs)(nil)
)
