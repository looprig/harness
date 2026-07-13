package event_test

import (
	"errors"
	"testing"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/gate"
	"github.com/looprig/harness/pkg/identity"
	"github.com/looprig/inference"
)

// vID mints a non-zero UUID for validation tests or fails.
func vID(t *testing.T) uuid.UUID {
	t.Helper()
	u, err := uuid.New()
	if err != nil {
		t.Fatalf("uuid.New: %v", err)
	}
	return u
}

// TestValidateEventValid asserts every event type, populated to its full required
// identity profile, passes ValidateEvent. This is the happy-path row per event type.
func TestValidateEventValid(t *testing.T) {
	t.Parallel()

	sess := vID(t)
	loop := vID(t)
	turn := vID(t)
	step := vID(t)
	evID := vID(t)
	toolID := vID(t)

	sessionH := event.Header{Coordinates: identity.Coordinates{SessionID: sess}, EventID: evID}
	loopH := event.Header{Coordinates: identity.Coordinates{SessionID: sess, LoopID: loop}, EventID: evID}
	turnH := event.Header{Coordinates: identity.Coordinates{SessionID: sess, LoopID: loop, TurnID: turn}, EventID: evID}
	stepH := event.Header{Coordinates: identity.Coordinates{SessionID: sess, LoopID: loop, TurnID: turn, StepID: step}, EventID: evID}
	runtime := event.ModelRuntime{Key: inference.ModelKey{Provider: "test", Model: "model"}}

	tests := []struct {
		name string
		ev   event.Event
	}{
		{"SessionStarted", event.SessionStarted{Header: sessionH}},
		{"SessionActive", event.SessionActive{Header: sessionH}},
		{"SessionIdle", event.SessionIdle{Header: sessionH}},
		{"SessionStopped", event.SessionStopped{Header: sessionH}},
		{"RestoreStarted", event.RestoreStarted{Header: sessionH}},
		{"RestoreDone", event.RestoreDone{Header: sessionH}},
		{"RestoreErrored", event.RestoreErrored{Header: sessionH}},
		{"WorkspaceCheckpointed", event.WorkspaceCheckpointed{Header: sessionH, Ref: "v1:sha256:aabbccddeeff00112233445566778899aabbccddeeff00112233445566778899", Consistency: event.SnapshotQuiescent, Trigger: event.SnapshotTriggerManual}},
		{"WorkspaceCheckpointed quiescent manual", event.WorkspaceCheckpointed{Header: sessionH, Ref: "v1:sha256:aabb", Consistency: event.SnapshotQuiescent, Trigger: event.SnapshotTriggerManual}},
		{"WorkspaceRestored", event.WorkspaceRestored{Header: sessionH, Ref: "v1:sha256:aabb"}},
		{"ActiveLoopChanged", event.ActiveLoopChanged{Header: sessionH, PreviousLoopID: loop, ActiveLoopID: vID(t)}},
		{"LoopIdle", event.LoopIdle{Header: loopH}},
		{"LoopInferenceChanged", event.LoopInferenceChanged{Header: loopH, Runtime: runtime}},
		{"LoopModeChanged", event.LoopModeChanged{Header: loopH, Runtime: runtime}},
		// LoopStarted: NEW loop in Header.Coordinates (SessionID+LoopID, Turn/Step zero);
		// the spawning loop/turn/step rides in Header.Cause and is unconstrained by the
		// profile (the validator never checks Cause).
		{"LoopStarted", event.LoopStarted{Header: event.Header{
			Coordinates: identity.Coordinates{SessionID: sess, LoopID: loop},
			EventID:     evID,
			Cause: identity.Cause{
				Coordinates: identity.Coordinates{LoopID: loop, TurnID: turn, StepID: step},
				Agency:      identity.AgencyMachine,
			},
		}, Runtime: runtime}},
		{"InputQueued", event.InputQueued{Header: loopH}},
		{"TurnRejected", event.TurnRejected{Header: loopH}},
		{"TurnStarted", event.TurnStarted{Header: turnH}},
		{"TurnFoldedInto", event.TurnFoldedInto{Header: turnH}},
		{"TurnDone", event.TurnDone{Header: turnH}},
		{"TurnFailed", event.TurnFailed{Header: turnH}},
		{"TurnInterrupted", event.TurnInterrupted{Header: turnH}},
		// InputCancelled: TurnID is OPTIONAL — the loop-scoped header (no turn) is valid.
		{"InputCancelled client retract (no turn)", event.InputCancelled{Header: loopH}},
		{"InputCancelled abnormal return (with turn)", event.InputCancelled{Header: turnH}},
		{"TokenDelta", event.TokenDelta{Header: stepH}},
		{"StepDone", event.StepDone{Header: stepH}},
		{"PermissionRequested", event.PermissionRequested{Header: stepH, ToolExecutionID: toolID}},
		{"PermissionDecided", event.PermissionDecided{Header: stepH, ToolExecutionID: toolID, Effect: event.PermissionEffectApprove}},
		{"UserInputRequested", event.UserInputRequested{Header: stepH, ToolExecutionID: toolID}},
		{"ToolCallStarted", event.ToolCallStarted{Header: stepH, ToolExecutionID: toolID}},
		{"ToolCallCompleted", event.ToolCallCompleted{Header: stepH, ToolExecutionID: toolID}},
		{"GatePrepared", event.GatePrepared{Header: stepH, Gate: gate.Gate{ID: gate.ID(toolID)}}},
		{"GateOpened", event.GateOpened{Header: stepH, Gate: gate.Gate{ID: gate.ID(toolID)}}},
		{"GateResolved", event.GateResolved{Header: stepH, GateID: gate.ID(toolID)}},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if err := event.ValidateEvent(tt.ev); err != nil {
				t.Errorf("ValidateEvent(%s) = %v, want nil", tt.name, err)
			}
		})
	}
}

// TestValidateEventInvalid asserts each forbidden/missing case returns a typed
// *InvalidEventError naming the offending field + rule. Covers: zero EventID; a
// ScopeSession event with a non-zero LoopID; StepID set without TurnID; a tool event
// missing StepID and missing ToolExecutionID; a step event missing StepID; a turn
// event with a non-zero StepID.
func TestValidateEventInvalid(t *testing.T) {
	t.Parallel()

	sess := vID(t)
	loop := vID(t)
	turn := vID(t)
	step := vID(t)
	evID := vID(t)
	toolID := vID(t)

	tests := []struct {
		name      string
		ev        event.Event
		wantField event.FieldName
		wantRule  event.Rule
		wantEvent event.EventName
	}{
		{
			name:      "zero EventID is invalid",
			ev:        event.SessionStarted{Header: event.Header{Coordinates: identity.Coordinates{SessionID: sess}}},
			wantField: event.FieldEventID,
			wantRule:  event.RuleRequired,
			wantEvent: "SessionStarted",
		},
		{
			name:      "session event with non-zero LoopID must be zero",
			ev:        event.SessionStarted{Header: event.Header{Coordinates: identity.Coordinates{SessionID: sess, LoopID: loop}, EventID: evID}},
			wantField: event.FieldLoopID,
			wantRule:  event.RuleMustBeZero,
			wantEvent: "SessionStarted",
		},
		{
			name:      "session event missing SessionID",
			ev:        event.SessionIdle{Header: event.Header{EventID: evID}},
			wantField: event.FieldSessionID,
			wantRule:  event.RuleRequired,
			wantEvent: "SessionIdle",
		},
		{
			name:      "loop event missing LoopID",
			ev:        event.LoopIdle{Header: event.Header{Coordinates: identity.Coordinates{SessionID: sess}, EventID: evID}},
			wantField: event.FieldLoopID,
			wantRule:  event.RuleRequired,
			wantEvent: "LoopIdle",
		},
		{
			name:      "LoopStarted missing EventID",
			ev:        event.LoopStarted{Header: event.Header{Coordinates: identity.Coordinates{SessionID: sess, LoopID: loop}}},
			wantField: event.FieldEventID,
			wantRule:  event.RuleRequired,
			wantEvent: "LoopStarted",
		},
		{
			name:      "LoopStarted missing LoopID",
			ev:        event.LoopStarted{Header: event.Header{Coordinates: identity.Coordinates{SessionID: sess}, EventID: evID}},
			wantField: event.FieldLoopID,
			wantRule:  event.RuleRequired,
			wantEvent: "LoopStarted",
		},
		{
			name:      "LoopStarted with non-zero TurnID must be zero",
			ev:        event.LoopStarted{Header: event.Header{Coordinates: identity.Coordinates{SessionID: sess, LoopID: loop, TurnID: turn}, EventID: evID}},
			wantField: event.FieldTurnID,
			wantRule:  event.RuleMustBeZero,
			wantEvent: "LoopStarted",
		},
		{
			name:      "turn event with non-zero StepID must be zero",
			ev:        event.TurnStarted{Header: event.Header{Coordinates: identity.Coordinates{SessionID: sess, LoopID: loop, TurnID: turn, StepID: step}, EventID: evID}},
			wantField: event.FieldStepID,
			wantRule:  event.RuleMustBeZero,
			wantEvent: "TurnStarted",
		},
		{
			name:      "turn event missing TurnID",
			ev:        event.TurnDone{Header: event.Header{Coordinates: identity.Coordinates{SessionID: sess, LoopID: loop}, EventID: evID}},
			wantField: event.FieldTurnID,
			wantRule:  event.RuleRequired,
			wantEvent: "TurnDone",
		},
		{
			name: "StepID set without TurnID",
			// A step event carrying StepID but a zero TurnID violates StepID ⇒ TurnID.
			ev:        event.StepDone{Header: event.Header{Coordinates: identity.Coordinates{SessionID: sess, LoopID: loop, StepID: step}, EventID: evID}},
			wantField: event.FieldTurnID,
			wantRule:  event.RuleRequired,
			wantEvent: "StepDone",
		},
		{
			name:      "step event missing StepID",
			ev:        event.TokenDelta{Header: event.Header{Coordinates: identity.Coordinates{SessionID: sess, LoopID: loop, TurnID: turn}, EventID: evID}},
			wantField: event.FieldStepID,
			wantRule:  event.RuleRequired,
			wantEvent: "TokenDelta",
		},
		{
			name:      "tool event missing StepID",
			ev:        event.ToolCallStarted{Header: event.Header{Coordinates: identity.Coordinates{SessionID: sess, LoopID: loop, TurnID: turn}, EventID: evID}, ToolExecutionID: toolID},
			wantField: event.FieldStepID,
			wantRule:  event.RuleRequired,
			wantEvent: "ToolCallStarted",
		},
		{
			name:      "tool event missing ToolExecutionID",
			ev:        event.ToolCallCompleted{Header: event.Header{Coordinates: identity.Coordinates{SessionID: sess, LoopID: loop, TurnID: turn, StepID: step}, EventID: evID}},
			wantField: event.FieldToolExecutionID,
			wantRule:  event.RuleRequired,
			wantEvent: "ToolCallCompleted",
		},
		{
			name:      "gate event missing ToolExecutionID",
			ev:        event.PermissionRequested{Header: event.Header{Coordinates: identity.Coordinates{SessionID: sess, LoopID: loop, TurnID: turn, StepID: step}, EventID: evID}},
			wantField: event.FieldToolExecutionID,
			wantRule:  event.RuleRequired,
			wantEvent: "PermissionRequested",
		},
		{
			name:      "InputCancelled with non-zero StepID must be zero",
			ev:        event.InputCancelled{Header: event.Header{Coordinates: identity.Coordinates{SessionID: sess, LoopID: loop, TurnID: turn, StepID: step}, EventID: evID}},
			wantField: event.FieldStepID,
			wantRule:  event.RuleMustBeZero,
			wantEvent: "InputCancelled",
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := event.ValidateEvent(tt.ev)
			var ve *event.InvalidEventError
			if !errors.As(err, &ve) {
				t.Fatalf("ValidateEvent error = %v (%T), want *event.InvalidEventError", err, err)
			}
			if ve.Field != tt.wantField {
				t.Errorf("Field = %q, want %q", ve.Field, tt.wantField)
			}
			if ve.Rule != tt.wantRule {
				t.Errorf("Rule = %q, want %q", ve.Rule, tt.wantRule)
			}
			if ve.Event != tt.wantEvent {
				t.Errorf("Event = %q, want %q", ve.Event, tt.wantEvent)
			}
		})
	}
}
