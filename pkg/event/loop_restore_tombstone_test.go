package event

import (
	"testing"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/identity"
)

func TestLoopRestoreTombstonedRoundTripsAndValidates(t *testing.T) {
	ev := LoopRestoreTombstoned{
		Header:   Header{Coordinates: identity.Coordinates{SessionID: uuid.UUID{1}, LoopID: uuid.UUID{2}}, EventID: uuid.UUID{3}},
		Category: LoopRestoreTombstoneRuntimeMismatch,
	}
	if err := ValidateEvent(ev); err != nil {
		t.Fatalf("ValidateEvent: %v", err)
	}
	wire, err := MarshalEvent(ev)
	if err != nil {
		t.Fatalf("MarshalEvent: %v", err)
	}
	decoded, err := UnmarshalEvent(wire)
	if err != nil {
		t.Fatalf("UnmarshalEvent: %v", err)
	}
	got, ok := decoded.(LoopRestoreTombstoned)
	if !ok || got != ev {
		t.Fatalf("decoded = %#v (%T), want %#v", decoded, decoded, ev)
	}
}

func TestLoopRestoreTombstoneCategoryIsClosed(t *testing.T) {
	ev := LoopRestoreTombstoned{
		Header:   Header{Coordinates: identity.Coordinates{SessionID: uuid.UUID{1}, LoopID: uuid.UUID{2}}, EventID: uuid.UUID{3}},
		Category: "untrusted",
	}
	if err := ValidateEvent(ev); err == nil {
		t.Fatal("ValidateEvent accepted unknown tombstone category")
	}
}
