package command

import (
	"encoding/json"
	"testing"
)

// TestSetSecurityCeilingRoundTrip proves the journaled SetSecurityCeiling command
// round-trips through the codec with Level fidelity across the ordinal domain, so the
// auditable intent-log record survives persist/restore.
func TestSetSecurityCeilingRoundTrip(t *testing.T) {
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
			cmd := SetSecurityCeiling{Header: fullHeader(), Level: tt.level}
			data, err := MarshalCommand(cmd)
			if err != nil {
				t.Fatalf("MarshalCommand: %v", err)
			}
			got, err := UnmarshalCommand(data)
			if err != nil {
				t.Fatalf("UnmarshalCommand: %v\nwire: %s", err, data)
			}
			sc, ok := got.(SetSecurityCeiling)
			if !ok {
				t.Fatalf("decoded %T, want SetSecurityCeiling", got)
			}
			if sc.Level != tt.level {
				t.Errorf("Level = %d, want %d\nwire: %s", sc.Level, tt.level, data)
			}
		})
	}
}

// TestSetSecurityCeilingEnvelopeTag asserts the wire envelope carries the stable type
// discriminator (the journal reader keys on it) and the level payload key.
func TestSetSecurityCeilingEnvelopeTag(t *testing.T) {
	t.Parallel()
	data, err := MarshalCommand(SetSecurityCeiling{Header: fullHeader(), Level: 1})
	if err != nil {
		t.Fatalf("MarshalCommand: %v", err)
	}
	var env struct {
		Type  CommandName `json:"type"`
		Level uint8       `json:"level"`
	}
	if err := json.Unmarshal(data, &env); err != nil {
		t.Fatalf("unmarshal envelope: %v", err)
	}
	if env.Type != CommandSetSecurityCeiling {
		t.Errorf("envelope type = %q, want %q", env.Type, CommandSetSecurityCeiling)
	}
	if env.Level != 1 {
		t.Errorf("envelope level = %d, want 1", env.Level)
	}
}
