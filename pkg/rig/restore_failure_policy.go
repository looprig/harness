package rig

// RestoreFailureOption is one declarative exception to Harness's fail-closed
// restore policy. Implementations are sealed so every option retains bounded,
// versioned semantics owned by Harness.
type RestoreFailureOption interface {
	applyRestoreFailure(*restoreFailurePolicy)
}

type restoreFailureOptionFunc func(*restoreFailurePolicy)

func (f restoreFailureOptionFunc) applyRestoreFailure(policy *restoreFailurePolicy) { f(policy) }

type restoreAllowance string

const (
	restoreAllowModel              restoreAllowance = "model"
	restoreAllowExternalCapability restoreAllowance = "external_capability"
	restoreAllowConfinement        restoreAllowance = "confinement"
	restoreAllowPermission         restoreAllowance = "permission"
	restoreAllowPermissionPosture  restoreAllowance = "permission_posture"
	restoreAllowPermissionReview   restoreAllowance = "permission_review"
	restoreAllowWorkspace          restoreAllowance = "workspace"
	restoreAllowTrust              restoreAllowance = "trust"
	restoreAllowAgentKind          restoreAllowance = "agent_kind"
	restoreAllowAgentName          restoreAllowance = "agent_name"
	restoreAllowAdapter            restoreAllowance = "adapter"
	restoreAllowRuntimeSkills      restoreAllowance = "runtime_skills"
	restoreAllowHookPolicy         restoreAllowance = "hook_policy"
	restoreAllowRuntimeProfile     restoreAllowance = "runtime_profile"
	restoreAllowRuntimeCatalog     restoreAllowance = "runtime_catalog"
	restoreAllowCredential         restoreAllowance = "credential"
	restoreAllowEffort             restoreAllowance = "effort"
)

type restoreFailurePolicy struct {
	allow map[restoreAllowance]struct{}
}

func compileRestoreFailurePolicy(options ...RestoreFailureOption) restoreFailurePolicy {
	policy := restoreFailurePolicy{allow: make(map[restoreAllowance]struct{})}
	for _, option := range options {
		if option != nil {
			option.applyRestoreFailure(&policy)
		}
	}
	return policy
}

func (p restoreFailurePolicy) allowanceCount() int { return len(p.allow) }

func allowRestoreDrift(allowance restoreAllowance) RestoreFailureOption {
	return restoreFailureOptionFunc(func(policy *restoreFailurePolicy) {
		policy.allow[allowance] = struct{}{}
	})
}

func AllowModelDrift() RestoreFailureOption { return allowRestoreDrift(restoreAllowModel) }

func AllowExternalCapabilityDrift() RestoreFailureOption {
	return allowRestoreDrift(restoreAllowExternalCapability)
}

func AllowConfinementDrift() RestoreFailureOption {
	return allowRestoreDrift(restoreAllowConfinement)
}

func AllowPermissionDrift() RestoreFailureOption {
	return allowRestoreDrift(restoreAllowPermission)
}

func AllowPermissionPostureDrift() RestoreFailureOption {
	return allowRestoreDrift(restoreAllowPermissionPosture)
}

func AllowPermissionReviewDrift() RestoreFailureOption {
	return allowRestoreDrift(restoreAllowPermissionReview)
}

func AllowWorkspaceDrift() RestoreFailureOption { return allowRestoreDrift(restoreAllowWorkspace) }

func AllowTrustDrift() RestoreFailureOption { return allowRestoreDrift(restoreAllowTrust) }

func AllowAgentKindDrift() RestoreFailureOption { return allowRestoreDrift(restoreAllowAgentKind) }

func AllowAgentNameDrift() RestoreFailureOption { return allowRestoreDrift(restoreAllowAgentName) }

func AllowAdapterDrift() RestoreFailureOption { return allowRestoreDrift(restoreAllowAdapter) }

func AllowRuntimeSkillsDrift() RestoreFailureOption {
	return allowRestoreDrift(restoreAllowRuntimeSkills)
}

func AllowHookPolicyDrift() RestoreFailureOption { return allowRestoreDrift(restoreAllowHookPolicy) }

func AllowRuntimeProfileDrift() RestoreFailureOption {
	return allowRestoreDrift(restoreAllowRuntimeProfile)
}

func AllowRuntimeCatalogDrift() RestoreFailureOption {
	return allowRestoreDrift(restoreAllowRuntimeCatalog)
}

func AllowCredentialDrift() RestoreFailureOption { return allowRestoreDrift(restoreAllowCredential) }

func AllowEffortDrift() RestoreFailureOption { return allowRestoreDrift(restoreAllowEffort) }
