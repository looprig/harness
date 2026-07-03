// Package phala is the Phala confidential-inference provider: it owns the pinned
// Phala/Dstack trust anchors and composes the reusable, provider-agnostic aci
// attestation protocol with them. aci enforces whatever acceptance Policy it is
// handed; this package supplies the production-leaning Policy (DefaultPolicy) that
// pins Phala's app-id, build provenance, and KMS root, and a typed constructor
// (New) that wires that Policy into an attested llm.LLM. aci never imports this
// package (that would cycle); the dependency is one-way, phala → aci.
package phala

import (
	"github.com/looprig/harness/pkg/llm"
	"github.com/looprig/harness/pkg/llm/aci"
	"github.com/looprig/harness/pkg/llm/auth"
)

// Pinned Phala/Dstack trust anchors, recovered from the live aci/1 fixture
// (aci/testdata/report_aci1.json) and independently re-derived across attestation
// Tasks 2.1, 2.5, and 2.7. They are domain constants here so DefaultPolicy stays a
// pure constructor. Relocated VERBATIM from the former aci.DefaultPhalaPolicy —
// not re-derived; every value below is byte-identical to the aci original.
//
// BLOCKER #3 — VERIFY INDEPENDENTLY: every value below is recovered FROM THE
// FIXTURE and has NOT yet been cross-checked against a published Phala/Dstack
// source. Before trusting these in production, confirm each against an
// authoritative Phala/Dstack publication. The KMS root (defaultKMSRoot)
// especially is recovered from the fixture's custody chain and is NOT yet
// externally confirmed; accepting it pins trust in whatever signed that chain.
const (
	// defaultAppID is the workload app-id, lowercase hex (Task 2.5 form). Verify
	// independently against a published Phala/Dstack source before production
	// trust (blocker #3).
	defaultAppID = "fdb7a14e5a6675f752e2cb69c9067a98ca402918"

	// defaultRepoURL / defaultRepoCommit* are the build provenance. TWO commits
	// are accepted: the fixture-audited commit and the currently deployed commit
	// observed live at Task 6.1. Both attest to the byte-identical
	// workload_keyset_digest (sha256:46cdea44…) — i.e. the same attested keyset,
	// app-id, identity key, and KMS root — so the live deployment is the same
	// workload redeployed at a bumped commit tag, not a different build. Verify
	// each independently against a published Phala/Dstack source before production
	// trust (blocker #3), and prune stale commits as the gateway upgrades.
	defaultRepoURL = "https://github.com/Dstack-TEE/private-ai-gateway.git"
	// defaultRepoCommit is the commit the offline fixture (report_aci1.json) was
	// captured against.
	defaultRepoCommit = "1b43f76e43c2459856faebe9cd97d8e01cb0df0c"
	// defaultRepoCommitDeployed is the commit the live gateway reported at Task 6.1
	// (2026-07-01); same workload_keyset_digest as the fixture commit.
	defaultRepoCommitDeployed = "e776e9cf1f9c2d61730da5d2f4b717e49041da0d"

	// defaultKMSRoot is the KMS-root public key, compressed-SEC1 hex (Task 2.7
	// form). RECOVERED FROM THE FIXTURE custody chain and NOT yet externally
	// confirmed — verify independently against a published Phala/Dstack KMS root
	// before production trust (blocker #3).
	defaultKMSRoot = "0334c76e0c3f52ec64cbf9bbf5c910c272330166fd656c0a86bb330963e46910e1"
)

// DefaultPolicy returns the production-leaning aci.Policy preset that pins the
// fixture-derived Phala/Dstack anchors: the workload app-id, the build provenance
// {repo_url, repo_commit}, and the KMS-root public key. AcceptedWorkloadIDs is
// left EMPTY on purpose — the workload_id rotates with the keyset epoch, so aci's
// step 9 is skipped by default and trust is anchored by app-id + provenance + KMS
// custody (the stable identifiers). It returns a FRESH value each call
// (independent maps), so a caller may narrow it without corrupting the package
// default.
//
// BLOCKER #3: the pinned anchors are recovered from the fixture and are NOT yet
// externally confirmed against a published Phala/Dstack source — verify each
// independently before relying on this preset in production.
func DefaultPolicy() aci.Policy {
	return aci.Policy{
		AcceptedWorkloadIDs: map[string]struct{}{},
		AcceptedSourceProvenance: map[aci.ProvenanceKey]struct{}{
			{RepoURL: defaultRepoURL, RepoCommit: defaultRepoCommit}:         {},
			{RepoURL: defaultRepoURL, RepoCommit: defaultRepoCommitDeployed}: {},
		},
		AcceptedAppIDs: map[string]struct{}{
			defaultAppID: {},
		},
		AcceptedKMSRootPubKeys: map[string]struct{}{
			defaultKMSRoot: {},
		},
	}
}

// New builds an attested Phala confidential-inference client: it composes the
// reusable aci attestation protocol with the supplied acceptance Policy (pass
// DefaultPolicy for the pinned preset). baseURL is the gateway origin (e.g.
// https://inference.phala.com); key is the bearer credential and is REQUIRED — the
// typed auth.APIKey parameter encodes that at compile time, and New fails closed on
// an empty string with a typed *llm.AuthRequiredError before any network call. On
// success it returns the llm.LLM aci implements.
func New(baseURL string, key auth.APIKey, p aci.Policy) (llm.LLM, error) {
	if key == "" {
		return nil, &llm.AuthRequiredError{Provider: llm.ProviderPhala, Kind: llm.AuthAPIKey}
	}
	return aci.New(baseURL, string(key), p), nil
}
