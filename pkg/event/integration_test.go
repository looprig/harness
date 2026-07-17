package event_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/identity"
)

// validStatus is a publishable IntegrationStatus. Tests mutate one field from it
// so a failure names exactly the field under test.
func validStatus(t *testing.T) event.IntegrationStatus {
	t.Helper()
	id, err := uuid.New()
	if err != nil {
		t.Fatalf("uuid.New() error = %v", err)
	}
	sid, err := uuid.New()
	if err != nil {
		t.Fatalf("uuid.New() error = %v", err)
	}
	return event.IntegrationStatus{
		Header: event.Header{
			EventID:         id,
			EventVisibility: event.Public,
			Coordinates:     identity.Coordinates{SessionID: sid},
		},
		Source: "mcp",
		Name:   "github",
		State:  event.IntegrationReady,
	}
}

func TestIntegrationStatusValidates(t *testing.T) {
	t.Parallel()
	if err := event.ValidateEvent(validStatus(t)); err != nil {
		t.Fatalf("ValidateEvent(valid) error = %v, want nil", err)
	}
}

// TestIntegrationStatusRejectsBadBody is the guard. Every case here is a body an
// integration outside this module could hand the publish boundary, and each must
// be refused rather than published: an unbounded Detail is a third-party server
// growing an event, and an undeclared State is a status nothing can render.
func TestIntegrationStatusRejectsBadBody(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		mutate func(*event.IntegrationStatus)
		field  event.FieldName
		rule   event.Rule
	}{
		{
			name:   "empty source",
			mutate: func(s *event.IntegrationStatus) { s.Source = "" },
			field:  event.FieldSource,
			rule:   event.RuleRequired,
		},
		{
			name:   "over-long source",
			mutate: func(s *event.IntegrationStatus) { s.Source = strings.Repeat("s", event.MaxIntegrationSourceBytes+1) },
			field:  event.FieldSource,
			rule:   event.RuleInvalid,
		},
		{
			name:   "empty name",
			mutate: func(s *event.IntegrationStatus) { s.Name = "" },
			field:  event.FieldIntegrationName,
			rule:   event.RuleRequired,
		},
		{
			name:   "over-long name",
			mutate: func(s *event.IntegrationStatus) { s.Name = strings.Repeat("n", event.MaxIntegrationNameBytes+1) },
			field:  event.FieldIntegrationName,
			rule:   event.RuleInvalid,
		},
		{
			name:   "zero state",
			mutate: func(s *event.IntegrationStatus) { s.State = 0 },
			field:  event.FieldState,
			rule:   event.RuleInvalid,
		},
		{
			name:   "undeclared state",
			mutate: func(s *event.IntegrationStatus) { s.State = event.IntegrationState(99) },
			field:  event.FieldState,
			rule:   event.RuleInvalid,
		},
		{
			name:   "over-long detail",
			mutate: func(s *event.IntegrationStatus) { s.Detail = strings.Repeat("d", event.MaxIntegrationDetailBytes+1) },
			field:  event.FieldDetail,
			rule:   event.RuleInvalid,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ev := validStatus(t)
			tt.mutate(&ev)
			err := event.ValidateEvent(ev)
			var invalid *event.InvalidEventError
			if !errors.As(err, &invalid) {
				t.Fatalf("ValidateEvent() error = %v, want *InvalidEventError", err)
			}
			if invalid.Event != "IntegrationStatus" {
				t.Errorf("Event = %q, want IntegrationStatus", invalid.Event)
			}
			if invalid.Field != tt.field {
				t.Errorf("Field = %q, want %q", invalid.Field, tt.field)
			}
			if invalid.Rule != tt.rule {
				t.Errorf("Rule = %q, want %q", invalid.Rule, tt.rule)
			}
		})
	}
}

// TestIntegrationStatusAcceptsBoundaryValues pins that the bounds are inclusive:
// a field exactly at its cap is legal. Without this, tightening a `>` to a `>=`
// would pass every rejection test above while silently refusing valid events.
func TestIntegrationStatusAcceptsBoundaryValues(t *testing.T) {
	t.Parallel()
	ev := validStatus(t)
	ev.Source = strings.Repeat("s", event.MaxIntegrationSourceBytes)
	ev.Name = strings.Repeat("n", event.MaxIntegrationNameBytes)
	ev.Detail = strings.Repeat("d", event.MaxIntegrationDetailBytes)
	if err := event.ValidateEvent(ev); err != nil {
		t.Fatalf("ValidateEvent(at bounds) error = %v, want nil", err)
	}
}

// TestIntegrationStatusIsEphemeral pins the classification. It is not a style
// choice: an integration is a live resource that a restore reconstructs by
// reconnecting, so a journaled status could only ever describe a connection that
// no longer exists. MarshalEvent must refuse it.
func TestIntegrationStatusIsEphemeral(t *testing.T) {
	t.Parallel()
	ev := validStatus(t)
	if got := ev.Class(); got != event.Ephemeral {
		t.Errorf("Class() = %v, want Ephemeral", got)
	}
	if ev.EndsTurn() {
		t.Error("EndsTurn() = true, want false")
	}
	if got := ev.Scope(); got != event.ScopeSession {
		t.Errorf("Scope() = %v, want ScopeSession", got)
	}

	_, err := event.MarshalEvent(ev)
	var ephemeral *event.EphemeralNotPersistableError
	if !errors.As(err, &ephemeral) {
		t.Fatalf("MarshalEvent() error = %v, want *EphemeralNotPersistableError", err)
	}
}

// TestIntegrationStatusIsInTheUnion proves classify knows the type. An event
// classify does not know is rejected as an unknown type, which would make the
// whole event unpublishable — and would do so with an error naming "Event"
// rather than this type, so it is worth asserting distinctly from the body
// rejections above.
func TestIntegrationStatusIsInTheUnion(t *testing.T) {
	t.Parallel()
	ev := validStatus(t)
	ev.Source = "" // fail the body, not the identity
	err := event.ValidateEvent(ev)
	var invalid *event.InvalidEventError
	if !errors.As(err, &invalid) {
		t.Fatalf("ValidateEvent() error = %v, want *InvalidEventError", err)
	}
	if invalid.Rule == event.RuleUnknownType {
		t.Fatal("ValidateEvent() rejected IntegrationStatus as an unknown type; it is missing from classify")
	}
}

func TestIntegrationStateString(t *testing.T) {
	t.Parallel()
	tests := []struct {
		state event.IntegrationState
		want  string
	}{
		{event.IntegrationStarting, "starting"},
		{event.IntegrationReady, "ready"},
		{event.IntegrationDegraded, "degraded"},
		{event.IntegrationFailed, "failed"},
		{event.IntegrationClosed, "closed"},
		{event.IntegrationState(0), "unknown"},
		{event.IntegrationState(99), "unknown"},
	}
	for _, tt := range tests {
		t.Run(tt.want, func(t *testing.T) {
			t.Parallel()
			if got := tt.state.String(); got != tt.want {
				t.Errorf("String() = %q, want %q", got, tt.want)
			}
		})
	}
}
