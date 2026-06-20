package event_test

import (
	"testing"

	"github.com/inventivepotter/urvi/internal/agent/loop/event"
	"github.com/inventivepotter/urvi/internal/tool"
	"github.com/inventivepotter/urvi/internal/uuid"
)

// newID mints a UUID or fails the test.
func newID(t *testing.T) uuid.UUID {
	t.Helper()
	u, err := uuid.New()
	if err != nil {
		t.Fatalf("uuid.New: %v", err)
	}
	return u
}

// TestToolEventsSatisfyEvent asserts each new tool event satisfies the sealed
// Event interface and round-trips its CallID where specified.
func TestToolEventsSatisfyEvent(t *testing.T) {
	t.Parallel()
	callID := newID(t)
	tests := []struct {
		name       string
		ev         event.Event
		wantCallID uuid.UUID
		getCallID  func(event.Event) uuid.UUID
	}{
		{
			name:       "PermissionRequested",
			ev:         event.PermissionRequested{CallID: callID, Request: tool.BashRequest{Command: "rm -rf /"}},
			wantCallID: callID,
			getCallID:  func(e event.Event) uuid.UUID { return e.(event.PermissionRequested).CallID },
		},
		{
			name:       "UserInputRequested",
			ev:         event.UserInputRequested{CallID: callID, Question: "proceed?", Choices: []string{"yes", "no"}},
			wantCallID: callID,
			getCallID:  func(e event.Event) uuid.UUID { return e.(event.UserInputRequested).CallID },
		},
		{
			name:       "ToolCallStarted",
			ev:         event.ToolCallStarted{CallID: callID, ToolName: "Bash", Summary: "run a command"},
			wantCallID: callID,
			getCallID:  func(e event.Event) uuid.UUID { return e.(event.ToolCallStarted).CallID },
		},
		{
			name:       "ToolCallCompleted",
			ev:         event.ToolCallCompleted{CallID: callID, IsError: true, ResultPreview: "boom"},
			wantCallID: callID,
			getCallID:  func(e event.Event) uuid.UUID { return e.(event.ToolCallCompleted).CallID },
		},
		{
			name:       "PermissionRequested zero CallID is boundary",
			ev:         event.PermissionRequested{Request: tool.UnknownRequest{Tool: "X"}},
			wantCallID: uuid.UUID{},
			getCallID:  func(e event.Event) uuid.UUID { return e.(event.PermissionRequested).CallID },
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := tt.getCallID(tt.ev); got != tt.wantCallID {
				t.Errorf("CallID = %v, want %v", got, tt.wantCallID)
			}
		})
	}
}
