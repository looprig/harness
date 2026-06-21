package event_test

import (
	"encoding/json"
	"testing"

	"github.com/inventivepotter/urvi/internal/agent/loop/event"
)

// fullFingerprint is a fingerprint with every field populated, used as the
// identical-baseline in the Equal table.
func fullFingerprint() event.ConfigFingerprint {
	return event.ConfigFingerprint{
		AgentKind:       "primary",
		ModelID:         "claude-test",
		SystemPromptRev: "abc123",
		ToolPolicyRev:   "def456",
	}
}

// TestConfigFingerprintEqual asserts Equal is true iff all four fields match: the
// identical-baseline is equal, and each single-field difference (one per field) is
// not. Zero-vs-zero is the boundary case (two empty fingerprints are equal).
func TestConfigFingerprintEqual(t *testing.T) {
	t.Parallel()

	base := fullFingerprint()

	diffKind := base
	diffKind.AgentKind = "subagent"
	diffModel := base
	diffModel.ModelID = "other-model"
	diffPrompt := base
	diffPrompt.SystemPromptRev = "999999"
	diffTools := base
	diffTools.ToolPolicyRev = "000000"

	tests := []struct {
		name string
		a, b event.ConfigFingerprint
		want bool
	}{
		{"identical full", base, fullFingerprint(), true},
		{"both zero", event.ConfigFingerprint{}, event.ConfigFingerprint{}, true},
		{"AgentKind differs", base, diffKind, false},
		{"ModelID differs", base, diffModel, false},
		{"SystemPromptRev differs", base, diffPrompt, false},
		{"ToolPolicyRev differs", base, diffTools, false},
		{"zero vs full differs", event.ConfigFingerprint{}, base, false},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := tt.a.Equal(tt.b); got != tt.want {
				t.Errorf("%+v.Equal(%+v) = %v, want %v", tt.a, tt.b, got, tt.want)
			}
			// Equal must be symmetric.
			if got := tt.b.Equal(tt.a); got != tt.want {
				t.Errorf("%+v.Equal(%+v) = %v, want %v (symmetry)", tt.b, tt.a, got, tt.want)
			}
		})
	}
}

// TestConfigFingerprintJSONRoundTrip asserts a ConfigFingerprint survives a JSON
// round-trip with snake_case keys, and that a zero fingerprint omits every field
// (omitzero) so an empty fingerprint adds nothing to the SessionStarted journal
// record.
func TestConfigFingerprintJSONRoundTrip(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		fp   event.ConfigFingerprint
	}{
		{"full", fullFingerprint()},
		{"zero is boundary", event.ConfigFingerprint{}},
		{"only model set", event.ConfigFingerprint{ModelID: "m"}},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			data, err := json.Marshal(tt.fp)
			if err != nil {
				t.Fatalf("json.Marshal: %v", err)
			}
			var got event.ConfigFingerprint
			if err := json.Unmarshal(data, &got); err != nil {
				t.Fatalf("json.Unmarshal: %v", err)
			}
			if !got.Equal(tt.fp) {
				t.Errorf("round-trip = %+v, want %+v", got, tt.fp)
			}
		})
	}

	// Zero fingerprint must emit "{}" (every field omitzero), so it never bloats the
	// SessionStarted record.
	data, err := json.Marshal(event.ConfigFingerprint{})
	if err != nil {
		t.Fatalf("json.Marshal(zero): %v", err)
	}
	if string(data) != "{}" {
		t.Errorf("zero ConfigFingerprint marshalled to %s, want {} (all fields omitzero)", data)
	}
}

// TestSessionStartedCarriesConfig asserts the Config field is part of the
// SessionStarted struct and survives a JSON round-trip on the event — the durable
// record carries the config fingerprint.
func TestSessionStartedCarriesConfig(t *testing.T) {
	t.Parallel()
	fp := fullFingerprint()
	ev := event.SessionStarted{Config: fp}
	if !ev.Config.Equal(fp) {
		t.Fatalf("SessionStarted.Config = %+v, want %+v", ev.Config, fp)
	}
	data, err := json.Marshal(ev)
	if err != nil {
		t.Fatalf("json.Marshal(SessionStarted): %v", err)
	}
	var got event.SessionStarted
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("json.Unmarshal(SessionStarted): %v", err)
	}
	if !got.Config.Equal(fp) {
		t.Errorf("round-trip SessionStarted.Config = %+v, want %+v", got.Config, fp)
	}
}
