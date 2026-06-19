package loop

import (
	"testing"

	"github.com/inventivepotter/urvi/internal/agent/loop/event"
	"github.com/inventivepotter/urvi/internal/uuid"
)

// TestStampLoopHeaderReplyEvents proves stampLoopHeader stamps the loop-scoped
// reply events InputQueued and TurnRejected: it fills the zero SessionID/LoopID
// from the loop identity while PRESERVING the producer-set CausationID and
// TriggeredByLoopID (which carry the submit id and the producing subagent loop).
// These two events have no turn (they resolve before a turn exists), so TurnID
// stays zero — they are loop-scoped, not turn-scoped.
func TestStampLoopHeaderReplyEvents(t *testing.T) {
	t.Parallel()

	sessionID := mustID(t)
	loopID := mustID(t)
	// turnID is deliberately non-zero to prove a loop-scoped reply event does NOT
	// inherit the active turn id (it carries no turn).
	turnID := mustID(t)
	causationID := mustID(t)
	inputID := causationID
	triggeredBy := mustID(t)

	tests := []struct {
		name string
		in   event.Event
		want func(t *testing.T, got event.Event)
	}{
		{
			name: "InputQueued fills session+loop, preserves causation/triggeredBy, no turn",
			in: event.InputQueued{
				Header:  event.Header{CausationID: causationID, TriggeredByLoopID: triggeredBy},
				InputID: inputID,
			},
			want: func(t *testing.T, got event.Event) {
				q, ok := got.(event.InputQueued)
				if !ok {
					t.Fatalf("got %T, want event.InputQueued", got)
				}
				if q.SessionID != sessionID || q.LoopID != loopID {
					t.Errorf("session/loop = %v/%v, want %v/%v", q.SessionID, q.LoopID, sessionID, loopID)
				}
				if !q.TurnID.IsZero() {
					t.Errorf("TurnID = %v, want zero (loop-scoped, no turn yet)", q.TurnID)
				}
				if q.CausationID != causationID || q.TriggeredByLoopID != triggeredBy {
					t.Errorf("causation/triggeredBy = %v/%v, want %v/%v (must be preserved)", q.CausationID, q.TriggeredByLoopID, causationID, triggeredBy)
				}
				if q.InputID != inputID {
					t.Errorf("InputID = %v, want %v", q.InputID, inputID)
				}
			},
		},
		{
			name: "TurnRejected fills session+loop, preserves causation/triggeredBy/reason, no turn",
			in: event.TurnRejected{
				Header:  event.Header{CausationID: causationID, TriggeredByLoopID: triggeredBy},
				InputID: inputID,
				Reason:  event.RejectQueueFull,
			},
			want: func(t *testing.T, got event.Event) {
				r, ok := got.(event.TurnRejected)
				if !ok {
					t.Fatalf("got %T, want event.TurnRejected", got)
				}
				if r.SessionID != sessionID || r.LoopID != loopID {
					t.Errorf("session/loop = %v/%v, want %v/%v", r.SessionID, r.LoopID, sessionID, loopID)
				}
				if !r.TurnID.IsZero() {
					t.Errorf("TurnID = %v, want zero (loop-scoped, no turn yet)", r.TurnID)
				}
				if r.CausationID != causationID || r.TriggeredByLoopID != triggeredBy {
					t.Errorf("causation/triggeredBy = %v/%v, want %v/%v (must be preserved)", r.CausationID, r.TriggeredByLoopID, causationID, triggeredBy)
				}
				if r.Reason != event.RejectQueueFull {
					t.Errorf("Reason = %v, want RejectQueueFull", r.Reason)
				}
			},
		},
		{
			name: "TurnRejected with zero session/loop is filled from loop identity",
			in: event.TurnRejected{
				Header:  event.Header{},
				InputID: inputID,
				Reason:  event.RejectShuttingDown,
			},
			want: func(t *testing.T, got event.Event) {
				r := got.(event.TurnRejected)
				if r.SessionID != sessionID || r.LoopID != loopID {
					t.Errorf("session/loop = %v/%v, want %v/%v", r.SessionID, r.LoopID, sessionID, loopID)
				}
			},
		},
		{
			name: "TurnRejected pre-set session/loop is preserved (only zero fields filled)",
			in: event.TurnRejected{
				Header:  event.Header{SessionID: uuid.UUID{1}, LoopID: uuid.UUID{2}},
				InputID: inputID,
				Reason:  event.RejectBusy,
			},
			want: func(t *testing.T, got event.Event) {
				r := got.(event.TurnRejected)
				if r.SessionID != (uuid.UUID{1}) || r.LoopID != (uuid.UUID{2}) {
					t.Errorf("session/loop = %v/%v, want preserved 1/2", r.SessionID, r.LoopID)
				}
			},
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := stampLoopHeader(tt.in, sessionID, loopID, turnID)
			tt.want(t, got)
		})
	}
}
