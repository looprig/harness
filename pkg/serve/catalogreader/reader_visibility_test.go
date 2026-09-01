package catalogreader

import (
	"errors"
	"reflect"
	"testing"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/identity"
)

func readerVisibilityUUID(seed byte) uuid.UUID {
	var id uuid.UUID
	for index := range id {
		id[index] = seed
	}
	return id
}

func TestReconstructStatusSummaryVisibilityAndDecodeBoundary(t *testing.T) {
	t.Parallel()
	sid, loop, turn := readerVisibilityUUID(1), readerVisibilityUUID(2), readerVisibilityUUID(3)
	publicDone := event.TurnDone{Header: event.Header{
		Coordinates: identity.Coordinates{SessionID: sid, LoopID: loop, TurnID: turn},
		EventID:     readerVisibilityUUID(4),
	}}
	internalDone := publicDone
	internalDone.EventVisibility = event.Internal
	publicDoneWire, err := event.MarshalEvent(publicDone)
	if err != nil {
		t.Fatalf("MarshalEvent(public TurnDone) error = %v", err)
	}
	internalDoneWire, err := event.MarshalEvent(internalDone)
	if err != nil {
		t.Fatalf("MarshalEvent(internal TurnDone) error = %v", err)
	}
	publicFailedWire, err := event.MarshalEvent(event.TurnFailed{Header: event.Header{
		Coordinates: identity.Coordinates{SessionID: sid, LoopID: loop, TurnID: turn},
		EventID:     readerVisibilityUUID(5),
	}})
	if err != nil {
		t.Fatalf("MarshalEvent(public TurnFailed) error = %v", err)
	}
	tests := []struct {
		name        string
		wire        []byte
		wantType    string
		wantErr     bool
		wantPrivate bool
	}{
		{name: "public TurnDone accepted", wire: publicDoneWire, wantType: "TurnDone"},
		{name: "public TurnFailed accepted", wire: publicFailedWire, wantType: "TurnFailed"},
		{name: "internal allowed kind refused", wire: internalDoneWire, wantErr: true, wantPrivate: true},
		{name: "unknown visibility sanitized", wire: []byte(`{"type":"TurnDone","v":1,"session_id":"01010101-0101-0101-0101-010101010101","loop_id":"02020202-0202-0202-0202-020202020202","turn_id":"03030303-0303-0303-0303-030303030303","event_id":"04040404-0404-0404-0404-040404040404","visibility":99}`), wantErr: true},
		{name: "malformed event sanitized", wire: []byte(`{"type":"TurnDone"`), wantErr: true},
	}
	for _, tt := range tests {
		testCase := tt
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			got, reconstructErr := reconstructStatusSummary(sid, statusSummaryLastTurn, 1, 1, testCase.wire)
			if (reconstructErr != nil) != testCase.wantErr {
				t.Fatalf("reconstructStatusSummary() error = %v, wantErr %v", reconstructErr, testCase.wantErr)
			}
			if testCase.wantPrivate {
				var private *PrivateEventError
				if !errors.As(reconstructErr, &private) {
					t.Fatalf("error = %T %v, want PrivateEventError", reconstructErr, reconstructErr)
				}
			}
			if testCase.wantType != "" {
				if got == nil || reflect.TypeOf(got.Event).Name() != testCase.wantType {
					t.Errorf("event = %T, want %s", got.Event, testCase.wantType)
				}
			}
		})
	}
}
