package catalogreader

import (
	"errors"
	"testing"
	"time"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/hustle"
	"github.com/looprig/harness/pkg/identity"
)

func readerVisibilityUUID(seed byte) uuid.UUID {
	var id uuid.UUID
	for index := range id {
		id[index] = seed
	}
	return id
}

func readerInternalEvent(t *testing.T) event.HustleStarted {
	t.Helper()
	definition, err := hustle.Define(
		hustle.WithName("private.status"),
		hustle.WithParticipation(hustle.ParticipationBackground),
		hustle.WithTimeout(time.Second),
		hustle.WithLimits(hustle.Limits{InputBytes: 1, OutputBytes: 1}),
		hustle.WithCurrentLoopModel(),
		hustle.WithSystemPrompt("secret", "p1"),
		hustle.WithPolicyRevision("v1"),
	)
	if err != nil {
		t.Fatalf("hustle.Define() error = %v", err)
	}
	return event.HustleStarted{
		Header: event.Header{
			Coordinates:     identity.Coordinates{SessionID: readerVisibilityUUID(1)},
			EventID:         readerVisibilityUUID(2),
			EventVisibility: event.Internal,
		},
		Run: event.HustleRunDescriptor{Definition: definition.Descriptor(), RunID: hustle.RunID(readerVisibilityUUID(3))},
	}
}

func TestReconstructRefusesNonPublicEvents(t *testing.T) {
	t.Parallel()
	internalWire, err := event.MarshalEvent(readerInternalEvent(t))
	if err != nil {
		t.Fatalf("MarshalEvent() error = %v", err)
	}
	publicWire, err := event.MarshalEvent(event.SessionStarted{Header: event.Header{
		Coordinates: identity.Coordinates{SessionID: readerVisibilityUUID(1)},
		EventID:     readerVisibilityUUID(4),
	}})
	if err != nil {
		t.Fatalf("MarshalEvent(public) error = %v", err)
	}
	tests := []struct {
		name    string
		wire    []byte
		wantErr bool
	}{
		{name: "public accepted", wire: publicWire},
		{name: "internal refused", wire: internalWire, wantErr: true},
		{name: "unknown visibility refused", wire: []byte(`{"type":"SessionStarted","v":1,"session_id":"01010101-0101-0101-0101-010101010101","event_id":"04040404-0404-0404-0404-040404040404","visibility":99}`), wantErr: true},
	}
	for _, tt := range tests {
		testCase := tt
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			_, reconstructErr := reconstruct(1, testCase.wire)
			if (reconstructErr != nil) != testCase.wantErr {
				t.Fatalf("reconstruct() error = %v, wantErr %v", reconstructErr, testCase.wantErr)
			}
			if testCase.name == "internal refused" {
				var private *PrivateEventError
				if !errors.As(reconstructErr, &private) {
					t.Fatalf("error = %T %v, want PrivateEventError", reconstructErr, reconstructErr)
				}
			}
		})
	}
}
