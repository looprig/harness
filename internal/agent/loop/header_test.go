package loop

import (
	"testing"

	"github.com/inventivepotter/urvi/internal/agent/loop/event"
	"github.com/inventivepotter/urvi/internal/agent/loop/identity"
	"github.com/inventivepotter/urvi/internal/uuid"
)

// TestStampLoopHeaderReplyEvents proves stampLoopHeader stamps the loop-scoped
// reply events InputQueued and TurnRejected: it fills the zero SessionID/LoopID
// from the loop identity while PRESERVING the producer-set Cause.CommandID and
// Cause.LoopID (which carry the submit id and the producing subagent loop).
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
	triggeredBy := mustID(t)

	tests := []struct {
		name string
		in   event.Event
		want func(t *testing.T, got event.Event)
	}{
		{
			name: "InputQueued fills session+loop, preserves causation/triggeredBy, no turn",
			in: event.InputQueued{
				Header: event.Header{Cause: identity.Cause{CommandID: causationID, Coordinates: identity.Coordinates{LoopID: triggeredBy}}},
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
				// The submit command id IS the cause, so checking Cause.CommandID ==
				// causationID covers what the former (now removed) InputID field did —
				// there is no separate InputID anymore.
				if q.Cause.CommandID != causationID || q.Cause.LoopID != triggeredBy {
					t.Errorf("causation/triggeredBy = %v/%v, want %v/%v (must be preserved)", q.Cause.CommandID, q.Cause.LoopID, causationID, triggeredBy)
				}
			},
		},
		{
			name: "TurnRejected fills session+loop, preserves causation/triggeredBy/reason, no turn",
			in: event.TurnRejected{
				Header: event.Header{Cause: identity.Cause{CommandID: causationID, Coordinates: identity.Coordinates{LoopID: triggeredBy}}},
				Reason: event.RejectQueueFull,
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
				if r.Cause.CommandID != causationID || r.Cause.LoopID != triggeredBy {
					t.Errorf("causation/triggeredBy = %v/%v, want %v/%v (must be preserved)", r.Cause.CommandID, r.Cause.LoopID, causationID, triggeredBy)
				}
				if r.Reason != event.RejectQueueFull {
					t.Errorf("Reason = %v, want RejectQueueFull", r.Reason)
				}
			},
		},
		{
			name: "TurnRejected with zero session/loop is filled from loop identity",
			in: event.TurnRejected{
				Header: event.Header{},
				Reason: event.RejectShuttingDown,
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
				Header: event.Header{Coordinates: identity.Coordinates{SessionID: uuid.UUID{1}, LoopID: uuid.UUID{2}}},
				Reason: event.RejectShuttingDown,
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

// TestStampStepID proves stampStepID sets Coordinates.StepID on the four tool/gate
// events ONLY, and leaves every other event (including events whose StepID must
// stay zero, and TokenDelta) untouched. The stamped StepID survives a subsequent
// stampLoopHeader (which fills only zero header fields), which is the property the
// "ToolExecutionID requires StepID" invariant depends on.
func TestStampStepID(t *testing.T) {
	t.Parallel()

	stepID := mustID(t)
	callID := mustID(t)

	// stepIDOf extracts Coordinates.StepID from any event via its header.
	stepIDOf := func(ev event.Event) uuid.UUID { return ev.EventHeader().StepID }

	tests := []struct {
		name        string
		in          event.Event
		wantStamped bool // true → the event's StepID must equal stepID after stamping
	}{
		{
			name:        "PermissionRequested is stamped",
			in:          event.PermissionRequested{ToolExecutionID: callID},
			wantStamped: true,
		},
		{
			name:        "UserInputRequested is stamped",
			in:          event.UserInputRequested{ToolExecutionID: callID},
			wantStamped: true,
		},
		{
			name:        "ToolCallStarted is stamped",
			in:          event.ToolCallStarted{ToolExecutionID: callID},
			wantStamped: true,
		},
		{
			name:        "ToolCallCompleted is stamped",
			in:          event.ToolCallCompleted{ToolExecutionID: callID},
			wantStamped: true,
		},
		{
			name:        "TurnStarted (StepID must be zero) is untouched",
			in:          event.TurnStarted{},
			wantStamped: false,
		},
		{
			name:        "TurnFoldedInto (StepID must be zero) is untouched",
			in:          event.TurnFoldedInto{},
			wantStamped: false,
		},
		{
			name:        "TurnDone (StepID must be zero) is untouched",
			in:          event.TurnDone{},
			wantStamped: false,
		},
		{
			name:        "StepDone already carries its own StepID; stampStepID leaves it untouched",
			in:          event.StepDone{},
			wantStamped: false,
		},
		{
			name:        "TokenDelta is not a tool/gate event; untouched",
			in:          event.TokenDelta{},
			wantStamped: false,
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := stampStepID(tt.in, stepID)
			gotStep := stepIDOf(got)
			if tt.wantStamped {
				if gotStep != stepID {
					t.Errorf("StepID = %v, want %v (must be stamped)", gotStep, stepID)
				}
			} else if !gotStep.IsZero() {
				t.Errorf("StepID = %v, want zero (must NOT be stamped)", gotStep)
			}
		})
	}
}

// TestStampStepIDPreservedThroughStampLoopHeader proves the StepID stamped at emit
// time survives the loop's later header completion: stampLoopHeader fills only zero
// header fields, so a tool/gate event ends up with StepID == the active step's id
// AND the loop's SessionID/LoopID/TurnID — exactly the full quartet the
// ToolExecutionID invariant requires.
func TestStampStepIDPreservedThroughStampLoopHeader(t *testing.T) {
	t.Parallel()

	sessionID := mustID(t)
	loopID := mustID(t)
	turnID := mustID(t)
	stepID := mustID(t)
	callID := mustID(t)

	in := event.ToolCallStarted{ToolExecutionID: callID}
	stamped := stampStepID(in, stepID)
	final := stampLoopHeader(stamped, sessionID, loopID, turnID)

	tcs, ok := final.(event.ToolCallStarted)
	if !ok {
		t.Fatalf("got %T, want event.ToolCallStarted", final)
	}
	if tcs.SessionID != sessionID || tcs.LoopID != loopID || tcs.TurnID != turnID {
		t.Errorf("session/loop/turn = %v/%v/%v, want %v/%v/%v",
			tcs.SessionID, tcs.LoopID, tcs.TurnID, sessionID, loopID, turnID)
	}
	if tcs.StepID != stepID {
		t.Errorf("StepID = %v, want %v (must survive stampLoopHeader)", tcs.StepID, stepID)
	}
	if tcs.ToolExecutionID != callID {
		t.Errorf("ToolExecutionID = %v, want %v", tcs.ToolExecutionID, callID)
	}
}

// TestStepStampingEmit proves the emit wrapper threads each event through
// stampStepID: tool/gate events emitted through it carry the step's StepID, while a
// non-tool event passes through unstamped.
func TestStepStampingEmit(t *testing.T) {
	t.Parallel()

	stepID := mustID(t)
	var got []event.Event
	emit := stepStampingEmit(func(ev event.Event) { got = append(got, ev) }, stepID)

	emit(event.ToolCallStarted{})
	emit(event.TokenDelta{})

	if len(got) != 2 {
		t.Fatalf("emitted %d events, want 2", len(got))
	}
	if sid := got[0].EventHeader().StepID; sid != stepID {
		t.Errorf("ToolCallStarted StepID = %v, want %v", sid, stepID)
	}
	if sid := got[1].EventHeader().StepID; !sid.IsZero() {
		t.Errorf("TokenDelta StepID = %v, want zero (not a tool/gate event)", sid)
	}
}
