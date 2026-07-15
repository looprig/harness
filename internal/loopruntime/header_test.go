package loopruntime

import (
	"errors"
	"testing"
	"time"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/identity"
)

// TestWithLoopHeaderStampsEveryEnduringType is the EXHAUSTIVE guard on
// withLoopHeader: it runs ONE instance of each Enduring loop-scoped
// event types through withLoopHeader(ev, h) with a known non-zero h and asserts
// the write-back actually took — EventID + CreatedAt (the minted persistence
// identity) AND the Coordinates/Cause carried by h all land on the returned
// event's header. A type that falls through withLoopHeader's default arm returns
// ev UNCHANGED (its EventID stays zero), so it fails here. This closes the silent
// zero-EventID-Enduring-event surface: a new Enduring loop event that someone
// forgets to add to withLoopHeader's switch is caught the moment it is added to
// this table (and a type dropped from the switch fails immediately). The list
// MUST equal loop.go's set of Enduring loop events the publish chokepoint stamps.
func TestWithLoopHeaderStampsEveryEnduringType(t *testing.T) {
	t.Parallel()

	// A fully-populated, non-zero Header so a missed write-back is unmistakable: a
	// type that falls through returns ev with its ZERO header, failing every assert.
	h := event.Header{
		Coordinates: identity.Coordinates{
			SessionID: uuid.UUID{0x11},
			LoopID:    uuid.UUID{0x22},
			TurnID:    uuid.UUID{0x33},
			StepID:    uuid.UUID{0x44},
		},
		EventID:   uuid.UUID{0x55},
		CreatedAt: time.Date(2026, 6, 21, 8, 0, 0, 0, time.UTC),
		Cause: identity.Cause{
			Coordinates: identity.Coordinates{LoopID: uuid.UUID{0x66}},
			CommandID:   uuid.UUID{0x77},
			Agency:      identity.AgencyUser,
		},
	}

	// in is one instance of each Enduring loop-scoped event type. These are exactly
	// the 19 cases withLoopHeader enumerates (the only enduring events the publish chokepoint
	// stamps); Ephemeral and session-scoped events never reach withLoopHeader.
	tests := []struct {
		name string
		in   event.Event
	}{
		{name: "TurnStarted", in: event.TurnStarted{}},
		{name: "DelegateRequestAccepted", in: event.DelegateRequestAccepted{}},
		{name: "StepDone", in: event.StepDone{}},
		{name: "TurnFoldedInto", in: event.TurnFoldedInto{}},
		{name: "InputCancelled", in: event.InputCancelled{}},
		{name: "LoopModeChanged", in: event.LoopModeChanged{}},
		{name: "LoopInferenceChanged", in: event.LoopInferenceChanged{}},
		{name: "TurnRejected", in: event.TurnRejected{}},
		{name: "LoopIdle", in: event.LoopIdle{}},
		{name: "TurnDone", in: event.TurnDone{}},
		{name: "TurnFailed", in: event.TurnFailed{}},
		{name: "TurnInterrupted", in: event.TurnInterrupted{}},
		{name: "PermissionRequested", in: event.PermissionRequested{}},
		{name: "PermissionDecided", in: event.PermissionDecided{}},
		{name: "UserInputRequested", in: event.UserInputRequested{}},
		{name: "CompactionCommitted", in: event.CompactionCommitted{}},
		{name: "CompactionRejected", in: event.CompactionRejected{}},
		{name: "CompactWaiterResolved", in: event.CompactWaiterResolved{}},
		{name: "CompactWaiterRejected", in: event.CompactWaiterRejected{}},
	}

	// Guard the count so adding an Enduring loop event without extending this
	// table is itself a failure (the test must enumerate every type).
	const wantEnduringLoopTypes = 19
	if len(tests) != wantEnduringLoopTypes {
		t.Fatalf("table has %d types, want %d Enduring loop-scoped event types", len(tests), wantEnduringLoopTypes)
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := withLoopHeader(tt.in, h)
			gh := got.EventHeader()
			// The minted persistence identity must be written back. A type that fell
			// through the default arm keeps its zero header, so EventID stays zero here.
			if gh.EventID != h.EventID {
				t.Errorf("EventID = %v, want %v (write-back missing — type fell through default arm?)", gh.EventID, h.EventID)
			}
			if !gh.CreatedAt.Equal(h.CreatedAt) {
				t.Errorf("CreatedAt = %v, want %v (write-back missing)", gh.CreatedAt, h.CreatedAt)
			}
			// withLoopHeader REPLACES the whole header with h, so the Coordinates and
			// Cause h carries must be preserved on the returned event too.
			if gh.Coordinates != h.Coordinates {
				t.Errorf("Coordinates = %+v, want %+v (header write-back lost coordinates)", gh.Coordinates, h.Coordinates)
			}
			if gh.Cause != h.Cause {
				t.Errorf("Cause = %+v, want %+v (header write-back lost cause)", gh.Cause, h.Cause)
			}
		})
	}
}

func TestStampLoopHeaderCompactionEvents(t *testing.T) {
	t.Parallel()

	sessionID := mustID(t)
	loopID := mustID(t)
	turnID := mustID(t)
	tests := []struct {
		name string
		ev   event.Event
	}{
		{name: "started", ev: event.CompactionStarted{}},
		{name: "committed", ev: event.CompactionCommitted{}},
		{name: "rejected", ev: event.CompactionRejected{}},
		{name: "waiter resolved", ev: event.CompactWaiterResolved{}},
		{name: "waiter rejected", ev: event.CompactWaiterRejected{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := stampLoopHeader(tt.ev, sessionID, loopID, turnID).EventHeader()
			if got.SessionID != sessionID || got.LoopID != loopID {
				t.Errorf("session/loop = %v/%v, want %v/%v", got.SessionID, got.LoopID, sessionID, loopID)
			}
			if !got.TurnID.IsZero() {
				t.Errorf("TurnID = %v, want zero for loop-scoped compaction event", got.TurnID)
			}
		})
	}
}

func TestStampLoopEventPreservesCompactWaiterReplyID(t *testing.T) {
	t.Parallel()

	createdAt := time.Date(2026, 7, 14, 9, 0, 0, 123, time.UTC)
	sessionID := uuid.UUID{0xa1}
	loopID := uuid.UUID{0xa2}
	commandID := uuid.UUID{0xa3}
	attemptID := event.CompactAttemptID(uuid.UUID{0xa4})
	committedID := uuid.UUID{0xa5}
	freshID := uuid.UUID{0xff}
	cause := identity.Cause{CommandID: commandID, Agency: identity.AgencyUser}
	tests := []struct {
		name string
		ev   event.Event
		want uuid.UUID
	}{
		{
			name: "resolved preserves deterministic id",
			ev: event.CompactWaiterResolved{
				Header:           event.Header{EventID: event.CompactWaiterReplyID(attemptID, commandID, true), Cause: cause},
				AttemptID:        attemptID,
				CommittedEventID: committedID,
			},
			want: event.CompactWaiterReplyID(attemptID, commandID, true),
		},
		{
			name: "rejected preserves deterministic id",
			ev: event.CompactWaiterRejected{
				Header:    event.Header{EventID: event.CompactWaiterReplyID(attemptID, commandID, false), Cause: cause},
				AttemptID: attemptID,
				Reason:    event.CompactRejectCanceled,
			},
			want: event.CompactWaiterReplyID(attemptID, commandID, false),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			mintCalls := 0
			factory := event.NewFactory(func() (uuid.UUID, error) {
				mintCalls++
				return freshID, nil
			}, func() time.Time { return createdAt })
			got, err := stampLoopEvent(tt.ev, factory, sessionID, loopID, uuid.UUID{})
			if err != nil {
				t.Fatalf("stampLoopEvent: %v", err)
			}
			header := got.EventHeader()
			if header.EventID != tt.want {
				t.Errorf("EventID = %v, want deterministic %v", header.EventID, tt.want)
			}
			if !header.CreatedAt.Equal(createdAt) {
				t.Errorf("CreatedAt = %v, want %v", header.CreatedAt, createdAt)
			}
			if header.SessionID != sessionID || header.LoopID != loopID || !header.TurnID.IsZero() || !header.StepID.IsZero() {
				t.Errorf("Coordinates = %+v, want loop-scoped %v/%v", header.Coordinates, sessionID, loopID)
			}
			if header.Cause != cause {
				t.Errorf("Cause = %+v, want preserved %+v", header.Cause, cause)
			}
			if mintCalls != 0 {
				t.Errorf("fresh ID generator calls = %d, want 0 for deterministic reply", mintCalls)
			}
			if err := event.ValidateEvent(got); err != nil {
				t.Errorf("ValidateEvent: %v", err)
			}
		})
	}
}

func TestStampLoopEventRejectsMalformedCompactWaiterReplyID(t *testing.T) {
	t.Parallel()

	commandID := uuid.UUID{0xb1}
	attemptID := event.CompactAttemptID(uuid.UUID{0xb2})
	deterministicResolved := event.CompactWaiterReplyID(attemptID, commandID, true)
	deterministicRejected := event.CompactWaiterReplyID(attemptID, commandID, false)
	tests := []struct {
		name     string
		ev       event.Event
		wantRule event.Rule
	}{
		{
			name: "resolved zero id",
			ev: event.CompactWaiterResolved{
				Header:           event.Header{Cause: identity.Cause{CommandID: commandID}},
				AttemptID:        attemptID,
				CommittedEventID: uuid.UUID{0xb3},
			},
			wantRule: event.RuleRequired,
		},
		{
			name: "resolved wrong id",
			ev: event.CompactWaiterResolved{
				Header:           event.Header{EventID: deterministicRejected, Cause: identity.Cause{CommandID: commandID}},
				AttemptID:        attemptID,
				CommittedEventID: uuid.UUID{0xb3},
			},
			wantRule: event.RuleInvalid,
		},
		{
			name: "rejected zero id",
			ev: event.CompactWaiterRejected{
				Header:    event.Header{Cause: identity.Cause{CommandID: commandID}},
				AttemptID: attemptID,
				Reason:    event.CompactRejectCanceled,
			},
			wantRule: event.RuleRequired,
		},
		{
			name: "rejected wrong id",
			ev: event.CompactWaiterRejected{
				Header:    event.Header{EventID: deterministicResolved, Cause: identity.Cause{CommandID: commandID}},
				AttemptID: attemptID,
				Reason:    event.CompactRejectCanceled,
			},
			wantRule: event.RuleInvalid,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			factory := event.NewFactory(func() (uuid.UUID, error) { return uuid.UUID{0xfe}, nil }, time.Now)
			got, err := stampLoopEvent(tt.ev, factory, uuid.UUID{0xb4}, uuid.UUID{0xb5}, uuid.UUID{})
			if got != nil {
				t.Errorf("stampLoopEvent event = %#v, want nil", got)
			}
			var invalid *event.InvalidEventError
			if !errors.As(err, &invalid) {
				t.Fatalf("stampLoopEvent error = %T %v, want *event.InvalidEventError", err, err)
			}
			if invalid.Field != event.FieldEventID || invalid.Rule != tt.wantRule {
				t.Errorf("validation = %+v, want field=%q rule=%q", invalid, event.FieldEventID, tt.wantRule)
			}
		})
	}
}

func TestStampLoopEventOrdinaryEventsUseFreshID(t *testing.T) {
	t.Parallel()

	createdAt := time.Date(2026, 7, 14, 10, 0, 0, 456, time.UTC)
	freshID := uuid.UUID{0xc1}
	sessionID := uuid.UUID{0xc2}
	loopID := uuid.UUID{0xc3}
	cause := identity.Cause{CommandID: uuid.UUID{0xc4}, Agency: identity.AgencyMachine}
	tests := []struct {
		name       string
		previousID uuid.UUID
	}{
		{name: "zero id is freshly minted"},
		{name: "preassigned id is replaced", previousID: uuid.UUID{0xc5}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			mintCalls := 0
			factory := event.NewFactory(func() (uuid.UUID, error) {
				mintCalls++
				return freshID, nil
			}, func() time.Time { return createdAt })
			got, err := stampLoopEvent(
				event.LoopIdle{Header: event.Header{
					Coordinates: identity.Coordinates{SessionID: sessionID, LoopID: loopID},
					EventID:     tt.previousID,
					Cause:       cause,
				}},
				factory,
				sessionID,
				loopID,
				uuid.UUID{},
			)
			if err != nil {
				t.Fatalf("stampLoopEvent: %v", err)
			}
			header := got.EventHeader()
			if header.EventID != freshID {
				t.Errorf("EventID = %v, want fresh %v", header.EventID, freshID)
			}
			if !header.CreatedAt.Equal(createdAt) {
				t.Errorf("CreatedAt = %v, want %v", header.CreatedAt, createdAt)
			}
			if header.SessionID != sessionID || header.LoopID != loopID || header.Cause != cause {
				t.Errorf("header identity = %+v, want coordinates %v/%v and cause %+v", header, sessionID, loopID, cause)
			}
			if mintCalls != 1 {
				t.Errorf("fresh ID generator calls = %d, want 1", mintCalls)
			}
			if err := event.ValidateEvent(got); err != nil {
				t.Errorf("ValidateEvent: %v", err)
			}
		})
	}
}

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

// TestStampStepID proves stampStepID sets Coordinates.StepID on the five tool/gate
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
			name:        "PermissionDecided is stamped",
			in:          event.PermissionDecided{ToolExecutionID: callID},
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
