package phala_test

import (
	"errors"
	"testing"

	"github.com/looprig/harness/pkg/llm"
	"github.com/looprig/harness/pkg/llm/aci"
	"github.com/looprig/harness/pkg/llm/auth"
	"github.com/looprig/harness/pkg/llm/providers/phala"
)

// This file tests the Phala provider preset: DefaultPolicy's pinned,
// fixture-derived trust anchors and the New constructor's fail-closed credential
// contract. The pinned values are re-pinned here as independent literals (NOT read
// back from the package under test) so a drift between the shipped constants and
// the expected values is caught — the same drift-detection the aci per-step tests
// give against the fixture (rtmr_test.go / keys_test.go).
//
// Blocker #3 reminder: every DefaultPolicy anchor is recovered from the aci/1
// fixture and NOT yet externally confirmed against a published Phala/Dstack
// source. These tests pin the CURRENT values; they are not an endorsement of their
// production trustworthiness.

// The expected pinned anchors, as independent literals (see file doc).
const (
	wantAppID              = "fdb7a14e5a6675f752e2cb69c9067a98ca402918"
	wantRepoURL            = "https://github.com/Dstack-TEE/private-ai-gateway.git"
	wantRepoCommit         = "1b43f76e43c2459856faebe9cd97d8e01cb0df0c"
	wantRepoCommitDeployed = "e776e9cf1f9c2d61730da5d2f4b717e49041da0d"
	wantKMSRoot            = "0334c76e0c3f52ec64cbf9bbf5c910c272330166fd656c0a86bb330963e46910e1"
)

// TestDefaultPolicyAnchors asserts DefaultPolicy pins exactly the fixture-derived
// app-id, source-provenance, and KMS-root anchors, and leaves AcceptedWorkloadIDs
// empty (aci step 9 skipped by default — the keyset, and thus workload_id,
// rotates; trust is anchored by app-id + provenance + KMS custody).
func TestDefaultPolicyAnchors(t *testing.T) {
	t.Parallel()
	p := phala.DefaultPolicy()

	// app-id: exactly the one fixture anchor, in lowercase-hex form.
	if len(p.AcceptedAppIDs) != 1 {
		t.Fatalf("AcceptedAppIDs has %d entries, want 1", len(p.AcceptedAppIDs))
	}
	if _, ok := p.AcceptedAppIDs[wantAppID]; !ok {
		t.Errorf("AcceptedAppIDs missing fixture app-id %q", wantAppID)
	}

	// source-provenance: two accepted {repo_url, repo_commit} pairs — the
	// fixture-audited commit and the live-deployed commit observed at Task 6.1
	// (both attest to the same workload_keyset_digest).
	if len(p.AcceptedSourceProvenance) != 2 {
		t.Fatalf("AcceptedSourceProvenance has %d entries, want 2", len(p.AcceptedSourceProvenance))
	}
	wantProv := aci.ProvenanceKey{RepoURL: wantRepoURL, RepoCommit: wantRepoCommit}
	if _, ok := p.AcceptedSourceProvenance[wantProv]; !ok {
		t.Errorf("AcceptedSourceProvenance missing fixture provenance %+v", wantProv)
	}
	wantProvDeployed := aci.ProvenanceKey{RepoURL: wantRepoURL, RepoCommit: wantRepoCommitDeployed}
	if _, ok := p.AcceptedSourceProvenance[wantProvDeployed]; !ok {
		t.Errorf("AcceptedSourceProvenance missing live-deployed provenance %+v", wantProvDeployed)
	}

	// KMS root: exactly the one fixture-recovered compressed-SEC1 hex (blocker #3).
	if len(p.AcceptedKMSRootPubKeys) != 1 {
		t.Fatalf("AcceptedKMSRootPubKeys has %d entries, want 1", len(p.AcceptedKMSRootPubKeys))
	}
	if _, ok := p.AcceptedKMSRootPubKeys[wantKMSRoot]; !ok {
		t.Errorf("AcceptedKMSRootPubKeys missing fixture KMS root %q", wantKMSRoot)
	}

	// workload_id: empty by design (step 9 skipped by default).
	if len(p.AcceptedWorkloadIDs) != 0 {
		t.Errorf("AcceptedWorkloadIDs = %d entries, want 0 (empty by design)", len(p.AcceptedWorkloadIDs))
	}
}

// TestDefaultPolicyIsCopy asserts DefaultPolicy returns an independent value each
// call: mutating one returned policy's maps must not leak into the next, so a
// caller that narrows a policy cannot corrupt the package default.
func TestDefaultPolicyIsCopy(t *testing.T) {
	t.Parallel()
	a := phala.DefaultPolicy()
	a.AcceptedWorkloadIDs["sha256:tampered"] = struct{}{}
	delete(a.AcceptedAppIDs, wantAppID)

	b := phala.DefaultPolicy()
	if _, ok := b.AcceptedWorkloadIDs["sha256:tampered"]; ok {
		t.Errorf("DefaultPolicy() AcceptedWorkloadIDs shares state across calls")
	}
	if _, ok := b.AcceptedAppIDs[wantAppID]; !ok {
		t.Errorf("DefaultPolicy() AcceptedAppIDs shares state across calls (lost fixture app-id)")
	}
}

// TestNew pins the constructor's fail-closed credential contract: an empty key is
// rejected with a typed *llm.AuthRequiredError (Provider phala, Kind api_key)
// before any network call; a non-empty key yields a non-nil llm.LLM and no error.
func TestNew(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		baseURL string
		key     auth.APIKey
		wantErr bool
	}{
		{name: "empty key rejected fail-closed", baseURL: "https://inference.phala.com", key: "", wantErr: true},
		{name: "non-empty key builds client", baseURL: "https://inference.phala.com", key: "sk-live-xyz", wantErr: false},
		{name: "empty key rejected even with empty baseURL", baseURL: "", key: "", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			client, err := phala.New(tt.baseURL, tt.key, phala.DefaultPolicy())
			if (err != nil) != tt.wantErr {
				t.Fatalf("New() error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				if client != nil {
					t.Errorf("New() returned non-nil client alongside error")
				}
				var are *llm.AuthRequiredError
				if !errors.As(err, &are) {
					t.Fatalf("New() error = %T (%v), want *llm.AuthRequiredError", err, err)
				}
				if are.Provider != llm.ProviderPhala || are.Kind != llm.AuthAPIKey {
					t.Errorf("AuthRequiredError = {Provider:%s Kind:%s}, want {phala api_key}", are.Provider, are.Kind)
				}
				return
			}
			if client == nil {
				t.Errorf("New() = nil client, want non-nil llm.LLM")
			}
		})
	}
}
