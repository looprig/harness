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
	model := inference.CustomModel("openai", "responses", "https://api.openai.com", "gpt-5")
	tests := []struct {
		name string
		ev   event.Event
	}{
		{"active loop", event.ActiveLoopChanged{Header: sessionHeader, PreviousLoopID: previousLoopID, ActiveLoopID: activeLoopID}},
		{"loop inference", event.LoopInferenceChanged{Header: loopHeader, Model: model, Effort: inference.EffortHigh}},
		{"loop mode", event.LoopModeChanged{Header: loopHeader, PreviousMode: "plan", Mode: "build"}},
		{"workspace restored", event.WorkspaceRestored{Header: sessionHeader, Ref: "v1:sha256:restored"}},
		{"workspace checkpoint", event.WorkspaceCheckpointed{Header: sessionHeader, Ref: "v1:sha256:checkpoint", Consistency: event.SnapshotQuiescent, Trigger: event.SnapshotTriggerManual}},
		{"loop started initial mode", event.LoopStarted{Header: loopHeader, InitialMode: "plan"}},
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
