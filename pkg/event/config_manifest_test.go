package event

import (
	"encoding/json"
	"strings"
	"testing"
)

func testManifest() ConfigManifest {
	return ConfigManifest{
		SchemaVersion:   ManifestSchemaVersion,
		AgentKind:       "carbon:operator",
		TopologyRev:     "aaaa",
		ModelID:         "claude-fable-5",
		SystemPromptRev: "bbbb",
		Tools: []ToolManifestEntry{
			{Name: "Bash", InputSchemaRev: "cc", OutputSchemaRev: "dd"},
			{Name: "Read", InputSchemaRev: "ee"},
		},
		RuntimeSkills:              true,
		WorkspaceRoot:              "/repo",
		WorkspaceTrust:             "trusted",
		AgentAdapter:               "",
		PermissionPosture:          "",
		NativePermissionPolicyRev:  "ffff",
		PermissionStrictness:       3,
		PermissionReviewConfigured: true,
		PermissionReviewPolicyRev:  "iiii",
		ConfinementRev:             "gggg",
		ConfinementStrictness:      2,
		ExternalCapabilityRev:      "hhhh",
		HookPolicyRev:              "jjjj",
		RuntimeProfile:             "acp/codex",
		RuntimeCatalogRev:          "catalog-v1",
		RuntimeIdentityRev:         "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		AppFields:                  map[string]string{"b": "2", "a": "1"},
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
		{name: "permission review configured change alters fingerprint", mutate: func(m *ConfigManifest) {
			m.PermissionReviewConfigured = false
		}, same: false},
		{name: "permission review policy revision change alters fingerprint", mutate: func(m *ConfigManifest) {
			m.PermissionReviewPolicyRev = "kkkk"
		}, same: false},
		{name: "hook policy change alters fingerprint", mutate: func(m *ConfigManifest) {
			m.HookPolicyRev = "other"
		}, same: false},
		{name: "runtime profile change alters fingerprint", mutate: func(m *ConfigManifest) {
			m.RuntimeProfile = "acp/claude"
		}, same: false},
		{name: "runtime catalog change alters fingerprint", mutate: func(m *ConfigManifest) {
			m.RuntimeCatalogRev = "catalog-v2"
		}, same: false},
		{name: "runtime identity change alters fingerprint", mutate: func(m *ConfigManifest) {
			m.RuntimeIdentityRev = "different-runtime-identity-revision"
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

func TestManifestV2FingerprintRemainsHistorical(t *testing.T) {
	t.Parallel()
	manifest := testManifest()
	manifest.SchemaVersion = 2
	manifest.RuntimeProfile = "must-not-enter-v2"
	manifest.RuntimeCatalogRev = "must-not-enter-v2"
	manifest.RuntimeIdentityRev = "must-not-enter-v2"
	const historical = "21812bba801a58b50aa752f09466b35ea4f8ebd7520c825a49efd79110403c4d"
	if got := manifest.Fingerprint(); got != historical {
		t.Fatalf("schema-v2 fingerprint = %s, want historical %s", got, historical)
	}
}

func TestManifestHookSchemaContract(t *testing.T) {
	t.Parallel()
	if ManifestSchemaVersion != 3 {
		t.Errorf("ManifestSchemaVersion = %d, want 3", ManifestSchemaVersion)
	}
	if manifestEncodingDomain != "looprig/config-manifest/v3" {
		t.Errorf("manifestEncodingDomain = %q, want v3 domain", manifestEncodingDomain)
	}
}

func TestManifestRuntimeIdentityIsOpaqueJSON(t *testing.T) {
	t.Parallel()
	manifest := ConfigManifest{
		SchemaVersion:      ManifestSchemaVersion,
		RuntimeProfile:     "acp/codex",
		RuntimeCatalogRev:  "catalog-v1",
		RuntimeIdentityRev: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
	}
	data, err := json.Marshal(manifest)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	encoded := string(data)
	for _, raw := range []string{"openai", "gpt-5.6-luna", "high", "target-alias"} {
		if strings.Contains(encoded, raw) {
			t.Fatalf("manifest JSON contains raw descriptor %q: %s", raw, encoded)
		}
	}
	if !strings.Contains(encoded, "runtime_identity_rev") {
		t.Fatalf("manifest JSON omitted opaque identity revision: %s", encoded)
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
	// Regenerated for the true v2 schema, which carries BOTH
	// PermissionReviewPolicyRev and HookPolicyRev together (see
	// ManifestSchemaVersion's doc comment) — neither field's original
	// standalone golden value is correct once both are present.
	const golden = "c46358c71c990ea83b64ee23280b32f39d28cbbc39a25febcec1d8f836c9e607"
	if golden != "" && got != golden {
		t.Errorf("canonical encoding drifted: fingerprint = %s, want %s", got, golden)
	}
}

func TestManifestV1FingerprintCompatibility(t *testing.T) {
	t.Parallel()
	manifest := testManifest()
	manifest.SchemaVersion = 1
	manifest.HookPolicyRev = ""

	const historical = "6dfa05a68de160225451630245e9d7a3ce5e709f39dd376dfc6708bfd4a6da3e"
	if got := manifest.Fingerprint(); got != historical {
		t.Fatalf("schema-v1 fingerprint = %s, want historical %s", got, historical)
	}

	manifest.HookPolicyRev = "must-not-enter-v1"
	if got := manifest.Fingerprint(); got != historical {
		t.Errorf("schema-v1 hook field changed fingerprint to %s, want %s", got, historical)
	}
}

// TestManifestFingerprintDuplicateToolOrderIndependent proves the full-tuple tool
// sort makes duplicate-name entries order-independent: two manifests differing only
// in the input order of same-named tools must fingerprint identically.
func TestManifestFingerprintDuplicateToolOrderIndependent(t *testing.T) {
	t.Parallel()
	a := ConfigManifest{SchemaVersion: ManifestSchemaVersion, Tools: []ToolManifestEntry{{Name: "A", InputSchemaRev: "x"}, {Name: "A", InputSchemaRev: "y"}}}
	b := ConfigManifest{SchemaVersion: ManifestSchemaVersion, Tools: []ToolManifestEntry{{Name: "A", InputSchemaRev: "y"}, {Name: "A", InputSchemaRev: "x"}}}
	if a.Fingerprint() != b.Fingerprint() {
		t.Error("duplicate-name tool order changed the fingerprint")
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

func TestManifestFromLegacy(t *testing.T) {
	t.Parallel()
	legacy := ConfigFingerprint{
		TopologyRev: "t", AgentKind: "k", ModelID: "m", SystemPromptRev: "s",
		ToolPolicyRev: "tp", RuntimeSkills: true, WorkspaceRoot: "/r",
		AgentAdapter: "a", PermissionPosture: "p",
		NativePermissionPolicyRev: "n", ExternalCapabilityRev: "x",
	}
	got := ManifestFromLegacy(legacy)
	if got.SchemaVersion != 0 {
		t.Errorf("SchemaVersion = %d, want 0 (legacy projection marker)", got.SchemaVersion)
	}
	// Every legacy field must survive the projection (superset requirement).
	if got.TopologyRev != "t" || got.AgentKind != "k" || got.ModelID != "m" ||
		got.SystemPromptRev != "s" || !got.RuntimeSkills || got.WorkspaceRoot != "/r" ||
		got.AgentAdapter != "a" || got.PermissionPosture != "p" ||
		got.NativePermissionPolicyRev != "n" || got.ExternalCapabilityRev != "x" {
		t.Errorf("legacy fields dropped in projection: %+v", got)
	}
	if got.legacyToolPolicyRev != "tp" {
		t.Errorf("legacyToolPolicyRev = %q, want %q", got.legacyToolPolicyRev, "tp")
	}
	if got.HookPolicyRev != "" {
		t.Errorf("HookPolicyRev = %q, want empty for legacy projection", got.HookPolicyRev)
	}
}

func TestLegacyToolPolicyRevDerivation(t *testing.T) {
	t.Parallel()
	m := ConfigManifest{
		SchemaVersion: ManifestSchemaVersion,
		Tools:         []ToolManifestEntry{{Name: "Read"}, {Name: "Bash"}},
	}
	// Must reproduce rig's names-only digest: sha256("Bash\nRead").
	if got, want := m.ToolNamesRev(), hexSHA256Event("Bash\nRead"); got != want {
		t.Errorf("ToolNamesRev() = %s, want %s", got, want)
	}
}
