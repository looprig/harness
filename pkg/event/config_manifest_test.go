package event

import (
	"strings"
	"testing"
)

func testManifest() ConfigManifest {
	return ConfigManifest{
		SchemaVersion:   ManifestSchemaVersion,
		AgentKind:       "coderig:operator",
		TopologyRev:     "aaaa",
		ModelID:         "claude-fable-5",
		SystemPromptRev: "bbbb",
		Tools: []ToolManifestEntry{
			{Name: "Bash", InputSchemaRev: "cc", OutputSchemaRev: "dd"},
			{Name: "Read", InputSchemaRev: "ee"},
		},
		RuntimeSkills:             true,
		WorkspaceRoot:             "/repo",
		WorkspaceTrust:            "trusted",
		AgentAdapter:              "",
		PermissionPosture:         "",
		NativePermissionPolicyRev: "ffff",
		PermissionStrictness:      3,
		ConfinementRev:            "gggg",
		ConfinementStrictness:     2,
		ExternalCapabilityRev:     "hhhh",
		AppFields:                 map[string]string{"b": "2", "a": "1"},
	}
}

func TestManifestFingerprint(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*ConfigManifest)
		same   bool
	}{
		{name: "identical manifests match", mutate: func(*ConfigManifest) {}, same: true},
		{name: "app-field map order is irrelevant", mutate: func(m *ConfigManifest) {
			m.AppFields = map[string]string{"a": "1", "b": "2"}
		}, same: true},
		{name: "model change alters fingerprint", mutate: func(m *ConfigManifest) { m.ModelID = "other" }, same: false},
		{name: "tool schema change alters fingerprint", mutate: func(m *ConfigManifest) {
			m.Tools[0].InputSchemaRev = "zz"
		}, same: false},
		{name: "strictness change alters fingerprint", mutate: func(m *ConfigManifest) {
			m.PermissionStrictness = 1
		}, same: false},
		{name: "schema version change alters fingerprint", mutate: func(m *ConfigManifest) {
			m.SchemaVersion = ManifestSchemaVersion + 1
		}, same: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			base := testManifest()
			other := testManifest()
			tt.mutate(&other)
			if got := base.Fingerprint() == other.Fingerprint(); got != tt.same {
				t.Errorf("fingerprint equality = %v, want %v", got, tt.same)
			}
		})
	}
}

// The canonical encoding is a durable contract: this golden vector pins it. If
// this test ever fails, the encoding changed — that is a manifest schema bump,
// not a test to update casually.
func TestManifestFingerprintGolden(t *testing.T) {
	got := testManifest().Fingerprint()
	if len(got) != 64 || strings.ToLower(got) != got {
		t.Fatalf("fingerprint %q is not lowercase hex sha256", got)
	}
	// Frozen on first run; drift here means the canonical encoding changed.
	const golden = "6dfa05a68de160225451630245e9d7a3ce5e709f39dd376dfc6708bfd4a6da3e"
	if golden != "" && got != golden {
		t.Errorf("canonical encoding drifted: fingerprint = %s, want %s", got, golden)
	}
}

func TestManifestFingerprintDomainSeparation(t *testing.T) {
	t.Parallel()
	// Empty manifest must not collide with the empty-string hash: the domain
	// tag guarantees it.
	empty := ConfigManifest{SchemaVersion: ManifestSchemaVersion}
	if empty.Fingerprint() == hexSHA256Event("") {
		t.Error("empty manifest fingerprint equals bare sha256 of empty string; domain tag missing")
	}
}
