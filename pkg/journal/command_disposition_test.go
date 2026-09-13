package journal_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/command"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/identity"
	"github.com/looprig/harness/pkg/journal"
	"github.com/looprig/harness/pkg/runtimecommand"
)

func dispositionFixture() runtimecommand.CommandDisposition {
	return runtimecommand.CommandDisposition{
		CommandID:           "public-command-1",
		RuntimeCommandID:    uuid.UUID{9, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15},
		Kind:                runtimecommand.KindInput,
		LeaseEpoch:          4,
		AttemptID:           "attempt-a",
		AttemptJournalEpoch: 4,
		Disposition:         runtimecommand.DispositionApplied,
	}
}

// TestCommandDispositionIdempotencyIDKeysOnTheAttempt is the guard that stops a
// successor's recovery closure from tombstoning a predecessor's durable
// application. Both records name the SAME attempt, so both derive the same
// idempotency id and the second append collides instead of landing.
//
// It also pins the namespace. The attempt id is an opaque string that may render a
// UUID or a decimal epoch — the shapes the event, command and fence records key on —
// so an un-namespaced id could collide with one of those by coincidence.
func TestCommandDispositionIdempotencyIDKeysOnTheAttempt(t *testing.T) {
	t.Parallel()
	applied := journal.NewCommandDispositionRecord(dispositionFixture())

	closure := dispositionFixture()
	closure.Disposition = runtimecommand.DispositionNotApplied
	closure.LeaseEpoch = 11
	closure.RuntimeCommandID = uuid.UUID{2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2}
	closed := journal.NewCommandDispositionRecord(closure)

	if applied.IdempotencyID() != closed.IdempotencyID() {
		t.Fatalf("two dispositions for one attempt derive %q and %q; they must collide",
			applied.IdempotencyID(), closed.IdempotencyID())
	}
	if got := applied.IdempotencyID(); !strings.HasPrefix(got, "command-disposition:") {
		t.Errorf("IdempotencyID() = %q, want a command-disposition: namespace", got)
	}
	if got, want := applied.IdempotencyID(), "command-disposition:attempt-a"; got != want {
		t.Errorf("IdempotencyID() = %q, want %q", got, want)
	}

	// A DIFFERENT attempt is a different id: the collision must be scoped to one
	// attempt, or a second command's disposition would deduplicate against the first.
	other := dispositionFixture()
	other.AttemptID = "attempt-b"
	if journal.NewCommandDispositionRecord(other).IdempotencyID() == applied.IdempotencyID() {
		t.Errorf("two attempts share one idempotency id %q", applied.IdempotencyID())
	}

	// The id must not key on the COMMAND: one command may be attempted twice, and a
	// successor's attempt must be able to write its own disposition.
	sameCommandNewAttempt := dispositionFixture()
	sameCommandNewAttempt.AttemptID = "attempt-c"
	if journal.NewCommandDispositionRecord(sameCommandNewAttempt).IdempotencyID() == applied.IdempotencyID() {
		t.Errorf("a second attempt at the same command collided with the first")
	}
}

// TestCommandDispositionIdempotencyIDIsDistinctFromAnApplicationPrefix keeps the two
// private command-keyed records in separate namespaces. They can name the same
// command in the same journal, and a shared id would make the prefix and the
// disposition deduplicate against one another — the disposition would silently never
// land, and the settlement reader would report no evidence for a command that was
// applied.
func TestCommandDispositionIdempotencyIDIsDistinctFromAnApplicationPrefix(t *testing.T) {
	t.Parallel()
	d := dispositionFixture()
	prefix := journal.NewCommandApplicationRecord(runtimecommand.Application{
		CommandID:        d.CommandID,
		RuntimeCommandID: d.RuntimeCommandID,
		LeaseEpoch:       d.LeaseEpoch,
		Kind:             d.Kind,
	})
	// The adversarial case: an attempt id spelled exactly like the public command id.
	collidingAttempt := d
	collidingAttempt.AttemptID = runtimecommand.AttemptID(d.CommandID)
	disposition := journal.NewCommandDispositionRecord(collidingAttempt)
	if prefix.IdempotencyID() == disposition.IdempotencyID() {
		t.Fatalf("an attempt id equal to the command id collided with the application prefix at %q",
			prefix.IdempotencyID())
	}
}

// TestCommandDispositionCodecRoundTrips pins the fingerprint bytes. The durable
// frame is bodiless, so these bytes are never stored — but the idempotency index is
// hydrated by re-encoding a replayed record, so the write side and the replay side
// must produce identical bytes or every redelivery reads as a collision.
func TestCommandDispositionCodecRoundTrips(t *testing.T) {
	t.Parallel()
	for name, d := range map[string]runtimecommand.CommandDisposition{
		"applied":     dispositionFixture(),
		"no_op":       withKind(dispositionFixture(), runtimecommand.DispositionNoOp),
		"refused":     withKind(dispositionFixture(), runtimecommand.DispositionRefused),
		"not_applied": laterGrant(withKind(dispositionFixture(), runtimecommand.DispositionNotApplied)),
	} {
		body, err := journal.MarshalCommandDispositionRecord(journal.NewCommandDispositionRecord(d))
		if err != nil {
			t.Errorf("%s: Marshal returned %v", name, err)
			continue
		}
		if len(body) == 0 {
			t.Errorf("%s: Marshal produced no bytes", name)
			continue
		}
		back, err := journal.UnmarshalCommandDispositionRecord(body)
		if err != nil {
			t.Errorf("%s: Unmarshal returned %v", name, err)
			continue
		}
		if back.Disposition() != d {
			t.Errorf("%s: round trip = %+v, want %+v", name, back.Disposition(), d)
		}
		again, err := journal.MarshalCommandDispositionRecord(back)
		if err != nil {
			t.Errorf("%s: re-marshal returned %v", name, err)
			continue
		}
		if string(again) != string(body) {
			t.Errorf("%s: re-encoded bytes %q differ from %q", name, again, body)
		}
	}
}

func withKind(d runtimecommand.CommandDisposition, k runtimecommand.DispositionKind) runtimecommand.CommandDisposition {
	d.Disposition = k
	return d
}

func laterGrant(d runtimecommand.CommandDisposition) runtimecommand.CommandDisposition {
	d.LeaseEpoch = d.AttemptJournalEpoch + 1
	return d
}

// TestCommandDispositionCodecFailsClosed covers the untrusted restore boundary and
// the write side's refusal to make an invalid statement durable.
func TestCommandDispositionCodecFailsClosed(t *testing.T) {
	t.Parallel()
	invalid := dispositionFixture()
	invalid.AttemptID = ""
	if _, err := journal.MarshalCommandDispositionRecord(journal.NewCommandDispositionRecord(invalid)); err == nil {
		t.Errorf("Marshal accepted a disposition with no attempt id")
	} else {
		var encodeErr *journal.CommandDispositionEncodeError
		if !errors.As(err, &encodeErr) {
			t.Errorf("Marshal error %v is not a *CommandDispositionEncodeError", err)
		}
	}

	valid, err := journal.MarshalCommandDispositionRecord(journal.NewCommandDispositionRecord(dispositionFixture()))
	if err != nil {
		t.Fatalf("the valid fixture failed to marshal: %v", err)
	}
	for name, body := range map[string][]byte{
		"empty":          nil,
		"not an object":  []byte(`"applied"`),
		"malformed json": []byte(`{`),
		"unknown field":  []byte(`{"command_id":"c","runtime_command_id":"09010203-0405-0607-0809-0a0b0c0d0e0f","command_kind":"input","lease_epoch":4,"attempt_id":"a","attempt_journal_epoch":4,"disposition_kind":"applied","extra":1}`),
		"trailing data":  append(append([]byte{}, valid...), []byte("trailing")...),
		"unknown kind":   []byte(`{"command_id":"c","runtime_command_id":"09010203-0405-0607-0809-0a0b0c0d0e0f","command_kind":"input","lease_epoch":4,"attempt_id":"a","attempt_journal_epoch":4,"disposition_kind":"rejected"}`),
		"forged grant":   []byte(`{"command_id":"c","runtime_command_id":"09010203-0405-0607-0809-0a0b0c0d0e0f","command_kind":"input","lease_epoch":9,"attempt_id":"a","attempt_journal_epoch":4,"disposition_kind":"applied"}`),
	} {
		if _, err := journal.UnmarshalCommandDispositionRecord(body); err == nil {
			t.Errorf("%s: Unmarshal accepted %q", name, body)
			continue
		} else {
			var decodeErr *journal.CommandDispositionDecodeError
			if !errors.As(err, &decodeErr) {
				t.Errorf("%s: error %v is not a *CommandDispositionDecodeError", name, err)
			}
		}
	}
}

// TestNamespacedCommandRecordsCannotCollideWithAnyOtherKind replaces a tautology.
//
// The test that stood here assigned a CommandDispositionRecord to a JournalRecord and
// asserted it came back — which the compiler already guarantees — and its doc claimed
// it proved the serializer's switch is exhaustive, a property that lives in
// pkg/sessionstore and that pkg/sessionstore's own framing tests measure.
//
// What IS this package's to prove is the rule the two private command-keyed records
// were given a namespace FOR. An idempotency id is a de-dup key: two records sharing
// one silently deduplicate against each other, and the loss is invisible. The other
// kinds key on a bare uuid rendering or a decimal epoch, while these two key on an
// OPAQUE caller-supplied string that could render either. So the ids are held apart
// ADVERSARIALLY — the public command id and the attempt id are both set to a string
// that renders exactly a uuid another record is keyed by — and they must still be
// distinct from it and from each other.
//
// SCOPE, stated because the fixture reveals more than the claim. An EventRecord and a
// CommandRecord carrying the same uuid DO share an idempotency id: both key on a bare
// rendering, in one namespace. That is PRE-EXISTING and untouched by this change, and
// it is unreachable in practice because an EventID and a CommandID are independently
// minted uuids. It is asserted below as a fact so that the namespaced ids are known
// to be separated by the NAMESPACE rather than by an accident of the fixture — not as
// an endorsement of the shared one.
func TestNamespacedCommandRecordsCannotCollideWithAnyOtherKind(t *testing.T) {
	t.Parallel()
	// One identity, worn by every kind that can wear it: a uuid, and its canonical
	// rendering as the opaque public command id and attempt id. This is the
	// adversarial case — a caller choosing ids to collide.
	id := uuid.UUID{9, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15}
	shared := runtimecommand.CommandID(id.String())

	bare := map[string]string{
		"event": journal.NewEventRecord(event.SessionStarted{Header: event.Header{
			Coordinates: identity.Coordinates{SessionID: id}, EventID: id,
		}}).IdempotencyID(),
		"command": journal.NewCommandRecord(id, id, command.Interrupt{
			Header: command.Header{CommandID: id},
		}).IdempotencyID(),
		"fence": journal.NewFenceRecord(id, journal.LeaseFence{Epoch: 7}).IdempotencyID(),
	}
	namespaced := map[string]string{
		"command application": journal.NewCommandApplicationRecord(runtimecommand.Application{
			CommandID: shared, RuntimeCommandID: id, LeaseEpoch: 7, Kind: runtimecommand.KindInput,
		}).IdempotencyID(),
		"command disposition": journal.NewCommandDispositionRecord(runtimecommand.CommandDisposition{
			CommandID: shared, RuntimeCommandID: id, Kind: runtimecommand.KindInput,
			LeaseEpoch: 7, AttemptID: runtimecommand.AttemptID(shared), AttemptJournalEpoch: 7,
			Disposition: runtimecommand.DispositionApplied,
		}).IdempotencyID(),
	}

	// Non-vacuity, and the scope note above made concrete: the bare kinds really do
	// key on the same underlying identity, so distinctness below is the namespace's
	// doing and not the fixture's.
	if bare["event"] != bare["command"] {
		t.Fatalf("the event and command ids (%q, %q) differ, so the shared-identity fixture is not adversarial",
			bare["event"], bare["command"])
	}
	if bare["event"] != id.String() {
		t.Fatalf("the bare kinds key on %q, not the uuid rendering %q", bare["event"], id.String())
	}

	if namespaced["command application"] == namespaced["command disposition"] {
		t.Errorf("the application prefix and the disposition share the id %q; a disposition would deduplicate against its own prefix",
			namespaced["command application"])
	}
	for nsKind, nsID := range namespaced {
		if nsID == "" {
			t.Errorf("%s has an empty idempotency id", nsKind)
			continue
		}
		for bareKind, bareID := range bare {
			if nsID == bareID {
				t.Errorf("%s collides with %s at %q, even though its id is caller-supplied and namespaced",
					nsKind, bareKind, nsID)
			}
		}
	}
}
