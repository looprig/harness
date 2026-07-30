package rig

import (
	"context"
	"testing"
)

// TestReadmeExamplePermissionReviewOptionsCompileAndSucceed guards
// pkg/rig/README.md's "How to use" example: every permission-review Option
// name, signature, and pairing requirement documented there
// (WithPermissionClassifiers, WithPermissionReviewPolicy,
// WithPermissionReviewSecurityCeiling, WithPermissionReviewEvidence,
// WithPermissionReviewLimits) is exercised together here exactly as a real
// consumer would call them, so a rename or signature change breaks this
// test instead of leaving the README silently stale — the same class of gap
// Phase 6 found and fixed for WithPermissionReviewPolicyRevision
// (commit c8fdca47). It reuses this package's own existing test
// collaborators (defineRigPermissionClassifier and friends) rather than
// hand-rolling a second classifier fake.
func TestReadmeExamplePermissionReviewOptionsCompileAndSucceed(t *testing.T) {
	t.Parallel()

	classifier := defineRigPermissionClassifier(t, "readme-example-classifier", rigEvidencePolicy("status"))
	permissionClassifiers := rigPermissionClassifierSet(t, classifier)
	reviewPolicy := rigReviewPolicy(t, "review-policy-v1")
	evidenceAccess := stubPermissionReviewEvidenceAccess{}
	evidenceContainment := stubPermissionReviewEvidenceContainment{}
	allowedEvidenceKinds := []string{"filesystem.read"}
	root := t.TempDir()

	r, err := Define(validRigOptions(t,
		WithHustleLimits(validHustleLimits()),
		WithPermissionClassifiers(permissionClassifiers),
		WithPermissionReviewPolicy(reviewPolicy),
		WithPermissionReviewSecurityCeiling("consumer-access-profile/v1"),
		WithPermissionReviewEvidence(evidenceAccess, evidenceContainment, allowedEvidenceKinds),
		WithPermissionReviewLimits(PermissionReviewLimits{
			MaxConsecutiveNeedsHuman: 20,
			MaxInvalidOrFailed:       20,
			MaxIdenticalSubjects:     20,
			MaxStaleResponses:        20,
			InterruptOnTrip:          false,
			Session: PermissionReviewSessionLimits{
				MaxConsecutiveNeedsHuman: 20,
				MaxInvalidOrFailed:       20,
				MaxIdenticalSubjects:     20,
				MaxStaleResponses:        20,
			},
		}),
		WithSessionWorkspaces(wsStoreT(t), root),
		WithSnapshots(SnapshotPolicy{}),
	)...)
	if err != nil {
		t.Fatalf("Define() with the README's documented permission-review options = %v, want success", err)
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

// TestReadmeExampleZeroPermissionReviewOptionsPreservesPlainRig guards the
// README's "omitting every one of them ... preserves the pre-classifier gate
// behavior byte-for-byte" claim: a rig built with none of the
// permission-review options must construct and open a session exactly as it
// did before this feature existed.
func TestReadmeExampleZeroPermissionReviewOptionsPreservesPlainRig(t *testing.T) {
	t.Parallel()

	r, err := Define(validRigOptions(t)...)
	if err != nil {
		t.Fatalf("Define() with zero permission-review options = %v, want success", err)
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
