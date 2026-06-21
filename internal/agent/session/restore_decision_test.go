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

// TestFindPrimaryLoopID covers locating the root loop: the single LoopStarted whose
// Cause.Coordinates is zero. Absence, and only-non-root LoopStarteds, are typed
// failures (a stream with no primary loop cannot be restored).
func TestFindPrimaryLoopID(t *testing.T) {
	t.Parallel()

	primary := uuid.UUID{0x01}
	sub := uuid.UUID{0x02}
	parentCoords := identity.Coordinates{LoopID: primary, TurnID: uuid.UUID{0x09}}

	tests := []struct {
		name    string
		events  []event.Event
		want    uuid.UUID
		wantErr bool
	}{
		{
			name: "single root loop",
			events: []event.Event{
				event.SessionStarted{},
				loopStarted(primary, identity.Coordinates{}),
			},
			want: primary,
		},
		{
			name: "root among subagent loops returns the root",
			events: []event.Event{
				loopStarted(primary, identity.Coordinates{}),
				loopStarted(sub, parentCoords),
			},
			want: primary,
		},
		{
			name: "no LoopStarted at all is an error",
			events: []event.Event{
				event.SessionStarted{},
				event.TurnStarted{},
			},
			wantErr: true,
		},
		{
			name: "only a non-root LoopStarted is an error",
			events: []event.Event{
				loopStarted(sub, parentCoords),
			},
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
			got, err := findPrimaryLoopID(tt.events)
			if (err != nil) != tt.wantErr {
				t.Fatalf("findPrimaryLoopID() err = %v, wantErr %v", err, tt.wantErr)
			}
			if !tt.wantErr && got != tt.want {
				t.Errorf("findPrimaryLoopID() = %v, want %v", got, tt.want)
			}
			if tt.wantErr {
				var rde *RestoreDiscoveryError
				if !errors.As(err, &rde) {
					t.Errorf("err = %v, want *RestoreDiscoveryError", err)
				}
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
