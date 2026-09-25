package runtimecommand_test

import (
	"errors"
	"testing"

	"github.com/looprig/core/content"
	sessionwire "github.com/looprig/core/sessionwire/v1"
	"github.com/looprig/harness/pkg/gate"
	"github.com/looprig/harness/pkg/runtimecommand"
)

func attributedAdmitted(kind runtimecommand.Kind) runtimecommand.Admitted {
	a := runtimecommand.Admitted{
		CommandID: "cmd-1", RuntimeCommandID: testUUID(1), Kind: kind,
		LeaseEpoch: 1, AttemptID: "attempt-1",
	}
	switch kind {
	case runtimecommand.KindInput:
		a.Blocks = []content.Block{&content.TextBlock{Text: "hi"}}
	case runtimecommand.KindGateResponse:
		a.GateResponse = &gate.GateResponse{GateID: testUUID(2), Action: "approve"}
	}
	return a
}

func requireAttributionField(t *testing.T, err error, field string) {
	t.Helper()
	var validationErr *runtimecommand.ValidationError
	if !errors.As(err, &validationErr) || validationErr.Field != field {
		t.Fatalf("error = %v, want *ValidationError{Field: %q}", err, field)
	}
}

func TestAdmittedPrincipalAndMetadata(t *testing.T) {
	t.Parallel()
	kinds := []runtimecommand.Kind{
		runtimecommand.KindInput, runtimecommand.KindCreate, runtimecommand.KindInterrupt,
		runtimecommand.KindRestore, runtimecommand.KindGateResponse,
	}
	for _, kind := range kinds {
		t.Run(string(kind)+"/principal accepted", func(t *testing.T) {
			t.Parallel()
			a := attributedAdmitted(kind)
			a.Principal = &sessionwire.Principal{Tenant: "acme", Subject: "user_01", Kind: sessionwire.PrincipalKindActor}
			if err := a.Validate(); err != nil {
				t.Fatalf("Validate() = %v, want nil", err)
			}
			if a.Application() != attributedAdmitted(kind).Application() {
				t.Fatal("the bodiless application prefix must not gain attribution")
			}
		})
		t.Run(string(kind)+"/invalid principal", func(t *testing.T) {
			t.Parallel()
			a := attributedAdmitted(kind)
			a.Principal = &sessionwire.Principal{Tenant: "acme"}
			requireAttributionField(t, a.Validate(), "Principal")
		})
		t.Run(string(kind)+"/metadata", func(t *testing.T) {
			t.Parallel()
			a := attributedAdmitted(kind)
			a.Metadata = sessionwire.MessageMetadata{"space": "family"}
			err := a.Validate()
			if kind == runtimecommand.KindInput || kind == runtimecommand.KindCreate {
				if err != nil {
					t.Fatalf("Validate() = %v, want nil", err)
				}
			} else {
				requireAttributionField(t, err, "Metadata")
			}
		})
	}
	a := attributedAdmitted(runtimecommand.KindInput)
	a.Metadata = sessionwire.MessageMetadata{"looprig_x": "v"}
	requireAttributionField(t, a.Validate(), "Metadata")
}
