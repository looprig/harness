package session

import (
	"errors"
	"testing"

	"github.com/inventivepotter/urvi/internal/agent/loop/event"
	"github.com/inventivepotter/urvi/internal/agent/loop/identity"
	"github.com/inventivepotter/urvi/internal/uuid"
)

// fp builds a ConfigFingerprint with a distinct model id so two fingerprints
// differ exactly when their model ids differ.
func fp(model string) event.ConfigFingerprint {
	return event.ConfigFingerprint{ModelID: model}
}

// TestCheckFingerprint covers the restore config-fingerprint decision: a match
// always proceeds; a mismatch rejects with a typed *ConfigMismatchError UNLESS the
// allow-mismatch override is set (fail-secure by default, opt-in to proceed).
func TestCheckFingerprint(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		persisted event.ConfigFingerprint
		live      event.ConfigFingerprint
		allow     bool
		wantErr   bool
	}{
		{
			name:      "identical fingerprints proceed",
			persisted: fp("model-x"),
			live:      fp("model-x"),
			allow:     false,
			wantErr:   false,
		},
		{
			name:      "mismatch rejects by default",
			persisted: fp("model-x"),
			live:      fp("model-y"),
			allow:     false,
			wantErr:   true,
		},
		{
			name:      "mismatch proceeds when override set",
			persisted: fp("model-x"),
			live:      fp("model-y"),
			allow:     true,
			wantErr:   false,
		},
		{
			name:      "two empty fingerprints match",
			persisted: event.ConfigFingerprint{},
			live:      event.ConfigFingerprint{},
			allow:     false,
			wantErr:   false,
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := checkFingerprint(tt.persisted, tt.live, tt.allow)
			if (err != nil) != tt.wantErr {
				t.Fatalf("checkFingerprint() err = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				var cme *ConfigMismatchError
				if !errors.As(err, &cme) {
					t.Fatalf("err = %v, want *ConfigMismatchError", err)
				}
				if !cme.Persisted.Equal(tt.persisted) || !cme.Live.Equal(tt.live) {
					t.Errorf("ConfigMismatchError carried persisted=%+v live=%+v, want %+v / %+v",
						cme.Persisted, cme.Live, tt.persisted, tt.live)
				}
			}
		})
	}
}

// loopStarted builds a LoopStarted whose Header.Cause.Coordinates is the supplied
// parent (zero parent = the root/primary loop) and whose own LoopID is loopID.
func loopStarted(loopID uuid.UUID, parent identity.Coordinates) event.LoopStarted {
	return event.LoopStarted{Header: event.Header{
		Coordinates: identity.Coordinates{LoopID: loopID},
		Cause:       identity.Cause{Coordinates: parent},
	}}
}

// loopStartedNamed builds a root (zero-parent) LoopStarted carrying an AgentName, used
// to drive the root-loop AgentName validation.
func loopStartedNamed(loopID uuid.UUID, name identity.AgentName) event.LoopStarted {
	ls := loopStarted(loopID, identity.Coordinates{})
	ls.Header.AgentName = name
	return ls
}

// TestCheckAgentName covers the restore root-loop AgentName decision: a match always
// proceeds; a mismatch rejects with a typed *AgentNameMismatchError UNLESS the
// allow-mismatch override is set. Critically, an EMPTY persisted name vs a non-empty
// configured name is a MISMATCH (a pre-AgentName/legacy record is not silently accepted
// as a match) — it routes through the same allow path, fail-secure by default.
func TestCheckAgentName(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		persisted  identity.AgentName
		configured identity.AgentName
		allow      bool
		wantErr    bool
	}{
		{name: "identical names proceed", persisted: "operator", configured: "operator", allow: false, wantErr: false},
		{name: "both empty (plain loop) proceed", persisted: "", configured: "", allow: false, wantErr: false},
		{name: "different names reject by default", persisted: "operator", configured: "reviewer", allow: false, wantErr: true},
		{name: "empty persisted vs configured rejects (legacy not silently accepted)", persisted: "", configured: "operator", allow: false, wantErr: true},
		{name: "configured empty vs named persisted rejects", persisted: "operator", configured: "", allow: false, wantErr: true},
		{name: "mismatch proceeds when override set", persisted: "operator", configured: "reviewer", allow: true, wantErr: false},
		{name: "empty-vs-named proceeds when override set", persisted: "", configured: "operator", allow: true, wantErr: false},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := checkAgentName(tt.persisted, tt.configured, tt.allow)
			if (err != nil) != tt.wantErr {
				t.Fatalf("checkAgentName() err = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				var ame *AgentNameMismatchError
				if !errors.As(err, &ame) {
					t.Fatalf("err = %v, want *AgentNameMismatchError", err)
				}
				if ame.Persisted != tt.persisted || ame.Configured != tt.configured {
					t.Errorf("AgentNameMismatchError carried persisted=%q configured=%q, want %q / %q",
						ame.Persisted, ame.Configured, tt.persisted, tt.configured)
				}
			}
		})
	}
}

// TestFindRootLoopStarted covers locating the root LoopStarted (the one whose
// Cause.Coordinates is zero) so restore can read both its LoopID and its stamped
// AgentName. Absence (no root LoopStarted) is a typed *RestoreDiscoveryError.
func TestFindRootLoopStarted(t *testing.T) {
	t.Parallel()

	primary := uuid.UUID{0x01}
	sub := uuid.UUID{0x02}
	parentCoords := identity.Coordinates{LoopID: primary, TurnID: uuid.UUID{0x09}}

	tests := []struct {
		name      string
		events    []event.Event
		wantLoop  uuid.UUID
		wantAgent identity.AgentName
		wantErr   bool
	}{
		{
			name:      "root loop with agent name",
			events:    []event.Event{event.SessionStarted{}, loopStartedNamed(primary, "operator")},
			wantLoop:  primary,
			wantAgent: "operator",
		},
		{
			name:     "root loop with empty agent name",
			events:   []event.Event{loopStarted(primary, identity.Coordinates{})},
			wantLoop: primary,
		},
		{
			name:      "root among subagent loops returns the root",
			events:    []event.Event{loopStartedNamed(primary, "operator"), loopStarted(sub, parentCoords)},
			wantLoop:  primary,
			wantAgent: "operator",
		},
		{
			name:    "no root LoopStarted is an error",
			events:  []event.Event{loopStarted(sub, parentCoords)},
			wantErr: true,
		},
		{
			name:    "empty events is an error",
			events:  nil,
			wantErr: true,
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := findRootLoopStarted(tt.events)
			if (err != nil) != tt.wantErr {
				t.Fatalf("findRootLoopStarted() err = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				var rde *RestoreDiscoveryError
				if !errors.As(err, &rde) {
					t.Errorf("err = %v, want *RestoreDiscoveryError", err)
				}
				return
			}
			if got.LoopID != tt.wantLoop {
				t.Errorf("findRootLoopStarted().LoopID = %v, want %v", got.LoopID, tt.wantLoop)
			}
			if got.Header.AgentName != tt.wantAgent {
				t.Errorf("findRootLoopStarted().AgentName = %q, want %q", got.Header.AgentName, tt.wantAgent)
			}
		})
	}
}

// TestCountSpawnedLoops covers the restore-time spawn-counter re-seed: it counts the
// NON-ROOT LoopStarted events (non-zero Header.Cause) and excludes the root (the primary),
// so the restored quota matches the live one. It mirrors the live NewLoop counter, which
// increments only on a successful subagent spawn.
func TestCountSpawnedLoops(t *testing.T) {
	t.Parallel()

	primary := uuid.UUID{0x01}
	subA := uuid.UUID{0x02}
	subB := uuid.UUID{0x03}
	subC := uuid.UUID{0x04}
	parentPrimary := identity.Coordinates{LoopID: primary, TurnID: uuid.UUID{0x09}}
	parentSubA := identity.Coordinates{LoopID: subA, TurnID: uuid.UUID{0x0A}}

	tests := []struct {
		name   string
		events []event.Event
		want   int
	}{
		{name: "empty stream counts zero", events: nil, want: 0},
		{
			name:   "root only (primary) counts zero",
			events: []event.Event{event.SessionStarted{}, loopStarted(primary, identity.Coordinates{})},
			want:   0,
		},
		{
			name: "one subagent counts one",
			events: []event.Event{
				loopStarted(primary, identity.Coordinates{}),
				loopStarted(subA, parentPrimary),
			},
			want: 1,
		},
		{
			name: "chain of subagents counts each non-root",
			events: []event.Event{
				loopStarted(primary, identity.Coordinates{}),
				loopStarted(subA, parentPrimary), // child of primary
				loopStarted(subB, parentPrimary), // sibling
				loopStarted(subC, parentSubA),    // child of subA (deeper)
			},
			want: 3,
		},
		{
			name: "non-LoopStarted events are ignored",
			events: []event.Event{
				event.SessionStarted{},
				loopStarted(primary, identity.Coordinates{}),
				event.LoopIdle{},
				loopStarted(subA, parentPrimary),
				event.RestoreStarted{},
			},
			want: 1,
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := countSpawnedLoops(tt.events); got != tt.want {
				t.Errorf("countSpawnedLoops() = %d, want %d", got, tt.want)
			}
		})
	}
}

// TestFirstSessionStarted covers extracting the persisted config fingerprint from
// the stream's first SessionStarted. Absence is a typed discovery failure.
func TestFirstSessionStarted(t *testing.T) {
	t.Parallel()

	want := fp("model-x")

	tests := []struct {
		name    string
		events  []event.Event
		want    event.ConfigFingerprint
		wantErr bool
	}{
		{
			name: "first SessionStarted carries the config",
			events: []event.Event{
				event.SessionStarted{Config: want},
				loopStarted(uuid.UUID{0x01}, identity.Coordinates{}),
			},
			want: want,
		},
		{
			name: "uses the FIRST SessionStarted when several appear",
			events: []event.Event{
				event.SessionStarted{Config: want},
				event.SessionStarted{Config: fp("model-y")},
			},
			want: want,
		},
		{
			name: "no SessionStarted is an error",
			events: []event.Event{
				loopStarted(uuid.UUID{0x01}, identity.Coordinates{}),
			},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := firstConfigFingerprint(tt.events)
			if (err != nil) != tt.wantErr {
				t.Fatalf("firstConfigFingerprint() err = %v, wantErr %v", err, tt.wantErr)
			}
			if !tt.wantErr && !got.Equal(tt.want) {
				t.Errorf("firstConfigFingerprint() = %+v, want %+v", got, tt.want)
			}
		})
	}
}
