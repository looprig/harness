package event_test

import (
	"testing"

	"github.com/ciram-co/looprig/pkg/event"
	"github.com/ciram-co/looprig/pkg/tool"
	"github.com/ciram-co/looprig/pkg/uuid"
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
// Event interface and round-trips its ToolExecutionID where specified.
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
			ev:         event.PermissionRequested{ToolExecutionID: callID, Request: tool.BashRequest{Command: "rm -rf /"}},
			wantCallID: callID,
			getCallID:  func(e event.Event) uuid.UUID { return e.(event.PermissionRequested).ToolExecutionID },
		},
		{
			name:       "UserInputRequested",
			ev:         event.UserInputRequested{ToolExecutionID: callID, Question: "proceed?", Choices: []string{"yes", "no"}},
			wantCallID: callID,
			getCallID:  func(e event.Event) uuid.UUID { return e.(event.UserInputRequested).ToolExecutionID },
		},
		{
			name:       "ToolCallStarted",
			ev:         event.ToolCallStarted{ToolExecutionID: callID, ToolName: "Bash", Summary: "run a command"},
			wantCallID: callID,
			getCallID:  func(e event.Event) uuid.UUID { return e.(event.ToolCallStarted).ToolExecutionID },
		},
		{
			name:       "ToolCallCompleted",
			ev:         event.ToolCallCompleted{ToolExecutionID: callID, IsError: true, ResultPreview: "boom"},
			wantCallID: callID,
			getCallID:  func(e event.Event) uuid.UUID { return e.(event.ToolCallCompleted).ToolExecutionID },
		},
		{
			name:       "PermissionRequested zero ToolExecutionID is boundary",
			ev:         event.PermissionRequested{Request: tool.UnknownRequest{Tool: "X"}},
			wantCallID: uuid.UUID{},
			getCallID:  func(e event.Event) uuid.UUID { return e.(event.PermissionRequested).ToolExecutionID },
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := tt.getCallID(tt.ev); got != tt.wantCallID {
				t.Errorf("ToolExecutionID = %v, want %v", got, tt.wantCallID)
			}
		})
	}
}
