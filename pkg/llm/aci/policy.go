package aci

// This file defines the attestation acceptance Policy, the allow-lists
// VerifyReport (verify.go, Task 2.8) enforces in attestation steps 5, 7, and 9.
// The policy decides whether a CRYPTOGRAPHICALLY GENUINE workload is one we are
// willing to trust: the earlier chain steps prove the report is internally
// coherent and quote-backed; the policy then pins WHICH app-id, build provenance,
// KMS root, and (optionally) workload_id we accept. Every check is "when
// configured" — an empty/nil accepted set skips that check (the corresponding
// verify.go helper returns nil on an empty set), so a zero-value Policy{} accepts
// any genuine report.
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
// membership. The zero value Policy{} therefore accepts any genuine, quote-backed
// report (no allow-listing) — callers narrow trust by populating fields.
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
}
