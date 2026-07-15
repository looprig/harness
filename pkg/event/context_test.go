package event_test

import (
	"errors"
	"reflect"
	"testing"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/identity"
	"github.com/looprig/inference"
)

func validContextMeasurement() event.ContextMeasurement {
	return event.ContextMeasurement{
		Basis:              event.ContextBasis{Revision: 1, ThroughEventID: uuid.UUID{9}},
		Model:              inference.ModelKey{Provider: "provider", Model: "model"},
		RequestFingerprint: [32]byte{1}, InputTokens: 80, InputLimit: 100,
		Quality: inference.CountQualityExactProvider,
	}
}

func TestContextMeasurementValidate(t *testing.T) {
	t.Parallel()
	valid := validContextMeasurement()
	tests := []struct {
		name        string
		measurement event.ContextMeasurement
		wantErr     bool
	}{
		{name: "valid", measurement: valid},
		{name: "over limit remains auditable", measurement: func() event.ContextMeasurement { value := valid; value.InputTokens = 101; return value }()},
		{name: "zero revision", measurement: func() event.ContextMeasurement { value := valid; value.Basis.Revision = 0; return value }(), wantErr: true},
		{name: "zero through event", measurement: func() event.ContextMeasurement {
			value := valid
			value.Basis.ThroughEventID = uuid.UUID{}
			return value
		}(), wantErr: true},
		{name: "zero model", measurement: func() event.ContextMeasurement { value := valid; value.Model = inference.ModelKey{}; return value }(), wantErr: true},
		{name: "zero fingerprint", measurement: func() event.ContextMeasurement { value := valid; value.RequestFingerprint = [32]byte{}; return value }(), wantErr: true},
		{name: "zero limit", measurement: func() event.ContextMeasurement { value := valid; value.InputLimit = 0; return value }(), wantErr: true},
		{name: "unknown quality", measurement: func() event.ContextMeasurement {
			value := valid
			value.Quality = inference.CountQualityUnknown
			return value
		}(), wantErr: true},
		{name: "out of range quality", measurement: func() event.ContextMeasurement {
			value := valid
			value.Quality = inference.CountQuality(255)
			return value
		}(), wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := tt.measurement.Validate()
			if (err != nil) != tt.wantErr {
				t.Fatalf("Validate() error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				var target *event.ContextValidationError
				if !errors.As(err, &target) {
					t.Fatalf("error = %T, want *event.ContextValidationError", err)
				}
			}
		})
	}
}

func TestContextEventsContractsAndCodec(t *testing.T) {
	t.Parallel()
	header := event.Header{Coordinates: identity.Coordinates{SessionID: uuid.UUID{1}, LoopID: uuid.UUID{2}}, EventID: uuid.UUID{3}}
	measurement := validContextMeasurement()
	tests := []struct {
		name        string
		ev          event.Event
		class       event.Class
		persistable bool
	}{
		{name: "measured enduring public", ev: event.ContextMeasured{Header: header, Measurement: measurement}, class: event.Enduring, persistable: true},
		{name: "pressure ephemeral public", ev: event.ContextPressure{Header: header, Measurement: measurement, Occupancy: 8_000, Previous: event.PressureNormal, Current: event.PressureCompact}, class: event.Ephemeral},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if tt.ev.Class() != tt.class || tt.ev.Scope() != event.ScopeLoop || tt.ev.Visibility() != event.Public || tt.ev.EndsTurn() {
				t.Fatalf("contract = class:%v scope:%v visibility:%v terminal:%v", tt.ev.Class(), tt.ev.Scope(), tt.ev.Visibility(), tt.ev.EndsTurn())
			}
			if err := event.ValidateEvent(tt.ev); err != nil {
				t.Fatalf("ValidateEvent() error = %v", err)
			}
			encoded, err := event.MarshalEvent(tt.ev)
			if !tt.persistable {
				var target *event.EphemeralNotPersistableError
				if !errors.As(err, &target) {
					t.Fatalf("MarshalEvent() error = %T %v", err, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("MarshalEvent() error = %v", err)
			}
			decoded, err := event.UnmarshalEvent(encoded)
			if err != nil || !reflect.DeepEqual(decoded, tt.ev) {
				t.Fatalf("round trip = %#v, %v; want %#v", decoded, err, tt.ev)
			}
		})
	}
}

func TestContextPressureValidate(t *testing.T) {
	t.Parallel()
	header := event.Header{Coordinates: identity.Coordinates{SessionID: uuid.UUID{1}, LoopID: uuid.UUID{2}}, EventID: uuid.UUID{3}}
	valid := event.ContextPressure{Header: header, Measurement: validContextMeasurement(), Occupancy: 8_000, Previous: event.PressureNormal, Current: event.PressureCompact}
	tests := []struct {
		name    string
		mutate  func(*event.ContextPressure)
		wantErr bool
	}{
		{name: "valid", mutate: func(*event.ContextPressure) {}},
		{name: "occupancy above display scale", mutate: func(value *event.ContextPressure) { value.Occupancy = event.FullScaleBasisPoints + 1 }, wantErr: true},
		{name: "unknown previous allowed for first transition", mutate: func(value *event.ContextPressure) { value.Previous = event.PressureUnknown }},
		{name: "unknown current", mutate: func(value *event.ContextPressure) { value.Current = event.PressureUnknown }, wantErr: true},
		{name: "same level", mutate: func(value *event.ContextPressure) { value.Current = value.Previous }, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			value := valid
			tt.mutate(&value)
			err := event.ValidateEvent(value)
			if (err != nil) != tt.wantErr {
				t.Fatalf("ValidateEvent() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestContextEventsRejectInternalVisibility(t *testing.T) {
	t.Parallel()
	header := event.Header{
		Coordinates:     identity.Coordinates{SessionID: uuid.UUID{1}, LoopID: uuid.UUID{2}},
		EventID:         uuid.UUID{3},
		EventVisibility: event.Internal,
	}
	measurement := validContextMeasurement()
	tests := []struct {
		name string
		ev   event.Event
	}{
		{name: "measured", ev: event.ContextMeasured{Header: header, Measurement: measurement}},
		{name: "pressure", ev: event.ContextPressure{Header: header, Measurement: measurement, Occupancy: 8_000, Previous: event.PressureNormal, Current: event.PressureCompact}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := event.ValidateEvent(tt.ev)
			var target *event.InvalidEventError
			if !errors.As(err, &target) || target.Field != event.FieldVisibility || target.Rule != event.RuleInvalid {
				t.Fatalf("ValidateEvent() error = %T %v", err, err)
			}
		})
	}
}

func FuzzContextMeasuredEvent(f *testing.F) {
	measurement := validContextMeasurement()
	header := event.Header{Coordinates: identity.Coordinates{SessionID: uuid.UUID{1}, LoopID: uuid.UUID{2}}, EventID: uuid.UUID{3}}
	seed, err := event.MarshalEvent(event.ContextMeasured{Header: header, Measurement: measurement})
	if err != nil {
		f.Fatal(err)
	}
	f.Add(seed)
	f.Fuzz(func(t *testing.T, data []byte) {
		decoded, err := event.UnmarshalEvent(data)
		if err != nil {
			return
		}
		measured, ok := decoded.(event.ContextMeasured)
		if !ok {
			return
		}
		if measured.Measurement.Validate() != nil {
			t.Fatal("decoder accepted invalid context measurement")
		}
	})
}
