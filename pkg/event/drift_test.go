package event

import "testing"

func TestAssessDrift(t *testing.T) {
	base := testManifest()
	tests := []struct {
		name         string
		mutate       func(*ConfigManifest)
		wantCategory DriftCategory
		wantSeverity DriftSeverity
	}{
		{name: "model change is info", mutate: func(m *ConfigManifest) { m.ModelID = "x" },
			wantCategory: DriftModel, wantSeverity: DriftInfo},
		{name: "prompt change is info", mutate: func(m *ConfigManifest) { m.SystemPromptRev = "x" },
			wantCategory: DriftPrompt, wantSeverity: DriftInfo},
		{name: "tool schema change is info", mutate: func(m *ConfigManifest) { m.Tools[0].InputSchemaRev = "x" },
			wantCategory: DriftTool, wantSeverity: DriftInfo},
		{name: "tool removed is info", mutate: func(m *ConfigManifest) { m.Tools = m.Tools[:1] },
			wantCategory: DriftTool, wantSeverity: DriftInfo},
		{name: "topology change is info", mutate: func(m *ConfigManifest) { m.TopologyRev = "x" },
			wantCategory: DriftTopology, wantSeverity: DriftInfo},
		{name: "external catalog change is info", mutate: func(m *ConfigManifest) { m.ExternalCapabilityRev = "x" },
			wantCategory: DriftExternal, wantSeverity: DriftInfo},
		{name: "confinement stricter is info", mutate: func(m *ConfigManifest) {
			m.ConfinementRev = "x"
			m.ConfinementStrictness = base.ConfinementStrictness + 1
		}, wantCategory: DriftConfinement, wantSeverity: DriftInfo},
		{name: "confinement broadened is warn", mutate: func(m *ConfigManifest) {
			m.ConfinementRev = "x"
			m.ConfinementStrictness = base.ConfinementStrictness - 1
		}, wantCategory: DriftConfinement, wantSeverity: DriftWarn},
		{name: "permission narrowed is info", mutate: func(m *ConfigManifest) {
			m.NativePermissionPolicyRev = "x"
			m.PermissionStrictness = base.PermissionStrictness + 1
		}, wantCategory: DriftPermission, wantSeverity: DriftInfo},
		{name: "permission broadened is warn", mutate: func(m *ConfigManifest) {
			m.NativePermissionPolicyRev = "x"
			m.PermissionStrictness = base.PermissionStrictness - 1
		}, wantCategory: DriftPermission, wantSeverity: DriftWarn},
		{name: "digest-only permission change is warn", mutate: func(m *ConfigManifest) {
			m.NativePermissionPolicyRev = "x"
			m.PermissionStrictness = 0 // unknown direction -> fail secure
		}, wantCategory: DriftPermission, wantSeverity: DriftWarn},
		{name: "workspace move is warn", mutate: func(m *ConfigManifest) { m.WorkspaceRoot = "/other" },
			wantCategory: DriftWorkspace, wantSeverity: DriftWarn},
		{name: "trust mode change is warn", mutate: func(m *ConfigManifest) { m.WorkspaceTrust = "untrusted" },
			wantCategory: DriftTrust, wantSeverity: DriftWarn},
		{name: "agent kind change is warn", mutate: func(m *ConfigManifest) { m.AgentKind = "x" },
			wantCategory: DriftAgentKind, wantSeverity: DriftWarn},
		{name: "adapter change is warn", mutate: func(m *ConfigManifest) { m.AgentAdapter = "x" },
			wantCategory: DriftAdapter, wantSeverity: DriftWarn},
		{name: "runtime skills flip is warn", mutate: func(m *ConfigManifest) { m.RuntimeSkills = false },
			wantCategory: DriftRuntimeSkills, wantSeverity: DriftWarn},
		{name: "app field change is info", mutate: func(m *ConfigManifest) { m.AppFields["a"] = "x" },
			wantCategory: DriftApp, wantSeverity: DriftInfo},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			candidate := testManifest()
			tt.mutate(&candidate)
			assessment := AssessDrift(testManifest(), candidate)
			if len(assessment.Changes) != 1 {
				t.Fatalf("Changes = %d entries (%+v), want exactly 1", len(assessment.Changes), assessment.Changes)
			}
			change := assessment.Changes[0]
			if change.Category != tt.wantCategory || change.Severity != tt.wantSeverity {
				t.Errorf("change = {%s %s}, want {%s %s}",
					change.Category, change.Severity, tt.wantCategory, tt.wantSeverity)
			}
		})
	}
}

func TestAssessDriftNoChanges(t *testing.T) {
	t.Parallel()
	assessment := AssessDrift(testManifest(), testManifest())
	if len(assessment.Changes) != 0 || assessment.AnyWarn() {
		t.Errorf("identical manifests produced drift: %+v", assessment)
	}
}

func TestAssessDriftLegacyBaseline(t *testing.T) {
	t.Parallel()
	legacy := ManifestFromLegacy(ConfigFingerprint{
		ModelID: "m", ToolPolicyRev: testManifest().ToolNamesRev(),
		NativePermissionPolicyRev: "old",
	})
	candidate := testManifest()
	candidate.ModelID = "m"
	assessment := AssessDrift(legacy, candidate)
	// Tool names unchanged -> no tool drift despite the legacy baseline having
	// no schema digests; schema-only info is invisible to a legacy baseline.
	for _, change := range assessment.Changes {
		if change.Category == DriftTool {
			t.Errorf("tool drift reported against name-identical legacy baseline: %+v", change)
		}
		// Legacy permission is digest-only: change must be Warn.
		if change.Category == DriftPermission && change.Severity != DriftWarn {
			t.Errorf("legacy permission drift severity = %s, want warn", change.Severity)
		}
	}
	if !assessment.BaselineUpgrade {
		t.Error("BaselineUpgrade = false for legacy baseline, want true")
	}
}

func TestAssessDriftDeterministicOrder(t *testing.T) {
	t.Parallel()
	// Multiple changes across map-iterated fields (tools + app fields) must
	// produce identical ordering on every run, because the assessment is
	// persisted durably.
	candidate := testManifest()
	candidate.ModelID = "z"
	candidate.Tools = append(candidate.Tools, ToolManifestEntry{Name: "Grep"}, ToolManifestEntry{Name: "Aaa"})
	candidate.AppFields = map[string]string{"a": "9", "z": "9", "m": "9"}
	first := AssessDrift(testManifest(), candidate)
	for i := 0; i < 20; i++ {
		next := AssessDrift(testManifest(), candidate)
		if len(next.Changes) != len(first.Changes) {
			t.Fatalf("change count varies across runs: %d vs %d", len(next.Changes), len(first.Changes))
		}
		for j := range first.Changes {
			if next.Changes[j] != first.Changes[j] {
				t.Fatalf("order/content differs at %d: %+v vs %+v", j, next.Changes[j], first.Changes[j])
			}
		}
	}
}
