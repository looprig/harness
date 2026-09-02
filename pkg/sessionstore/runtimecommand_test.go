package sessionstore

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	coresessionwire "github.com/looprig/core/sessionwire/v1"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/command"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/identity"
	"github.com/looprig/harness/pkg/journal"
	"github.com/looprig/harness/pkg/runtimecommand"
	durablestore "github.com/looprig/sessionstore"
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
		Kind:             runtimecommand.KindInput,
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
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			// The prefix must be FILTERED from the public stream, not merely rejected
			// by it: a decode error here would break every public reader of a session
			// that has ever applied a runtime command.
			t.Fatalf("public event replay error = %v, want a clean drain to io.EOF", err)
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

// TestApplicationPrefixIsFramedForTheReleasedReader is the CROSS-AUTHORITY guard:
// it decodes the stored frame with the PINNED durablestore codec rather than with
// Harness's own read path, because the counterparty that has to understand this
// record is the released module, not us.
//
// Two properties, and the second is the one that used to fail. The frame must carry
// EnvelopeKindApplicationPrefix — the generic RuntimeControl slot is invisible to
// FindCommandApplication's matcher — and its identity field must be the RAW public
// id, byte for byte. Framed as RuntimeControl the identity was
// "command_application|command-application:" + id, two 20-byte namespaces against a
// 256-byte limit, so an id of 217 bytes or more was refused at the append with an
// untyped error and no prefix written. Carrying the id in the envelope's own
// identity field makes Harness's acceptance and the durable boundary's acceptance
// the same rule rather than two that drift.
func TestApplicationPrefixIsFramedForTheReleasedReader(t *testing.T) {
	t.Parallel()
	ids := map[string]runtimecommand.CommandID{
		"short":                     "v1:short",
		"216 bytes (the old cliff)": runtimecommand.CommandID(strings.Repeat("x", 216)),
		"217 bytes (once stranded)": runtimecommand.CommandID(strings.Repeat("x", 217)),
		"256 bytes (Core's limit)":  runtimecommand.CommandID(strings.Repeat("x", 256)),
		// The boundary must be counted in BYTES, and the last rune must survive it: an
		// id whose 256th byte is the tail of a 4-byte rune is the case a naive
		// truncation would corrupt rather than reject.
		"256 bytes ending in a 4-byte rune": runtimecommand.CommandID(
			strings.Repeat("y", 252) + "\U0001F680"),
		"256 bytes of multi-byte runes": runtimecommand.CommandID(strings.Repeat("é", 128)),
	}
	if len(ids) < 6 {
		t.Fatalf("guard consumes too few identities: %d", len(ids))
	}
	checked := 0
	for name, id := range ids {
		if len(id) > 256 {
			t.Fatalf("%s: fixture is %d bytes, over Core's limit", name, len(id))
		}
		store, sid, lease, j := runtimeCommandStore(t)
		log, err := store.OpenRuntimeCommandLog(sid, j)
		if err != nil {
			t.Fatalf("%s: OpenRuntimeCommandLog: %v", name, err)
		}
		app := runtimecommand.Application{
			CommandID:        id,
			RuntimeCommandID: uuid.UUID{0x7c, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15},
			LeaseEpoch:       lease.Epoch(),
			Kind:             runtimecommand.KindInput,
		}
		res, err := log.AppendCommandApplication(context.Background(), app)
		if err != nil {
			t.Errorf("%s (%d bytes): AppendCommandApplication = %v; Core admits this id, so the durable boundary must too",
				name, len(id), err)
			continue
		}

		// Decode the STORED frame with the pinned codec.
		raw := readRawFrame(t, store, sid, res.Sequence)
		env, err := durablestore.DecodeEnvelope(raw)
		if err != nil {
			t.Errorf("%s: the released DecodeEnvelope rejected the stored frame: %v", name, err)
			continue
		}
		if env.Kind != durablestore.EnvelopeKindApplicationPrefix {
			t.Errorf("%s: stored Kind = %d, want EnvelopeKindApplicationPrefix (%d):"+
				" FindCommandApplication's matcher can never see any other kind",
				name, env.Kind, durablestore.EnvelopeKindApplicationPrefix)
			continue
		}
		if string(env.CommandID) != string(id) {
			t.Errorf("%s: stored CommandID is not the raw public id byte-for-byte", name)
			continue
		}
		if env.RuntimeCommandID != app.RuntimeCommandID || env.LeaseEpoch != app.LeaseEpoch {
			t.Errorf("%s: stored correlation = (%v, %d), want (%v, %d)",
				name, env.RuntimeCommandID, env.LeaseEpoch, app.RuntimeCommandID, app.LeaseEpoch)
			continue
		}
		if env.CommandKind != string(runtimecommand.KindInput) {
			t.Errorf("%s: stored CommandKind = %q, want %q; the reader correlates on it",
				name, env.CommandKind, runtimecommand.KindInput)
			continue
		}
		// The record-shape rule for this kind forbids a body, and a derived RecordID
		// is exactly what reintroduces the length cliff.
		if env.RecordID != "" {
			t.Errorf("%s: stored RecordID = %q, want empty", name, env.RecordID)
		}
		checked++
	}
	if checked != len(ids) {
		t.Fatalf("only %d of %d identities were verified against the released codec", checked, len(ids))
	}
}

// readRawFrame returns the exact bytes stored at seq, bypassing every Harness decode
// path. The point of the guard above is to read what the COUNTERPARTY reads.
func readRawFrame(t *testing.T, store *Store, id uuid.UUID, seq uint64) []byte {
	t.Helper()
	name, err := sessionName(id)
	if err != nil {
		t.Fatalf("sessionName: %v", err)
	}
	cur, err := store.backend.Ledger.Read(context.Background(), name, seq)
	if err != nil {
		t.Fatalf("Ledger.Read: %v", err)
	}
	defer func() { _ = cur.Close() }()
	rec, err := cur.Next(context.Background())
	if err != nil {
		t.Fatalf("cursor.Next: %v", err)
	}
	if rec.Seq != seq {
		t.Fatalf("read seq %d, want %d", rec.Seq, seq)
	}
	return rec.Payload
}

// TestReleasedReaderSettlesHarnessApplications is the guard for the property the
// reframing actually exists for, and it deliberately asserts against the RELEASED
// reader rather than against Harness's own decode: the counterparty that has to draw
// a conclusion from these records is the released module.
//
// The failure it prevents is the worst one available here. FindCommandApplication
// matches on `Kind == EnvelopeKindApplicationPrefix && CommandID == record.CommandID`,
// so a prefix framed as generic RuntimeControl reads as ABSENT — and ABSENT is the
// single outcome that licenses the deadline reconciler to settle `rejected`. A
// command whose effect is durably committed would be settled rejected, with no error
// raised anywhere. That is the OMISSION failure inbox_recovery.go's own doc names as
// unenforceable from the reader's side: only the writer can avoid it.
func TestReleasedReaderSettlesHarnessApplications(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		runtimeID   uuid.UUID
		admitted    uuid.UUID
		kind        runtimecommand.Kind
		admittedKnd durablestore.CommandKind
		want        durablestore.CommandApplicationOutcome
	}{
		{
			name: "matching mapping resolves committed", kind: runtimecommand.KindInput,
			admittedKnd: durablestore.CommandKind(runtimecommand.KindInput),
			want:        durablestore.CommandApplicationCommitted,
		},
		{
			// The kind must be CARRIED from the record, not pinned. Without a non-input
			// row a frame that hard-coded "input" would satisfy every other row here.
			name: "interrupt kind resolves committed", kind: runtimecommand.KindInterrupt,
			admittedKnd: durablestore.CommandKind(runtimecommand.KindInterrupt),
			want:        durablestore.CommandApplicationCommitted,
		},
		{
			name: "mismatched runtime id resolves CONFLICTED, not absent", kind: runtimecommand.KindInput,
			admittedKnd: durablestore.CommandKind(runtimecommand.KindInput),
			want:        durablestore.CommandApplicationConflicted,
		},
		{
			name: "mismatched kind resolves CONFLICTED, not absent", kind: runtimecommand.KindInput,
			admittedKnd: durablestore.CommandKind(runtimecommand.KindInterrupt),
			want:        durablestore.CommandApplicationConflicted,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			store, sid, lease, j := runtimeCommandStore(t)
			commandID := coresessionwire.CommandID("v1:settled-" + tt.name)
			prefixRuntimeID := uuid.UUID{0x5e, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15}
			admittedRuntimeID := prefixRuntimeID
			if strings.Contains(tt.name, "mismatched runtime id") {
				admittedRuntimeID = uuid.UUID{0x6f, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15}
			}

			// The released reader correlates against the ADMITTED record, never against
			// identities the caller supplies, so the inbox entry has to exist first.
			if _, _, err := store.durable.AdmitCommand(context.Background(), durablestore.AdmitCommandRequest{
				TenantID:                 harnessTenantID,
				SessionID:                harnessSessionID(sid),
				CommandID:                commandID,
				ProposedRuntimeCommandID: durablestore.RuntimeCommandID(admittedRuntimeID.String()),
				Kind:                     tt.admittedKnd,
				AcceptedAt:               time.Now().UTC(),
				ApplyDeadline:            time.Now().UTC().Add(time.Hour),
			}); err != nil {
				t.Fatalf("AdmitCommand: %v", err)
			}

			log, err := store.OpenRuntimeCommandLog(sid, j)
			if err != nil {
				t.Fatalf("OpenRuntimeCommandLog: %v", err)
			}
			res, err := log.AppendCommandApplication(context.Background(), runtimecommand.Application{
				CommandID:        runtimecommand.CommandID(commandID),
				RuntimeCommandID: prefixRuntimeID,
				LeaseEpoch:       lease.Epoch(),
				Kind:             tt.kind,
			})
			if err != nil {
				t.Fatalf("AppendCommandApplication: %v", err)
			}
			// The effect: the released correlation resolves a prefix by ADJACENCY, so
			// the very next record must be the public event the command caused.
			if _, err := j.Append(context.Background(), journal.NewEventRecord(event.SessionStarted{Header: event.Header{
				Coordinates: identity.Coordinates{SessionID: sid},
				EventID:     uuid.UUID{0xE7, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15},
			}})); err != nil {
				t.Fatalf("Append(effect): %v", err)
			}

			got, err := store.durable.FindCommandApplication(context.Background(), durablestore.FindCommandApplicationRequest{
				TenantID:  harnessTenantID,
				SessionID: harnessSessionID(sid),
				CommandID: commandID,
			})
			if err != nil {
				t.Fatalf("FindCommandApplication: %v", err)
			}
			if got.Outcome != tt.want {
				t.Fatalf("released reader resolved %q, want %q (PrefixSeq=%d, appended at %d)",
					got.Outcome, tt.want, got.PrefixSeq, res.Sequence)
			}
			// ABSENT is the outcome that licenses a rejection over a durable effect, so
			// it must never be the answer once a prefix is on the ledger.
			if got.Outcome == durablestore.CommandApplicationAbsent {
				t.Fatalf("released reader reports ABSENT for a durable prefix; the reconciler would settle rejected")
			}
			if got.PrefixSeq != res.Sequence {
				t.Errorf("released reader located the prefix at %d, want %d", got.PrefixSeq, res.Sequence)
			}
		})
	}
}

// FuzzApplicationPrefixIdentityParity is the PARITY property: any string the
// admission authority accepts must not fail the durable append on identity grounds.
// It is the generative counterpart to TestApplicationPrefixIsFramedForTheReleasedReader's
// fixed boundary cases — the fixed cases say the boundary is in the right place, this
// says nothing in between is stranded.
//
// The seeds are the guard. A plain `go test` run executes seed inputs ONLY, so a
// property that matters on every run has to be seeded rather than left for a corpus
// to discover: every length from 1 to MaxIDBytes is seeded below, which is exactly
// the range the old framing broke at 217.
func FuzzApplicationPrefixIdentityParity(f *testing.F) {
	for n := 1; n <= coresessionwire.MaxIDBytes; n++ {
		f.Add(strings.Repeat("x", n))
	}
	// Multi-byte and boundary shapes the ASCII ladder cannot reach.
	f.Add(strings.Repeat("é", 128))
	f.Add(strings.Repeat("y", 252) + "\U0001F680")
	f.Add(" leading-space")
	f.Add("embedded\x00nul")
	f.Add("v1:AAECAwQFBgcICQoLDA0ODw")

	f.Fuzz(func(t *testing.T, raw string) {
		id := coresessionwire.CommandID(raw)
		// The oracle is Core's own validator; anything it refuses is out of scope.
		if id.Validate() != nil {
			t.Skip()
		}
		store, sid, lease, j := runtimeCommandStore(t)
		log, err := store.OpenRuntimeCommandLog(sid, j)
		if err != nil {
			t.Fatalf("OpenRuntimeCommandLog: %v", err)
		}
		app := runtimecommand.Application{
			CommandID:        runtimecommand.CommandID(raw),
			RuntimeCommandID: uuid.UUID{0x3d, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15},
			LeaseEpoch:       lease.Epoch(),
			Kind:             runtimecommand.KindInput,
		}
		// Harness must accept it too: a divergence in EITHER direction is the drift
		// this property exists to forbid.
		if err := app.Validate(); err != nil {
			t.Fatalf("Core accepts a %d-byte id that Harness refuses: %v", len(raw), err)
		}
		res, err := log.AppendCommandApplication(context.Background(), app)
		if err != nil {
			t.Fatalf("Core accepts a %d-byte id that the durable append refuses: %v", len(raw), err)
		}
		back, err := store.ReadCommandApplicationAt(context.Background(), sid, res.Sequence)
		if err != nil {
			t.Fatalf("read back a %d-byte id: %v", len(raw), err)
		}
		if back != app {
			t.Fatalf("round trip of a %d-byte id = %+v, want %+v", len(raw), back, app)
		}
	})
}

// TestPrefixDeduplicatesAfterIndexHydration is the guard for the invariant the
// bodiless framing creates: the read path must reconstruct byte-identically to what
// the write path fingerprinted.
//
// The application prefix stores NO body — the envelope fields are the record — so the
// idempotency fingerprint is derived on write from a canonical encoding and on
// HYDRATION from an encoding rebuilt out of those fields. If the two ever disagree in
// any field, a journal reopened after a restart does not recognise the durable prefix:
// a redelivery appends a SECOND prefix instead of deduplicating, and the command is
// applied twice. Nothing else in the suite would notice, because the live index in the
// first process holds the write-path fingerprint either way.
//
// Both kinds are exercised deliberately. A reconstruction that pinned any field to
// its input-command value would round-trip an input perfectly and corrupt everything
// else, which is exactly the shape of bug a single-kind table cannot see.
func TestPrefixDeduplicatesAfterIndexHydration(t *testing.T) {
	t.Parallel()
	for _, kind := range []runtimecommand.Kind{runtimecommand.KindInput, runtimecommand.KindInterrupt} {
		t.Run(string(kind), func(t *testing.T) {
			t.Parallel()
			store, sid, first0, _ := runtimeCommandStore(t)
			// Advance the epoch past 1 BEFORE writing the prefix. A reconstruction that
			// pinned the epoch to its most common value would otherwise round-trip
			// perfectly against a first-lease fixture and corrupt every later one.
			if err := first0.Release(context.Background()); err != nil {
				t.Fatalf("release the opening lease: %v", err)
			}
			lease, err := store.AcquireLease(context.Background(), sid)
			if err != nil {
				t.Fatalf("AcquireLease: %v", err)
			}
			if lease.Epoch() < 2 {
				t.Fatalf("fixture precondition: epoch must be past 1, got %d", lease.Epoch())
			}
			j, err := store.OpenJournal(context.Background(), sid, lease)
			if err != nil {
				t.Fatalf("OpenJournal: %v", err)
			}
			log, err := store.OpenRuntimeCommandLog(sid, j)
			if err != nil {
				t.Fatalf("OpenRuntimeCommandLog: %v", err)
			}
			app := runtimecommand.Application{
				CommandID:        runtimecommand.CommandID("v1:hydrated-" + kind),
				RuntimeCommandID: uuid.UUID{0x2b, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15},
				LeaseEpoch:       lease.Epoch(),
				Kind:             kind,
			}
			first, err := log.AppendCommandApplication(context.Background(), app)
			if err != nil {
				t.Fatalf("AppendCommandApplication: %v", err)
			}
			if !first.Appended {
				t.Fatalf("first append deduplicated against nothing")
			}

			// Hand the session over: a NEW lease and a NEW journal, whose idempotency
			// index is hydrated from the ledger rather than inherited in memory. This is
			// the restart the invariant is about.
			if err := lease.Release(context.Background()); err != nil {
				t.Fatalf("release lease: %v", err)
			}
			successorLease, err := store.AcquireLease(context.Background(), sid)
			if err != nil {
				t.Fatalf("re-acquire lease: %v", err)
			}
			successorJournal, err := store.OpenJournal(context.Background(), sid, successorLease)
			if err != nil {
				t.Fatalf("re-open journal: %v", err)
			}
			successorLog, err := store.OpenRuntimeCommandLog(sid, successorJournal)
			if err != nil {
				t.Fatalf("OpenRuntimeCommandLog (successor): %v", err)
			}

			again, err := successorLog.AppendCommandApplication(context.Background(), app)
			if err != nil {
				t.Fatalf("redelivery after hydration: %v", err)
			}
			if again.Appended {
				t.Fatalf("redelivery after hydration APPENDED a second prefix at %d:"+
					" the hydrated fingerprint does not match the one the write path stored", again.Sequence)
			}
			if again.Sequence != first.Sequence {
				t.Errorf("redelivery resolved to seq %d, want the ORIGINAL %d", again.Sequence, first.Sequence)
			}

			// And the reconstruction must be faithful field by field, not merely
			// fingerprint-equal by luck.
			back, err := store.ReadCommandApplicationAt(context.Background(), sid, first.Sequence)
			if err != nil {
				t.Fatalf("ReadCommandApplicationAt: %v", err)
			}
			if back != app {
				t.Errorf("reconstructed correlation = %+v, want %+v", back, app)
			}
		})
	}
}

// TestPrefixFollowedByANonEventResolvesUnresolved pins the OTHER half of the
// adjacency contract, and records a known gap rather than leaving it implicit.
//
// The released correlation resolves a prefix by the record at prefix+1: a public
// event means committed, a higher opening fence means abandoned, and ANYTHING ELSE
// means unresolved. Unresolved is not a failure — it never licenses a rejection, so
// it is the safe side — but an unresolved command never settles either, so it is a
// liveness gap and not a correctness one.
//
// KindInterrupt is in exactly that position today, measured on the real dispatch
// path: an interrupt of an IDLE session is fail-quiet and appends no public event at
// all, so it has nothing to be adjacent to and can never resolve; an interrupt of a
// busy session produces TurnInterrupted, but only after the per-loop audit intent
// records the fan-out writes first. Giving KindInterrupt a guaranteed durable effect
// record is a public-event-vocabulary decision, not a framing one, so it is left to
// the Host adapter task — with this test standing in front of it so the semantics
// cannot drift while it waits.
func TestPrefixFollowedByANonEventResolvesUnresolved(t *testing.T) {
	t.Parallel()
	store, sid, lease, j := runtimeCommandStore(t)
	commandID := coresessionwire.CommandID("v1:no-effect-event")
	runtimeID := uuid.UUID{0x8a, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15}
	if _, _, err := store.durable.AdmitCommand(context.Background(), durablestore.AdmitCommandRequest{
		TenantID:                 harnessTenantID,
		SessionID:                harnessSessionID(sid),
		CommandID:                commandID,
		ProposedRuntimeCommandID: durablestore.RuntimeCommandID(runtimeID.String()),
		Kind:                     durablestore.CommandKind(runtimecommand.KindInterrupt),
		AcceptedAt:               time.Now().UTC(),
		ApplyDeadline:            time.Now().UTC().Add(time.Hour),
	}); err != nil {
		t.Fatalf("AdmitCommand: %v", err)
	}
	log, err := store.OpenRuntimeCommandLog(sid, j)
	if err != nil {
		t.Fatalf("OpenRuntimeCommandLog: %v", err)
	}
	res, err := log.AppendCommandApplication(context.Background(), runtimecommand.Application{
		CommandID:        runtimecommand.CommandID(commandID),
		RuntimeCommandID: runtimeID,
		LeaseEpoch:       lease.Epoch(),
		Kind:             runtimecommand.KindInterrupt,
	})
	if err != nil {
		t.Fatalf("AppendCommandApplication: %v", err)
	}
	// A runtime-control record in the adjacent slot: an audit intent record is exactly
	// this shape, which is what the interrupt fan-out writes before it delivers.
	if _, err := j.Append(context.Background(), journal.NewCommandRecord(sid, uuid.UUID{}, command.Interrupt{
		Header: command.Header{CommandID: uuid.UUID{0xC1, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15}},
	})); err != nil {
		t.Fatalf("Append(non-event): %v", err)
	}

	got, err := store.durable.FindCommandApplication(context.Background(), durablestore.FindCommandApplicationRequest{
		TenantID: harnessTenantID, SessionID: harnessSessionID(sid), CommandID: commandID,
	})
	if err != nil {
		t.Fatalf("FindCommandApplication: %v", err)
	}
	if got.Outcome != durablestore.CommandApplicationUnresolved {
		t.Fatalf("released reader resolved %q, want %q", got.Outcome, durablestore.CommandApplicationUnresolved)
	}
	// The safe side of the gap, and the reason this is liveness rather than
	// correctness: an unresolved command is never settled `rejected` over its effect.
	if got.Outcome == durablestore.CommandApplicationAbsent {
		t.Fatalf("released reader reports ABSENT; the reconciler would settle rejected")
	}
	if got.PrefixSeq != res.Sequence {
		t.Errorf("located the prefix at %d, want %d", got.PrefixSeq, res.Sequence)
	}
}
