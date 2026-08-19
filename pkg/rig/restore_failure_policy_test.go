package rig

import (
	"context"
	"testing"

	"github.com/looprig/harness/pkg/event"
)

func TestRestoreFailurePolicyOptionsCompileAndDeduplicate(t *testing.T) {
	t.Parallel()
	options := []RestoreFailureOption{
		AllowModelDrift(),
		AllowExternalCapabilityDrift(),
		AllowConfinementDrift(),
		AllowPermissionDrift(),
		AllowPermissionPostureDrift(),
		AllowPermissionReviewDrift(),
		AllowWorkspaceDrift(),
		AllowTrustDrift(),
		AllowAgentKindDrift(),
		AllowAgentNameDrift(),
		AllowAdapterDrift(),
		AllowRuntimeSkillsDrift(),
		AllowHookPolicyDrift(),
		AllowRuntimeProfileDrift(),
		AllowRuntimeCatalogDrift(),
		AllowCredentialDrift(),
		AllowEffortDrift(),
		AllowModelDrift(),
	}
	policy := compileRestoreFailurePolicy(options...)
	if got, want := policy.allowanceCount(), len(options)-1; got != want {
		t.Fatalf("allowance count = %d, want deduplicated %d", got, want)
	}
}

func TestEmptyRestoreFailurePolicyHasNoAllowances(t *testing.T) {
	t.Parallel()
	if got := compileRestoreFailurePolicy().allowanceCount(); got != 0 {
		t.Fatalf("empty allowance count = %d, want 0", got)
	}
}

func TestRestoreFailurePolicyDeciderFailsClosedExceptAllowedWarnings(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		policy     restoreFailurePolicy
		changes    []event.DriftChange
		wantAccept bool
	}{
		{name: "empty accepts information", policy: compileRestoreFailurePolicy(), changes: []event.DriftChange{{Category: event.DriftPrompt, Severity: event.DriftInfo}}, wantAccept: true},
		{name: "empty rejects warning", policy: compileRestoreFailurePolicy(), changes: []event.DriftChange{{Category: event.DriftWorkspace, Severity: event.DriftWarn}}},
		{name: "matching warning accepted", policy: compileRestoreFailurePolicy(AllowWorkspaceDrift()), changes: []event.DriftChange{{Category: event.DriftWorkspace, Severity: event.DriftWarn}}, wantAccept: true},
		{name: "mixed warning rejected", policy: compileRestoreFailurePolicy(AllowWorkspaceDrift()), changes: []event.DriftChange{{Category: event.DriftWorkspace, Severity: event.DriftWarn}, {Category: event.DriftTrust, Severity: event.DriftWarn}}},
		{name: "runtime catalog accepted", policy: compileRestoreFailurePolicy(AllowRuntimeCatalogDrift()), changes: []event.DriftChange{{Category: event.DriftRuntime, Field: "catalog_rev", Severity: event.DriftWarn}}, wantAccept: true},
		{name: "runtime profile remains independent", policy: compileRestoreFailurePolicy(AllowRuntimeCatalogDrift()), changes: []event.DriftChange{{Category: event.DriftRuntime, Field: "profile", Severity: event.DriftWarn}}},
		{name: "model allowance admits opaque runtime identity", policy: compileRestoreFailurePolicy(AllowModelDrift()), changes: []event.DriftChange{{Category: event.DriftRuntime, Field: "identity_rev", Severity: event.DriftWarn}}, wantAccept: true},
		{name: "effort allowance admits opaque runtime identity", policy: compileRestoreFailurePolicy(AllowEffortDrift()), changes: []event.DriftChange{{Category: event.DriftRuntime, Field: "identity_rev", Severity: event.DriftWarn}}, wantAccept: true},
		{name: "credential allowance admits opaque runtime identity", policy: compileRestoreFailurePolicy(AllowCredentialDrift()), changes: []event.DriftChange{{Category: event.DriftRuntime, Field: "identity_rev", Severity: event.DriftWarn}}, wantAccept: true},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			decision, err := testCase.policy.DecideRestore(context.Background(), event.DriftAssessment{Changes: testCase.changes})
			if err != nil {
				t.Fatal(err)
			}
			if decision.Accept != testCase.wantAccept {
				t.Fatalf("decision = %+v, want accept=%v", decision, testCase.wantAccept)
			}
			if decision.Source != event.DecisionSourcePolicy {
				t.Fatalf("decision source = %q, want policy", decision.Source)
			}
		})
	}
}

func TestRestoreFailurePolicyDeciderPermissionScope(t *testing.T) {
	t.Parallel()
	warnings := []event.DriftChange{
		{Category: event.DriftPermission, Severity: event.DriftWarn},
		{Category: event.DriftPermission, Field: "posture", Severity: event.DriftWarn},
		{Category: event.DriftPermission, Field: "review_configured", Severity: event.DriftWarn},
		{Category: event.DriftPermission, Field: "review_policy_rev", Severity: event.DriftWarn},
	}
	for _, testCase := range []struct {
		name       string
		policy     restoreFailurePolicy
		wantAccept []bool
	}{
		{name: "broad", policy: compileRestoreFailurePolicy(AllowPermissionDrift()), wantAccept: []bool{true, true, true, true}},
		{name: "posture only", policy: compileRestoreFailurePolicy(AllowPermissionPostureDrift()), wantAccept: []bool{false, true, false, false}},
		{name: "review only", policy: compileRestoreFailurePolicy(AllowPermissionReviewDrift()), wantAccept: []bool{false, false, true, true}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			for index, warning := range warnings {
				decision, err := testCase.policy.DecideRestore(context.Background(), event.DriftAssessment{Changes: []event.DriftChange{warning}})
				if err != nil {
					t.Fatal(err)
				}
				if decision.Accept != testCase.wantAccept[index] {
					t.Errorf("warning %+v accept=%v, want %v", warning, decision.Accept, testCase.wantAccept[index])
				}
			}
		})
	}
}
