package sessionstore

import (
	"context"
	"errors"
	"testing"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/identity"
	"github.com/looprig/harness/pkg/journal"
	"github.com/looprig/harness/pkg/runtimecommand"
	"github.com/looprig/storage/memstore"
)

func runtimeCommandStore(t *testing.T) (*Store, uuid.UUID, journal.Lease, journal.SessionJournal) {
	t.Helper()
	store, err := Open(memstore.New())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	id := newTestUUID(t)
	lease, err := store.AcquireLease(context.Background(), id)
	if err != nil {
		t.Fatalf("AcquireLease: %v", err)
	}
	j, err := store.OpenJournal(context.Background(), id, lease)
	if err != nil {
		t.Fatalf("OpenJournal: %v", err)
	}
	return store, id, lease, j
}

func testApplication(commandID runtimecommand.CommandID, epoch uint64) runtimecommand.Application {
	return runtimecommand.Application{
		CommandID:        commandID,
		RuntimeCommandID: uuid.UUID{0x9a, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15},
		LeaseEpoch:       epoch,
	}
}

// TestRuntimeCommandLogAppendsAndReadsBack proves the whole private prefix round
// trip through the REAL lease-fenced journal and the REAL ledger read: the append
// reports a durable sequence, a byte-identical retry deduplicates to that ORIGINAL
// sequence without appending, and the read at that sequence returns the exact
// correlation that was written.
func TestRuntimeCommandLogAppendsAndReadsBack(t *testing.T) {
	t.Parallel()
	store, id, lease, j := runtimeCommandStore(t)
	log, err := store.OpenRuntimeCommandLog(id, j)
	if err != nil {
		t.Fatalf("OpenRuntimeCommandLog: %v", err)
	}
	app := testApplication("v1:public", lease.Epoch())

	first, err := log.AppendCommandApplication(context.Background(), app)
	if err != nil {
		t.Fatalf("AppendCommandApplication: %v", err)
	}
	if !first.Appended || first.Sequence == 0 {
		t.Fatalf("first append = %+v, want a new durable frame", first)
	}
	retry, err := log.AppendCommandApplication(context.Background(), app)
	if err != nil {
		t.Fatalf("retry AppendCommandApplication: %v", err)
	}
	if retry.Appended {
		t.Errorf("retry Appended = true, want a deduplicated retry")
	}
	if retry.Sequence != first.Sequence {
		t.Errorf("retry Sequence = %d, want the ORIGINAL %d", retry.Sequence, first.Sequence)
	}

	back, err := log.ReadCommandApplicationAt(context.Background(), first.Sequence)
	if err != nil {
		t.Fatalf("ReadCommandApplicationAt: %v", err)
	}
	if back != app {
		t.Errorf("read back %+v, want %+v", back, app)
	}
}

// TestRuntimeCommandLogFailsClosedOnADifferentMapping proves the same public id
// under a DIFFERENT runtime id is refused by the journal's collision rule rather
// than appended a second time. The offered payload is one that would append
// successfully if the idempotency id were derived from anything but the public id.
func TestRuntimeCommandLogFailsClosedOnADifferentMapping(t *testing.T) {
	t.Parallel()
	store, id, lease, j := runtimeCommandStore(t)
	log, err := store.OpenRuntimeCommandLog(id, j)
	if err != nil {
		t.Fatalf("OpenRuntimeCommandLog: %v", err)
	}
	app := testApplication("v1:public", lease.Epoch())
	first, err := log.AppendCommandApplication(context.Background(), app)
	if err != nil {
		t.Fatalf("AppendCommandApplication: %v", err)
	}

	conflicting := app
	conflicting.RuntimeCommandID = uuid.UUID{0xbb, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15}
	_, err = log.AppendCommandApplication(context.Background(), conflicting)
	var collision *journal.IdempotencyCollisionError
	if !errors.As(err, &collision) {
		t.Fatalf("conflicting append error = %v, want *journal.IdempotencyCollisionError", err)
	}
	if collision.Seq != first.Sequence {
		t.Errorf("collision Seq = %d, want the durable %d", collision.Seq, first.Sequence)
	}
	// The durable prefix is untouched: the read still yields the ORIGINAL mapping.
	back, err := log.ReadCommandApplicationAt(context.Background(), first.Sequence)
	if err != nil {
		t.Fatalf("ReadCommandApplicationAt: %v", err)
	}
	if back != app {
		t.Errorf("durable prefix = %+v after a refused conflicting append, want %+v", back, app)
	}
}

// TestReadCommandApplicationAtFailsClosedOnAnotherKind proves the read never
// reports a non-application record as a correlation. Reporting one as absent (or,
// worse, as a zero correlation) would let an applied command be applied twice.
func TestReadCommandApplicationAtFailsClosedOnAnotherKind(t *testing.T) {
	t.Parallel()
	store, id, _, j := runtimeCommandStore(t)
	seq, err := j.Append(context.Background(), journal.NewEventRecord(event.SessionStarted{Header: event.Header{
		Coordinates: identity.Coordinates{SessionID: id},
		EventID:     uuid.UUID{0xE1, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15},
	}}))
	if err != nil {
		t.Fatalf("Append(event): %v", err)
	}
	if _, err := store.ReadCommandApplicationAt(context.Background(), id, seq); err == nil {
		t.Fatalf("ReadCommandApplicationAt over an event record = nil error, want a refusal")
	}
	var notFound *CommandApplicationNotFoundError
	if _, err := store.ReadCommandApplicationAt(context.Background(), id, seq+1000); !errors.As(err, &notFound) {
		t.Errorf("ReadCommandApplicationAt past the tip error = %v, want *CommandApplicationNotFoundError", err)
	}
}

// TestCommandApplicationIsPrivate proves the prefix is never projected to a public
// wire body: the PUBLIC event replay must not surface it, while the privileged
// record replay must.
func TestCommandApplicationIsPrivate(t *testing.T) {
	t.Parallel()
	store, id, lease, j := runtimeCommandStore(t)
	log, err := store.OpenRuntimeCommandLog(id, j)
	if err != nil {
		t.Fatalf("OpenRuntimeCommandLog: %v", err)
	}
	if _, err := log.AppendCommandApplication(context.Background(), testApplication("v1:private", lease.Epoch())); err != nil {
		t.Fatalf("AppendCommandApplication: %v", err)
	}
	replayer, err := store.OpenEventReplayer(id, ReplayRequest{})
	if err != nil {
		t.Fatalf("OpenEventReplayer: %v", err)
	}
	cursor, err := replayer.Open(context.Background(), journal.ReplayRequest{SessionID: id, From: journal.Beginning()})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = cursor.Close() }()
	for {
		ev, _, err := cursor.Next(context.Background())
		if err != nil {
			break
		}
		t.Errorf("public event replay surfaced %T; the application prefix must be private", ev)
	}
	// The control: the privileged record replay DOES see it, so the loop above is
	// silent because the record is filtered, not because nothing was written.
	records, err := store.OpenInternalRecordReplayer(id, ReplayRequest{})
	if err != nil {
		t.Fatalf("OpenInternalRecordReplayer: %v", err)
	}
	recCursor, err := records.Open(context.Background(), journal.ReplayRequest{SessionID: id, From: journal.Beginning()})
	if err != nil {
		t.Fatalf("record Open: %v", err)
	}
	defer func() { _ = recCursor.Close() }()
	found := false
	for {
		rec, _, err := recCursor.Next(context.Background())
		if err != nil {
			break
		}
		if _, ok := rec.(journal.CommandApplicationRecord); ok {
			found = true
		}
	}
	if !found {
		t.Fatalf("the privileged record replay did not surface the application prefix; the public-replay assertion above is vacuous")
	}
}
