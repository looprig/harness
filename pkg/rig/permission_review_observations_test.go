package rig

import (
	"context"
	"errors"
	"testing"

	"github.com/looprig/harness/pkg/gate"
)

// stubPermissionReviewObservationVerifier is a minimal
// gate.EvidenceObservationVerifier test double: rig never evaluates it
// itself, it only forwards it down to sessionruntime at construction —
// mirroring stubPermissionReviewEvidenceContainment's role for
// WithPermissionReviewEvidence.
type stubPermissionReviewObservationVerifier struct{}

func (stubPermissionReviewObservationVerifier) VerifyEvidenceObservations(
	context.Context, gate.EvidenceContainmentPolicy, []gate.ObservationRequirement,
) error {
	return nil
}

// TestWithPermissionReviewObservationsRejectsNilAndDuplicate mirrors
// TestWithPermissionReviewEvidenceRejectsInvalidAndDuplicateValues's shape
// for the new TOCTOU-recheck option.
func TestWithPermissionReviewObservationsRejectsNilAndDuplicate(t *testing.T) {
	t.Parallel()

	t.Run("nil verifier", func(t *testing.T) {
		t.Parallel()
		state := &definitionState{seen: make(map[singletonKey]bool)}
		err := WithPermissionReviewObservations(nil)(state)
		var target *DefinitionError
		if !errors.As(err, &target) || target.Kind != DefinitionInvalidPermissionReviewObservations {
			t.Fatalf("option error = %T %v, want DefinitionInvalidPermissionReviewObservations", err, err)
		}
	})

	t.Run("duplicate option", func(t *testing.T) {
		t.Parallel()
		state := &definitionState{seen: make(map[singletonKey]bool)}
		verifier := stubPermissionReviewObservationVerifier{}
		if err := WithPermissionReviewObservations(verifier)(state); err != nil {
			t.Fatalf("first WithPermissionReviewObservations: %v", err)
		}
		err := WithPermissionReviewObservations(verifier)(state)
		var target *DefinitionError
		if !errors.As(err, &target) || target.Kind != DefinitionDuplicateOption {
			t.Fatalf("second WithPermissionReviewObservations error = %T %v, want duplicate option", err, err)
		}
	})
}

// TestDefinePermissionReviewRejectsObservationsOptionWithoutClassifiers
// proves the "unused config" half of WithPermissionReviewObservations'
// pairing: configuring the TOCTOU verifier with no permission classifier
// registered at all is dead configuration, rejected rather than silently
// accepted — mirroring TestDefinePermissionReviewRejectsEvidenceOptionWhenNoClassifierNeedsIt.
func TestDefinePermissionReviewRejectsObservationsOptionWithoutClassifiers(t *testing.T) {
	t.Parallel()
	_, err := Define(validRigOptions(t,
		WithPermissionReviewObservations(stubPermissionReviewObservationVerifier{}),
	)...)
	var target *DefinitionError
	if !errors.As(err, &target) || target.Kind != DefinitionUnusedPermissionReviewObservations {
		t.Fatalf("Define() error = %T %v, want DefinitionUnusedPermissionReviewObservations", err, err)
	}
}

// TestValidatePermissionReviewObservationsRejectsMissingEvidence exercises
// validatePermissionReviewObservations directly (rather than through
// Define()) to isolate its second "unused" branch — classifiers registered,
// observations configured, but WithPermissionReviewEvidence never called.
// Every registered classifier in this codebase currently needs an evidence
// runtime (anyClassifierNeedsEvidence's own doc comment), so this exact
// combination would ALSO trip the earlier, unrelated
// DefinitionMissingPermissionReviewEvidence check first inside a real
// Define() call — this test proves validatePermissionReviewObservations'
// OWN logic is correct in isolation, independent of that ordering.
func TestValidatePermissionReviewObservationsRejectsMissingEvidence(t *testing.T) {
	t.Parallel()
	classifier := defineRigPermissionClassifier(t, "needs-evidence", rigEvidencePolicy("status"))
	set := rigPermissionClassifierSet(t, classifier)
	state := &definitionState{seen: make(map[singletonKey]bool)}
	if err := WithPermissionClassifiers(set)(state); err != nil {
		t.Fatalf("WithPermissionClassifiers: %v", err)
	}
	if err := WithPermissionReviewObservations(stubPermissionReviewObservationVerifier{})(state); err != nil {
		t.Fatalf("WithPermissionReviewObservations: %v", err)
	}
	err := validatePermissionReviewObservations(state)
	var target *DefinitionError
	if !errors.As(err, &target) || target.Kind != DefinitionUnusedPermissionReviewObservations {
		t.Fatalf("validatePermissionReviewObservations() error = %T %v, want DefinitionUnusedPermissionReviewObservations", err, err)
	}
}

// TestDefinePermissionReviewSucceedsWithoutObservationsOption proves the
// documented degrade-safely default: a classifier-registered, fully wired
// session (evidence configured, no target-sensitive evidence tool exists
// yet) never needs WithPermissionReviewObservations at all — Define()
// succeeds and NewSession brings up a live session exactly as it did before
// this addendum.
func TestDefinePermissionReviewSucceedsWithoutObservationsOption(t *testing.T) {
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
		WithPermissionReviewSecurityCeiling("consumer-access-profile/v1"),
		WithSessionWorkspaces(wsStoreT(t), root),
		WithSnapshots(SnapshotPolicy{}),
	)...)
	if err != nil {
		t.Fatalf("Define() error = %v, want success (WithPermissionReviewObservations must stay optional)", err)
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

// TestDefinePermissionReviewSucceedsWithObservationsOption proves the
// positive path end to end: a fully wired session that ALSO configures
// WithPermissionReviewObservations still constructs and brings up a live
// session — the verifier reaches sessionruntime without breaking anything
// evidence-related. internal/sessionruntime's own test suite proves the
// exact field-level threading this black-box test cannot see across the
// package boundary.
func TestDefinePermissionReviewSucceedsWithObservationsOption(t *testing.T) {
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
		WithPermissionReviewSecurityCeiling("consumer-access-profile/v1"),
		WithPermissionReviewObservations(stubPermissionReviewObservationVerifier{}),
		WithSessionWorkspaces(wsStoreT(t), root),
		WithSnapshots(SnapshotPolicy{}),
	)...)
	if err != nil {
		t.Fatalf("Define() error = %v, want success", err)
	}

	controller, err := r.NewSession(context.Background())
	if err != nil {
		t.Fatalf("NewSession() error = %v, want a live session (observation verifier should be fully wired)", err)
	}
	t.Cleanup(func() { _ = controller.Shutdown(context.Background()) })
	if controller.SessionID().IsZero() {
		t.Fatal("NewSession returned a zero session ID")
	}
}
