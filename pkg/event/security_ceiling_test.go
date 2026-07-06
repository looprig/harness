package event

import (
	"encoding/json"
	"testing"
)

// TestSecurityCeilingChangedRoundTrip proves the SecurityCeilingChanged event round-trips
// through the codec with Level fidelity, so the durable ceiling-change record survives
// persist/restore (the fold that re-seeds the live ceiling on replay depends on it).
func TestSecurityCeilingChangedRoundTrip(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		level uint8
	}{
		{"most restrictive", 0},
		{"mid", 2},
		{"max ordinal", 255},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ev := SecurityCeilingChanged{Header: fullHeaderSession(), Level: tt.level}
			data, err := MarshalEvent(ev)
			if err != nil {
				t.Fatalf("MarshalEvent: %v", err)
			}
			got, err := UnmarshalEvent(data)
			if err != nil {
				t.Fatalf("UnmarshalEvent: %v\nwire: %s", err, data)
			}
			sc, ok := got.(SecurityCeilingChanged)
			if !ok {
				t.Fatalf("decoded %T, want SecurityCeilingChanged", got)
			}
			if sc.Level != tt.level {
				t.Errorf("Level = %d, want %d\nwire: %s", sc.Level, tt.level, data)
			}
		})
	}
}

// TestSecurityCeilingChangedClassScopeWire pins the event's lifecycle/scope contract
// (session-scoped + Enduring, so it is durably journaled and always fans out) and the
// stable wire tag/payload key.
func TestSecurityCeilingChangedClassScopeWire(t *testing.T) {
	t.Parallel()
	ev := SecurityCeilingChanged{Header: fullHeaderSession(), Level: 3}
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
	if env.Type != "SecurityCeilingChanged" {
		t.Errorf("wire type = %q, want SecurityCeilingChanged", env.Type)
	}
	if env.Level != 3 {
		t.Errorf("wire level = %d, want 3", env.Level)
	}
}
