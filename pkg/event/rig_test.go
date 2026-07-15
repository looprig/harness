package event_test

import (
	"errors"
	"reflect"
	"testing"

	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/identity"
	"github.com/looprig/inference"
)

func TestRigEventsRoundTrip(t *testing.T) {
	t.Parallel()
	sessionID, loopID, previousLoopID, activeLoopID := vID(t), vID(t), vID(t), vID(t)
	sessionHeader := event.Header{Coordinates: identity.Coordinates{SessionID: sessionID}, EventID: vID(t)}
	loopHeader := event.Header{Coordinates: identity.Coordinates{SessionID: sessionID, LoopID: loopID}, EventID: vID(t)}
	runtime := event.ModelRuntime{Key: inference.ModelKey{Provider: "openai", Model: "gpt-5"}, Limits: inference.ContextLimits{WindowTokens: 128_000}, Effort: inference.EffortHigh}
	tests := []struct {
		name string
		ev   event.Event
	}{
		{"active loop", event.ActiveLoopChanged{Header: sessionHeader, PreviousLoopID: previousLoopID, ActiveLoopID: activeLoopID}},
		{"loop inference", event.LoopInferenceChanged{Header: loopHeader, Runtime: runtime}},
		{"loop mode", event.LoopModeChanged{Header: loopHeader, PreviousMode: "plan", Mode: "build", Runtime: runtime}},
		{"workspace restored", event.WorkspaceRestored{Header: sessionHeader, Ref: "v1:sha256:restored"}},
		{"workspace checkpoint", event.WorkspaceCheckpointed{Header: sessionHeader, Ref: "v1:sha256:checkpoint", Consistency: event.SnapshotQuiescent, Trigger: event.SnapshotTriggerManual}},
		{"loop started initial mode", event.LoopStarted{Header: loopHeader, InitialMode: "plan", Runtime: runtime}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			data, err := event.MarshalEvent(tt.ev)
			if err != nil {
				t.Fatalf("MarshalEvent: %v", err)
			}
			got, err := event.UnmarshalEvent(data)
			if err != nil {
				t.Fatalf("UnmarshalEvent: %v\n%s", err, data)
			}
			if !reflect.DeepEqual(got, tt.ev) {
				t.Errorf("round trip = %#v, want %#v", got, tt.ev)
			}
		})
	}
}

func TestLoopInferenceChangedValidation(t *testing.T) {
	t.Parallel()
	sessionID, loopID := vID(t), vID(t)
	h := event.Header{Coordinates: identity.Coordinates{SessionID: sessionID, LoopID: loopID}, EventID: vID(t)}
	validRuntime := event.ModelRuntime{Key: inference.ModelKey{Provider: "test", Model: "model"}, Limits: inference.ContextLimits{WindowTokens: 128_000}}
	tests := []struct {
		name      string
		ev        event.LoopInferenceChanged
		wantField event.FieldName
	}{
		{name: "valid runtime", ev: event.LoopInferenceChanged{Header: h, Runtime: validRuntime}},
		{name: "maximum effort", ev: event.LoopInferenceChanged{Header: h, Runtime: event.ModelRuntime{Key: validRuntime.Key, Limits: validRuntime.Limits, Effort: inference.EffortMax}}},
		{name: "invalid model key", ev: event.LoopInferenceChanged{Header: h, Runtime: event.ModelRuntime{Key: inference.ModelKey{Provider: "test"}}}, wantField: event.FieldModelKey},
		{name: "invalid limits", ev: event.LoopInferenceChanged{Header: h, Runtime: event.ModelRuntime{Key: validRuntime.Key, Limits: inference.ContextLimits{WindowTokens: 10, MaxOutputTokens: 11}}}, wantField: event.FieldContextLimits},
		{name: "invalid effort", ev: event.LoopInferenceChanged{Header: h, Runtime: event.ModelRuntime{Key: validRuntime.Key, Effort: inference.Effort("extreme")}}, wantField: event.FieldEffort},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := event.ValidateEvent(tt.ev)
			if tt.wantField == "" {
				if err != nil {
					t.Fatalf("ValidateEvent error = %v", err)
				}
				return
			}
			var invalid *event.InvalidEventError
			if !errors.As(err, &invalid) {
				t.Fatalf("ValidateEvent error = %T %v, want InvalidEventError", err, err)
			}
			if invalid.Event != "LoopInferenceChanged" || invalid.Field != tt.wantField || invalid.Rule != event.RuleInvalid {
				t.Fatalf("InvalidEventError = %+v, want event=LoopInferenceChanged field=%s rule=%s", invalid, tt.wantField, event.RuleInvalid)
			}
		})
	}
}

func TestLoopInferenceChangedRejectsMalformedWireRuntime(t *testing.T) {
	t.Parallel()
	sessionID, loopID := vID(t), vID(t)
	h := event.Header{Coordinates: identity.Coordinates{SessionID: sessionID, LoopID: loopID}, EventID: vID(t)}
	tests := []struct {
		name  string
		wire  string
		field event.FieldName
	}{
		{name: "missing key", wire: `{"key":{"Provider":"test"},"limits":{}}`, field: event.FieldModelKey},
		{name: "invalid effort", wire: `{"key":{"Provider":"test","Model":"model"},"limits":{},"effort":"invalid"}`, field: event.FieldEffort},
	}
	for _, tt := range tests {
		data := []byte(`{"type":"LoopInferenceChanged","v":1,"session_id":"` + h.SessionID.String() + `","loop_id":"` + h.LoopID.String() + `","event_id":"` + h.EventID.String() + `","runtime":` + tt.wire + `}`)
		got, err := event.UnmarshalEvent(data)
		if got != nil {
			t.Errorf("UnmarshalEvent returned %#v on error", got)
		}
		var invalid *event.InvalidEventError
		if !errors.As(err, &invalid) || invalid.Field != tt.field || invalid.Rule != event.RuleInvalid {
			t.Errorf("UnmarshalEvent error = %T %+v, want InvalidEventError %s/is invalid", err, err, tt.field)
		}
	}
}

func TestWorkspaceCheckpointCauseShapes(t *testing.T) {
	t.Parallel()
	sessionID, loopID, turnID, stepID := vID(t), vID(t), vID(t), vID(t)
	base := event.Header{Coordinates: identity.Coordinates{SessionID: sessionID}, EventID: vID(t)}
	tests := []struct {
		name    string
		trigger event.SnapshotTriggerKind
		cause   identity.Cause
	}{
		{"manual has zero cause", event.SnapshotTriggerManual, identity.Cause{}},
		{"seed has zero cause", event.SnapshotTriggerSeed, identity.Cause{}},
		{"idle identifies SessionIdle", event.SnapshotTriggerIdle, identity.Cause{Coordinates: identity.Coordinates{SessionID: sessionID}, EventID: vID(t)}},
		{"interrupt identifies TurnInterrupted", event.SnapshotTriggerInterrupt, identity.Cause{Coordinates: identity.Coordinates{SessionID: sessionID, LoopID: loopID, TurnID: turnID}, EventID: vID(t)}},
		{"turn identifies TurnDone", event.SnapshotTriggerTurnDone, identity.Cause{Coordinates: identity.Coordinates{SessionID: sessionID, LoopID: loopID, TurnID: turnID}, EventID: vID(t)}},
		{"step identifies StepDone", event.SnapshotTriggerStepDone, identity.Cause{Coordinates: identity.Coordinates{SessionID: sessionID, LoopID: loopID, TurnID: turnID, StepID: stepID}, EventID: vID(t)}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ev := event.WorkspaceCheckpointed{Header: base, Ref: "v1:sha256:x", Consistency: event.SnapshotQuiescent, Trigger: tt.trigger}
			ev.Cause = tt.cause
			if err := event.ValidateEvent(ev); err != nil {
				t.Fatalf("ValidateEvent: %v", err)
			}
		})
	}
}

func TestWorkspaceCheckpointRejectsInvalidMetadataAndCause(t *testing.T) {
	t.Parallel()
	sessionID, loopID, turnID := vID(t), vID(t), vID(t)
	validHeader := event.Header{Coordinates: identity.Coordinates{SessionID: sessionID}, EventID: vID(t)}
	validTurnCause := identity.Cause{Coordinates: identity.Coordinates{SessionID: sessionID, LoopID: loopID, TurnID: turnID}, EventID: vID(t)}
	tests := []event.WorkspaceCheckpointed{
		{Header: validHeader, Consistency: event.SnapshotConsistencyUnknown, Trigger: event.SnapshotTriggerManual},
		{Header: validHeader, Consistency: event.SnapshotQuiescent, Trigger: event.SnapshotTriggerKindUnknown},
		{Header: event.Header{Coordinates: validHeader.Coordinates, EventID: validHeader.EventID, Cause: validTurnCause}, Consistency: event.SnapshotQuiescent, Trigger: event.SnapshotTriggerManual},
		{Header: validHeader, Consistency: event.SnapshotQuiescent, Trigger: event.SnapshotTriggerTurnDone},
	}
	for i, ev := range tests {
		var invalid *event.InvalidEventError
		if err := event.ValidateEvent(ev); !errors.As(err, &invalid) {
			t.Errorf("case %d error = %v, want InvalidEventError", i, err)
		}
	}
}

func TestWorkspaceCheckpointLegacyMissingMetadataDecodesUnknown(t *testing.T) {
	t.Parallel()
	h := event.Header{Coordinates: identity.Coordinates{SessionID: vID(t)}, EventID: vID(t)}
	// A legacy fixture intentionally omits both additive fields.
	data := []byte(`{"type":"WorkspaceCheckpointed","v":1,"session_id":"` + h.SessionID.String() + `","event_id":"` + h.EventID.String() + `","ref":"v1:sha256:legacy"}`)
	got, err := event.UnmarshalEvent(data)
	if err != nil {
		t.Fatalf("UnmarshalEvent legacy: %v", err)
	}
	cp := got.(event.WorkspaceCheckpointed)
	if cp.Consistency != event.SnapshotConsistencyUnknown || cp.Trigger != event.SnapshotTriggerKindUnknown {
		t.Fatalf("legacy metadata = (%v,%v), want unknown/unknown", cp.Consistency, cp.Trigger)
	}
}

func TestWorkspaceCheckpointRejectsExplicitUnknownMetadataOnDecode(t *testing.T) {
	t.Parallel()
	h := event.Header{Coordinates: identity.Coordinates{SessionID: vID(t)}, EventID: vID(t)}
	data := []byte(`{"type":"WorkspaceCheckpointed","v":1,"session_id":"` + h.SessionID.String() + `","event_id":"` + h.EventID.String() + `","ref":"v1:sha256:x","consistency":0,"trigger":0}`)
	if _, err := event.UnmarshalEvent(data); err == nil {
		t.Fatal("UnmarshalEvent explicit unknown = nil error")
	}
}

func TestWorkspaceCheckpointMetadataPresenceMatchesJSONFieldNames(t *testing.T) {
	t.Parallel()
	h := event.Header{Coordinates: identity.Coordinates{SessionID: vID(t)}, EventID: vID(t)}
	prefix := `{"type":"WorkspaceCheckpointed","v":1,"session_id":"` + h.SessionID.String() + `","event_id":"` + h.EventID.String() + `","ref":"v1:sha256:x"`
	tests := []struct {
		name     string
		metadata string
		wantErr  bool
	}{
		{name: "canonical current", metadata: `,"consistency":1,"trigger":1`},
		{name: "Go field casing current", metadata: `,"Consistency":1,"Trigger":1`},
		{name: "mixed casing current", metadata: `,"ConSiStEnCy":1,"TRIGGER":1`},
		{name: "Go field casing explicit unknown", metadata: `,"Consistency":0,"Trigger":0`, wantErr: true},
		{name: "mixed casing explicit unknown", metadata: `,"cOnSiStEnCy":0,"tRiGgEr":0`, wantErr: true},
		{name: "partial consistency alias", metadata: `,"Consistency":1`, wantErr: true},
		{name: "partial trigger alias", metadata: `,"Trigger":1`, wantErr: true},
		{name: "duplicate consistency aliases same", metadata: `,"consistency":1,"Consistency":1,"trigger":1`, wantErr: true},
		{name: "duplicate consistency aliases conflicting", metadata: `,"consistency":1,"Consistency":2,"trigger":1`, wantErr: true},
		{name: "duplicate trigger aliases same", metadata: `,"consistency":1,"trigger":1,"Trigger":1`, wantErr: true},
		{name: "duplicate trigger aliases conflicting", metadata: `,"consistency":1,"trigger":1,"Trigger":2`, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := event.UnmarshalEvent([]byte(prefix + tt.metadata + `}`))
			if tt.wantErr {
				if err == nil || got != nil {
					t.Fatalf("UnmarshalEvent = (%#v, %v), want nil,error", got, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("UnmarshalEvent error = %v", err)
			}
			checkpoint := got.(event.WorkspaceCheckpointed)
			if checkpoint.Consistency != event.SnapshotQuiescent || checkpoint.Trigger != event.SnapshotTriggerManual {
				t.Fatalf("metadata = (%v,%v), want quiescent/manual", checkpoint.Consistency, checkpoint.Trigger)
			}
		})
	}
}

func TestMarshalWorkspaceCheckpointRejectsUnknownMetadata(t *testing.T) {
	t.Parallel()
	h := event.Header{Coordinates: identity.Coordinates{SessionID: vID(t)}, EventID: vID(t)}
	for _, ev := range []event.WorkspaceCheckpointed{
		{Header: h, Consistency: event.SnapshotConsistencyUnknown, Trigger: event.SnapshotTriggerManual},
		{Header: h, Consistency: event.SnapshotQuiescent, Trigger: event.SnapshotTriggerKindUnknown},
	} {
		if data, err := event.MarshalEvent(ev); err == nil || data != nil {
			t.Errorf("MarshalEvent(%+v) = (%s, %v), want nil,error", ev, data, err)
		}
	}
}

func TestWorkspaceEventsAreSessionScoped(t *testing.T) {
	t.Parallel()
	sess, loop, eid := vID(t), vID(t), vID(t)
	for _, ev := range []event.Event{
		event.WorkspaceCheckpointed{Header: event.Header{Coordinates: identity.Coordinates{SessionID: sess, LoopID: loop}, EventID: eid}, Consistency: event.SnapshotQuiescent, Trigger: event.SnapshotTriggerManual},
		event.WorkspaceRestored{Header: event.Header{Coordinates: identity.Coordinates{SessionID: sess, LoopID: loop}, EventID: eid}},
	} {
		if err := event.ValidateEvent(ev); err == nil {
			t.Errorf("ValidateEvent(%T) = nil", ev)
		}
	}
}
