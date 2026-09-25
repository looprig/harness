package journal

import (
	"fmt"
	"testing"
	"time"

	"github.com/looprig/core/content"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/command"
	"github.com/looprig/harness/pkg/identity"
)

func TestCommandRecordPhaseAwarePhysicalID(t *testing.T) {
	commandID := uuid.MustParse("01234567-89ab-cdef-0123-456789abcdef")
	loopID := uuid.MustParse("fedcba98-7654-3210-fedc-ba9876543210")
	base := command.UserInput{
		Header: command.Header{
			CommandID: commandID,
			Agency:    identity.AgencyMachine,
			CreatedAt: time.Unix(123, 456),
		},
		Blocks:       []content.Block{&content.TextBlock{Text: "payload"}},
		TargetLoopID: loopID,
	}
	intent := NewCommandRecord(commandID, loopID, withDeliveryPhase(base, command.DelegateDeliveryPhaseIntent))
	fallback := NewCommandRecord(commandID, loopID, withDeliveryPhase(base, command.DelegateDeliveryPhaseFallbackQueued))

	if got, want := intent.IdempotencyID(), commandID.String(); got != want {
		t.Fatalf("intent physical id = %q, want %q", got, want)
	}
	wantFallback := commandID.String() + "/fallback_queued"
	if got := fallback.IdempotencyID(); got != wantFallback {
		t.Fatalf("fallback physical id = %q, want %q", got, wantFallback)
	}
	if got := fallback.PhysicalID().String(); got != wantFallback {
		t.Fatalf("typed fallback physical id = %q, want %q", got, wantFallback)
	}
}

func TestCommandRecordNormalizedDeliveryFingerprintIgnoresPhase(t *testing.T) {
	commandID := uuid.MustParse("01234567-89ab-cdef-0123-456789abcdef")
	loopID := uuid.MustParse("fedcba98-7654-3210-fedc-ba9876543210")
	base := command.UserInput{
		Header:       command.Header{CommandID: commandID, Agency: identity.AgencyMachine, CreatedAt: time.Unix(123, 456)},
		Blocks:       []content.Block{&content.TextBlock{Text: "payload"}},
		TargetLoopID: loopID,
	}
	intent := NewCommandRecord(commandID, loopID, withDeliveryPhase(base, command.DelegateDeliveryPhaseIntent))
	fallback := NewCommandRecord(commandID, loopID, withDeliveryPhase(base, command.DelegateDeliveryPhaseFallbackQueued))
	changed := base
	changed.Blocks = []content.Block{&content.TextBlock{Text: "changed"}}
	changedRecord := NewCommandRecord(commandID, loopID, withDeliveryPhase(changed, command.DelegateDeliveryPhaseFallbackQueued))

	intentFP, err := intent.NormalizedDeliveryFingerprint()
	if err != nil {
		t.Fatalf("intent NormalizedDeliveryFingerprint: %v", err)
	}
	fallbackFP, err := fallback.NormalizedDeliveryFingerprint()
	if err != nil {
		t.Fatalf("fallback NormalizedDeliveryFingerprint: %v", err)
	}
	if intentFP != fallbackFP {
		t.Fatal("intent and fallback normalized fingerprints differ despite identical payload")
	}
	// This machine input carries no attribution. Its normalized durable bytes
	// must remain exactly those from before presenter support was added.
	const wantNormalizedDeliverySHA = "26b0d50567aff531bccf8f80b22d5d5745c80dc6b2f59cf8e81708eccab50519"
	if got := fmt.Sprintf("%x", intentFP.sum); got != wantNormalizedDeliverySHA {
		t.Fatalf("machine input fingerprint changed: %s, want %s", got, wantNormalizedDeliverySHA)
	}
	changedFP, err := changedRecord.NormalizedDeliveryFingerprint()
	if err != nil {
		t.Fatalf("changed NormalizedDeliveryFingerprint: %v", err)
	}
	if changedFP == intentFP {
		t.Fatal("changed payload normalized fingerprint unexpectedly matched")
	}
}

func withDeliveryPhase(input command.UserInput, phase command.DelegateDeliveryPhase) command.UserInput {
	input.DelegateDeliveryPhase = phase
	return input
}
