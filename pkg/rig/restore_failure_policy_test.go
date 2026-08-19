package rig

import "testing"

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
