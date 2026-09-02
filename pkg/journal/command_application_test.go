package journal_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/journal"
	"github.com/looprig/harness/pkg/runtimecommand"
)

func applicationUUID(b byte) uuid.UUID {
	return uuid.UUID{b, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15}
}

func validApplication() runtimecommand.Application {
	return runtimecommand.Application{
		CommandID:        "v1:public-command",
		RuntimeCommandID: applicationUUID(0x41),
		LeaseEpoch:       7,
	}
}

// TestCommandApplicationIdempotencyIDIsNamespaced pins the property that makes the
// public CommandID safe as a durable idempotency key: it is namespaced, so an
// OPAQUE public id can never collide with an event's UUID rendering or a fence's
// decimal epoch, and the namespacing stays injective over CommandIDs.
func TestCommandApplicationIdempotencyIDIsNamespaced(t *testing.T) {
	t.Parallel()
	// A public id that renders exactly like the other record kinds' ids.
	collidingShapes := []runtimecommand.CommandID{
		runtimecommand.CommandID(applicationUUID(0x41).String()),
		"7",
		"0",
	}
	seen := map[string]runtimecommand.CommandID{}
	for _, id := range collidingShapes {
		app := validApplication()
		app.CommandID = id
		got := journal.NewCommandApplicationRecord(app).IdempotencyID()
		if got == string(id) {
			t.Errorf("IdempotencyID for %q is the bare public id; it must be namespaced", id)
		}
		if !strings.HasSuffix(got, string(id)) {
			t.Errorf("IdempotencyID(%q) = %q, want it to end in the public id", id, got)
		}
		if prior, dup := seen[got]; dup {
			t.Errorf("IdempotencyID collision: %q and %q both map to %q", id, prior, got)
		}
		seen[got] = id
	}
	if len(seen) != len(collidingShapes) {
		t.Fatalf("guard consumed %d distinct ids, want %d", len(seen), len(collidingShapes))
	}
}

// TestCommandApplicationRoundTrip proves the codec preserves all three correlated
// values exactly.
func TestCommandApplicationRoundTrip(t *testing.T) {
	t.Parallel()
	rec := journal.NewCommandApplicationRecord(validApplication())
	body, err := journal.MarshalCommandApplicationRecord(rec)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	back, err := journal.UnmarshalCommandApplicationRecord(body)
	if err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if back.Application() != rec.Application() {
		t.Errorf("round trip = %+v, want %+v", back.Application(), rec.Application())
	}
	if back.IdempotencyID() != rec.IdempotencyID() {
		t.Errorf("round-trip IdempotencyID = %q, want %q", back.IdempotencyID(), rec.IdempotencyID())
	}
}

// TestCommandApplicationCodecFailsClosed proves the decode boundary refuses every
// malformed body rather than yielding a zero correlation. A zero correlation read
// as "already applied" would SUPPRESS an application, which is the failure this
// record exists to prevent.
func TestCommandApplicationCodecFailsClosed(t *testing.T) {
	t.Parallel()
	bodies := map[string]string{
		"empty":              ``,
		"not an object":      `"x"`,
		"unknown field":      `{"command_id":"c","runtime_command_id":"` + applicationUUID(1).String() + `","lease_epoch":1,"extra":1}`,
		"trailing data":      `{"command_id":"c","runtime_command_id":"` + applicationUUID(1).String() + `","lease_epoch":1}trailing`,
		"missing command id": `{"runtime_command_id":"` + applicationUUID(1).String() + `","lease_epoch":1}`,
		"zero runtime id":    `{"command_id":"c","runtime_command_id":"00000000-0000-0000-0000-000000000000","lease_epoch":1}`,
		"zero epoch":         `{"command_id":"c","runtime_command_id":"` + applicationUUID(1).String() + `","lease_epoch":0}`,
		"bad runtime id":     `{"command_id":"c","runtime_command_id":"not-a-uuid","lease_epoch":1}`,
		"command id too long": `{"command_id":"` + strings.Repeat("x", runtimecommand.MaxCommandIDBytes+1) +
			`","runtime_command_id":"` + applicationUUID(1).String() + `","lease_epoch":1}`,
	}
	if len(bodies) < 9 {
		t.Fatalf("guard consumes too few bodies: %d", len(bodies))
	}
	for name, body := range bodies {
		rec, err := journal.UnmarshalCommandApplicationRecord([]byte(body))
		if err == nil {
			t.Errorf("Unmarshal(%s) = %+v, nil error; want a fail-closed refusal", name, rec.Application())
			continue
		}
		var decodeErr *journal.CommandApplicationDecodeError
		if !errors.As(err, &decodeErr) {
			t.Errorf("Unmarshal(%s) error = %T, want *journal.CommandApplicationDecodeError", name, err)
		}
		if rec.Application() != (runtimecommand.Application{}) {
			t.Errorf("Unmarshal(%s) returned a non-zero record alongside its error", name)
		}
	}
}

// TestCodecCarriesAnOpaquePublicIDVerbatim proves the codec is as permissive as the
// admission authority: an id carrying a control character and surrounding space —
// which Core accepts and Harness must therefore apply — round-trips byte for byte
// rather than being refused at the durable boundary.
func TestCodecCarriesAnOpaquePublicIDVerbatim(t *testing.T) {
	t.Parallel()
	opaque := []runtimecommand.CommandID{" leading", "trailing ", "a\x00b", "a\tb", "a\x7fb"}
	if len(opaque) < 5 {
		t.Fatalf("guard consumes too few identities: %d", len(opaque))
	}
	carried := 0
	for _, id := range opaque {
		app := validApplication()
		app.CommandID = id
		body, err := journal.MarshalCommandApplicationRecord(journal.NewCommandApplicationRecord(app))
		if err != nil {
			t.Errorf("Marshal(%q) = %v, want nil: Core admits this id", id, err)
			continue
		}
		back, err := journal.UnmarshalCommandApplicationRecord(body)
		if err != nil {
			t.Errorf("Unmarshal(%q) = %v, want nil", id, err)
			continue
		}
		if back.Application().CommandID != id {
			t.Errorf("round trip = %q, want %q verbatim", back.Application().CommandID, id)
			continue
		}
		carried++
	}
	// Floor the quantity the guard CONSUMES: every id must have completed the round
	// trip, or a codec that refused most of them would still pass on the remainder.
	if carried != len(opaque) {
		t.Fatalf("only %d of %d identities round-tripped verbatim", carried, len(opaque))
	}
}

// TestMarshalRefusesAnInvalidCorrelation proves an invalid correlation is refused
// on the WRITE side too, so a malformed prefix is never made durable.
func TestMarshalRefusesAnInvalidCorrelation(t *testing.T) {
	t.Parallel()
	invalid := map[string]runtimecommand.Application{
		"empty command id": {RuntimeCommandID: applicationUUID(1), LeaseEpoch: 1},
		"zero runtime id":  {CommandID: "c", LeaseEpoch: 1},
		"zero epoch":       {CommandID: "c", RuntimeCommandID: applicationUUID(1)},
	}
	if len(invalid) < 3 {
		t.Fatalf("guard consumes too few correlations: %d", len(invalid))
	}
	refused := 0
	for name, app := range invalid {
		if _, err := journal.MarshalCommandApplicationRecord(journal.NewCommandApplicationRecord(app)); err == nil {
			t.Errorf("Marshal(%s) = nil error, want a refusal", name)
			continue
		}
		refused++
	}
	if refused != len(invalid) {
		t.Fatalf("only %d of %d invalid correlations were refused", refused, len(invalid))
	}
}

// TestRuntimeCommandAppenderRequiresIdempotency proves the appender constructor
// refuses a journal that cannot deduplicate — and accepts one that can, including
// through journal.Decorate. The decorated case is the load-bearing one: the whole
// duplicate-delivery contract rides on IdempotentJournal surviving decoration, and a
// decorator that demoted it would silently disable duplicate detection.
func TestRuntimeCommandAppenderRequiresIdempotency(t *testing.T) {
	t.Parallel()
	if _, err := journal.NewJournalRuntimeCommandAppenderChecked(nil); err == nil {
		t.Errorf("nil journal accepted, want *journal.NilJournalError")
	}
	plain := plainAppendOnlyJournal{}
	_, err := journal.NewJournalRuntimeCommandAppenderChecked(plain)
	var nonIdempotent *journal.NonIdempotentJournalError
	if !errors.As(err, &nonIdempotent) {
		t.Errorf("plain journal error = %v, want *journal.NonIdempotentJournalError", err)
	}
	idempotent := idempotentTestJournal{}
	if _, err := journal.NewJournalRuntimeCommandAppenderChecked(idempotent); err != nil {
		t.Errorf("idempotent journal refused: %v", err)
	}
	decorated := journal.Decorate(idempotent, passThroughAround)
	if _, err := journal.NewJournalRuntimeCommandAppenderChecked(decorated); err != nil {
		t.Errorf("DECORATED idempotent journal refused: %v — decoration must preserve the contract", err)
	}
	// The control: the same decorator over a plain journal must still be refused, so
	// the assertion above is about the delegate's contract, not about Decorate always
	// advertising everything.
	if _, err := journal.NewJournalRuntimeCommandAppenderChecked(journal.Decorate(plain, passThroughAround)); err == nil {
		t.Errorf("decorated PLAIN journal accepted, want a refusal")
	}
}

// --- doubles ---------------------------------------------------------------

func passThroughAround(ctx context.Context, _ journal.JournalRecord, next func(context.Context) error) error {
	return next(ctx)
}

// plainAppendOnlyJournal advertises only the base SessionJournal contract.
type plainAppendOnlyJournal struct{}

func (plainAppendOnlyJournal) Append(context.Context, journal.JournalRecord) (uint64, error) {
	return 1, nil
}

// idempotentTestJournal additionally advertises IdempotentJournal.
type idempotentTestJournal struct{ plainAppendOnlyJournal }

func (idempotentTestJournal) AppendIdempotent(context.Context, journal.JournalRecord) (journal.AppendResult, error) {
	return journal.AppendResult{Sequence: 1, Appended: true}, nil
}
