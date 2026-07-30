package event

import (
	"errors"
	"fmt"
	"testing"
)

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
		{name: "permission review classifiers disabled is info", mutate: func(m *ConfigManifest) {
			m.PermissionReviewConfigured = false
		}, wantCategory: DriftPermission, wantSeverity: DriftInfo},
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
		{name: "hook policy change is warn", mutate: func(m *ConfigManifest) { m.HookPolicyRev = "hook-v2" },
			wantCategory: DriftHookPolicy, wantSeverity: DriftWarn},
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

// TestAssessDriftPermissionReviewConfiguredTransitions proves the four
// permission-review-classifier-activation transitions design §21 cares about
// classify correctly: disabled -> enabled is the silent-reviewer-activation
// bug (must be Warn, requiring explicit accept), the other three transitions
// must not regress.
func TestAssessDriftPermissionReviewConfiguredTransitions(t *testing.T) {
	t.Parallel()

	base := ConfigManifest{SchemaVersion: ManifestSchemaVersion, ModelID: "m", TopologyRev: "topo-a"}

	t.Run("absent to absent is unaffected", func(t *testing.T) {
		t.Parallel()
		baseline := base
		candidate := base
		assessment := AssessDrift(baseline, candidate)
		if len(assessment.Changes) != 0 || assessment.AnyWarn() {
			t.Fatalf("absent->absent produced drift: %+v", assessment)
		}
	})

	t.Run("disabled to enabled is warn", func(t *testing.T) {
		t.Parallel()
		baseline := base
		baseline.PermissionReviewConfigured = false
		candidate := base
		candidate.PermissionReviewConfigured = true
		assessment := AssessDrift(baseline, candidate)
		if !assessment.AnyWarn() {
			t.Fatalf("disabled->enabled did not warn: %+v", assessment)
		}
		found := false
		for _, change := range assessment.Changes {
			if change.Category == DriftPermission && change.Field == "review_configured" {
				found = true
				if change.Severity != DriftWarn {
					t.Errorf("severity = %s, want warn", change.Severity)
				}
			}
		}
		if !found {
			t.Fatalf("no review_configured change reported: %+v", assessment)
		}
	})

	t.Run("enabled to enabled with different classifier identity stays info via topology", func(t *testing.T) {
		t.Parallel()
		baseline := base
		baseline.PermissionReviewConfigured = true
		candidate := base
		candidate.PermissionReviewConfigured = true
		candidate.TopologyRev = "topo-b" // identity change folds into TopologyRev today
		assessment := AssessDrift(baseline, candidate)
		if assessment.AnyWarn() {
			t.Fatalf("enabled->enabled identity change unexpectedly warned: %+v", assessment)
		}
		if len(assessment.Changes) != 1 || assessment.Changes[0].Category != DriftTopology {
			t.Fatalf("changes = %+v, want exactly one DriftTopology info change", assessment.Changes)
		}
		for _, change := range assessment.Changes {
			if change.Category == DriftPermission && change.Field == "review_configured" {
				t.Fatalf("unexpected review_configured change on identity-only drift: %+v", change)
			}
		}
	})

	// TestAssessDriftPermissionReviewConfiguredTransitions/enabled_to_enabled_with_different_review_policy_revision_is_warn
	// proves the fix for the confirmed gap: a session opened under one review
	// policy identity (e.g. a strict custom policy) and restored under a
	// DIFFERENT review policy identity (e.g. a looser default), with
	// classifiers configured on BOTH sides, must warn — it must not fall
	// through the opaque TopologyRev-only comparison (which is Info, like any
	// other topology change) the way the classifier-identity-only subtest
	// above deliberately still does.
	t.Run("enabled to enabled with different review policy revision is warn", func(t *testing.T) {
		t.Parallel()
		baseline := base
		baseline.PermissionReviewConfigured = true
		baseline.PermissionReviewPolicyRev = "strict-policy-v1"
		candidate := base
		candidate.PermissionReviewConfigured = true
		candidate.PermissionReviewPolicyRev = "default-policy-v1"
		assessment := AssessDrift(baseline, candidate)
		if !assessment.AnyWarn() {
			t.Fatalf("enabled->enabled review policy revision change did not warn: %+v", assessment)
		}
		found := false
		for _, change := range assessment.Changes {
			if change.Category == DriftPermission && change.Field == "review_policy_rev" {
				found = true
				if change.Severity != DriftWarn {
					t.Errorf("severity = %s, want warn", change.Severity)
				}
				if change.Old != "strict-policy-v1" || change.New != "default-policy-v1" {
					t.Errorf("old/new = %q/%q, want the two policy revisions", change.Old, change.New)
				}
			}
		}
		if !found {
			t.Fatalf("no review_policy_rev change reported: %+v", assessment)
		}
	})

	// The disabled-side variants must never warn on a policy-revision
	// difference: PermissionReviewConfigured's own transition already governs
	// whichever side is unconfigured, and a policy revision recorded while
	// unconfigured carries no live meaning.
	t.Run("disabled to enabled ignores stale policy revision on the disabled side", func(t *testing.T) {
		t.Parallel()
		baseline := base
		baseline.PermissionReviewConfigured = false
		baseline.PermissionReviewPolicyRev = "stale-v0"
		candidate := base
		candidate.PermissionReviewConfigured = true
		candidate.PermissionReviewPolicyRev = "default-policy-v1"
		assessment := AssessDrift(baseline, candidate)
		for _, change := range assessment.Changes {
			if change.Category == DriftPermission && change.Field == "review_policy_rev" {
				t.Fatalf("unexpected review_policy_rev change while one side is unconfigured: %+v", change)
			}
		}
	})

	t.Run("enabled to enabled with identical review policy revision stays unaffected", func(t *testing.T) {
		t.Parallel()
		baseline := base
		baseline.PermissionReviewConfigured = true
		baseline.PermissionReviewPolicyRev = "default-policy-v1"
		candidate := base
		candidate.PermissionReviewConfigured = true
		candidate.PermissionReviewPolicyRev = "default-policy-v1"
		assessment := AssessDrift(baseline, candidate)
		if len(assessment.Changes) != 0 || assessment.AnyWarn() {
			t.Fatalf("identical review policy revision produced drift: %+v", assessment)
		}
	})

	t.Run("enabled to disabled is not warn", func(t *testing.T) {
		t.Parallel()
		baseline := base
		baseline.PermissionReviewConfigured = true
		candidate := base
		candidate.PermissionReviewConfigured = false
		assessment := AssessDrift(baseline, candidate)
		if assessment.AnyWarn() {
			t.Fatalf("enabled->disabled warned: %+v", assessment)
		}
		found := false
		for _, change := range assessment.Changes {
			if change.Category == DriftPermission && change.Field == "review_configured" {
				found = true
				if change.Severity != DriftInfo {
					t.Errorf("severity = %s, want info", change.Severity)
				}
			}
		}
		if !found {
			t.Fatalf("no review_configured change reported: %+v", assessment)
		}
	})
}

func TestAssessDriftLegacyHookPolicyUpgradeFailsSecure(t *testing.T) {
	t.Parallel()
	legacy := ManifestFromLegacy(ConfigFingerprint{ToolPolicyRev: hexSHA256Event("")})
	candidate := legacy
	candidate.SchemaVersion = ManifestSchemaVersion
	candidate.HookPolicyRev = "guard-v1"

	assessment := AssessDrift(legacy, candidate)
	if !assessment.BaselineUpgrade {
		t.Fatal("BaselineUpgrade = false, want true")
	}
	if len(assessment.Changes) != 1 {
		t.Fatalf("Changes = %+v, want one hook policy change", assessment.Changes)
	}
	change := assessment.Changes[0]
	if change.Category != DriftHookPolicy || change.Severity != DriftWarn {
		t.Errorf("change = %+v, want hook policy warn", change)
	}
}

func TestConfigDriftBudgetCoversDisjointValidManifests(t *testing.T) {
	t.Parallel()
	baseline := ConfigManifest{
		SchemaVersion:             ManifestSchemaVersion,
		AgentKind:                 "old-kind",
		TopologyRev:               "old-topology",
		ModelID:                   "old-model",
		SystemPromptRev:           "old-prompt",
		RuntimeSkills:             false,
		WorkspaceRoot:             "old-root",
		WorkspaceTrust:            "old-trust",
		AgentAdapter:              "old-adapter",
		PermissionPosture:         "old-posture",
		NativePermissionPolicyRev: "old-permission",
		PermissionStrictness:      2,
		ConfinementRev:            "old-confinement",
		ConfinementStrictness:     2,
		ExternalCapabilityRev:     "old-external",
		HookPolicyRev:             "old-hook",
		Tools:                     make([]ToolManifestEntry, maxConfigManifestTools),
		AppFields:                 make(map[string]string, maxConfigManifestAppFields),
	}
	candidate := ConfigManifest{
		SchemaVersion:             ManifestSchemaVersion,
		AgentKind:                 "new-kind",
		TopologyRev:               "new-topology",
		ModelID:                   "new-model",
		SystemPromptRev:           "new-prompt",
		RuntimeSkills:             true,
		WorkspaceRoot:             "new-root",
		WorkspaceTrust:            "new-trust",
		AgentAdapter:              "new-adapter",
		PermissionPosture:         "new-posture",
		NativePermissionPolicyRev: "new-permission",
		PermissionStrictness:      3,
		ConfinementRev:            "new-confinement",
		ConfinementStrictness:     3,
		ExternalCapabilityRev:     "new-external",
		HookPolicyRev:             "new-hook",
		Tools:                     make([]ToolManifestEntry, maxConfigManifestTools),
		AppFields:                 make(map[string]string, maxConfigManifestAppFields),
	}
	for index := range baseline.Tools {
		baseline.Tools[index].Name = fmt.Sprintf("old-tool-%04d", index)
		candidate.Tools[index].Name = fmt.Sprintf("new-tool-%04d", index)
	}
	for index := 0; index < maxConfigManifestAppFields; index++ {
		baseline.AppFields[fmt.Sprintf("old-field-%04d", index)] = "old"
		candidate.AppFields[fmt.Sprintf("new-field-%04d", index)] = "new"
	}

	assessment := AssessDrift(baseline, candidate)
	assessment.Changes = append(assessment.Changes, DriftChange{
		Category: DriftAgentName,
		Old:      "old-agent",
		New:      "new-agent",
		Severity: DriftWarn,
	})
	adoption := ConfigurationAdopted{
		Header:             fullHeaderSession(),
		Epoch:              2,
		AdoptedFingerprint: candidate.Fingerprint(),
		Manifest:           candidate,
		Drift:              assessment.Changes,
		Source:             DecisionSourcePolicy,
	}
	if err := ValidateEvent(adoption); err != nil {
		t.Fatalf("maximum valid drift with agent name rejected: %v (changes=%d budget=%d)",
			err, len(adoption.Drift), maxConfigDriftChanges)
	}

	adoption.Drift = append(adoption.Drift, DriftChange{
		Category: DriftAgentName,
		Old:      "another-old-agent",
		New:      "another-new-agent",
		Severity: DriftWarn,
	})
	err := ValidateEvent(adoption)
	var invalid *InvalidEventError
	if !errors.As(err, &invalid) || invalid.Field != FieldDrift || invalid.Rule != RuleInvalid {
		t.Fatalf("one-over-limit error = %T %v, want invalid Drift", err, err)
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
