package rig

import (
	"context"
	"errors"
	"testing"

	"github.com/looprig/harness/pkg/gate"
	"github.com/looprig/harness/pkg/tool"
)

// stubPermissionReviewEvidenceAccess is a minimal gate.EvidenceAccessEvaluator
// test double: rig never evaluates it itself, it only forwards it down to
// sessionruntime/hustleruntime at construction.
type stubPermissionReviewEvidenceAccess struct{}

func (stubPermissionReviewEvidenceAccess) AccessFor(tool.Requirement) (uint8, error) {
	return gate.AccessAllow, nil
}

// stubPermissionReviewEvidenceContainment is a minimal
// gate.EvidenceContainmentVerifier test double, mirroring
// stubPermissionReviewEvidenceAccess's role.
type stubPermissionReviewEvidenceContainment struct{}

func (stubPermissionReviewEvidenceContainment) VerifyEvidenceContainment(context.Context, gate.EvidenceContainmentPolicy, tool.Request) error {
	return nil
}

// TestWithPermissionReviewEvidenceRejectsInvalidAndDuplicateValues mirrors
// TestPermissionReviewOptionsRejectInvalidAndDuplicateValues's shape for the
// new evidence-boundary option.
func TestWithPermissionReviewEvidenceRejectsInvalidAndDuplicateValues(t *testing.T) {
	t.Parallel()
	validAccess := stubPermissionReviewEvidenceAccess{}
	validContainment := stubPermissionReviewEvidenceContainment{}
	tests := []struct {
		name string
		run  func(*definitionState) error
	}{
		{name: "nil access", run: func(state *definitionState) error {
			return WithPermissionReviewEvidence(nil, validContainment, []string{"filesystem.read"})(state)
		}},
		{name: "nil containment", run: func(state *definitionState) error {
			return WithPermissionReviewEvidence(validAccess, nil, []string{"filesystem.read"})(state)
		}},
		{name: "empty allowed kinds", run: func(state *definitionState) error {
			return WithPermissionReviewEvidence(validAccess, validContainment, nil)(state)
		}},
	}
	for _, testCase := range tests {
		testCase := testCase
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			state := &definitionState{seen: make(map[singletonKey]bool)}
			err := testCase.run(state)
			var target *DefinitionError
			if !errors.As(err, &target) || target.Kind != DefinitionInvalidPermissionReviewEvidence {
				t.Fatalf("option error = %T %v, want DefinitionInvalidPermissionReviewEvidence", err, err)
			}
		})
	}

	t.Run("duplicate option", func(t *testing.T) {
		t.Parallel()
		state := &definitionState{seen: make(map[singletonKey]bool)}
		if err := WithPermissionReviewEvidence(validAccess, validContainment, []string{"filesystem.read"})(state); err != nil {
			t.Fatalf("first WithPermissionReviewEvidence: %v", err)
		}
		err := WithPermissionReviewEvidence(validAccess, validContainment, []string{"filesystem.read"})(state)
		var target *DefinitionError
		if !errors.As(err, &target) || target.Kind != DefinitionDuplicateOption {
			t.Fatalf("second WithPermissionReviewEvidence error = %T %v, want duplicate option", err, err)
		}
	})
}

// TestDefinePermissionReviewRequiresEvidenceOptionWhenClassifierNeedsIt
// proves the fail-closed pairing requirement: a classifier whose Definition
// needs evidence tools makes WithPermissionReviewEvidence mandatory, exactly
// mirroring validateHustleRegistration's DefinitionMissingHustleLimits
// precedent for the sibling "config X requires config Y" pairing.
func TestDefinePermissionReviewRequiresEvidenceOptionWhenClassifierNeedsIt(t *testing.T) {
	t.Parallel()
	classifier := defineRigPermissionClassifier(t, "needs-evidence", rigEvidencePolicy("status"))
	set := rigPermissionClassifierSet(t, classifier)
	_, err := Define(validRigOptions(t,
		WithPermissionClassifiers(set),
		WithPermissionReviewPolicy(rigReviewPolicy(t, "review-v1")),
		WithHustleLimits(validHustleLimits()),
	)...)
	var target *DefinitionError
	if !errors.As(err, &target) || target.Kind != DefinitionMissingPermissionReviewEvidence {
		t.Fatalf("Define() error = %T %v, want DefinitionMissingPermissionReviewEvidence", err, err)
	}
}

// TestDefinePermissionReviewRejectsEvidenceOptionWhenNoClassifierNeedsIt is
// the symmetric "unused config" check: WithPermissionReviewEvidence supplied
// when no registered classifier's Definition needs evidence tools (here, no
// classifiers at all) is rejected rather than silently accepted, mirroring
// DefinitionUnusedHustleLimits/DefinitionUnusedPermissionReviewLimits.
func TestDefinePermissionReviewRejectsEvidenceOptionWhenNoClassifierNeedsIt(t *testing.T) {
	t.Parallel()
	_, err := Define(validRigOptions(t,
		WithPermissionReviewEvidence(
			stubPermissionReviewEvidenceAccess{}, stubPermissionReviewEvidenceContainment{}, []string{"filesystem.read"},
		),
	)...)
	var target *DefinitionError
	if !errors.As(err, &target) || target.Kind != DefinitionUnusedPermissionReviewEvidence {
		t.Fatalf("Define() error = %T %v, want DefinitionUnusedPermissionReviewEvidence", err, err)
	}
}

// TestDefinePermissionReviewSucceedsAndConstructsSessionWithEvidenceOption
// proves the positive path end to end: with WithPermissionReviewEvidence
// supplied and a managed workspace placement configured (evidence tools
// need a canonical read root), Define() succeeds AND the constructed Rig can
// actually bring up a live session — the exact operation that, before this
// addendum, failed 100% of the time for any classifier-registered session
// needing evidence tools (ConfigError{Reason: ConfigMissingCollaborator,
// Field: "runtime.evidence"}). Session construction succeeding is the
// observable, public-surface proof that the access/containment/allowed-kinds
// values this test supplied genuinely reached hustleruntime.RuntimeConfig.Evidence
// — internal/sessionruntime's own test suite
// (TestWithLifecyclePermissionReviewEvidenceThreadsToNewSessionAndRestoreSession,
// TestBindSessionHustlesWiresEvidenceRuntimeFromSessionWorkspaceAndOption)
// proves the exact field-level wiring this black-box test cannot see across
// the package boundary.
func TestDefinePermissionReviewSucceedsAndConstructsSessionWithEvidenceOption(t *testing.T) {
	t.Parallel()
	classifier := defineRigPermissionClassifier(t, "needs-evidence", rigEvidencePolicy("status"))
	set := rigPermissionClassifierSet(t, classifier)
	root := t.TempDir()
	r, err := Define(validRigOptions(t,
		WithPermissionClassifiers(set),
		WithPermissionReviewPolicy(rigReviewPolicy(t, "review-v1")),
		WithHustleLimits(validHustleLimits()),
		WithPermissionReviewEvidence(
			stubPermissionReviewEvidenceAccess{}, stubPermissionReviewEvidenceContainment{}, []string{"filesystem.read"},
		),
		WithSessionWorkspaces(wsStoreT(t), root),
		WithSnapshots(SnapshotPolicy{}),
	)...)
	if err != nil {
		t.Fatalf("Define() error = %v, want success", err)
	}

	controller, err := r.NewSession(context.Background())
	if err != nil {
		t.Fatalf("NewSession() error = %v, want a live session (evidence runtime should be fully wired)", err)
	}
	t.Cleanup(func() { _ = controller.Shutdown(context.Background()) })
	if controller.SessionID().IsZero() {
		t.Fatal("NewSession returned a zero session ID")
	}
}
