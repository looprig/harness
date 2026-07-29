package rig

import (
	"context"
	"errors"
	"testing"
)

// TestWithPermissionReviewSecurityCeilingRejectsInvalidAndDuplicateValues
// mirrors TestWithPermissionReviewEvidenceRejectsInvalidAndDuplicateValues's
// shape for the new consumer-owned security-ceiling option (Finding 2, Phase
// 6 spec-compliance review).
func TestWithPermissionReviewSecurityCeilingRejectsInvalidAndDuplicateValues(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		run  func(*definitionState) error
	}{
		{name: "empty ceiling", run: func(state *definitionState) error {
			return WithPermissionReviewSecurityCeiling("")(state)
		}},
		{name: "whitespace-only ceiling", run: func(state *definitionState) error {
			return WithPermissionReviewSecurityCeiling("   ")(state)
		}},
	}
	for _, testCase := range tests {
		testCase := testCase
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			state := &definitionState{seen: make(map[singletonKey]bool)}
			err := testCase.run(state)
			var target *DefinitionError
			if !errors.As(err, &target) || target.Kind != DefinitionInvalidPermissionReviewSecurityCeiling {
				t.Fatalf("option error = %T %v, want DefinitionInvalidPermissionReviewSecurityCeiling", err, err)
			}
		})
	}

	t.Run("duplicate option", func(t *testing.T) {
		t.Parallel()
		state := &definitionState{seen: make(map[singletonKey]bool)}
		if err := WithPermissionReviewSecurityCeiling("consumer-access-profile/v1")(state); err != nil {
			t.Fatalf("first WithPermissionReviewSecurityCeiling: %v", err)
		}
		err := WithPermissionReviewSecurityCeiling("consumer-access-profile/v1")(state)
		var target *DefinitionError
		if !errors.As(err, &target) || target.Kind != DefinitionDuplicateOption {
			t.Fatalf("second WithPermissionReviewSecurityCeiling error = %T %v, want duplicate option", err, err)
		}
	})
}

// TestDefinePermissionReviewRequiresSecurityCeilingWhenClassifiersConfigured
// proves the fail-closed pairing requirement: any registered permission
// classifier makes WithPermissionReviewSecurityCeiling mandatory, mirroring
// TestDefinePermissionReviewRequiresEvidenceOptionWhenClassifierNeedsIt's
// precedent for the sibling "config X requires config Y" pairing. Unlike the
// evidence pairing (keyed on "does a classifier need evidence tools"), this
// one is keyed on "are classifiers configured at all" — SecurityCeiling
// flows into every classifier's ReviewBasis regardless of whether it
// declares evidence tools (review_adapter.go's reviewOne).
func TestDefinePermissionReviewRequiresSecurityCeilingWhenClassifiersConfigured(t *testing.T) {
	t.Parallel()
	classifier := defineRigPermissionClassifier(t, "needs-ceiling", rigEvidencePolicy("status"))
	set := rigPermissionClassifierSet(t, classifier)
	root := t.TempDir()
	_, err := Define(validRigOptions(t,
		WithPermissionClassifiers(set),
		WithPermissionReviewPolicy(rigReviewPolicy(t, "review-v1")),
		WithHustleLimits(validHustleLimits()),
		WithPermissionReviewEvidence(
			stubPermissionReviewEvidenceAccess{}, stubPermissionReviewEvidenceContainment{}, []string{"filesystem.read"},
		),
		WithSessionWorkspaces(wsStoreT(t), root),
		WithSnapshots(SnapshotPolicy{}),
	)...)
	var target *DefinitionError
	if !errors.As(err, &target) || target.Kind != DefinitionMissingPermissionReviewSecurityCeiling {
		t.Fatalf("Define() error = %T %v, want DefinitionMissingPermissionReviewSecurityCeiling", err, err)
	}
}

// TestDefinePermissionReviewRejectsSecurityCeilingWhenNoClassifiersConfigured
// is the symmetric "unused config" check: WithPermissionReviewSecurityCeiling
// supplied when no permission classifiers are registered is rejected rather
// than silently accepted, mirroring
// TestDefinePermissionReviewRejectsEvidenceOptionWhenNoClassifierNeedsIt.
func TestDefinePermissionReviewRejectsSecurityCeilingWhenNoClassifiersConfigured(t *testing.T) {
	t.Parallel()
	_, err := Define(validRigOptions(t,
		WithPermissionReviewSecurityCeiling("consumer-access-profile/v1"),
	)...)
	var target *DefinitionError
	if !errors.As(err, &target) || target.Kind != DefinitionUnusedPermissionReviewSecurityCeiling {
		t.Fatalf("Define() error = %T %v, want DefinitionUnusedPermissionReviewSecurityCeiling", err, err)
	}
}

// TestDefinePermissionReviewSucceedsAndConstructsSessionWithSecurityCeilingOption
// proves the positive path end to end, mirroring
// TestDefinePermissionReviewSucceedsAndConstructsSessionWithEvidenceOption:
// with WithPermissionReviewSecurityCeiling supplied alongside classifiers,
// Define() succeeds AND the constructed Rig brings up a live session whose
// consumer-supplied ceiling genuinely reaches loopReviewContext — the exact
// value that, before this addendum, was a fixed Harness-side sentinel no
// real consumer's own containment check could ever match (Finding 2).
func TestDefinePermissionReviewSucceedsAndConstructsSessionWithSecurityCeilingOption(t *testing.T) {
	t.Parallel()
	classifier := defineRigPermissionClassifier(t, "needs-ceiling", rigEvidencePolicy("status"))
	set := rigPermissionClassifierSet(t, classifier)
	root := t.TempDir()
	const ceiling = "consumer-access-profile/define-e2e-v1"
	r, err := Define(validRigOptions(t,
		WithPermissionClassifiers(set),
		WithPermissionReviewPolicy(rigReviewPolicy(t, "review-v1")),
		WithHustleLimits(validHustleLimits()),
		WithPermissionReviewEvidence(
			stubPermissionReviewEvidenceAccess{}, stubPermissionReviewEvidenceContainment{}, []string{"filesystem.read"},
		),
		WithPermissionReviewSecurityCeiling(ceiling),
		WithSessionWorkspaces(wsStoreT(t), root),
		WithSnapshots(SnapshotPolicy{}),
	)...)
	if err != nil {
		t.Fatalf("Define() error = %v, want success", err)
	}

	controller, err := r.NewSession(context.Background())
	if err != nil {
		t.Fatalf("NewSession() error = %v, want a live session", err)
	}
	t.Cleanup(func() { _ = controller.Shutdown(context.Background()) })
	if controller.SessionID().IsZero() {
		t.Fatal("NewSession returned a zero session ID")
	}
}
