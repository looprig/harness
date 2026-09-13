package sessionstore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/looprig/core/content"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/command"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/identity"
	"github.com/looprig/harness/pkg/journal"
	"github.com/looprig/harness/pkg/runtimecommand"
	durablestore "github.com/looprig/sessionstore"
	"github.com/looprig/storage/memstore"
)

// legacyJournalFingerprint is the SHA-256 of every raw ledger frame a legacy session
// writes, concatenated in sequence order, for the fixture below.
//
// It is a GOLDEN VALUE and it is the point of this file. The disposition work added a
// journal record kind, an envelope kind, a framing arm and a replay arm; the claim
// that a session which uses none of them is byte-for-byte unchanged was, until now,
// resting on a REVIEW — a one-off measurement somebody made once by hand — and on
// TestNoAttemptIDWritesNoDisposition, which counts DECODED RECORDS and so cannot see
// a change in the bytes those records encode to.
//
// The difference matters because of who is downstream. A binary pinned below the
// sessionstore release that knows the new envelope kind REFUSES a journal containing
// one, by design; and nine dependent modules read journals this package wrote. A
// one-off measurement is not a regression guard. This is.
//
// IF THIS TEST FAILS, the legacy write path's bytes changed. That is not automatically
// wrong, but it is never incidental: decide whether existing journals can still be
// read by every pinned consumer, and only then update the constant, in a commit that
// says so.
//
// ITS VALUE WAS MEASURED AT BOTH ENDS OF THE CHANGE, not asserted. This same fixture
// was run in a clean clone at cb329b71 — released v0.33.0, pinned to
// sessionstore v0.1.0, before any of this work existed — and it produced this exact
// digest. That is the cross-version comparison the compatibility claim needs, and
// pinning it here is what turns a one-off measurement into a regression guard.
const legacyJournalFingerprint = "e2838e7876edfb6147c3c4aba06bddffe2b89144252cd36fe84b47cbc7794c12"

// writeLegacyJournal writes one session's worth of records using ONLY the paths that
// existed before the disposition work: an opening fence, public and internal enduring
// events, an audit intent command record, and an application prefix with NO attempt
// id. Every identity, timestamp and body is fixed, so the frames are deterministic.
func writeLegacyJournal(t *testing.T, store *Store, sid uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	lease, err := store.AcquireLease(ctx, sid)
	if err != nil {
		t.Fatalf("AcquireLease: %v", err)
	}
	j, err := store.OpenJournal(ctx, sid, lease)
	if err != nil {
		t.Fatalf("OpenJournal: %v", err)
	}
	at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	loopID := legacyUUID(2)
	header := func(n byte) event.Header {
		return event.Header{
			Coordinates: identity.Coordinates{SessionID: sid, LoopID: loopID},
			EventID:     legacyUUID(n),
			CreatedAt:   at,
		}
	}
	turn := header(4)
	turn.TurnID = legacyUUID(5)
	turn.Cause = identity.Cause{CommandID: legacyUUID(6), Agency: identity.AgencyUser}
	internal := header(7)
	internal.TurnID = legacyUUID(8)
	internal.EventVisibility = event.Internal

	for _, rec := range []journal.JournalRecord{
		journal.NewEventRecord(event.SessionStarted{Header: event.Header{
			Coordinates: identity.Coordinates{SessionID: sid}, EventID: legacyUUID(3), CreatedAt: at,
		}}),
		journal.NewEventRecord(event.LoopStarted{Header: header(9)}),
		journal.NewEventRecord(event.TurnStarted{Header: turn, TurnIndex: 1}),
		journal.NewEventRecord(event.TurnStarted{
			Header: internal, TurnIndex: 2,
			Message: legacyUserMessage("an internal projection body"),
		}),
		journal.NewCommandRecord(sid, loopID, command.Interrupt{
			Header: command.Header{CommandID: legacyUUID(10)},
		}),
		// The application prefix with NO AttemptID: the legacy shape exactly.
		journal.NewCommandApplicationRecord(runtimecommand.Application{
			CommandID:        "v1:legacy-command",
			RuntimeCommandID: legacyUUID(6),
			LeaseEpoch:       lease.Epoch(),
			Kind:             runtimecommand.KindInput,
		}),
	} {
		if _, err := j.Append(ctx, rec); err != nil {
			t.Fatalf("Append(%T): %v", rec, err)
		}
	}
}

// legacyUUID reuses the package's deterministic seed-byte uuid so this fixture's
// identities are stable across runs, which is what makes a byte fingerprint possible
// at all.
func legacyUUID(seed byte) uuid.UUID { return fixedUUID(seed) }

func legacyUserMessage(text string) *content.UserMessage {
	msg := &content.UserMessage{}
	msg.Blocks = []content.Block{&content.TextBlock{Text: text}}
	return msg
}

// legacyFrames reads every raw frame from the session's ledger, in order. It is
// deliberately local to this file rather than shared with the disposition tests: a
// compatibility guard that depended on a helper introduced by the change it guards
// could stop compiling — or stop meaning the same thing — in exactly the commit it
// exists to check.
func legacyFrames(t *testing.T, s *Store, id uuid.UUID) [][]byte {
	t.Helper()
	ctx := context.Background()
	cursor, err := s.backend.Ledger.Read(ctx, ledgerName(id), 1)
	if err != nil {
		t.Fatalf("Ledger.Read: %v", err)
	}
	defer func() { _ = cursor.Close() }()
	var frames [][]byte
	for {
		rec, err := cursor.Next(ctx)
		if errors.Is(err, io.EOF) {
			return frames
		}
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		frames = append(frames, append([]byte(nil), rec.Payload...))
	}
}

// TestLegacyJournalBytesAreUnchanged is the backward-compatibility GUARD, asserted
// over RAW LEDGER PAYLOADS rather than over decoded records.
//
// A test that counts decoded records answers "did a disposition appear?". This one
// answers the question a downstream consumer actually asks: "are the bytes the same?"
// — which also catches a field added to an existing record's body, a changed envelope
// tag, an offload threshold that moved, and every other way a frame can change while
// still decoding to the same thing.
func TestLegacyJournalBytesAreUnchanged(t *testing.T) {
	t.Parallel()
	store, err := Open(memstore.New())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	sid := legacyUUID(1)
	writeLegacyJournal(t, store, sid)

	frames := legacyFrames(t, store, sid)
	// Non-vacuity: a fingerprint over an empty or truncated journal would be stable
	// for the wrong reason. The fence plus six appends is seven frames.
	if len(frames) != 7 {
		t.Fatalf("the legacy journal holds %d frames, want 7", len(frames))
	}
	digest := sha256.New()
	for _, frame := range frames {
		digest.Write(frame)
	}
	got := hex.EncodeToString(digest.Sum(nil))
	if got != legacyJournalFingerprint {
		t.Fatalf("the legacy write path's raw bytes changed.\n got %s\nwant %s\n"+
			"See the constant's doc before updating it: a byte change here is visible to every "+
			"pinned consumer that reads journals this package wrote.", got, legacyJournalFingerprint)
	}

	// And no disposition frame exists, which is the narrower claim the decoded-record
	// test makes. Keeping it here too means the byte guard cannot pass for the
	// uninteresting reason that nothing was written at all.
	for i, frame := range frames {
		env, err := durablestore.DecodeEnvelope(frame)
		if err != nil {
			t.Fatalf("frame %d does not decode: %v", i+1, err)
		}
		if env.Kind == durablestore.EnvelopeKindCommandDisposition {
			t.Fatalf("frame %d is a disposition; a legacy session must write none", i+1)
		}
	}
}
