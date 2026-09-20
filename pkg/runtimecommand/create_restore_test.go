package runtimecommand_test

import (
	"errors"
	"testing"

	"github.com/looprig/core/content"
	"github.com/looprig/harness/pkg/runtimecommand"
)

// TestCreateAndRestoreKindSpellingsAreFactorysDurableBytes pins the fourth and fifth
// kinds' spellings. They are not labels: the kind is written verbatim into the
// application prefix and the disposition frame, and the released evidence reader
// compares it BYTE FOR BYTE against the kind Factory admitted
// (factory/internal/command/kind.go spells them "create" and "restore"). A
// disagreeing spelling would make every create's evidence a conflict rather than a
// settlement — which is exactly the defect this release fixes, in a new place.
func TestCreateAndRestoreKindSpellingsAreFactorysDurableBytes(t *testing.T) {
	t.Parallel()
	for spelling, k := range map[string]runtimecommand.Kind{
		"create":  runtimecommand.KindCreate,
		"restore": runtimecommand.KindRestore,
	} {
		if string(k) != spelling {
			t.Errorf("Kind %v spells %q, want the admitted spelling %q", k, string(k), spelling)
		}
	}
}

// TestKindVocabularyIsTheAdmittedFive walks the whole vocabulary in both directions.
// Factory admits five kinds; a closed set of three here is what left a create
// unsettleable forever.
func TestKindVocabularyIsTheAdmittedFive(t *testing.T) {
	t.Parallel()
	for _, k := range []runtimecommand.Kind{
		runtimecommand.KindInput,
		runtimecommand.KindInterrupt,
		runtimecommand.KindGateResponse,
		runtimecommand.KindCreate,
		runtimecommand.KindRestore,
	} {
		if !k.Valid() {
			t.Errorf("Kind(%q).Valid() = false, want true", k)
		}
	}
	for _, k := range []runtimecommand.Kind{
		"", "Create", "create ", " create", "creates", "create_session",
		"Restore", "restore ", "restored", "reboot",
	} {
		if k.Valid() {
			t.Errorf("Kind(%q).Valid() = true, want false", k)
		}
	}
}

func admittedCreate() runtimecommand.Admitted {
	return runtimecommand.Admitted{
		CommandID:        "command-create",
		RuntimeCommandID: testUUID(0x61),
		Kind:             runtimecommand.KindCreate,
		LeaseEpoch:       6,
		AttemptID:        "attempt/create",
		Blocks:           []content.Block{&content.TextBlock{Text: "first message"}},
	}
}

func admittedRestore() runtimecommand.Admitted {
	return runtimecommand.Admitted{
		CommandID:        "command-restore",
		RuntimeCommandID: testUUID(0x62),
		Kind:             runtimecommand.KindRestore,
		LeaseEpoch:       6,
		AttemptID:        "attempt/restore",
	}
}

// TestCreateCarriesOptionalBlocksAndRestoreCarriesNone is the payload rule for the
// two new kinds, in both directions.
//
// A create MAY carry the first message — Factory admits the session's opening input
// as part of the create record — so the blocks are OPTIONAL rather than required or
// forbidden. A restore resumes a session that already has its conversation, so a
// payload riding one would be silently dropped, which is the same reason every kind
// but input has forbidden them since v0.33.0.
func TestCreateCarriesOptionalBlocksAndRestoreCarriesNone(t *testing.T) {
	t.Parallel()
	if err := admittedCreate().Validate(); err != nil {
		t.Fatalf("a create carrying blocks was refused: %v", err)
	}
	bare := admittedCreate()
	bare.Blocks = nil
	if err := bare.Validate(); err != nil {
		t.Fatalf("a create carrying no blocks was refused: %v", err)
	}
	if err := admittedRestore().Validate(); err != nil {
		t.Fatalf("a restore was refused: %v", err)
	}

	rows := map[string]struct {
		base   runtimecommand.Admitted
		mutate func(*runtimecommand.Admitted)
		field  string
	}{
		"restore carrying blocks": {
			base:   admittedRestore(),
			mutate: func(a *runtimecommand.Admitted) { a.Blocks = []content.Block{&content.TextBlock{Text: "x"}} },
			field:  "Blocks",
		},
		"create carrying a nil block": {
			base:   admittedCreate(),
			mutate: func(a *runtimecommand.Admitted) { a.Blocks = []content.Block{nil} },
			field:  "Blocks",
		},
		// A gate response riding either kind would be silently dropped, exactly as
		// it would on an input or an interrupt.
		"create carrying a gate response": {
			base:   admittedCreate(),
			mutate: func(a *runtimecommand.Admitted) { a.GateResponse = validGateResponse() },
			field:  "GateResponse",
		},
		"restore carrying a gate response": {
			base:   admittedRestore(),
			mutate: func(a *runtimecommand.Admitted) { a.GateResponse = validGateResponse() },
			field:  "GateResponse",
		},
		"create under a zero lease epoch": {
			base:   admittedCreate(),
			mutate: func(a *runtimecommand.Admitted) { a.LeaseEpoch = 0 },
			field:  "LeaseEpoch",
		},
		"restore with an over-long attempt id": {
			base:   admittedRestore(),
			mutate: func(a *runtimecommand.Admitted) { a.AttemptID = runtimecommand.AttemptID(longID(257)) },
			field:  "AttemptID",
		},
	}
	for name, row := range rows {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			bad := row.base
			row.mutate(&bad)
			err := bad.Validate()
			var verr *runtimecommand.ValidationError
			if !errors.As(err, &verr) {
				t.Fatalf("Validate() = %v, want *runtimecommand.ValidationError", err)
			}
			if verr.Field != row.field {
				t.Errorf("refused field = %q, want %q (%v)", verr.Field, row.field, err)
			}
		})
	}
}

// TestCreateAndRestoreAttemptIDIsOptional puts the two new kinds on the SAME footing
// as input and interrupt rather than on gate_response's. gate_response requires an
// attempt because it has no legacy records; create and restore reach this seam only
// from a Host that dispatches under an attempt, but the rule that enforces it lives
// in Host's admission, not here — a record with no attempt writes no disposition and
// is a legacy shape, not a malformed one.
func TestCreateAndRestoreAttemptIDIsOptional(t *testing.T) {
	t.Parallel()
	for name, base := range map[string]runtimecommand.Admitted{
		"create":  admittedCreate(),
		"restore": admittedRestore(),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			a := base
			a.AttemptID = ""
			if err := a.Validate(); err != nil {
				t.Fatalf("%s with no attempt id was refused: %v", name, err)
			}
		})
	}
}

// TestCreateAndRestoreAreAcceptedEverywhereTheKindIsCarried walks every other value
// that carries a Kind, exactly as the gate_response test does. The prefix, the
// disposition and the closure are all decoded from storage and validated on the way
// in; a kind the admitted record accepts but a durable record refuses is a journal
// the applier can write and then never reopen — and a closure that refuses the kind
// is a command no successor can ever close, which is half of the defect this release
// fixes.
func TestCreateAndRestoreAreAcceptedEverywhereTheKindIsCarried(t *testing.T) {
	t.Parallel()
	for name, adm := range map[string]runtimecommand.Admitted{
		"create":  admittedCreate(),
		"restore": admittedRestore(),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if err := adm.Application().Validate(); err != nil {
				t.Errorf("Application.Validate: %v", err)
			}
			if got, want := adm.Application().Kind, adm.Kind; got != want {
				t.Errorf("Application.Kind = %q, want %q", got, want)
			}
			for _, d := range []runtimecommand.DispositionKind{
				runtimecommand.DispositionApplied,
				runtimecommand.DispositionNoOp,
				runtimecommand.DispositionRefused,
			} {
				if err := adm.DispositionFor(d, adm.LeaseEpoch).Validate(); err != nil {
					t.Errorf("DispositionFor(%s).Validate: %v", d, err)
				}
			}
			closure := runtimecommand.Closure{
				CommandID:           adm.CommandID,
				RuntimeCommandID:    adm.RuntimeCommandID,
				Kind:                adm.Kind,
				AttemptID:           "attempt/" + runtimecommand.AttemptID(adm.Kind),
				AttemptJournalEpoch: adm.LeaseEpoch,
			}
			if err := closure.Validate(); err != nil {
				t.Errorf("Closure.Validate: %v", err)
			}
		})
	}
}

func longID(n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = 'a'
	}
	return string(b)
}
