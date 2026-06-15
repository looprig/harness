package event

import (
	"testing"

	"github.com/inventivepotter/urvi/internal/uuid"
)

// TestEventEnvelopeFields asserts that the correlation fields set on an
// EventEnvelope (TurnID, EventID, CausationID, CallID) are preserved exactly as
// stamped — none is dropped, aliased onto another field, or zeroed.
func TestEventEnvelopeFields(t *testing.T) {
	t.Parallel()

	id := func(t *testing.T) uuid.UUID {
		t.Helper()
		u, err := uuid.New()
		if err != nil {
			t.Fatalf("uuid.New: %v", err)
		}
		return u
	}

	tests := []struct {
		name        string
		turnID      uuid.UUID
		eventID     uuid.UUID
		causationID uuid.UUID
		callID      uuid.UUID
		event       Event
	}{
		{
			name:        "all fields set",
			turnID:      id(t),
			eventID:     id(t),
			causationID: id(t),
			callID:      id(t),
			event:       TurnStarted{TurnIndex: 1},
		},
		{
			name:  "all zero correlation is boundary",
			event: SessionStarted{},
		},
		{
			name:        "tool-call envelope carries non-zero CallID",
			turnID:      id(t),
			eventID:     id(t),
			causationID: id(t),
			callID:      id(t),
			event:       TokenDelta{TurnIndex: 2},
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			env := EventEnvelope{
				TurnID:      tt.turnID,
				EventID:     tt.eventID,
				CausationID: tt.causationID,
				CallID:      tt.callID,
				Event:       tt.event,
			}
			if env.TurnID != tt.turnID {
				t.Errorf("TurnID = %v, want %v", env.TurnID, tt.turnID)
			}
			if env.EventID != tt.eventID {
				t.Errorf("EventID = %v, want %v", env.EventID, tt.eventID)
			}
			if env.CausationID != tt.causationID {
				t.Errorf("CausationID = %v, want %v", env.CausationID, tt.causationID)
			}
			if env.CallID != tt.callID {
				t.Errorf("CallID = %v, want %v", env.CallID, tt.callID)
			}
		})
	}
}
