package event_test

import (
	"encoding/json"
	"testing"

	"github.com/looprig/harness/pkg/event"
)

// fullFingerprint is a fingerprint with every field populated, used as the
// identical-baseline in the Equal table.
func fullFingerprint() event.ConfigFingerprint {
	return event.ConfigFingerprint{
		AgentKind:         "primary",
		ModelID:           "claude-test",
		SystemPromptRev:   "abc123",
		ToolPolicyRev:     "def456",
		RuntimeSkills:     true,
		WorkspaceRoot:     "/home/user/repo",
		AgentAdapter:      "claude",
		PermissionPosture: "default",
	}
}

// TestConfigFingerprintEqual asserts Equal is true iff every field matches: the
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
	diffRuntimeSkills := base
	diffRuntimeSkills.RuntimeSkills = false
	diffWorkspaceRoot := base
	diffWorkspaceRoot.WorkspaceRoot = "/other/repo"
	diffAdapter := base
	diffAdapter.AgentAdapter = "codex"
	diffPosture := base
	diffPosture.PermissionPosture = "acceptEdits"

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
		{"RuntimeSkills differs", base, diffRuntimeSkills, false},
		{"WorkspaceRoot differs", base, diffWorkspaceRoot, false},
		{"AgentAdapter differs", base, diffAdapter, false},
		{"PermissionPosture differs", base, diffPosture, false},
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

// TestConfigFingerprint_NativePermissionPolicyRev asserts the native permission
// policy digest field participates in Equal: two records that differ only in it are
// not Equal, two that share it are, and it evolves additively (empty vs empty Equal).
func TestConfigFingerprint_NativePermissionPolicyRev(t *testing.T) {
	t.Parallel()
	a := event.ConfigFingerprint{NativePermissionPolicyRev: "aaa"}
	b := event.ConfigFingerprint{NativePermissionPolicyRev: "bbb"}
	if a.Equal(b) {
		t.Errorf("different NativePermissionPolicyRev must not be Equal")
	}
	if !a.Equal(event.ConfigFingerprint{NativePermissionPolicyRev: "aaa"}) {
		t.Errorf("same NativePermissionPolicyRev must be Equal")
	}
	// Additive/omitzero: an old record (empty) equals a current record that also leaves it empty.
	if !(event.ConfigFingerprint{}).Equal(event.ConfigFingerprint{}) {
		t.Errorf("empty fingerprints must be Equal")
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
		{"runtime skills only", event.ConfigFingerprint{RuntimeSkills: true}},
		{"workspace root only", event.ConfigFingerprint{WorkspaceRoot: "/r"}},
		{"agent adapter only", event.ConfigFingerprint{AgentAdapter: "claude"}},
		{"permission posture only", event.ConfigFingerprint{PermissionPosture: "default"}},
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

// TestConfigFingerprintOldRecordCompat asserts the new fields (RuntimeSkills,
// WorkspaceRoot) are ADDITIVE: an OLD journal record that predates them — its JSON
// carries only the original four keys — decodes to a fingerprint whose new fields are
// the zero values, and compares Equal to a fingerprint built today with the new fields
// left empty. This is the compatibility path: a session persisted before P2b restores
// equal to the same config re-derived today (the new fields default to empty), so the
// additive evolution never spuriously rejects an old record.
func TestConfigFingerprintOldRecordCompat(t *testing.T) {
	t.Parallel()

	// An old SessionStarted record: only the original four fingerprint keys exist.
	oldJSON := `{"agent_kind":"primary","model_id":"claude-test","system_prompt_rev":"abc123","tool_policy_rev":"def456"}`
	var fromOld event.ConfigFingerprint
	if err := json.Unmarshal([]byte(oldJSON), &fromOld); err != nil {
		t.Fatalf("json.Unmarshal(old record): %v", err)
	}
	if fromOld.RuntimeSkills {
		t.Errorf("RuntimeSkills decoded from old record = true, want false (absent key)")
	}
	if fromOld.WorkspaceRoot != "" {
		t.Errorf("WorkspaceRoot decoded from old record = %q, want \"\" (absent key)", fromOld.WorkspaceRoot)
	}

	// A fingerprint built today from the same config but with the new fields left empty
	// (e.g. a non-swarm caller that does not set them) must compare Equal to the old one.
	today := event.ConfigFingerprint{
		AgentKind:       "primary",
		ModelID:         "claude-test",
		SystemPromptRev: "abc123",
		ToolPolicyRev:   "def456",
	}
	if !fromOld.Equal(today) {
		t.Errorf("old record %+v not Equal to today's empty-new-fields fingerprint %+v", fromOld, today)
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
