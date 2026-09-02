package runtimecommand_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/looprig/core/content"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/runtimecommand"
)

func testUUID(b byte) uuid.UUID {
	return uuid.UUID{b, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15}
}

// TestCommandIDValidateAcceptsOpaqueIdentities pins the contract that a public
// CommandID is an OPAQUE bounded UTF-8 string. The rows deliberately include ids
// that are not UUIDs at all; a validator that parsed them as UUIDs would reject
// every row but the last.
func TestCommandIDValidateAcceptsOpaqueIdentities(t *testing.T) {
	accepted := []runtimecommand.CommandID{
		"v1:AAECAwQFBgcICQoLDA0ODw",
		"command-1",
		"tenant/session/17",
		"héllo-cømmand",
		"0",
		runtimecommand.CommandID(strings.Repeat("x", runtimecommand.MaxCommandIDBytes)),
		"6ba7b810-9dad-11d1-80b4-00c04fd430c8",
	}
	if len(accepted) < 7 {
		t.Fatalf("guard consumes too few identities: %d", len(accepted))
	}
	for _, id := range accepted {
		if err := id.Validate(); err != nil {
			t.Errorf("Validate(%q) = %v, want nil", id, err)
		}
	}
}

// TestCommandIDValidateRejectsUnboundedOrIllFormed proves the bound and the
// well-formedness rules fail closed.
func TestCommandIDValidateRejectsUnboundedOrIllFormed(t *testing.T) {
	rejected := map[string]runtimecommand.CommandID{
		"empty":          "",
		"too long":       runtimecommand.CommandID(strings.Repeat("x", runtimecommand.MaxCommandIDBytes+1)),
		"invalid utf8":   runtimecommand.CommandID([]byte{0x66, 0xff, 0x66}),
		"c0 control":     "a\x00b",
		"newline":        "a\nb",
		"del":            "a\x7fb",
		"leading spaces": " a",
	}
	if len(rejected) < 7 {
		t.Fatalf("guard consumes too few identities: %d", len(rejected))
	}
	for name, id := range rejected {
		err := id.Validate()
		if err == nil {
			t.Errorf("Validate(%s) = nil, want error", name)
			continue
		}
		var verr *runtimecommand.ValidationError
		if !errors.As(err, &verr) {
			t.Errorf("Validate(%s) error = %T, want *runtimecommand.ValidationError", name, err)
		}
	}
}

// TestAdmittedValidateFailsClosed covers every admitted-record rule. An admitted
// record reaching Harness has already been admitted by Host, but Harness still
// refuses one it cannot apply without inventing an identity.
func TestAdmittedValidateFailsClosed(t *testing.T) {
	blocks := []content.Block{&content.TextBlock{Text: "hi"}}
	valid := runtimecommand.Admitted{
		CommandID:        "command-1",
		RuntimeCommandID: testUUID(0x11),
		Kind:             runtimecommand.KindInput,
		LeaseEpoch:       3,
		Blocks:           blocks,
	}
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid input Validate: %v", err)
	}
	validInterrupt := runtimecommand.Admitted{
		CommandID:        "command-2",
		RuntimeCommandID: testUUID(0x12),
		Kind:             runtimecommand.KindInterrupt,
		LeaseEpoch:       3,
	}
	if err := validInterrupt.Validate(); err != nil {
		t.Fatalf("valid interrupt Validate: %v", err)
	}

	mutate := map[string]func(*runtimecommand.Admitted){
		"empty command id":     func(a *runtimecommand.Admitted) { a.CommandID = "" },
		"zero runtime id":      func(a *runtimecommand.Admitted) { a.RuntimeCommandID = uuid.UUID{} },
		"unknown kind":         func(a *runtimecommand.Admitted) { a.Kind = "reboot" },
		"empty kind":           func(a *runtimecommand.Admitted) { a.Kind = "" },
		"input without blocks": func(a *runtimecommand.Admitted) { a.Blocks = nil },
		"input with nil block": func(a *runtimecommand.Admitted) { a.Blocks = []content.Block{nil} },
		"zero lease epoch":     func(a *runtimecommand.Admitted) { a.LeaseEpoch = 0 },
		"interrupt carrying kind": func(a *runtimecommand.Admitted) {
			a.Kind = runtimecommand.KindInterrupt
		},
	}
	if len(mutate) < 8 {
		t.Fatalf("guard consumes too few mutations: %d", len(mutate))
	}
	for name, apply := range mutate {
		bad := valid
		bad.Blocks = append([]content.Block(nil), blocks...)
		apply(&bad)
		if name == "interrupt carrying kind" {
			// An interrupt that still carries input blocks is a malformed record:
			// Harness would silently drop the payload.
			if err := bad.Validate(); err == nil {
				t.Errorf("Validate(%s) = nil, want error", name)
			}
			continue
		}
		if err := bad.Validate(); err == nil {
			t.Errorf("Validate(%s) = nil, want error", name)
		}
	}
}

// TestApplicationIsTheDurableCorrelation pins the three fields the private
// application prefix must carry, and that it is derived from the admitted record
// rather than re-minted.
func TestApplicationIsTheDurableCorrelation(t *testing.T) {
	adm := runtimecommand.Admitted{
		CommandID:        "command-1",
		RuntimeCommandID: testUUID(0x21),
		Kind:             runtimecommand.KindInput,
		LeaseEpoch:       9,
		Blocks:           []content.Block{&content.TextBlock{Text: "hi"}},
	}
	app := adm.Application()
	if app.CommandID != adm.CommandID {
		t.Errorf("Application().CommandID = %q, want %q", app.CommandID, adm.CommandID)
	}
	if app.RuntimeCommandID != adm.RuntimeCommandID {
		t.Errorf("Application().RuntimeCommandID = %v, want %v", app.RuntimeCommandID, adm.RuntimeCommandID)
	}
	if app.LeaseEpoch != adm.LeaseEpoch {
		t.Errorf("Application().LeaseEpoch = %d, want %d", app.LeaseEpoch, adm.LeaseEpoch)
	}
}

// TestErrorsCarryBothIdentities proves the typed refusals name the public id and
// the runtime id rather than collapsing them.
func TestErrorsCarryBothIdentities(t *testing.T) {
	conflict := &runtimecommand.MappingConflictError{
		CommandID:        "command-1",
		RuntimeCommandID: testUUID(0x31),
		DurableRuntimeID: testUUID(0x32),
		Sequence:         7,
	}
	msg := conflict.Error()
	for _, want := range []string{"command-1", testUUID(0x31).String(), testUUID(0x32).String()} {
		if !strings.Contains(msg, want) {
			t.Errorf("MappingConflictError.Error() = %q, missing %q", msg, want)
		}
	}
	stale := &runtimecommand.StaleLeaseEpochError{CommandID: "command-1", Admitted: 2, Current: 5}
	if !strings.Contains(stale.Error(), "command-1") {
		t.Errorf("StaleLeaseEpochError.Error() = %q, missing command id", stale.Error())
	}
}
