package runtimecommand_test

import (
	"errors"
	"testing"

	"github.com/looprig/core/content"
	"github.com/looprig/harness/pkg/gate"
	"github.com/looprig/harness/pkg/runtimecommand"
)

// TestGateResponseKindSpellingIsFactorysDurableByte pins the third kind's spelling.
// It is not a label: the kind is written verbatim into the application prefix and the
// disposition frame, and the released evidence reader compares it BYTE FOR BYTE
// against the kind Factory admitted (factory/internal/command/kind.go spells it
// "gate_response"). A disagreeing spelling would make every gate answer's evidence a
// conflict rather than a settlement.
func TestGateResponseKindSpellingIsFactorysDurableByte(t *testing.T) {
	t.Parallel()
	if runtimecommand.KindGateResponse != "gate_response" {
		t.Fatalf("KindGateResponse = %q, want the admitted spelling %q", runtimecommand.KindGateResponse, "gate_response")
	}
	for _, k := range []runtimecommand.Kind{
		runtimecommand.KindInput, runtimecommand.KindInterrupt, runtimecommand.KindGateResponse,
	} {
		if !k.Valid() {
			t.Errorf("Kind(%q).Valid() = false, want true", k)
		}
	}
	for _, k := range []runtimecommand.Kind{
		"", "gate-response", "gateresponse", "Gate_Response", "gate_response ", "restore", "create",
	} {
		if k.Valid() {
			t.Errorf("Kind(%q).Valid() = true, want false", k)
		}
	}
}

func validGateResponse() *gate.GateResponse {
	return &gate.GateResponse{
		GateID: gate.ID(testUUID(0x51)),
		Action: string(gate.ApprovalApprove),
		Source: gate.ResponseSource{Kind: gate.ResponseFromUser},
	}
}

func admittedGateResponse() runtimecommand.Admitted {
	return runtimecommand.Admitted{
		CommandID:        "command-gate",
		RuntimeCommandID: testUUID(0x52),
		Kind:             runtimecommand.KindGateResponse,
		LeaseEpoch:       4,
		AttemptID:        "attempt/gate",
		GateResponse:     validGateResponse(),
	}
}

// TestAdmittedGateResponseIsRequiredIffTheKindIsGateResponse is the payload guard in
// both directions. A gate_response with no response has nothing to apply, and a
// response riding any other kind would be silently dropped — the same reason Blocks
// is forbidden on every kind but input.
func TestAdmittedGateResponseIsRequiredIffTheKindIsGateResponse(t *testing.T) {
	t.Parallel()
	if err := admittedGateResponse().Validate(); err != nil {
		t.Fatalf("valid gate_response Validate: %v", err)
	}

	rows := map[string]struct {
		mutate func(*runtimecommand.Admitted)
		field  string
	}{
		"gate_response without a response": {
			mutate: func(a *runtimecommand.Admitted) { a.GateResponse = nil },
			field:  "GateResponse",
		},
		"gate_response naming no gate": {
			mutate: func(a *runtimecommand.Admitted) { a.GateResponse.GateID = gate.ID{} },
			field:  "GateResponse",
		},
		// The empty-attempt arm is for LEGACY input/interrupt records only; a
		// gate_response with no attempt would apply and write no evidence.
		"gate_response without an attempt": {
			mutate: func(a *runtimecommand.Admitted) { a.AttemptID = "" },
			field:  "AttemptID",
		},
		"gate_response carrying input blocks": {
			mutate: func(a *runtimecommand.Admitted) { a.Blocks = []content.Block{&content.TextBlock{Text: "x"}} },
			field:  "Blocks",
		},
		"input carrying a gate response": {
			mutate: func(a *runtimecommand.Admitted) {
				a.Kind = runtimecommand.KindInput
				a.Blocks = []content.Block{&content.TextBlock{Text: "x"}}
			},
			field: "GateResponse",
		},
		"interrupt carrying a gate response": {
			mutate: func(a *runtimecommand.Admitted) { a.Kind = runtimecommand.KindInterrupt },
			field:  "GateResponse",
		},
	}
	for name, row := range rows {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			bad := admittedGateResponse()
			apply := row.mutate
			apply(&bad)
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

// TestGateResponseKindIsAcceptedEverywhereTheKindIsCarried walks every other value
// that carries a Kind. The prefix, the disposition and the closure are all decoded
// from storage and validated on the way in; a kind the admitted record accepts but a
// durable record refuses is a journal the applier can write and then never reopen.
func TestGateResponseKindIsAcceptedEverywhereTheKindIsCarried(t *testing.T) {
	t.Parallel()
	adm := admittedGateResponse()
	adm.AttemptID = "attempt/gate"
	if err := adm.Application().Validate(); err != nil {
		t.Errorf("Application.Validate: %v", err)
	}
	if got := adm.Application().Kind; got != runtimecommand.KindGateResponse {
		t.Errorf("Application.Kind = %q, want gate_response", got)
	}
	for _, d := range []runtimecommand.DispositionKind{
		runtimecommand.DispositionApplied, runtimecommand.DispositionNoOp, runtimecommand.DispositionRefused,
	} {
		if err := adm.DispositionFor(d, 4).Validate(); err != nil {
			t.Errorf("DispositionFor(%s).Validate: %v", d, err)
		}
	}
	closure := runtimecommand.Closure{
		CommandID: adm.CommandID, RuntimeCommandID: adm.RuntimeCommandID,
		Kind: runtimecommand.KindGateResponse, AttemptID: adm.AttemptID, AttemptJournalEpoch: 4,
	}
	if err := closure.Validate(); err != nil {
		t.Errorf("Closure.Validate: %v", err)
	}
}
