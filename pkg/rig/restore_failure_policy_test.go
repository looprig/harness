package rig

import (
	"context"
	"errors"
	"testing"

	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/identity"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/session"
	model "github.com/looprig/inference/model"
)

func TestRestoreFailurePolicyOptionsCompileAndDeduplicate(t *testing.T) {
	t.Parallel()
	options := []RestoreFailureOption{
		AllowModelDrift(),
		AllowExternalCapabilityDrift(),
		AllowConfinementDrift(),
		AllowPermissionDrift(),
		AllowNativePermissionDrift(),
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
		{name: "native only", policy: compileRestoreFailurePolicy(AllowNativePermissionDrift()), wantAccept: []bool{true, false, false, false}},
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

func TestRestoreFailurePolicyRuntimeAllowsOnlyConfiguredMismatch(t *testing.T) {
	t.Parallel()
	catalog := restoreFailurePolicyCatalog(t)
	tests := []struct {
		name      string
		policy    restoreFailurePolicy
		mismatch  string
		wantError bool
	}{
		{name: "model allows target", policy: compileRestoreFailurePolicy(AllowModelDrift()), mismatch: session.RestoreRuntimeTargetMismatch},
		{name: "effort allows effort", policy: compileRestoreFailurePolicy(AllowEffortDrift()), mismatch: session.RestoreRuntimeEffortMismatch},
		{name: "credential allows credential", policy: compileRestoreFailurePolicy(AllowCredentialDrift()), mismatch: session.RestoreRuntimeCredentialMismatch},
		{name: "profile allows unavailable", policy: compileRestoreFailurePolicy(AllowRuntimeProfileDrift()), mismatch: session.RestoreRuntimeUnavailable},
		{name: "catalog alone does not allow target", policy: compileRestoreFailurePolicy(AllowRuntimeCatalogDrift()), mismatch: session.RestoreRuntimeTargetMismatch, wantError: true},
		{name: "effort does not allow target", policy: compileRestoreFailurePolicy(AllowEffortDrift()), mismatch: session.RestoreRuntimeTargetMismatch, wantError: true},
		{name: "missing runtime is never recoverable", policy: compileRestoreFailurePolicy(AllowRuntimeProfileDrift(), AllowModelDrift(), AllowCredentialDrift(), AllowEffortDrift()), mismatch: session.RestoreRuntimeMissing, wantError: true},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			resolved, err := testCase.policy.ResolveRuntimeRestore(context.Background(), session.RuntimeRestoreRequest{
				AgentName: "worker", Harness: "codex", Profile: "acp/old", Mismatch: testCase.mismatch, Catalog: catalog,
			})
			if testCase.wantError {
				if err == nil {
					t.Fatalf("ResolveRuntimeRestore() = %+v, nil; want error", resolved)
				}
				return
			}
			if err != nil {
				t.Fatalf("ResolveRuntimeRestore() error = %v", err)
			}
			if resolved.AgentType != "worker" || resolved.AgentHarness != "codex" || resolved.Profile != "acp/current" || resolved.ModelAlias != "current" || resolved.Effort != model.EffortHigh {
				t.Fatalf("resolved = %+v, want current same-harness default", resolved)
			}
		})
	}
}

func TestRestoreFailurePolicyRuntimeCannotResolveMissingHarness(t *testing.T) {
	t.Parallel()
	policy := compileRestoreFailurePolicy(AllowRuntimeProfileDrift())
	resolved, err := policy.ResolveRuntimeRestore(context.Background(), session.RuntimeRestoreRequest{
		AgentName: "worker", Harness: "claude-code", Profile: "acp/claude-code", Mismatch: session.RestoreRuntimeUnavailable, Catalog: restoreFailurePolicyCatalog(t),
	})
	if err == nil {
		t.Fatalf("ResolveRuntimeRestore() = %+v, nil; want missing harness error", resolved)
	}
}

func restoreFailurePolicyCatalog(t *testing.T) loop.RuntimeCatalog {
	t.Helper()
	catalog, err := loop.NewRuntimeCatalog([]loop.RuntimeCatalogEntry{{
		AgentType: identity.AgentName("worker"), AgentHarness: "codex", Profile: "acp/current",
		Credential: loop.CredentialGatewayBacked, Default: true, DefaultModel: "current",
		Models: []loop.RuntimeModelOption{{
			Alias: "current", Target: model.Model{Provider: "provider", Name: "current-target"},
			DefaultEffort: model.EffortHigh, Efforts: []model.Effort{model.EffortMedium, model.EffortHigh},
		}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	return catalog
}

type restorePolicyTestDecider struct{}

func (restorePolicyTestDecider) DecideRestore(context.Context, event.DriftAssessment) (session.RestoreDecision, error) {
	return session.RestoreDecision{}, nil
}

type restorePolicyTestResolver struct{}

func (restorePolicyTestResolver) ResolveRuntimeRestore(context.Context, session.RuntimeRestoreRequest) (loop.Resolved, error) {
	return loop.Resolved{}, nil
}

func TestWithRestoreFailurePolicyInstallsBothRestoreCollaborators(t *testing.T) {
	t.Parallel()
	state := &definitionState{seen: make(map[singletonKey]bool)}
	err := WithRestoreFailurePolicy(AllowExternalCapabilityDrift(), AllowModelDrift())(state)
	if err != nil {
		t.Fatalf("WithRestoreFailurePolicy() error = %v", err)
	}
	if !state.seen[keyRestoreFailurePolicy] || len(state.lifecycleOptions) != 2 {
		t.Fatalf("compiled state seen=%v lifecycle options=%d, want policy and two collaborators", state.seen, len(state.lifecycleOptions))
	}
}

func TestWithRestoreFailurePolicyEmptyIsValidAndNilOptionFails(t *testing.T) {
	t.Parallel()
	state := &definitionState{seen: make(map[singletonKey]bool)}
	if err := WithRestoreFailurePolicy()(state); err != nil {
		t.Fatalf("empty WithRestoreFailurePolicy() error = %v", err)
	}

	state = &definitionState{seen: make(map[singletonKey]bool)}
	err := WithRestoreFailurePolicy(nil)(state)
	var definitionErr *DefinitionError
	if !errors.As(err, &definitionErr) || definitionErr.Kind != DefinitionInvalidRestoreFailurePolicy {
		t.Fatalf("nil policy option error = %T %v, want invalid restore failure policy", err, err)
	}
}

func TestRestorePolicyEntryPointsAreMutuallyExclusive(t *testing.T) {
	t.Parallel()
	policy := WithRestoreFailurePolicy(AllowModelDrift())
	others := []struct {
		name   string
		option Option
	}{
		{name: "decider", option: WithRestoreDecider(restorePolicyTestDecider{})},
		{name: "runtime resolver", option: WithRuntimeRestoreResolver(restorePolicyTestResolver{})},
		{name: "legacy mismatch", option: WithAllowConfigMismatch()},
	}
	for _, other := range others {
		other := other
		for _, order := range []struct {
			name    string
			options []Option
		}{
			{name: "policy first", options: []Option{policy, other.option}},
			{name: "policy second", options: []Option{other.option, policy}},
		} {
			order := order
			t.Run(other.name+"/"+order.name, func(t *testing.T) {
				state := &definitionState{seen: make(map[singletonKey]bool)}
				if err := order.options[0](state); err != nil {
					t.Fatalf("first option error = %v", err)
				}
				err := order.options[1](state)
				var definitionErr *DefinitionError
				if !errors.As(err, &definitionErr) || definitionErr.Kind != DefinitionDuplicateOption {
					t.Fatalf("second option error = %T %v, want duplicate policy", err, err)
				}
			})
		}
	}
}
