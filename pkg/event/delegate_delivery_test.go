package event_test

import (
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/identity"
)

func TestDelegateDeliveryStateChangedRoundTripsEachDurableResolutionState(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		state event.DelegateDeliveryState
	}{
		{"steer attempt reserved", event.DelegateDeliverySteerAttemptReserved},
		{"resolved unknown", event.DelegateDeliveryResolvedUnknown},
		{"resolved untrackable", event.DelegateDeliveryResolvedUntrackable},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			in := event.DelegateDeliveryStateChanged{
				Header: event.Header{
					Coordinates: identity.Coordinates{SessionID: uuid.UUID{1}},
					EventID:     uuid.UUID{4},
				},
				RequestID:    uuid.UUID{2},
				TargetLoopID: uuid.UUID{3},
				State:        tt.state,
			}

			if err := event.ValidateEvent(in); err != nil {
				t.Fatalf("ValidateEvent() = %v, want nil", err)
			}
			if got := in.Class(); got != event.Enduring {
				t.Fatalf("Class() = %v, want Enduring", got)
			}

			data, err := event.MarshalEvent(in)
			if err != nil {
				t.Fatalf("MarshalEvent() = %v", err)
			}
			var wire map[string]json.RawMessage
			if err := json.Unmarshal(data, &wire); err != nil {
				t.Fatalf("wire JSON: %v", err)
			}
			for _, forbidden := range []string{"blocks", "user_input", "broker_token", "origin_session_id", "origin_loop_id"} {
				if _, ok := wire[forbidden]; ok {
					t.Fatalf("wire unexpectedly carries forbidden field %q: %s", forbidden, data)
				}
			}

			out, err := event.UnmarshalEvent(data)
			if err != nil {
				t.Fatalf("UnmarshalEvent() = %v\nwire: %s", err, data)
			}
			if !reflect.DeepEqual(out, in) {
				t.Fatalf("round-trip mismatch:\n got = %#v\nwant = %#v\nwire: %s", out, in, data)
			}
		})
	}
}

func TestDelegateDeliveryStateChangedRequiresDurableAddressing(t *testing.T) {
	t.Parallel()

	base := event.DelegateDeliveryStateChanged{
		Header: event.Header{
			Coordinates: identity.Coordinates{SessionID: uuid.UUID{1}},
			EventID:     uuid.UUID{4},
		},
		RequestID:    uuid.UUID{2},
		TargetLoopID: uuid.UUID{3},
		State:        event.DelegateDeliverySteerAttemptReserved,
	}
	tests := []struct {
		name   string
		mutate func(*event.DelegateDeliveryStateChanged)
		field  event.FieldName
	}{
		{"request id", func(v *event.DelegateDeliveryStateChanged) { v.RequestID = uuid.UUID{} }, event.FieldRequestID},
		{"target loop id", func(v *event.DelegateDeliveryStateChanged) { v.TargetLoopID = uuid.UUID{} }, event.FieldTargetLoopID},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			value := base
			tt.mutate(&value)
			var invalid *event.InvalidEventError
			if err := event.ValidateEvent(value); !errors.As(err, &invalid) {
				t.Fatalf("ValidateEvent() error = %T (%v), want *InvalidEventError", err, err)
			} else if invalid.Field != tt.field || invalid.Rule != event.RuleRequired {
				t.Fatalf("InvalidEventError = %+v, want %s/required", invalid, tt.field)
			}
		})
	}
}

func TestDelegateDeliveryStateChangedRejectsUnknownState(t *testing.T) {
	t.Parallel()

	in := event.DelegateDeliveryStateChanged{
		Header: event.Header{
			Coordinates: identity.Coordinates{SessionID: uuid.UUID{1}},
			EventID:     uuid.UUID{4},
		},
		RequestID:    uuid.UUID{2},
		TargetLoopID: uuid.UUID{3},
		State:        event.DelegateDeliveryState("future"),
	}
	err := event.ValidateEvent(in)
	var invalid *event.InvalidEventError
	if !errors.As(err, &invalid) {
		t.Fatalf("ValidateEvent() error = %T (%v), want *InvalidEventError", err, err)
	}
	if invalid.Field != event.FieldState || invalid.Rule != event.RuleInvalid {
		t.Fatalf("InvalidEventError = %+v, want State/invalid", invalid)
	}
}
