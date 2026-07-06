package event

import (
	"errors"
	"testing"

	"github.com/looprig/harness/pkg/identity"
	"github.com/looprig/core/uuid"
)

// unknownEvent is a test-only concrete Event NOT in the sealed union (it is not
// listed in doc.go's guard and not handled by classify). It exists solely to drive
// ValidateEvent's fail-secure unknown-type path. It must live in an INTERNAL test
// (package event) because Event.isEvent() is unexported — an external test cannot
// satisfy the interface.
type unknownEvent struct {
	ephemeral
	sessionScoped
	Header
}

func (unknownEvent) isEvent() {}

// Compile-time check that unknownEvent really satisfies Event (so the test exercises
// the genuine interface path, not a near-miss).
var _ Event = unknownEvent{}

// TestValidateEventUnknownType asserts the fail-secure path: a concrete Event whose
// type is not in the sealed union is rejected with FieldType/RuleUnknownType (not a
// misleading EventID/coordinate error), even when its header is fully populated —
// so a journal/restore caller branching on .Field/.Rule learns the true cause.
func TestValidateEventUnknownType(t *testing.T) {
	t.Parallel()

	mint := func() uuid.UUID {
		u, err := uuid.New()
		if err != nil {
			t.Fatalf("uuid.New: %v", err)
		}
		return u
	}

	tests := []struct {
		name string
		ev   Event
	}{
		{
			name: "unknown type with zero header",
			ev:   unknownEvent{},
		},
		{
			name: "unknown type with a fully populated header",
			ev: unknownEvent{Header: Header{
				Coordinates: identity.Coordinates{SessionID: mint(), LoopID: mint(), TurnID: mint(), StepID: mint()},
				EventID:     mint(),
			}},
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := ValidateEvent(tt.ev)
			var ve *InvalidEventError
			if !errors.As(err, &ve) {
				t.Fatalf("ValidateEvent error = %v (%T), want *InvalidEventError", err, err)
			}
			if ve.Field != FieldType {
				t.Errorf("Field = %q, want %q", ve.Field, FieldType)
			}
			if ve.Rule != RuleUnknownType {
				t.Errorf("Rule = %q, want %q", ve.Rule, RuleUnknownType)
			}
		})
	}
}

// TestClassifyExhaustive asserts classify recognizes EVERY event type in the sealed
// union (doc.go's guard) — each yields ok==true and a concrete name (never the
// "Event" unknown-type fallback). It guards against a new event type being added to
// the union without a classify arm, which would otherwise silently fall through to
// the fail-secure default. This table IS the union; keep it in lockstep with doc.go.
func TestClassifyExhaustive(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		ev   Event
	}{
		{"SessionStarted", SessionStarted{}},
		{"SessionActive", SessionActive{}},
		{"SessionIdle", SessionIdle{}},
		{"SessionStopped", SessionStopped{}},
		{"RestoreStarted", RestoreStarted{}},
		{"RestoreDone", RestoreDone{}},
		{"RestoreErrored", RestoreErrored{}},
		{"WorkspaceCheckpointed", WorkspaceCheckpointed{}},
		{"SecurityCeilingChanged", SecurityCeilingChanged{}},
		{"LoopIdle", LoopIdle{}},
		{"LoopStarted", LoopStarted{}},
		{"TokenDelta", TokenDelta{}},
		{"TurnStarted", TurnStarted{}},
		{"StepDone", StepDone{}},
		{"TurnFoldedInto", TurnFoldedInto{}},
		{"InputCancelled", InputCancelled{}},
		{"InputQueued", InputQueued{}},
		{"TurnRejected", TurnRejected{}},
		{"TurnDone", TurnDone{}},
		{"TurnFailed", TurnFailed{}},
		{"TurnInterrupted", TurnInterrupted{}},
		{"PermissionRequested", PermissionRequested{}},
		{"UserInputRequested", UserInputRequested{}},
		{"ToolCallStarted", ToolCallStarted{}},
		{"ToolCallCompleted", ToolCallCompleted{}},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			name, _, ok := classify(tt.ev)
			if !ok {
				t.Errorf("classify(%s) ok = false, want true (type missing a classify arm)", tt.name)
			}
			if name != tt.name {
				t.Errorf("classify(%s) name = %q, want %q", tt.name, name, tt.name)
			}
			if name == "Event" {
				t.Errorf("classify(%s) fell through to the unknown-type %q name", tt.name, name)
			}
		})
	}
}
