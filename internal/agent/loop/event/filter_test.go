package event_test

import (
	"testing"

	"github.com/inventivepotter/urvi/internal/agent/loop/event"
	"github.com/inventivepotter/urvi/internal/uuid"
)

// mustID mints a non-zero UUID for tests or fails fast.
func mustID(t *testing.T) uuid.UUID {
	t.Helper()
	id, err := uuid.New()
	if err != nil {
		t.Fatalf("uuid.New() error = %v", err)
	}
	return id
}

// TestLoopScopeMatches covers the LoopScope.Matches predicate: All short-circuits
// to true; otherwise membership in the Loops set decides; an empty/nil Loops set
// matches nothing.
func TestLoopScopeMatches(t *testing.T) {
	t.Parallel()
	loopA := mustID(t)
	loopB := mustID(t)
	tests := []struct {
		name   string
		scope  event.LoopScope
		loopID uuid.UUID
		want   bool
	}{
		{
			name:   "All short-circuits to true",
			scope:  event.LoopScope{All: true},
			loopID: loopA,
			want:   true,
		},
		{
			name:   "All true with empty Loops still matches",
			scope:  event.LoopScope{All: true, Loops: map[uuid.UUID]struct{}{}},
			loopID: loopA,
			want:   true,
		},
		{
			name:   "member of Loops matches",
			scope:  event.LoopScope{Loops: map[uuid.UUID]struct{}{loopA: {}}},
			loopID: loopA,
			want:   true,
		},
		{
			name:   "non-member of Loops does not match",
			scope:  event.LoopScope{Loops: map[uuid.UUID]struct{}{loopA: {}}},
			loopID: loopB,
			want:   false,
		},
		{
			name:   "empty Loops map matches nothing",
			scope:  event.LoopScope{Loops: map[uuid.UUID]struct{}{}},
			loopID: loopA,
			want:   false,
		},
		{
			name:   "nil Loops map matches nothing",
			scope:  event.LoopScope{},
			loopID: loopA,
			want:   false,
		},
		{
			name:   "zero loop id non-member does not match",
			scope:  event.LoopScope{Loops: map[uuid.UUID]struct{}{loopA: {}}},
			loopID: uuid.UUID{},
			want:   false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := tt.scope.Matches(tt.loopID); got != tt.want {
				t.Errorf("Matches(%v) = %v, want %v", tt.loopID, got, tt.want)
			}
		})
	}
}

// TestShouldDeliver covers the fan-out filter: session-scoped events always
// deliver (bypassing LoopScope); loop-scoped events are matched by the
// class-appropriate LoopScope against the producer LoopID.
func TestShouldDeliver(t *testing.T) {
	t.Parallel()
	primary := mustID(t)
	subagent := mustID(t)
	session := mustID(t)

	// A filter that streams live tokens from the primary loop only, but takes
	// enduring (results/gates/terminals) from every loop.
	tuiFilter := event.EventFilter{
		Ephemeral: event.LoopScope{Loops: map[uuid.UUID]struct{}{primary: {}}},
		Enduring:  event.LoopScope{All: true},
	}
	// A filter that wants nothing loop-scoped (empty scopes), to prove session
	// events still bypass it.
	emptyFilter := event.EventFilter{}

	tests := []struct {
		name   string
		filter event.EventFilter
		ev     event.Event
		want   bool
	}{
		{
			name:   "session-scoped SessionStarted always delivers despite empty filter",
			filter: emptyFilter,
			ev:     event.SessionStarted{Header: event.Header{SessionID: session}},
			want:   true,
		},
		{
			name:   "session-scoped SessionIdle always delivers despite empty filter",
			filter: emptyFilter,
			ev:     event.SessionIdle{Header: event.Header{SessionID: session}},
			want:   true,
		},
		{
			name:   "session-scoped SessionStopped always delivers",
			filter: emptyFilter,
			ev:     event.SessionStopped{Header: event.Header{SessionID: session}},
			want:   true,
		},
		{
			name:   "ephemeral TokenDelta from primary matches ephemeral scope",
			filter: tuiFilter,
			ev:     event.TokenDelta{Header: event.Header{SessionID: session, LoopID: primary}},
			want:   true,
		},
		{
			name:   "ephemeral TokenDelta from subagent does not match ephemeral scope",
			filter: tuiFilter,
			ev:     event.TokenDelta{Header: event.Header{SessionID: session, LoopID: subagent}},
			want:   false,
		},
		{
			name:   "enduring LoopIdle from subagent matches All enduring scope",
			filter: tuiFilter,
			ev:     event.LoopIdle{Header: event.Header{SessionID: session, LoopID: subagent}},
			want:   true,
		},
		{
			name:   "enduring StepDone from primary matches All enduring scope",
			filter: tuiFilter,
			ev:     event.StepDone{Header: event.Header{SessionID: session, LoopID: primary}},
			want:   true,
		},
		{
			name:   "enduring TurnStarted from subagent matches All enduring scope",
			filter: tuiFilter,
			ev:     event.TurnStarted{Header: event.Header{SessionID: session, LoopID: subagent}},
			want:   true,
		},
		{
			name:   "terminal TurnDone (enduring class) matched by enduring scope",
			filter: tuiFilter,
			ev:     event.TurnDone{Header: event.Header{SessionID: session, LoopID: subagent}},
			want:   true,
		},
		{
			name: "enduring LoopIdle from subagent rejected when enduring scope excludes it",
			filter: event.EventFilter{
				Ephemeral: event.LoopScope{All: true},
				Enduring:  event.LoopScope{Loops: map[uuid.UUID]struct{}{primary: {}}},
			},
			ev:   event.LoopIdle{Header: event.Header{SessionID: session, LoopID: subagent}},
			want: false,
		},
		{
			name: "ephemeral TokenDelta uses ephemeral scope not enduring scope",
			filter: event.EventFilter{
				Ephemeral: event.LoopScope{Loops: map[uuid.UUID]struct{}{primary: {}}},
				Enduring:  event.LoopScope{All: true},
			},
			// subagent token: enduring is All, but ephemeral excludes subagent.
			ev:   event.TokenDelta{Header: event.Header{SessionID: session, LoopID: subagent}},
			want: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := event.ShouldDeliver(tt.filter, tt.ev); got != tt.want {
				t.Errorf("ShouldDeliver() = %v, want %v", got, tt.want)
			}
		})
	}
}
