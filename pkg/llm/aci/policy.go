package aci

// This file defines the attestation acceptance Policy, the allow-lists
// VerifyReport (verify.go, Task 2.8) enforces in attestation steps 5, 7, and 9.
// The policy decides whether a CRYPTOGRAPHICALLY GENUINE workload is one we are
// willing to trust: the earlier chain steps prove the report is internally
// coherent and quote-backed; the policy then pins WHICH app-id, build provenance,
// KMS root, and (optionally) workload_id we accept. Every check is "when
// configured" — an empty/nil accepted set skips that check (the corresponding
// verify.go helper returns nil on an empty set), so a zero-value Policy{} accepts
// any genuine report. NOTE: that fail-open behavior is the LOW-LEVEL verifyReport
// mechanism only; the PUBLIC entry points (New/VerifyReport) fail closed via
// Policy.requireAcceptable, rejecting an unpinned Policy unless the caller opts in
// with UnpinnedPolicy() (see the Policy type doc below).
//
// Policy is deliberately provider-agnostic: it carries no pinned values of its
// own. A provider package (e.g. pkg/llm/providers/phala) owns the pinned trust
// anchors and constructs a populated Policy from them; aci only enforces the sets
// it is handed. Every field type is exported so such a constructor can populate
// the sets directly from outside this package.
//
// The accepted-set KEY FORMS are dictated by the verify.go helpers that consume
// them and MUST match byte-for-byte:
//   - AcceptedWorkloadIDs:      the report's workload_id string ("sha256:<hex>").
//   - AcceptedSourceProvenance: ProvenanceKey{RepoURL, RepoCommit} (rtmr.go).
//   - AcceptedAppIDs:           lowercase hex of the app-id bytes (encoding/hex).
//   - AcceptedKMSRootPubKeys:   compressed-SEC1 hex of the recovered KMS root.
//
// Each set is a map[...]struct{} (a set, not a list) so membership is an O(1)
// lookup and duplicates are impossible.

// Policy is the attestation acceptance allow-list set: which app-ids, source
// provenances, KMS roots, and workload_ids VerifyReport will accept. A nil/empty
// field skips its check ("when configured"); a non-empty field requires
// membership. At the LOW-LEVEL verifyReport (verify.go) a zero value Policy{}
// still accepts any genuine, quote-backed report (no allow-listing) — that is the
// mechanism. The PUBLIC entry points (New/VerifyReport, wired in later tasks)
// instead FAIL CLOSED via requireAcceptable: an unpinned Policy is rejected with
// *UnpinnedPolicyError unless the caller opts in with UnpinnedPolicy(). Callers
// narrow trust by populating fields.
type Policy struct {
	// AcceptedWorkloadIDs is the set of accepted workload_id strings
	// ("sha256:<hex>"). Commonly left EMPTY: the workload_id is a digest of the
	// keyset, which ROTATES with each keyset epoch, so pinning it would reject a
	// legitimately rotated keyset. Trust is instead anchored by app-id +
	// provenance + KMS-root custody (the fields below), which are stable across
	// rotations. Populate this only to pin one exact keyset.
	AcceptedWorkloadIDs map[string]struct{}

	// AcceptedSourceProvenance is the set of accepted {repo_url, repo_commit}
	// build-provenance pairs (rtmr.go's ProvenanceKey). Checked in step 5.
	AcceptedSourceProvenance map[ProvenanceKey]struct{}

	// AcceptedAppIDs is the set of accepted workload app-ids as lowercase hex of
	// the app-id bytes (encoding/hex form). Checked in step 5.
	AcceptedAppIDs map[string]struct{}

	// AcceptedKMSRootPubKeys is the set of accepted KMS-root public keys as
	// compressed-SEC1 hex. Checked in step 7 (key custody recovers the root and
	// requires membership).
	AcceptedKMSRootPubKeys map[string]struct{}

	// allowUnpinned, set ONLY by UnpinnedPolicy(), opts into genuineness-only
	// verification with no allow-listing. Unexported so no struct literal outside this
	// package can select the fail-open path by accident; the constructor is the only door.
	allowUnpinned bool
}

// IsPinned reports whether the policy pins at least one acceptance set. Any non-empty
// set means VerifyReport runs at least one allow-list check (steps 5/7/9), so the
// policy is not fail-open. AcceptedWorkloadIDs counts: a workload-ID-only policy is the
// strictest pin (one exact keyset digest).
func (p Policy) IsPinned() bool {
	return len(p.AcceptedWorkloadIDs) > 0 ||
		len(p.AcceptedSourceProvenance) > 0 ||
		len(p.AcceptedAppIDs) > 0 ||
		len(p.AcceptedKMSRootPubKeys) > 0
}

// UnpinnedPolicy returns a Policy that explicitly accepts any cryptographically genuine
// report WITHOUT allow-listing a workload. This is a deliberate, greppable opt-out of
// the fail-closed default; prefer a pinned Policy in production.
func UnpinnedPolicy() Policy { return Policy{allowUnpinned: true} }

// requireAcceptable is the shared fail-closed gate applied at every public aci entry
// (New, VerifyReport): a policy that pins nothing and did not opt into unpinned mode is
// rejected before any chain runs or network object exists.
func (p Policy) requireAcceptable() error {
	if !p.IsPinned() && !p.allowUnpinned {
		return &UnpinnedPolicyError{}
	}
	return nil
}
