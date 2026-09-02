package runtimecommand_test

import (
	"errors"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/looprig/core/content"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/runtimecommand"
)

func testUUID(b byte) uuid.UUID {
	return uuid.UUID{b, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15}
}

// TestCommandIDValidityIsExactlyCoreRule is the ORACLE for the opacity rule. The
// rule is restated here in primitives — non-empty, at most MaxCommandIDBytes bytes,
// valid UTF-8 — and checked against Validate over a corpus, so a rule ADDED to
// Validate is caught without anyone remembering to add a row for it. A table of
// accepted/rejected literals generalises no further than its rows and would have
// happily accommodated an extra whitespace or control-character rule.
func TestCommandIDValidityIsExactlyCoreRule(t *testing.T) {
	t.Parallel()
	corpus := []runtimecommand.CommandID{
		"v1:AAECAwQFBgcICQoLDA0ODw",
		"command-1",
		"tenant/session/17",
		"héllo-cømmand",
		"0",
		"6ba7b810-9dad-11d1-80b4-00c04fd430c8",
		// The shapes an over-strict applier would strand. Every one of these is a
		// value the admission authority accepts, so every one must apply.
		" x",
		"x ",
		"   ",
		"a\tb",
		"a\nb",
		"a\x00b",
		"a\x7fb",
		"a\u0085b",
		"\u200bzero-width",
		// Boundaries.
		runtimecommand.CommandID(strings.Repeat("x", runtimecommand.MaxCommandIDBytes)),
		runtimecommand.CommandID(strings.Repeat("x", runtimecommand.MaxCommandIDBytes+1)),
		// A multi-byte id whose RUNE count is under the limit but whose BYTE count is
		// over it: the bound is on encoded bytes, so this must be rejected.
		runtimecommand.CommandID(strings.Repeat("é", runtimecommand.MaxCommandIDBytes/2+1)),
		"",
		runtimecommand.CommandID([]byte{0x66, 0xff, 0x66}),
		runtimecommand.CommandID([]byte{0xff}),
	}
	if len(corpus) < 20 {
		t.Fatalf("oracle consumes too few identities: %d", len(corpus))
	}
	accepted, rejected := 0, 0
	for _, id := range corpus {
		want := id != "" && len(id) <= runtimecommand.MaxCommandIDBytes && utf8.ValidString(string(id))
		err := id.Validate()
		if want {
			accepted++
			if err != nil {
				t.Errorf("Validate(%q) = %v, want nil: Harness must not be stricter than the admission authority", id, err)
			}
			continue
		}
		rejected++
		if err == nil {
			t.Errorf("Validate(%q) = nil, want a refusal", id)
			continue
		}
		var verr *runtimecommand.ValidationError
		if !errors.As(err, &verr) {
			t.Errorf("Validate(%q) error = %T, want *runtimecommand.ValidationError", id, err)
		}
	}
	// Both arms of the oracle must have been exercised, or the walk proves nothing.
	if accepted < 14 || rejected < 5 {
		t.Fatalf("oracle exercised %d accepted / %d rejected, want both arms populated", accepted, rejected)
	}
}

// TestCommandIDMatchesCoreValidationRules is the named regression guard for the
// defect the oracle above generalises: Harness once rejected control characters and
// leading/trailing ASCII space. Core's sessionwire/v1 CommandID does not, so every
// one of these is an id Factory can durably admit — and an applier that refuses it
// leaves the command unapplicable until its apply deadline turns it into a rejection
// no one can explain from the admission side.
func TestCommandIDMatchesCoreValidationRules(t *testing.T) {
	t.Parallel()
	admissibleButOnceRefused := []runtimecommand.CommandID{
		" x", "x ", " x ", "a\tb", "a\nb", "a\rb", "a\x00b", "a\x1fb", "a\x7fb", "a\u009fb",
	}
	if len(admissibleButOnceRefused) < 10 {
		t.Fatalf("guard consumes too few identities: %d", len(admissibleButOnceRefused))
	}
	for _, id := range admissibleButOnceRefused {
		if err := id.Validate(); err != nil {
			t.Errorf("Validate(%q) = %v; Core accepts this id, so Harness must apply it", id, err)
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
