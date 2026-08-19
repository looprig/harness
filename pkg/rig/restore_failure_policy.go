package rig

import (
	"context"
	"errors"

	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/session"
	model "github.com/looprig/inference/model"
)

const restoreFailurePolicyAdoptionMessage = "adopted configuration allowed by Rig restore failure policy"

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
	restoreAllowNativePermission   restoreAllowance = "native_permission"
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

func (p restoreFailurePolicy) has(allowance restoreAllowance) bool {
	_, ok := p.allow[allowance]
	return ok
}

func (p restoreFailurePolicy) DecideRestore(_ context.Context, assessment event.DriftAssessment) (session.RestoreDecision, error) {
	for _, change := range assessment.Changes {
		if change.Severity == event.DriftWarn && !p.allowsChange(change) {
			return session.RestoreDecision{Source: event.DecisionSourcePolicy}, nil
		}
	}
	return session.RestoreDecision{
		Accept:  true,
		Source:  event.DecisionSourcePolicy,
		Message: restoreFailurePolicyAdoptionMessage,
	}, nil
}

func (p restoreFailurePolicy) allowsChange(change event.DriftChange) bool {
	switch change.Category {
	case event.DriftModel:
		return p.has(restoreAllowModel)
	case event.DriftExternal:
		return p.has(restoreAllowExternalCapability)
	case event.DriftConfinement:
		return p.has(restoreAllowConfinement)
	case event.DriftPermission:
		if p.has(restoreAllowPermission) {
			return true
		}
		switch change.Field {
		case "":
			return p.has(restoreAllowNativePermission)
		case "posture":
			return p.has(restoreAllowPermissionPosture)
		case "review_configured", "review_policy_rev":
			return p.has(restoreAllowPermissionReview)
		default:
			return false
		}
	case event.DriftWorkspace:
		return p.has(restoreAllowWorkspace)
	case event.DriftTrust:
		return p.has(restoreAllowTrust)
	case event.DriftAgentKind:
		return p.has(restoreAllowAgentKind)
	case event.DriftAgentName:
		return p.has(restoreAllowAgentName)
	case event.DriftAdapter:
		return p.has(restoreAllowAdapter)
	case event.DriftRuntimeSkills:
		return p.has(restoreAllowRuntimeSkills)
	case event.DriftHookPolicy:
		return p.has(restoreAllowHookPolicy)
	case event.DriftRuntime:
		switch change.Field {
		case "profile":
			return p.has(restoreAllowRuntimeProfile)
		case "catalog_rev":
			return p.has(restoreAllowRuntimeCatalog)
		case "identity_rev":
			return p.has(restoreAllowRuntimeProfile) || p.has(restoreAllowModel) || p.has(restoreAllowCredential) || p.has(restoreAllowEffort)
		default:
			return false
		}
	default:
		return false
	}
}

func (p restoreFailurePolicy) ResolveRuntimeRestore(ctx context.Context, request session.RuntimeRestoreRequest) (loop.Resolved, error) {
	if err := ctx.Err(); err != nil {
		return loop.Resolved{}, err
	}
	if !p.allowsRuntimeMismatch(request.Mismatch) {
		return loop.Resolved{}, errors.New("rig: runtime restore mismatch is critical")
	}
	resolved, err := request.Catalog.Resolve(request.AgentName, request.Harness, "", model.EffortNone)
	if err != nil || resolved.AgentType != request.AgentName || resolved.AgentHarness != request.Harness {
		return loop.Resolved{}, errors.New("rig: current same-harness runtime unavailable")
	}
	return resolved, nil
}

func (p restoreFailurePolicy) allowsRuntimeMismatch(mismatch string) bool {
	switch mismatch {
	case session.RestoreRuntimeTargetMismatch:
		return p.has(restoreAllowModel)
	case session.RestoreRuntimeCredentialMismatch:
		return p.has(restoreAllowCredential)
	case session.RestoreRuntimeEffortMismatch:
		return p.has(restoreAllowEffort)
	case session.RestoreRuntimeUnavailable:
		return p.has(restoreAllowRuntimeProfile)
	default:
		return false
	}
}

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

func AllowNativePermissionDrift() RestoreFailureOption {
	return allowRestoreDrift(restoreAllowNativePermission)
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
