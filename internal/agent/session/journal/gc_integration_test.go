//go:build integration

package journal_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/inventivepotter/urvi/internal/agent/loop/event"
	"github.com/inventivepotter/urvi/internal/agent/session/journal"
	"github.com/inventivepotter/urvi/internal/uuid"
	"github.com/nats-io/nats.go"
)

// gcGraceWindowProbe is a duration safely LONGER than the production grace window
// (gcGraceWindow is unexported, ~5m). The "reap an orphan" and "keep referenced" tests
// advance GC's injected clock by this much past a real object ModTime so the grace check
// is provably satisfied; the "within grace" test uses now == real time so the object is
// trivially younger than any window. The exact production value is not asserted here
// (that is documented in gc.go); these tests only need a value past it.
const gcGraceWindowProbe = 30 * time.Minute

// putOrphanObject uploads payload directly to the per-session object bucket WITHOUT
// appending a pointer, so it is an unreferenced ("orphan") object addressed by
// hex(sha256(payload)). It returns the object id. This is the "an upload whose pointer
// append never happened" case GC must reap once it ages past the grace window.
func putOrphanObject(t *testing.T, js nats.JetStreamContext, sid uuid.UUID, payload []byte) string {
	t.Helper()
	store, err := js.ObjectStore(journal.SessionObjectBucket(sid))
	if err != nil {
		t.Fatalf("ObjectStore: %v", err)
	}
	sum := sha256.Sum256(payload)
	id := hex.EncodeToString(sum[:])
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := store.PutBytes(id, payload, nats.Context(ctx)); err != nil {
		t.Fatalf("PutBytes(orphan): %v", err)
	}
	return id
}

// objectStore binds the session's object bucket for an assertion (GetInfo after GC).
func objectStore(t *testing.T, js nats.JetStreamContext, sid uuid.UUID) nats.ObjectStore {
	t.Helper()
	store, err := js.ObjectStore(journal.SessionObjectBucket(sid))
	if err != nil {
		t.Fatalf("ObjectStore: %v", err)
	}
	return store
}

// fixedClock returns a now func pinned to a fixed instant, for the grace check.
func fixedClock(at time.Time) func() time.Time {
	return func() time.Time { return at }
}

// TestGCReapsOrphan asserts an unreferenced object older than the grace window IS
// deleted under a held lease: an orphan uploaded directly (no pointer), with GC's
// injected clock advanced well past the object's ModTime + grace window, is gone after
// the pass (GetInfo -> ErrObjectNotFound) and the result counts one deletion.
func TestGCReapsOrphan(t *testing.T) {
	sid := seedUUID(0x80)
	_, js := newEmbeddedJS(t)
	lease := mustAcquireLease(t, js, sid)
	if _, err := journal.NewSessionJournal(js, sid, lease); err != nil {
		t.Fatalf("NewSessionJournal: %v", err)
	}

	payload := bytes.Repeat([]byte("orphan-"), 1024)
	id := putOrphanObject(t, js, sid, payload)

	store := objectStore(t, js, sid)
	// Confirm the orphan exists before GC and read its real (server-set) ModTime.
	info, err := store.GetInfo(id)
	if err != nil {
		t.Fatalf("GetInfo(before GC): %v", err)
	}

	// GC clock: well past the orphan's ModTime + the grace window.
	now := fixedClock(info.ModTime.Add(gcGraceWindowProbe))
	gc, err := journal.NewObjectGC(js, store, lease, sid, journal.WithGCClock(now))
	if err != nil {
		t.Fatalf("NewObjectGC: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	res, err := gc.GC(ctx)
	if err != nil {
		t.Fatalf("GC: %v", err)
	}
	if res.Deleted != 1 {
		t.Errorf("GCResult.Deleted = %d, want 1", res.Deleted)
	}
	if res.Referenced != 0 {
		t.Errorf("GCResult.Referenced = %d, want 0", res.Referenced)
	}

	// The orphan is gone: GetInfo (which hides deleted objects) reports not-found.
	if _, err := store.GetInfo(id); !errors.Is(err, nats.ErrObjectNotFound) {
		t.Fatalf("GetInfo(after GC) err = %v, want ErrObjectNotFound (orphan reaped)", err)
	}
}

// TestGCKeepsReferenced asserts an object referenced by a real offloaded event's pointer
// is NOT deleted even with the clock advanced far past the grace window: a live pointer
// references it, so it is not an orphan.
func TestGCKeepsReferenced(t *testing.T) {
	sid := seedUUID(0x81)
	lid := seedUUID(0x82)
	const blockChars = 700 * 1024 // > 512 KiB inline threshold => offloaded.

	_, js := newEmbeddedJS(t)
	lease := mustAcquireLease(t, js, sid)
	j, err := journal.NewSessionJournal(js, sid, lease)
	if err != nil {
		t.Fatalf("NewSessionJournal: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Append a real large event: the offload path uploads the object AND appends a
	// pointer referencing it.
	ev := largeStepDone(sid, lid, 0x83, blockChars)
	payload, err := event.MarshalEvent(ev)
	if err != nil {
		t.Fatalf("MarshalEvent: %v", err)
	}
	sum := sha256.Sum256(payload)
	wantID := hex.EncodeToString(sum[:])
	if _, err := j.Append(ctx, journal.NewEventRecord(ev)); err != nil {
		t.Fatalf("Append (large): %v", err)
	}

	store := objectStore(t, js, sid)
	info, err := store.GetInfo(wantID)
	if err != nil {
		t.Fatalf("GetInfo(referenced object): %v", err)
	}

	// GC clock advanced far past the grace window: only the reference keeps the object.
	now := fixedClock(info.ModTime.Add(gcGraceWindowProbe))
	gc, err := journal.NewObjectGC(js, store, lease, sid, journal.WithGCClock(now))
	if err != nil {
		t.Fatalf("NewObjectGC: %v", err)
	}

	res, err := gc.GC(ctx)
	if err != nil {
		t.Fatalf("GC: %v", err)
	}
	if res.Deleted != 0 {
		t.Errorf("GCResult.Deleted = %d, want 0 (referenced object must survive)", res.Deleted)
	}
	if res.Referenced != 1 {
		t.Errorf("GCResult.Referenced = %d, want 1", res.Referenced)
	}

	// The referenced object is still present with its bytes intact.
	got, err := store.GetBytes(wantID)
	if err != nil {
		t.Fatalf("GetBytes(after GC): %v (referenced object was wrongly reaped?)", err)
	}
	if !bytes.Equal(got, payload) {
		t.Errorf("referenced object bytes changed after GC")
	}
}

// TestGCKeepsWithinGrace asserts an unreferenced object whose ModTime is WITHIN the
// grace window of now is NOT deleted — this protects an object whose pointer append is
// still in flight. now is the real wall clock, so a just-uploaded orphan is trivially
// younger than any grace window.
func TestGCKeepsWithinGrace(t *testing.T) {
	sid := seedUUID(0x84)
	_, js := newEmbeddedJS(t)
	lease := mustAcquireLease(t, js, sid)
	if _, err := journal.NewSessionJournal(js, sid, lease); err != nil {
		t.Fatalf("NewSessionJournal: %v", err)
	}

	payload := bytes.Repeat([]byte("fresh-"), 1024)
	id := putOrphanObject(t, js, sid, payload)
	store := objectStore(t, js, sid)

	// now == real time: the object was just uploaded, so it is well within any grace
	// window. No clock advance.
	gc, err := journal.NewObjectGC(js, store, lease, sid, journal.WithGCClock(time.Now))
	if err != nil {
		t.Fatalf("NewObjectGC: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	res, err := gc.GC(ctx)
	if err != nil {
		t.Fatalf("GC: %v", err)
	}
	if res.Deleted != 0 {
		t.Errorf("GCResult.Deleted = %d, want 0 (within-grace object must survive)", res.Deleted)
	}
	if res.WithinGrace != 1 {
		t.Errorf("GCResult.WithinGrace = %d, want 1", res.WithinGrace)
	}

	// Still present: a within-grace orphan is protected.
	if _, err := store.GetInfo(id); err != nil {
		t.Fatalf("GetInfo(after GC) = %v, want present (within-grace orphan must survive)", err)
	}
}

// TestGCRefusesWithoutLease asserts GC with a lost/invalid lease is refused with a typed
// *GCLeaseNotHeldError and deletes nothing — even an aged, unreferenced orphan survives,
// because GC must be the single writer. The lease is driven to lost via injected-clock
// TTL expiry + a successor takeover (the same mechanism the lease tests use).
func TestGCRefusesWithoutLease(t *testing.T) {
	sid := seedUUID(0x85)
	_, js := newEmbeddedJS(t)

	var (
		mu  sync.Mutex
		clk = time.Date(2026, 6, 21, 0, 0, 0, 0, time.UTC)
	)
	now := func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return clk
	}
	advance := func(d time.Duration) {
		mu.Lock()
		defer mu.Unlock()
		clk = clk.Add(d)
	}
	lm := newLeaseManager(t, js, now)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	lease, err := lm.Acquire(ctx, sid)
	if err != nil {
		t.Fatalf("Acquire(A): %v", err)
	}
	epochA := lease.Epoch()

	// Provision the stream + object bucket under A so GC has something to bind.
	if _, err := journal.NewSessionJournal(js, sid, lease); err != nil {
		t.Fatalf("NewSessionJournal: %v", err)
	}

	// An aged, unreferenced orphan that WOULD be reaped if the lease were held.
	payload := bytes.Repeat([]byte("orphan-no-lease-"), 1024)
	id := putOrphanObject(t, js, sid, payload)
	store := objectStore(t, js, sid)
	info, err := store.GetInfo(id)
	if err != nil {
		t.Fatalf("GetInfo(before GC): %v", err)
	}

	// Expire A and take over with B, so A's lease becomes observably lost.
	advance(leaseTTL * 4)
	b, err := lm.Acquire(ctx, sid)
	if err != nil {
		t.Fatalf("Acquire(B) after expiry: %v", err)
	}
	defer func() { _ = b.Release(ctx) }()
	select {
	case <-lease.Lost():
	case <-time.After(5 * time.Second):
		t.Fatalf("A.Lost() never fired after B took over")
	}
	if lease.Valid() {
		t.Fatalf("A.Valid() = true after takeover; want false")
	}

	// GC under A (the lost lease) with the clock advanced past the grace window: it must
	// refuse and delete nothing.
	gcNow := fixedClock(info.ModTime.Add(gcGraceWindowProbe))
	gc, err := journal.NewObjectGC(js, store, lease, sid, journal.WithGCClock(gcNow))
	if err != nil {
		t.Fatalf("NewObjectGC: %v", err)
	}

	res, err := gc.GC(ctx)
	if err == nil {
		t.Fatalf("GC under lost lease succeeded; want *GCLeaseNotHeldError")
	}
	var notHeld *journal.GCLeaseNotHeldError
	if !errors.As(err, &notHeld) {
		t.Fatalf("GC error %v is not *GCLeaseNotHeldError", err)
	}
	if notHeld.SessionID != sid {
		t.Errorf("GCLeaseNotHeldError.SessionID = %v, want %v", notHeld.SessionID, sid)
	}
	if notHeld.Epoch != epochA {
		t.Errorf("GCLeaseNotHeldError.Epoch = %d, want %d (A's epoch)", notHeld.Epoch, epochA)
	}
	if res.Deleted != 0 {
		t.Errorf("GCResult.Deleted = %d, want 0 (refused GC deletes nothing)", res.Deleted)
	}

	// The orphan still exists: a refused GC never deleted it.
	if _, err := store.GetInfo(id); err != nil {
		t.Fatalf("GetInfo(after refused GC) = %v, want present (nothing deleted)", err)
	}
}
