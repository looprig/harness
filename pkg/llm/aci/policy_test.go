package aci

import "testing"

// This file tests the attestation verification policy (Task 2.8): the Policy
// struct's accepted-set shapes and DefaultPhalaPolicy's pinned, fixture-derived
// anchors. The pinned values are the SAME constants the per-step tests already
// verified against the live fixture (rtmr_test.go's fixtureAppIDHex /
// fixtureRepoURL / fixtureRepoCommit, keys_test.go's fixtureKMSRoot); this test
// re-pins them through DefaultPhalaPolicy so a drift in either side is caught.
//
// Blocker #3 reminder: every DefaultPhalaPolicy anchor is recovered from the
// fixture and NOT yet externally confirmed against a published Phala/Dstack
// source. These tests pin the CURRENT values; they are not an endorsement of
// their production trustworthiness.

// TestDefaultPhalaPolicyAnchors asserts DefaultPhalaPolicy pins exactly the
// fixture-derived app-id, source-provenance, and KMS-root anchors, and leaves
// AcceptedWorkloadIDs empty (step 9 skipped by default — the keyset, and thus
// workload_id, rotates; trust is anchored by app-id + provenance + KMS custody).
func TestDefaultPhalaPolicyAnchors(t *testing.T) {
	t.Parallel()
	p := DefaultPhalaPolicy()

	// app-id: exactly the one fixture anchor, in lowercase-hex form.
	if len(p.AcceptedAppIDs) != 1 {
		t.Fatalf("AcceptedAppIDs has %d entries, want 1", len(p.AcceptedAppIDs))
	}
	if _, ok := p.AcceptedAppIDs[fixtureAppIDHex]; !ok {
		t.Errorf("AcceptedAppIDs missing fixture app-id %q", fixtureAppIDHex)
	}

	// source-provenance: exactly the one fixture {repo_url, repo_commit} pair.
	if len(p.AcceptedSourceProvenance) != 1 {
		t.Fatalf("AcceptedSourceProvenance has %d entries, want 1", len(p.AcceptedSourceProvenance))
	}
	wantProv := provenanceKey{RepoURL: fixtureRepoURL, RepoCommit: fixtureRepoCommit}
	if _, ok := p.AcceptedSourceProvenance[wantProv]; !ok {
		t.Errorf("AcceptedSourceProvenance missing fixture provenance %+v", wantProv)
	}

	// KMS root: exactly the one fixture-recovered compressed-SEC1 hex (blocker #3).
	if len(p.AcceptedKMSRootPubKeys) != 1 {
		t.Fatalf("AcceptedKMSRootPubKeys has %d entries, want 1", len(p.AcceptedKMSRootPubKeys))
	}
	if _, ok := p.AcceptedKMSRootPubKeys[fixtureKMSRoot]; !ok {
		t.Errorf("AcceptedKMSRootPubKeys missing fixture KMS root %q", fixtureKMSRoot)
	}

	// workload_id: empty by design (step 9 skipped by default).
	if len(p.AcceptedWorkloadIDs) != 0 {
		t.Errorf("AcceptedWorkloadIDs = %d entries, want 0 (empty by design)", len(p.AcceptedWorkloadIDs))
	}
}

// TestDefaultPhalaPolicyIsCopy asserts DefaultPhalaPolicy returns an independent
// value each call: mutating one returned policy's maps must not leak into the
// next, so a caller that narrows a policy cannot corrupt the package default.
func TestDefaultPhalaPolicyIsCopy(t *testing.T) {
	t.Parallel()
	a := DefaultPhalaPolicy()
	a.AcceptedWorkloadIDs["sha256:tampered"] = struct{}{}
	delete(a.AcceptedAppIDs, fixtureAppIDHex)

	b := DefaultPhalaPolicy()
	if _, ok := b.AcceptedWorkloadIDs["sha256:tampered"]; ok {
		t.Errorf("DefaultPhalaPolicy() AcceptedWorkloadIDs shares state across calls")
	}
	if _, ok := b.AcceptedAppIDs[fixtureAppIDHex]; !ok {
		t.Errorf("DefaultPhalaPolicy() AcceptedAppIDs shares state across calls (lost fixture app-id)")
	}
}
