package event_test

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/identity"
)

func validWorkflowActivity() event.WorkflowActivity {
	return event.WorkflowActivity{
		Header: event.Header{
			Coordinates: identity.Coordinates{SessionID: uuid.UUID{1}},
			EventID:     uuid.UUID{2},
		},
		RunID:             uuid.UUID{3},
		WorkflowName:      "source_document_extract",
		WorkflowVersion:   "v1",
		Kind:              event.WorkflowActivityRunStarted,
		Status:            event.WorkflowRunStatusRunning,
		TotalVertices:     3,
		CompletedVertices: 0,
		Message:           "workflow started",
		OccurredAt:        time.Date(2026, time.August, 9, 12, 0, 0, 0, time.UTC),
	}
}

func TestWorkflowActivityContractAndRoundTrip(t *testing.T) {
	t.Parallel()

	in := validWorkflowActivity()
	if err := event.ValidateEvent(in); err != nil {
		t.Fatalf("ValidateEvent() = %v, want nil", err)
	}
	if in.Class() != event.Enduring || in.Scope() != event.ScopeSession || in.Visibility() != event.Public || in.EndsTurn() {
		t.Fatalf("contract = class:%v scope:%v visibility:%v terminal:%v", in.Class(), in.Scope(), in.Visibility(), in.EndsTurn())
	}

	wire, err := event.MarshalEvent(in)
	if err != nil {
		t.Fatalf("MarshalEvent() = %v", err)
	}
	got, err := event.UnmarshalEvent(wire)
	if err != nil {
		t.Fatalf("UnmarshalEvent() = %v\nwire: %s", err, wire)
	}
	if !reflect.DeepEqual(got, in) {
		t.Fatalf("round-trip mismatch:\n got = %#v\nwant = %#v\nwire: %s", got, in, wire)
	}
}

func TestWorkflowActivityRejectsInvalidFields(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		mutate func(*event.WorkflowActivity)
		field  event.FieldName
	}{
		{name: "run id", mutate: func(v *event.WorkflowActivity) { v.RunID = uuid.UUID{} }, field: event.FieldRunID},
		{name: "workflow name", mutate: func(v *event.WorkflowActivity) { v.WorkflowName = "../unsafe" }, field: event.FieldWorkflowName},
		{name: "workflow version", mutate: func(v *event.WorkflowActivity) { v.WorkflowVersion = "" }, field: event.FieldWorkflowVersion},
		{name: "activity kind", mutate: func(v *event.WorkflowActivity) { v.Kind = event.WorkflowActivityKind("future") }, field: event.FieldActivityKind},
		{name: "run status", mutate: func(v *event.WorkflowActivity) { v.Status = event.WorkflowRunStatus("future") }, field: event.FieldStatus},
		{name: "occurred at", mutate: func(v *event.WorkflowActivity) { v.OccurredAt = time.Time{} }, field: event.FieldOccurredAt},
		{name: "message", mutate: func(v *event.WorkflowActivity) {
			v.Message = strings.Repeat("x", event.MaxWorkflowActivityMessageBytes+1)
		}, field: event.FieldMessage},
		{name: "progress order", mutate: func(v *event.WorkflowActivity) { v.CompletedVertices = v.TotalVertices + 1 }, field: event.FieldProgress},
		{name: "vertex label without id", mutate: func(v *event.WorkflowActivity) { v.VertexLabel = "label" }, field: event.FieldVertexID},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			value := validWorkflowActivity()
			tt.mutate(&value)
			var invalid *event.InvalidEventError
			if err := event.ValidateEvent(value); !errors.As(err, &invalid) {
				t.Fatalf("ValidateEvent() = %T %v, want *InvalidEventError", err, err)
			} else if invalid.Field != tt.field || invalid.Rule != event.RuleInvalid && invalid.Rule != event.RuleRequired {
				t.Fatalf("InvalidEventError = %+v, want field %s", invalid, tt.field)
			}
		})
	}
}

func TestWorkflowActivityRejectsInternalVisibility(t *testing.T) {
	t.Parallel()

	value := validWorkflowActivity()
	value.EventVisibility = event.Internal
	var invalid *event.InvalidEventError
	if err := event.ValidateEvent(value); !errors.As(err, &invalid) {
		t.Fatalf("ValidateEvent() = %T %v, want *InvalidEventError", err, err)
	} else if invalid.Field != event.FieldVisibility || invalid.Rule != event.RuleInvalid {
		t.Fatalf("InvalidEventError = %+v, want Visibility/invalid", invalid)
	}
	if event.ShouldDeliver(event.EventFilter{}, value) {
		t.Fatal("ShouldDeliver() accepted an internal WorkflowActivity")
	}
}

func TestWorkflowActivityIsAlwaysVisibleToSessionFilter(t *testing.T) {
	t.Parallel()

	value := validWorkflowActivity()
	otherLoop := uuid.UUID{99}
	filter := event.EventFilter{
		Ephemeral: event.LoopScope{Loops: map[uuid.UUID]struct{}{otherLoop: {}}},
		Enduring:  event.LoopScope{Loops: map[uuid.UUID]struct{}{otherLoop: {}}},
	}
	if !event.ShouldDeliver(filter, value) {
		t.Fatal("ShouldDeliver() filtered an owning-session WorkflowActivity")
	}
}

func TestWorkflowActivityRejectsUnknownWireFields(t *testing.T) {
	t.Parallel()

	wire, err := event.MarshalEvent(validWorkflowActivity())
	if err != nil {
		t.Fatalf("MarshalEvent() = %v", err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(wire, &fields); err != nil {
		t.Fatalf("json.Unmarshal() = %v", err)
	}
	fields["metadata"] = json.RawMessage(`{"untrusted":"text"}`)
	mutated, err := json.Marshal(fields)
	if err != nil {
		t.Fatalf("json.Marshal() = %v", err)
	}
	if got, err := event.UnmarshalEvent(mutated); got != nil || err == nil {
		t.Fatalf("UnmarshalEvent() = %#v, %v; want strict unknown-field rejection", got, err)
	}
}

func TestFactoryStampWorkflowActivityPreservesDeterministicID(t *testing.T) {
	t.Parallel()

	deterministicID := uuid.UUID{55}
	called := false
	factory := event.NewFactory(
		func() (uuid.UUID, error) {
			called = true
			return uuid.UUID{56}, nil
		},
		func() time.Time { return time.Date(2026, time.August, 9, 13, 0, 0, 0, time.UTC) },
	)
	input := validWorkflowActivity()
	input.EventID = uuid.UUID{}
	activity, err := factory.StampWorkflowActivity(input, deterministicID)
	if err != nil {
		t.Fatalf("StampWorkflowActivity() = %v", err)
	}
	if called {
		t.Fatal("StampWorkflowActivity() called the fresh-ID generator")
	}
	if activity.EventID != deterministicID {
		t.Fatalf("EventID = %v, want deterministic %v", activity.EventID, deterministicID)
	}
	if !activity.CreatedAt.Equal(time.Date(2026, time.August, 9, 13, 0, 0, 0, time.UTC)) {
		t.Fatalf("CreatedAt = %v, want factory clock", activity.CreatedAt)
	}
	if err := event.ValidateEvent(activity); err != nil {
		t.Fatalf("stamped activity validation = %v", err)
	}
}

func TestFactoryStampWorkflowActivityRejectsZeroID(t *testing.T) {
	t.Parallel()

	factory := event.NewFactory(
		func() (uuid.UUID, error) { return uuid.UUID{56}, nil },
		func() time.Time { return time.Unix(1, 0).UTC() },
	)
	_, err := factory.StampWorkflowActivity(validWorkflowActivity(), uuid.UUID{})
	var invalid *event.InvalidEventError
	if !errors.As(err, &invalid) || invalid.Field != event.FieldEventID {
		t.Fatalf("StampWorkflowActivity() error = %T %v, want EventID validation", err, err)
	}
}
