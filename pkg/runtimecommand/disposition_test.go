package runtimecommand_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/looprig/core/content"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/runtimecommand"
)

// TestAttemptIDValidityIsTheDurableBoundaryRule pins the attempt id's rule to the
// one the durable boundary applies: non-empty, at most MaxAttemptIDBytes bytes,
// valid UTF-8. It is the same shape as CommandID's rule and for the same reason —
// sessionstore's validateEnvelope refuses an attempt_id that is empty, longer than
// sessionwire.MaxIDBytes, or not valid UTF-8, and a stricter rule here would strand
// an attempt Host has already durably recorded.
func TestAttemptIDValidityIsTheDurableBoundaryRule(t *testing.T) {
	t.Parallel()
	for _, id := range []runtimecommand.AttemptID{
		"attempt-1",
		"v1:AAECAwQFBgcICQoLDA0ODw",
		"héllo-attempt",
		"0",
		" leading space",
		"embedded\ttab",
		"\u202eBiDi",
		"\U0001F600astral",
		runtimecommand.AttemptID(strings.Repeat("a", runtimecommand.MaxAttemptIDBytes)),
	} {
		if err := id.Validate(); err != nil {
			t.Errorf("AttemptID(%q).Validate() = %v, want nil", id, err)
		}
	}
	for name, id := range map[string]runtimecommand.AttemptID{
		"empty":        "",
		"over length":  runtimecommand.AttemptID(strings.Repeat("a", runtimecommand.MaxAttemptIDBytes+1)),
		"invalid utf8": runtimecommand.AttemptID([]byte{0xff, 0xfe}),
	} {
		err := id.Validate()
		if err == nil {
			t.Errorf("%s: AttemptID(%q).Validate() = nil, want an error", name, id)
			continue
		}
		var invalid *runtimecommand.ValidationError
		if !errors.As(err, &invalid) {
			t.Errorf("%s: error %v is not a *ValidationError", name, err)
			continue
		}
		if invalid.Field != "AttemptID" {
			t.Errorf("%s: Field = %q, want %q", name, invalid.Field, "AttemptID")
		}
	}
}

// TestDispositionKindVocabularyIsTheReleasedFour holds the vocabulary to
// sessionstore's DispositionOutcomeKind. Both directions matter: a kind this package
// accepts and the store does not is a record the store refuses to decode, and a kind
// the store accepts and this package rejects is an outcome a runtime can never write.
func TestDispositionKindVocabularyIsTheReleasedFour(t *testing.T) {
	t.Parallel()
	for _, k := range []runtimecommand.DispositionKind{
		runtimecommand.DispositionApplied,
		runtimecommand.DispositionNoOp,
		runtimecommand.DispositionRefused,
		runtimecommand.DispositionNotApplied,
	} {
		if !k.Valid() {
			t.Errorf("DispositionKind(%q).Valid() = false, want true", k)
		}
	}
	for _, k := range []runtimecommand.DispositionKind{
		"", "Applied", "applied ", "noop", "no-op", "rejected", "not applied", "unknown",
	} {
		if k.Valid() {
			t.Errorf("DispositionKind(%q).Valid() = true, want false", k)
		}
	}
	// The four spellings are the durable bytes. A rename is a wire change.
	for spelling, k := range map[string]runtimecommand.DispositionKind{
		"applied":     runtimecommand.DispositionApplied,
		"no_op":       runtimecommand.DispositionNoOp,
		"refused":     runtimecommand.DispositionRefused,
		"not_applied": runtimecommand.DispositionNotApplied,
	} {
		if string(k) != spelling {
			t.Errorf("kind %v spells %q, want %q", k, string(k), spelling)
		}
	}
}

func validDisposition() runtimecommand.CommandDisposition {
	return runtimecommand.CommandDisposition{
		CommandID:           "command-1",
		RuntimeCommandID:    testUUID(7),
		Kind:                runtimecommand.KindInput,
		LeaseEpoch:          9,
		AttemptID:           "attempt-1",
		AttemptJournalEpoch: 9,
		Disposition:         runtimecommand.DispositionApplied,
	}
}

// TestCommandDispositionValidateFailsClosedPerField checks every field the durable
// envelope requires. Each row mutates exactly one field of an otherwise valid
// disposition, so a row that passes names the field whose guard is missing.
func TestCommandDispositionValidateFailsClosedPerField(t *testing.T) {
	t.Parallel()
	if err := validDisposition().Validate(); err != nil {
		t.Fatalf("the valid baseline failed: %v", err)
	}
	for field, mutate := range map[string]func(*runtimecommand.CommandDisposition){
		"CommandID":           func(d *runtimecommand.CommandDisposition) { d.CommandID = "" },
		"RuntimeCommandID":    func(d *runtimecommand.CommandDisposition) { d.RuntimeCommandID = uuid.UUID{} },
		"Kind":                func(d *runtimecommand.CommandDisposition) { d.Kind = "shutdown" },
		"LeaseEpoch":          func(d *runtimecommand.CommandDisposition) { d.LeaseEpoch = 0 },
		"AttemptID":           func(d *runtimecommand.CommandDisposition) { d.AttemptID = "" },
		"AttemptJournalEpoch": func(d *runtimecommand.CommandDisposition) { d.AttemptJournalEpoch = 0 },
		"Disposition":         func(d *runtimecommand.CommandDisposition) { d.Disposition = "rejected" },
	} {
		d := validDisposition()
		mutate(&d)
		err := d.Validate()
		if err == nil {
			t.Errorf("%s: Validate() = nil, want an error", field)
			continue
		}
		var invalid *runtimecommand.ValidationError
		if !errors.As(err, &invalid) {
			t.Errorf("%s: error %v is not a *ValidationError", field, err)
			continue
		}
		if invalid.Field != field {
			t.Errorf("%s: Field = %q, want %q", field, invalid.Field, field)
		}
	}
}

// TestApplicationDispositionsShareTheAuthorGrant pins the rule the settlement
// verifier enforces for applied/no_op/refused: the author grant and the attempt
// grant are EQUAL. A disposition of one of those three kinds whose LeaseEpoch
// differs from its AttemptJournalEpoch is refused here rather than written and
// refused later by the reader, where it would read as a forged epoch.
func TestApplicationDispositionsShareTheAuthorGrant(t *testing.T) {
	t.Parallel()
	for _, k := range []runtimecommand.DispositionKind{
		runtimecommand.DispositionApplied,
		runtimecommand.DispositionNoOp,
		runtimecommand.DispositionRefused,
	} {
		d := validDisposition()
		d.Disposition = k
		d.LeaseEpoch, d.AttemptJournalEpoch = 10, 9
		err := d.Validate()
		if err == nil {
			t.Errorf("%s: a later author grant was accepted, want an error", k)
			continue
		}
		var invalid *runtimecommand.ValidationError
		if !errors.As(err, &invalid) || invalid.Field != "LeaseEpoch" {
			t.Errorf("%s: error %v, want a *ValidationError on LeaseEpoch", k, err)
		}
		d.LeaseEpoch, d.AttemptJournalEpoch = 9, 9
		if err := d.Validate(); err != nil {
			t.Errorf("%s: an equal-grant disposition was refused: %v", k, err)
		}
	}
}

// TestRecoveryClosureNeedsAStrictlyLaterGrant is the other half of the same rule.
// not_applied is authored by a STRICTLY later grant; equal or lower is refused. The
// equal case is the one that matters: it is exactly what an in-place "close my own
// attempt" bug produces, and the released verifier refuses it with
// author_journal_epoch.
func TestRecoveryClosureNeedsAStrictlyLaterGrant(t *testing.T) {
	t.Parallel()
	for name, epochs := range map[string][2]uint64{
		"equal":  {9, 9},
		"lower":  {8, 9},
		"zeroed": {0, 9},
	} {
		d := validDisposition()
		d.Disposition = runtimecommand.DispositionNotApplied
		d.LeaseEpoch, d.AttemptJournalEpoch = epochs[0], epochs[1]
		if err := d.Validate(); err == nil {
			t.Errorf("%s: author grant %d over attempt grant %d was accepted, want an error", name, epochs[0], epochs[1])
		}
	}
	d := validDisposition()
	d.Disposition = runtimecommand.DispositionNotApplied
	d.LeaseEpoch, d.AttemptJournalEpoch = 10, 9
	if err := d.Validate(); err != nil {
		t.Errorf("a strictly later author grant was refused: %v", err)
	}
}

// TestAdmittedAttemptIDIsOptionalButValidatedWhenPresent is the compatibility
// contract. An admitted record with no attempt id is a LEGACY record and stays
// valid — the applier writes no disposition for it — while a malformed non-empty
// one is refused before any effect.
func TestAdmittedAttemptIDIsOptionalButValidatedWhenPresent(t *testing.T) {
	t.Parallel()
	legacy := runtimecommand.Admitted{
		CommandID:        "command-1",
		RuntimeCommandID: testUUID(3),
		Kind:             runtimecommand.KindInput,
		LeaseEpoch:       4,
		Blocks:           []content.Block{&content.TextBlock{Text: "hi"}},
	}
	if err := legacy.Validate(); err != nil {
		t.Fatalf("a legacy admitted record with no attempt id was refused: %v", err)
	}
	withAttempt := legacy
	withAttempt.AttemptID = "attempt-1"
	if err := withAttempt.Validate(); err != nil {
		t.Fatalf("a well-formed attempt id was refused: %v", err)
	}
	malformed := legacy
	malformed.AttemptID = runtimecommand.AttemptID(strings.Repeat("a", runtimecommand.MaxAttemptIDBytes+1))
	err := malformed.Validate()
	if err == nil {
		t.Fatalf("an over-length attempt id was accepted")
	}
	var invalid *runtimecommand.ValidationError
	if !errors.As(err, &invalid) || invalid.Field != "AttemptID" {
		t.Fatalf("error %v, want a *ValidationError on AttemptID", err)
	}
}

// TestDispositionForCopiesIdentitiesAndStampsTheHeldGrant pins the builder. Both
// epochs come from the grant the applier ACTUALLY held, never from the admitted
// record's own LeaseEpoch: a record admitted under a superseded epoch is refused
// before this is reached, but if the two ever diverged the record's claim would be
// the forged one the reader fails closed on.
func TestDispositionForCopiesIdentitiesAndStampsTheHeldGrant(t *testing.T) {
	t.Parallel()
	admitted := runtimecommand.Admitted{
		CommandID:        "command-1",
		RuntimeCommandID: testUUID(5),
		Kind:             runtimecommand.KindInterrupt,
		LeaseEpoch:       2,
		AttemptID:        "attempt-9",
	}
	got := admitted.DispositionFor(runtimecommand.DispositionNoOp, 7)
	want := runtimecommand.CommandDisposition{
		CommandID:           "command-1",
		RuntimeCommandID:    testUUID(5),
		Kind:                runtimecommand.KindInterrupt,
		LeaseEpoch:          7,
		AttemptID:           "attempt-9",
		AttemptJournalEpoch: 7,
		Disposition:         runtimecommand.DispositionNoOp,
	}
	if got != want {
		t.Fatalf("DispositionFor() = %+v, want %+v", got, want)
	}
}

// TestClosureValidateFailsClosedPerField covers the recovery request's own shape.
// It carries no author grant of its own — the closer stamps that from the live
// lease, which is the whole point of the capability — so the only fields here are
// the ones that name the attempt being closed.
func TestClosureValidateFailsClosedPerField(t *testing.T) {
	t.Parallel()
	valid := runtimecommand.Closure{
		CommandID:           "command-1",
		RuntimeCommandID:    testUUID(2),
		Kind:                runtimecommand.KindInput,
		AttemptID:           "attempt-1",
		AttemptJournalEpoch: 3,
	}
	if err := valid.Validate(); err != nil {
		t.Fatalf("the valid baseline failed: %v", err)
	}
	for field, mutate := range map[string]func(*runtimecommand.Closure){
		"CommandID":           func(c *runtimecommand.Closure) { c.CommandID = "" },
		"RuntimeCommandID":    func(c *runtimecommand.Closure) { c.RuntimeCommandID = uuid.UUID{} },
		"Kind":                func(c *runtimecommand.Closure) { c.Kind = "" },
		"AttemptID":           func(c *runtimecommand.Closure) { c.AttemptID = "" },
		"AttemptJournalEpoch": func(c *runtimecommand.Closure) { c.AttemptJournalEpoch = 0 },
	} {
		c := valid
		mutate(&c)
		err := c.Validate()
		if err == nil {
			t.Errorf("%s: Validate() = nil, want an error", field)
			continue
		}
		var invalid *runtimecommand.ValidationError
		if !errors.As(err, &invalid) {
			t.Errorf("%s: error %v is not a *ValidationError", field, err)
			continue
		}
		if invalid.Field != field {
			t.Errorf("%s: Field = %q, want %q", field, invalid.Field, field)
		}
	}
}
