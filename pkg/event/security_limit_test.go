package event

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/looprig/harness/pkg/security"
)

// TestSecurityLimitChangedRoundTrip proves the SecurityLimitChanged event round-trips
// through the codec with Level fidelity, so the durable security limit-change record survives
// persist/restore (the fold that re-seeds the live security limit on replay depends on it).
func TestSecurityLimitChangedRoundTrip(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		level security.Level
	}{
		{"most restrictive", 0},
		{"mid", 2},
		{"max ordinal", 255},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ev := SecurityLimitChanged{Header: fullHeaderSession(), Level: tt.level}
			data, err := MarshalEvent(ev)
			if err != nil {
				t.Fatalf("MarshalEvent: %v", err)
			}
			got, err := UnmarshalEvent(data)
			if err != nil {
				t.Fatalf("UnmarshalEvent: %v\nwire: %s", err, data)
			}
			sc, ok := got.(SecurityLimitChanged)
			if !ok {
				t.Fatalf("decoded %T, want SecurityLimitChanged", got)
			}
			if sc.Level != tt.level {
				t.Errorf("Level = %d, want %d\nwire: %s", sc.Level, tt.level, data)
			}
		})
	}
}

func TestSecurityLimitChangedDecodesLegacyCeilingTag(t *testing.T) {
	t.Parallel()

	wire, err := MarshalEvent(SecurityLimitChanged{Header: fullHeaderSession(), Level: 2})
	if err != nil {
		t.Fatalf("MarshalEvent: %v", err)
	}
	wire = bytes.Replace(wire, []byte(`"SecurityLimitChanged"`), []byte(`"SecurityCeilingChanged"`), 1)

	got, err := UnmarshalEvent(wire)
	if err != nil {
		t.Fatalf("UnmarshalEvent legacy tag: %v", err)
	}
	if _, ok := got.(SecurityLimitChanged); !ok {
		t.Fatalf("decoded %T, want SecurityLimitChanged", got)
	}
}

// TestSecurityLimitChangedClassScopeWire pins the event's lifecycle/scope contract
// (session-scoped + Enduring, so it is durably journaled and always fans out) and the
// stable wire tag/payload key.
func TestSecurityLimitChangedClassScopeWire(t *testing.T) {
	t.Parallel()
	ev := SecurityLimitChanged{Header: fullHeaderSession(), Level: 3}
	if ev.Scope() != ScopeSession {
		t.Errorf("Scope() = %v, want ScopeSession", ev.Scope())
	}
	if ev.Class() != Enduring {
		t.Errorf("Class() = %v, want Enduring", ev.Class())
	}
	data, err := MarshalEvent(ev)
	if err != nil {
		t.Fatalf("MarshalEvent: %v", err)
	}
	var env struct {
		Type  string `json:"type"`
		Level uint8  `json:"level"`
	}
	if err := json.Unmarshal(data, &env); err != nil {
		t.Fatalf("unmarshal envelope: %v", err)
	}
	if env.Type != "SecurityLimitChanged" {
		t.Errorf("wire type = %q, want SecurityLimitChanged", env.Type)
	}
	if env.Level != 3 {
		t.Errorf("wire level = %d, want 3", env.Level)
	}
}
