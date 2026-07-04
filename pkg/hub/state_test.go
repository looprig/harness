package hub

import (
	"testing"

	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/core/uuid"
)

// TestSessionStateZeroValue confirms the enum zero value: a freshly built
// sessionState is SessionIdle with an empty (here nil-but-initialized) active set.
func TestSessionStateZeroValue(t *testing.T) {
	t.Parallel()
	var s sessionState
	if s.phase != SessionIdle {
		t.Errorf("zero sessionState phase = %v, want SessionIdle", s.phase)
	}
	if len(s.active) != 0 {
		t.Errorf("zero sessionState active len = %d, want 0", len(s.active))
	}
}

// TestActivityKeyCoexist proves {loop, X} and {wake, X} are distinct entries that
// coexist in the active set for the same uuid.
func TestActivityKeyCoexist(t *testing.T) {
	t.Parallel()
	x := mustID(t)
	s := newSessionState()
	s.add(activityKey{kind: kindLoop, id: x})
	s.add(activityKey{kind: kindWake, id: x})
	if len(s.active) != 2 {
		t.Fatalf("active len = %d, want 2 ({loop,X} and {wake,X} coexist)", len(s.active))
	}
	s.remove(activityKey{kind: kindLoop, id: x})
	if len(s.active) != 1 {
		t.Fatalf("active len after removing {loop,X} = %d, want 1 ({wake,X} persists)", len(s.active))
	}
	if _, ok := s.active[activityKey{kind: kindWake, id: x}]; !ok {
		t.Errorf("{wake,X} not present after removing {loop,X}")
	}
}

// TestApplyActivity covers the edge-derivation table: which active mutation
// crosses which Idle/Active edge, and the SessionStopped no-op.
func TestApplyActivity(t *testing.T) {
	t.Parallel()
	x := mustID(t)
	parent := mustID(t)
	subagent := mustID(t)
	session := mustID(t)

	type wantEvent uint8
	const (
		wantNone wantEvent = iota
		wantActive
		wantIdle
	)

	tests := []struct {
		name       string
		seed       func(s *sessionState) // pre-state before the op under test
		mutate     func(s *sessionState)
		want       wantEvent
		wantPhase  SessionPhase
		startPhase SessionPhase
	}{
		{
			name:       "add from empty -> SessionActive",
			seed:       func(s *sessionState) {},
			mutate:     func(s *sessionState) { s.add(activityKey{kind: kindLoop, id: x}) },
			want:       wantActive,
			wantPhase:  SessionActive,
			startPhase: SessionIdle,
		},
		{
			name:       "remove to empty -> SessionIdle (caller wakes waiters)",
			seed:       func(s *sessionState) { s.add(activityKey{kind: kindLoop, id: x}); s.phase = SessionActive },
			mutate:     func(s *sessionState) { s.remove(activityKey{kind: kindLoop, id: x}) },
			want:       wantIdle,
			wantPhase:  SessionIdle,
			startPhase: SessionActive,
		},
		{
			name: "idempotent add crosses no second edge",
			seed: func(s *sessionState) { s.add(activityKey{kind: kindLoop, id: x}); s.phase = SessionActive },
			mutate: func(s *sessionState) {
				s.add(activityKey{kind: kindLoop, id: x}) // already present
			},
			want:       wantNone,
			wantPhase:  SessionActive,
			startPhase: SessionActive,
		},
		{
			name: "remove wake and add loop in one step crosses no edge",
			seed: func(s *sessionState) { s.add(activityKey{kind: kindWake, id: subagent}); s.phase = SessionActive },
			mutate: func(s *sessionState) {
				s.remove(activityKey{kind: kindWake, id: subagent})
				s.add(activityKey{kind: kindLoop, id: parent})
			},
			want:       wantNone,
			wantPhase:  SessionActive,
			startPhase: SessionActive,
		},
		{
			name:       "add wake from empty -> SessionActive",
			seed:       func(s *sessionState) {},
			mutate:     func(s *sessionState) { s.add(activityKey{kind: kindWake, id: subagent}) },
			want:       wantActive,
			wantPhase:  SessionActive,
			startPhase: SessionIdle,
		},
		{
			name: "remove non-final entry crosses no edge",
			seed: func(s *sessionState) {
				s.add(activityKey{kind: kindLoop, id: x})
				s.add(activityKey{kind: kindWake, id: subagent})
				s.phase = SessionActive
			},
			mutate:     func(s *sessionState) { s.remove(activityKey{kind: kindLoop, id: x}) },
			want:       wantNone,
			wantPhase:  SessionActive,
			startPhase: SessionActive,
		},
		{
			name:       "stopped phase is a no-op returning nil",
			seed:       func(s *sessionState) { s.phase = SessionStopped },
			mutate:     func(s *sessionState) { s.add(activityKey{kind: kindLoop, id: x}) },
			want:       wantNone,
			wantPhase:  SessionStopped,
			startPhase: SessionStopped,
		},
		{
			name:       "remove from empty underflow-safe, no edge",
			seed:       func(s *sessionState) {},
			mutate:     func(s *sessionState) { s.remove(activityKey{kind: kindLoop, id: x}) },
			want:       wantNone,
			wantPhase:  SessionIdle,
			startPhase: SessionIdle,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			s := newSessionState()
			tt.seed(&s)

			got := s.applyActivity(session, func() { tt.mutate(&s) })

			// In the stopped no-op case, mutate must NOT have run.
			if tt.startPhase == SessionStopped {
				if len(s.active) != 0 {
					t.Errorf("stopped applyActivity ran mutate: active len = %d, want 0", len(s.active))
				}
			}

			switch tt.want {
			case wantNone:
				if got != nil {
					t.Errorf("applyActivity returned %T, want nil", got)
				}
			case wantActive:
				ev, ok := got.(event.SessionActive)
				if !ok {
					t.Fatalf("applyActivity returned %T, want event.SessionActive", got)
				}
				if ev.SessionID != session {
					t.Errorf("SessionActive.SessionID = %v, want %v", ev.SessionID, session)
				}
				if ev.LoopID != (uuid.UUID{}) {
					t.Errorf("SessionActive.LoopID = %v, want zero", ev.LoopID)
				}
			case wantIdle:
				ev, ok := got.(event.SessionIdle)
				if !ok {
					t.Fatalf("applyActivity returned %T, want event.SessionIdle", got)
				}
				if ev.SessionID != session {
					t.Errorf("SessionIdle.SessionID = %v, want %v", ev.SessionID, session)
				}
			}

			if s.phase != tt.wantPhase {
				t.Errorf("phase after applyActivity = %v, want %v", s.phase, tt.wantPhase)
			}
		})
	}
}

// TestApplyActivitySetSemantics proves active is a set: a doubled add does not
// double-count, so a single matching remove returns to empty and fires Idle.
func TestApplyActivitySetSemantics(t *testing.T) {
	t.Parallel()
	x := mustID(t)
	session := mustID(t)
	s := newSessionState()

	// add X, add X again (idempotent) -> still one entry, Active.
	_ = s.applyActivity(session, func() { s.add(activityKey{kind: kindLoop, id: x}) })
	_ = s.applyActivity(session, func() { s.add(activityKey{kind: kindLoop, id: x}) })
	if len(s.active) != 1 {
		t.Fatalf("active len after doubled add = %d, want 1 (set semantics)", len(s.active))
	}

	// one remove returns to empty and fires Idle.
	got := s.applyActivity(session, func() { s.remove(activityKey{kind: kindLoop, id: x}) })
	if _, ok := got.(event.SessionIdle); !ok {
		t.Fatalf("single remove after doubled add returned %T, want SessionIdle", got)
	}
}
