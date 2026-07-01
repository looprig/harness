package aci

// This file defines the attestation acceptance Policy and DefaultPhalaPolicy, the
// allow-lists VerifyReport (verify.go, Task 2.8) enforces in attestation steps 5,
// 7, and 9. The policy decides whether a CRYPTOGRAPHICALLY GENUINE workload is one
// we are willing to trust: the earlier chain steps prove the report is internally
// coherent and quote-backed; the policy then pins WHICH app-id, build provenance,
// KMS root, and (optionally) workload_id we accept. Every check is "when
// configured" — an empty/nil accepted set skips that check (the corresponding
// verify.go helper returns nil on an empty set), so a zero-value Policy{} accepts
// any genuine report and DefaultPhalaPolicy is the production-leaning preset.
//
// The accepted-set KEY FORMS are dictated by the verify.go helpers that consume
// them and MUST match byte-for-byte:
//   - AcceptedWorkloadIDs:      the report's workload_id string ("sha256:<hex>").
//   - AcceptedSourceProvenance: provenanceKey{RepoURL, RepoCommit} (rtmr.go).
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
	// ("sha256:<hex>"). EMPTY by default in DefaultPhalaPolicy: the workload_id is
	// a digest of the keyset, which ROTATES with each keyset epoch, so pinning it
	// would reject a legitimately rotated keyset. Trust is instead anchored by
	// app-id + provenance + KMS-root custody (the fields below), which are stable
	// across rotations. Populate this only to pin one exact keyset.
	AcceptedWorkloadIDs map[string]struct{}

	// AcceptedSourceProvenance is the set of accepted {repo_url, repo_commit}
	// build-provenance pairs (rtmr.go's provenanceKey). Checked in step 5.
	AcceptedSourceProvenance map[provenanceKey]struct{}

	// AcceptedAppIDs is the set of accepted workload app-ids as lowercase hex of
	// the app-id bytes (encoding/hex form). Checked in step 5.
	AcceptedAppIDs map[string]struct{}

	// AcceptedKMSRootPubKeys is the set of accepted KMS-root public keys as
	// compressed-SEC1 hex. Checked in step 7 (key custody recovers the root and
	// requires membership).
	AcceptedKMSRootPubKeys map[string]struct{}
}

// Pinned DefaultPhalaPolicy anchors, recovered from the live aci/1 fixture
// (testdata/report_aci1.json) and independently re-derived across Tasks 2.1, 2.5,
// and 2.7. They are domain constants here so DefaultPhalaPolicy stays a pure
// constructor.
//
// BLOCKER #3 — VERIFY INDEPENDENTLY: every value below is recovered FROM THE
// FIXTURE and has NOT yet been cross-checked against a published Phala/Dstack
// source. Before trusting these in production, confirm each against an
// authoritative Phala/Dstack publication. The KMS root (defaultPhalaKMSRoot)
// especially is recovered from the fixture's custody chain and is NOT yet
// externally confirmed; accepting it pins trust in whatever signed that chain.
const (
	// defaultPhalaAppID is the workload app-id, lowercase hex (Task 2.5 form).
	// Verify independently against a published Phala/Dstack source before
	// production trust (blocker #3).
	defaultPhalaAppID = "fdb7a14e5a6675f752e2cb69c9067a98ca402918"

	// defaultPhalaRepoURL / defaultPhalaRepoCommit* are the build provenance.
	// TWO commits are accepted: the fixture-audited commit and the currently
	// deployed commit observed live at Task 6.1. Both attest to the byte-identical
	// workload_keyset_digest (sha256:46cdea44…) — i.e. the same attested keyset,
	// app-id, identity key, and KMS root — so the live deployment is the same
	// workload redeployed at a bumped commit tag, not a different build.
	// Verify each independently against a published Phala/Dstack source before
	// production trust (blocker #3), and prune stale commits as the gateway upgrades.
	defaultPhalaRepoURL = "https://github.com/Dstack-TEE/private-ai-gateway.git"
	// defaultPhalaRepoCommit is the commit the offline fixture (report_aci1.json)
	// was captured against.
	defaultPhalaRepoCommit = "1b43f76e43c2459856faebe9cd97d8e01cb0df0c"
	// defaultPhalaRepoCommitDeployed is the commit the live gateway reported at
	// Task 6.1 (2026-07-01); same workload_keyset_digest as the fixture commit.
	defaultPhalaRepoCommitDeployed = "e776e9cf1f9c2d61730da5d2f4b717e49041da0d"

	// defaultPhalaKMSRoot is the KMS-root public key, compressed-SEC1 hex (Task
	// 2.7 form). RECOVERED FROM THE FIXTURE custody chain and NOT yet externally
	// confirmed — verify independently against a published Phala/Dstack KMS root
	// before production trust (blocker #3).
	defaultPhalaKMSRoot = "0334c76e0c3f52ec64cbf9bbf5c910c272330166fd656c0a86bb330963e46910e1"
)

// DefaultPhalaPolicy returns the production-leaning preset that pins the
// fixture-derived Phala/Dstack anchors: the workload app-id, the build provenance
// {repo_url, repo_commit}, and the KMS-root public key. AcceptedWorkloadIDs is
// left EMPTY on purpose — the workload_id rotates with the keyset epoch, so step 9
// is skipped by default and trust is anchored by app-id + provenance + KMS custody
// (the stable identifiers). It returns a FRESH value each call (independent maps),
// so a caller may narrow it without corrupting the package default.
//
// BLOCKER #3: the pinned anchors are recovered from the fixture and are NOT yet
// externally confirmed against a published Phala/Dstack source — verify each
// independently before relying on this preset in production.
func DefaultPhalaPolicy() Policy {
	return Policy{
		AcceptedWorkloadIDs: map[string]struct{}{},
		AcceptedSourceProvenance: map[provenanceKey]struct{}{
			{RepoURL: defaultPhalaRepoURL, RepoCommit: defaultPhalaRepoCommit}:         {},
			{RepoURL: defaultPhalaRepoURL, RepoCommit: defaultPhalaRepoCommitDeployed}: {},
		},
		AcceptedAppIDs: map[string]struct{}{
			defaultPhalaAppID: {},
		},
		AcceptedKMSRootPubKeys: map[string]struct{}{
			defaultPhalaKMSRoot: {},
		},
	}
}
