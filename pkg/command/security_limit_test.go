package command

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/looprig/harness/pkg/security"
)

// TestSetSecurityLimitRoundTrip proves the journaled SetSecurityLimit command
// round-trips through the codec with Level fidelity across the ordinal domain, so the
// auditable intent-log record survives persist/restore.
func TestSetSecurityLimitRoundTrip(t *testing.T) {
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
			cmd := SetSecurityLimit{Header: fullHeader(), Level: tt.level}
			data, err := MarshalCommand(cmd)
			if err != nil {
				t.Fatalf("MarshalCommand: %v", err)
			}
			got, err := UnmarshalCommand(data)
			if err != nil {
				t.Fatalf("UnmarshalCommand: %v\nwire: %s", err, data)
			}
			sc, ok := got.(SetSecurityLimit)
			if !ok {
				t.Fatalf("decoded %T, want SetSecurityLimit", got)
			}
			if sc.Level != tt.level {
				t.Errorf("Level = %d, want %d\nwire: %s", sc.Level, tt.level, data)
			}
		})
	}
}

func TestSetSecurityLimitDecodesLegacyCeilingTag(t *testing.T) {
	t.Parallel()

	wire, err := MarshalCommand(SetSecurityLimit{Header: fullHeader(), Level: 2})
	if err != nil {
		t.Fatalf("MarshalCommand: %v", err)
	}
	wire = bytes.Replace(wire, []byte(`"SetSecurityLimit"`), []byte(`"SetSecurityCeiling"`), 1)

	got, err := UnmarshalCommand(wire)
	if err != nil {
		t.Fatalf("UnmarshalCommand legacy tag: %v", err)
	}
	if _, ok := got.(SetSecurityLimit); !ok {
		t.Fatalf("decoded %T, want SetSecurityLimit", got)
	}
}

// TestSetSecurityLimitEnvelopeTag asserts the wire envelope carries the stable type
// discriminator (the journal reader keys on it) and the level payload key.
func TestSetSecurityLimitEnvelopeTag(t *testing.T) {
	t.Parallel()
	data, err := MarshalCommand(SetSecurityLimit{Header: fullHeader(), Level: 1})
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
	if env.Type != CommandSetSecurityLimit {
		t.Errorf("envelope type = %q, want %q", env.Type, CommandSetSecurityLimit)
	}
	if env.Level != 1 {
		t.Errorf("envelope level = %d, want 1", env.Level)
	}
}
