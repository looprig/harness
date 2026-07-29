package hub

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/gate"
	"github.com/looprig/harness/pkg/identity"
)

// Finding 1 (Phase 6 spec-compliance review): PermissionReviewStarted/
// PermissionReviewCompleted are Internal, Enduring audit events — the SAME
// kind of private audit record HustleStarted/Completed/Failed already are —
// but validateInternalPublication's allowlist never recognized them, so
// every real review's durable audit append was rejected at the boundary
// (PublishBoundaryType) and, per design's own Integrity-failure handling,
// faulted the whole session. These tests prove PublishInternalEventChecked
// now accepts both review event types (value form — the form review_adapter.go
// actually publishes) while validatePublicPublication continues to reject
// them on the public-only path (both value and pointer forms), exactly
// mirroring the existing Hustle lifecycle coverage in hustle_test.go.

func TestPublishInternalEventCheckedAcceptsPermissionReviewLifecycle(t *testing.T) {
	t.Parallel()
	sessionID := mustID(t)
	gateID := gate.ID(mustID(t))

	started := event.PermissionReviewStarted{
		Header: event.Header{
			Coordinates:     identity.Coordinates{SessionID: sessionID, LoopID: mustID(t), TurnID: mustID(t), StepID: mustID(t)},
			EventID:         mustID(t),
			CreatedAt:       time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC),
			EventVisibility: event.Internal,
		},
		GateID:             gateID,
		ToolExecutionID:    mustID(t),
		Classifier:         "classifier",
		ClassifierRevision: "rev-1",
	}
	completed := event.PermissionReviewCompleted{
		Header: event.Header{
			Coordinates:     identity.Coordinates{SessionID: sessionID, LoopID: mustID(t), TurnID: mustID(t), StepID: mustID(t)},
			EventID:         mustID(t),
			CreatedAt:       time.Date(2026, 7, 29, 12, 0, 1, 0, time.UTC),
			EventVisibility: event.Internal,
		},
		GateID:             gateID,
		ToolExecutionID:    mustID(t),
		Classifier:         "classifier",
		ClassifierRevision: "rev-1",
		Status:             gate.ReviewStatusNotApplicable,
	}

	for _, tt := range []struct {
		name string
		ev   event.Event
	}{
		{name: "started", ev: started},
		{name: "completed", ev: completed},
	} {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			appender := &fakeAppender{}
			h := New(sessionID, WithAppender(appender), WithFactory(testFactory()))
			sub, err := h.SubscribeEvents(allFilter())
			if err != nil {
				t.Fatalf("SubscribeEvents() error = %v", err)
			}

			if err := h.PublishInternalEventChecked(context.Background(), tt.ev); err != nil {
				t.Fatalf("PublishInternalEventChecked() error = %v, want nil", err)
			}
			appended := appender.events()
			if len(appended) != 1 || !reflect.DeepEqual(appended[0], tt.ev) {
				t.Fatalf("appended = %#v, want only the triggering event", appended)
			}
			// PublishInternalEventChecked deliberately bypasses subscriber
			// delivery and quiescence mutation (hub.go's doc comment on the
			// method) — a review's audit trail is private, and its blocking
			// state is owned elsewhere (Hustle activity leases / the gate
			// evaluation that spawned the review).
			expectNone(t, sub)
			h.mu.RLock()
			phase, active := h.state.phase, len(h.state.active)
			h.mu.RUnlock()
			if phase != SessionIdle || active != 0 {
				t.Fatalf("state after private review audit = (%v,%d), want idle/0", phase, active)
			}
		})
	}
}

func TestOrdinaryPublicationRejectsPermissionReviewLifecycle(t *testing.T) {
	t.Parallel()
	sessionID := mustID(t)
	gateID := gate.ID(mustID(t))
	started := event.PermissionReviewStarted{
		Header: event.Header{
			Coordinates:     identity.Coordinates{SessionID: sessionID},
			EventID:         mustID(t),
			CreatedAt:       time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC),
			EventVisibility: event.Public,
		},
		GateID:             gateID,
		ToolExecutionID:    mustID(t),
		Classifier:         "classifier",
		ClassifierRevision: "rev-1",
	}
	completed := event.PermissionReviewCompleted{
		Header: event.Header{
			Coordinates:     identity.Coordinates{SessionID: sessionID},
			EventID:         mustID(t),
			CreatedAt:       time.Date(2026, 7, 29, 12, 0, 1, 0, time.UTC),
			EventVisibility: event.Public,
		},
		GateID:             gateID,
		ToolExecutionID:    mustID(t),
		Classifier:         "classifier",
		ClassifierRevision: "rev-1",
		Status:             gate.ReviewStatusNotApplicable,
	}

	tests := []struct {
		name    string
		ev      event.Event
		publish func(*Hub, context.Context, event.Event) error
	}{
		{name: "unchecked rejects public started", ev: started, publish: (*Hub).PublishEvent},
		{name: "checked rejects public started", ev: started, publish: (*Hub).PublishEventChecked},
		{name: "unchecked rejects public completed", ev: completed, publish: (*Hub).PublishEvent},
		{name: "checked rejects public completed", ev: completed, publish: (*Hub).PublishEventChecked},
		{name: "unchecked rejects started pointer", ev: &started, publish: (*Hub).PublishEvent},
		{name: "checked rejects started pointer", ev: &started, publish: (*Hub).PublishEventChecked},
		{name: "unchecked rejects completed pointer", ev: &completed, publish: (*Hub).PublishEvent},
		{name: "checked rejects completed pointer", ev: &completed, publish: (*Hub).PublishEventChecked},
	}
	for _, tt := range tests {
		testCase := tt
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			appender := &fakeAppender{}
			h := New(sessionID, WithAppender(appender))
			sub, err := h.SubscribeEvents(allFilter())
			if err != nil {
				t.Fatalf("SubscribeEvents() error = %v", err)
			}

			err = testCase.publish(h, context.Background(), testCase.ev)
			var boundary *PublishBoundaryError
			if !errors.As(err, &boundary) || boundary.Reason != PublishBoundaryType {
				t.Fatalf("error = %T %v, want type PublishBoundaryError", err, err)
			}
			if appender.callCount() != 0 {
				t.Fatalf("append calls = %d, want 0", appender.callCount())
			}
			expectNone(t, sub)
		})
	}
}

func TestPublishInternalEventCheckedRejectsPermissionReviewPointerForm(t *testing.T) {
	t.Parallel()
	sessionID := mustID(t)
	gateID := gate.ID(mustID(t))
	started := event.PermissionReviewStarted{
		Header: event.Header{
			Coordinates:     identity.Coordinates{SessionID: sessionID, LoopID: mustID(t), TurnID: mustID(t), StepID: mustID(t)},
			EventID:         mustID(t),
			CreatedAt:       time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC),
			EventVisibility: event.Internal,
		},
		GateID:             gateID,
		ToolExecutionID:    mustID(t),
		Classifier:         "classifier",
		ClassifierRevision: "rev-1",
	}

	appender := &fakeAppender{}
	h := New(sessionID, WithAppender(appender))
	err := h.PublishInternalEventChecked(context.Background(), &started)
	// isInternalAuditEventType matches the pointer form too (symmetric with
	// validatePublicPublication's pre-existing pointer-matching denylist, so
	// a publicly-visible pointer-form event stays denied there without
	// regressing TestOrdinaryPublicationRejectsHustleLifecyclePointers).
	// event.ValidateEvent's own type classification only recognizes the
	// value form, so a pointer form still fails closed here — just via
	// PublishBoundaryInvalid instead of PublishBoundaryType. Either way it
	// must never be nil: the value form is the only one review_adapter.go
	// (or hustleruntime/audit.go) ever actually publishes.
	var boundary *PublishBoundaryError
	if !errors.As(err, &boundary) || err == nil {
		t.Fatalf("error = %T %v, want a PublishBoundaryError (pointer form must never be accepted)", err, err)
	}
	if appender.callCount() != 0 {
		t.Fatalf("append calls = %d, want 0", appender.callCount())
	}
}
