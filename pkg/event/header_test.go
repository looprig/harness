package event_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/looprig/core/content"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/identity"
)

// TestEventClass asserts every concrete event reports the Class its lifecycle
// mixin dictates: TokenDelta and the ToolCall* events (ToolCallStarted/
// ToolCallCompleted) are Ephemeral; every other loop event is Enduring (terminal
// events fold in Class()==Enduring by construction).
func TestEventClass(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		ev   event.Event
		want event.Class
	}{
		{"TokenDelta ephemeral", event.TokenDelta{}, event.Ephemeral},
		{"SessionStarted enduring", event.SessionStarted{}, event.Enduring},
		{"SessionActive enduring", event.SessionActive{}, event.Enduring},
		{"SessionIdle enduring", event.SessionIdle{}, event.Enduring},
		{"SessionStopped enduring", event.SessionStopped{}, event.Enduring},
		{"HustleStarted enduring", event.HustleStarted{}, event.Enduring},
		{"HustleCompleted enduring", event.HustleCompleted{}, event.Enduring},
		{"HustleFailed enduring", event.HustleFailed{}, event.Enduring},
		{"RestoreStarted enduring", event.RestoreStarted{}, event.Enduring},
		{"RestoreDone enduring", event.RestoreDone{}, event.Enduring},
		{"RestoreErrored enduring", event.RestoreErrored{}, event.Enduring},
		{"WorkspaceCheckpointed enduring", event.WorkspaceCheckpointed{}, event.Enduring},
		{"WorkspaceRestored enduring", event.WorkspaceRestored{}, event.Enduring},
		{"ActiveLoopChanged enduring", event.ActiveLoopChanged{}, event.Enduring},
		{"LoopInferenceChanged enduring", event.LoopInferenceChanged{}, event.Enduring},
		{"LoopModeChanged enduring", event.LoopModeChanged{}, event.Enduring},
		{"LoopIdle enduring", event.LoopIdle{}, event.Enduring},
		{"TurnStarted enduring", event.TurnStarted{}, event.Enduring},
		{"StepDone enduring", event.StepDone{}, event.Enduring},
		{"TurnFoldedInto enduring", event.TurnFoldedInto{}, event.Enduring},
		{"InputCancelled enduring", event.InputCancelled{}, event.Enduring},
		{"InputQueued ephemeral", event.InputQueued{}, event.Ephemeral},
		{"TurnRejected enduring", event.TurnRejected{}, event.Enduring},
		{"TurnDone terminal is enduring", event.TurnDone{}, event.Enduring},
		{"TurnFailed terminal is enduring", event.TurnFailed{}, event.Enduring},
		{"TurnInterrupted terminal is enduring", event.TurnInterrupted{}, event.Enduring},
		{"PermissionRequested enduring", event.PermissionRequested{}, event.Enduring},
		{"UserInputRequested enduring", event.UserInputRequested{}, event.Enduring},
		{"ToolCallStarted ephemeral", event.ToolCallStarted{}, event.Ephemeral},
		{"ToolCallCompleted ephemeral", event.ToolCallCompleted{}, event.Ephemeral},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := tt.ev.Class(); got != tt.want {
				t.Errorf("Class() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestEventEndsTurn asserts only the three turn-enders report EndsTurn()==true;
// every ephemeral and non-terminal enduring event reports false.
func TestEventEndsTurn(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		ev   event.Event
		want bool
	}{
		{"TurnDone ends turn", event.TurnDone{}, true},
		{"TurnFailed ends turn", event.TurnFailed{}, true},
		{"TurnInterrupted ends turn", event.TurnInterrupted{}, true},
		{"TokenDelta does not end turn", event.TokenDelta{}, false},
		{"TurnStarted does not end turn", event.TurnStarted{}, false},
		{"StepDone does not end turn", event.StepDone{}, false},
		{"TurnFoldedInto does not end turn", event.TurnFoldedInto{}, false},
		{"InputCancelled does not end turn", event.InputCancelled{}, false},
		{"LoopIdle does not end turn", event.LoopIdle{}, false},
		{"SessionStarted does not end turn", event.SessionStarted{}, false},
		{"SessionActive does not end turn", event.SessionActive{}, false},
		{"SessionIdle does not end turn", event.SessionIdle{}, false},
		{"SessionStopped does not end turn", event.SessionStopped{}, false},
		{"HustleStarted does not end turn", event.HustleStarted{}, false},
		{"HustleCompleted does not end turn", event.HustleCompleted{}, false},
		{"HustleFailed does not end turn", event.HustleFailed{}, false},
		{"RestoreStarted does not end turn", event.RestoreStarted{}, false},
		{"RestoreDone does not end turn", event.RestoreDone{}, false},
		{"RestoreErrored does not end turn", event.RestoreErrored{}, false},
		{"WorkspaceCheckpointed does not end turn", event.WorkspaceCheckpointed{}, false},
		{"WorkspaceRestored does not end turn", event.WorkspaceRestored{}, false},
		{"ActiveLoopChanged does not end turn", event.ActiveLoopChanged{}, false},
		{"LoopInferenceChanged does not end turn", event.LoopInferenceChanged{}, false},
		{"LoopModeChanged does not end turn", event.LoopModeChanged{}, false},
		{"PermissionRequested does not end turn", event.PermissionRequested{}, false},
		{"UserInputRequested does not end turn", event.UserInputRequested{}, false},
		{"ToolCallStarted does not end turn", event.ToolCallStarted{}, false},
		{"ToolCallCompleted does not end turn", event.ToolCallCompleted{}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := tt.ev.EndsTurn(); got != tt.want {
				t.Errorf("EndsTurn() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestEventScope asserts session events report ScopeSession and loop/turn/step/
// tool events report ScopeLoop.
func TestEventScope(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		ev   event.Event
		want event.Scope
	}{
		{"SessionStarted session-scoped", event.SessionStarted{}, event.ScopeSession},
		{"SessionActive session-scoped", event.SessionActive{}, event.ScopeSession},
		{"SessionIdle session-scoped", event.SessionIdle{}, event.ScopeSession},
		{"SessionStopped session-scoped", event.SessionStopped{}, event.ScopeSession},
		{"HustleStarted session-scoped", event.HustleStarted{}, event.ScopeSession},
		{"HustleCompleted session-scoped", event.HustleCompleted{}, event.ScopeSession},
		{"HustleFailed session-scoped", event.HustleFailed{}, event.ScopeSession},
		{"RestoreStarted session-scoped", event.RestoreStarted{}, event.ScopeSession},
		{"RestoreDone session-scoped", event.RestoreDone{}, event.ScopeSession},
		{"RestoreErrored session-scoped", event.RestoreErrored{}, event.ScopeSession},
		{"WorkspaceCheckpointed session-scoped", event.WorkspaceCheckpointed{}, event.ScopeSession},
		{"WorkspaceRestored session-scoped", event.WorkspaceRestored{}, event.ScopeSession},
		{"ActiveLoopChanged session-scoped", event.ActiveLoopChanged{}, event.ScopeSession},
		{"LoopInferenceChanged loop-scoped", event.LoopInferenceChanged{}, event.ScopeLoop},
		{"LoopModeChanged loop-scoped", event.LoopModeChanged{}, event.ScopeLoop},
		{"LoopIdle loop-scoped", event.LoopIdle{}, event.ScopeLoop},
		{"TokenDelta loop-scoped", event.TokenDelta{}, event.ScopeLoop},
		{"TurnStarted loop-scoped", event.TurnStarted{}, event.ScopeLoop},
		{"StepDone loop-scoped", event.StepDone{}, event.ScopeLoop},
		{"TurnFoldedInto loop-scoped", event.TurnFoldedInto{}, event.ScopeLoop},
		{"InputCancelled loop-scoped", event.InputCancelled{}, event.ScopeLoop},
		{"TurnDone loop-scoped", event.TurnDone{}, event.ScopeLoop},
		{"TurnFailed loop-scoped", event.TurnFailed{}, event.ScopeLoop},
		{"TurnInterrupted loop-scoped", event.TurnInterrupted{}, event.ScopeLoop},
		{"PermissionRequested loop-scoped", event.PermissionRequested{}, event.ScopeLoop},
		{"UserInputRequested loop-scoped", event.UserInputRequested{}, event.ScopeLoop},
		{"ToolCallStarted loop-scoped", event.ToolCallStarted{}, event.ScopeLoop},
		{"ToolCallCompleted loop-scoped", event.ToolCallCompleted{}, event.ScopeLoop},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := tt.ev.Scope(); got != tt.want {
				t.Errorf("Scope() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestEventHeaderRoundTrip asserts EventHeader() returns the embedded Header
// verbatim — every producer-identity field is preserved, none aliased or zeroed.
func TestEventHeaderRoundTrip(t *testing.T) {
	t.Parallel()

	id := func(t *testing.T) uuid.UUID {
		t.Helper()
		u, err := uuid.New()
		if err != nil {
			t.Fatalf("uuid.New: %v", err)
		}
		return u
	}

	tests := []struct {
		name string
		hdr  event.Header
	}{
		{
			name: "full header",
			hdr: event.Header{
				Coordinates: identity.Coordinates{
					SessionID: id(t),
					LoopID:    id(t),
					TurnID:    id(t),
					StepID:    id(t),
				},
				EventID: id(t),
				Cause: identity.Cause{
					CommandID:       id(t),
					EventID:         id(t),
					ToolExecutionID: id(t),
					Coordinates:     identity.Coordinates{LoopID: id(t)},
					Agency:          identity.AgencyUser,
				},
			},
		},
		{
			name: "zero header is boundary",
			hdr:  event.Header{},
		},
		{
			name: "session-scoped header only SessionID set",
			hdr:  event.Header{Coordinates: identity.Coordinates{SessionID: id(t)}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ev := event.StepDone{Header: tt.hdr}
			if got := ev.EventHeader(); got != tt.hdr {
				t.Errorf("EventHeader() = %+v, want %+v", got, tt.hdr)
			}
		})
	}
}

// TestHeaderCreatedAt asserts Header.CreatedAt survives a JSON round-trip and is
// surfaced verbatim by EventHeader() — the creation timestamp every Enduring
// event must carry into the journal.
func TestHeaderCreatedAt(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		ts   time.Time
	}{
		{name: "non-zero UTC timestamp", ts: time.Date(2026, 6, 21, 15, 0, 0, 0, time.UTC)},
		{name: "zero time is boundary", ts: time.Time{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			hdr := event.Header{CreatedAt: tt.ts}

			data, err := json.Marshal(hdr)
			if err != nil {
				t.Fatalf("json.Marshal(Header) err = %v", err)
			}
			var got event.Header
			if err := json.Unmarshal(data, &got); err != nil {
				t.Fatalf("json.Unmarshal(Header) err = %v", err)
			}
			if !got.CreatedAt.Equal(tt.ts) {
				t.Errorf("round-trip CreatedAt = %v, want %v", got.CreatedAt, tt.ts)
			}
			ev := event.StepDone{Header: hdr}
			if !ev.EventHeader().CreatedAt.Equal(tt.ts) {
				t.Errorf("EventHeader().CreatedAt = %v, want %v", ev.EventHeader().CreatedAt, tt.ts)
			}
		})
	}
}

// TestNewEventFields asserts the new loop-machine events carry their
// distinguishing payload fields verbatim: the submit-resolution events
// (TurnStarted/TurnFoldedInto/InputCancelled) carry InputID and the user
// Message, InputCancelled carries its CancelReason, and StepDone carries the
// step's message group.
func TestNewEventFields(t *testing.T) {
	t.Parallel()

	inputID, err := uuid.New()
	if err != nil {
		t.Fatalf("uuid.New: %v", err)
	}
	userMsg := &content.UserMessage{Message: content.Message{Role: content.RoleUser}}
	stepGroup := content.AgenticMessages{
		&content.AIMessage{Message: content.Message{Role: content.RoleAssistant}},
	}

	t.Run("TurnStarted carries InputID and Message", func(t *testing.T) {
		t.Parallel()
		ev := event.TurnStarted{
			Header:    event.Header{Cause: identity.Cause{CommandID: inputID}},
			TurnIndex: 1,
			Message:   userMsg,
		}
		if ev.Cause.CommandID != inputID {
			t.Errorf("InputID = %v, want %v", ev.Cause.CommandID, inputID)
		}
		if ev.EventHeader().Cause.CommandID != inputID {
			t.Errorf("Cause.CommandID = %v, want %v (must equal InputID)", ev.EventHeader().Cause.CommandID, inputID)
		}
		if ev.Message != userMsg {
			t.Errorf("Message = %v, want %v", ev.Message, userMsg)
		}
	})

	t.Run("TurnFoldedInto carries Cause.LoopID for a hand-back", func(t *testing.T) {
		t.Parallel()
		fromLoop, err := uuid.New()
		if err != nil {
			t.Fatalf("uuid.New: %v", err)
		}
		ev := event.TurnFoldedInto{
			Header:    event.Header{Cause: identity.Cause{CommandID: inputID, Coordinates: identity.Coordinates{LoopID: fromLoop}}},
			TurnIndex: 2,
			Message:   userMsg,
		}
		if ev.EventHeader().Cause.LoopID != fromLoop {
			t.Errorf("Cause.LoopID = %v, want %v", ev.EventHeader().Cause.LoopID, fromLoop)
		}
		if ev.Message != userMsg {
			t.Errorf("Message = %v, want %v", ev.Message, userMsg)
		}
	})

	t.Run("InputCancelled carries Reason and Message", func(t *testing.T) {
		t.Parallel()
		ev := event.InputCancelled{
			Header:  event.Header{Cause: identity.Cause{CommandID: inputID}},
			Reason:  event.CancelClientRetracted,
			Message: userMsg,
		}
		if ev.Reason != event.CancelClientRetracted {
			t.Errorf("Reason = %v, want %v", ev.Reason, event.CancelClientRetracted)
		}
		if ev.Message != userMsg {
			t.Errorf("Message = %v, want %v", ev.Message, userMsg)
		}
	})

	t.Run("InputCancelled zero Message is boundary", func(t *testing.T) {
		t.Parallel()
		ev := event.InputCancelled{Reason: event.CancelTurnInterrupted}
		if ev.Message != nil {
			t.Errorf("Message = %v, want nil", ev.Message)
		}
		if ev.Reason != event.CancelTurnInterrupted {
			t.Errorf("Reason = %v, want %v", ev.Reason, event.CancelTurnInterrupted)
		}
	})

	t.Run("InputCancelled carries CancelTurnFailed reason", func(t *testing.T) {
		t.Parallel()
		ev := event.InputCancelled{
			Header:  event.Header{Cause: identity.Cause{CommandID: inputID}},
			Reason:  event.CancelTurnFailed,
			Message: userMsg,
		}
		if ev.Reason != event.CancelTurnFailed {
			t.Errorf("Reason = %v, want %v", ev.Reason, event.CancelTurnFailed)
		}
		if ev.Message != userMsg {
			t.Errorf("Message = %v, want %v", ev.Message, userMsg)
		}
	})

	t.Run("StepDone carries the step message group", func(t *testing.T) {
		t.Parallel()
		ev := event.StepDone{Header: event.Header{}, Messages: stepGroup}
		if len(ev.Messages) != len(stepGroup) {
			t.Fatalf("len(Messages) = %d, want %d", len(ev.Messages), len(stepGroup))
		}
		if ev.Messages[0] != stepGroup[0] {
			t.Errorf("Messages[0] = %v, want %v", ev.Messages[0], stepGroup[0])
		}
	})

	t.Run("StepDone nil messages is boundary", func(t *testing.T) {
		t.Parallel()
		ev := event.StepDone{}
		if ev.Messages != nil {
			t.Errorf("Messages = %v, want nil", ev.Messages)
		}
	})

	t.Run("InputQueued carries InputID and Cause.CommandID", func(t *testing.T) {
		t.Parallel()
		ev := event.InputQueued{
			Header: event.Header{Cause: identity.Cause{CommandID: inputID}},
		}
		if ev.Cause.CommandID != inputID {
			t.Errorf("InputID = %v, want %v", ev.Cause.CommandID, inputID)
		}
		if ev.EventHeader().Cause.CommandID != inputID {
			t.Errorf("Cause.CommandID = %v, want %v (must equal InputID)", ev.EventHeader().Cause.CommandID, inputID)
		}
	})

	t.Run("TurnRejected carries InputID and Reason", func(t *testing.T) {
		t.Parallel()
		ev := event.TurnRejected{
			Header: event.Header{Cause: identity.Cause{CommandID: inputID}},
			Reason: event.RejectQueueFull,
		}
		if ev.Cause.CommandID != inputID {
			t.Errorf("InputID = %v, want %v", ev.Cause.CommandID, inputID)
		}
		if ev.Reason != event.RejectQueueFull {
			t.Errorf("Reason = %v, want %v", ev.Reason, event.RejectQueueFull)
		}
		if ev.EventHeader().Cause.CommandID != inputID {
			t.Errorf("Cause.CommandID = %v, want %v (must equal InputID)", ev.EventHeader().Cause.CommandID, inputID)
		}
	})

	t.Run("TurnRejected zero Reason is RejectUnspecified boundary", func(t *testing.T) {
		t.Parallel()
		ev := event.TurnRejected{}
		if ev.Reason != event.RejectUnspecified {
			t.Errorf("zero Reason = %v, want %v (RejectUnspecified)", ev.Reason, event.RejectUnspecified)
		}
	})
}
