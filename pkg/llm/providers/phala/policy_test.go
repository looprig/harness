package phala_test

import (
	"errors"
	"testing"

	"github.com/ciram-co/looprig/pkg/llm"
	"github.com/ciram-co/looprig/pkg/llm/aci"
	"github.com/ciram-co/looprig/pkg/llm/auth"
	"github.com/ciram-co/looprig/pkg/llm/providers/phala"
)

// This file tests the Phala provider's New constructor: its fail-closed credential
// contract, its empty-baseURL self-default, and its forwarding of the caller's
// acceptance Policy into aci. The package ships no pinned trust anchors; the caller
// supplies the Policy, so these tests build a minimal pinned policy inline
// (testPinnedPolicy) to exercise construction/forwarding.

// testPinnedPolicy builds a minimal pinned aci.Policy for construction/forwarding
// tests. The app-id is a fixture-derived value used ONLY as test input.
func testPinnedPolicy() aci.Policy {
	return aci.Policy{AcceptedAppIDs: map[string]struct{}{"fdb7a14e5a6675f752e2cb69c9067a98ca402918": {}}}
}

// TestNew pins the constructor's fail-closed credential contract AND its two
// forwarding/defaulting behaviors: an empty key is rejected with a typed
// *llm.AuthRequiredError (Provider phala, Kind api_key) before any network call and
// before the base-URL default (so a missing key always wins); an empty baseURL with
// a valid key self-defaults to the canonical host and still builds a non-nil client;
// and an unpinned aci.Policy is forwarded through aci.New as a typed
// *aci.UnpinnedPolicyError. A non-empty key with a pinned policy yields a non-nil
// llm.LLM and no error.
func TestNew(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		baseURL      string
		key          auth.APIKey
		policy       aci.Policy
		wantErr      bool
		wantAuthErr  bool // expect *llm.AuthRequiredError
		wantUnpinned bool // expect *aci.UnpinnedPolicyError
	}{
		{name: "empty key rejected fail-closed", baseURL: "https://inference.phala.com", key: "", policy: testPinnedPolicy(), wantErr: true, wantAuthErr: true},
		{name: "non-empty key builds client", baseURL: "https://inference.phala.com", key: "sk-live-xyz", policy: testPinnedPolicy(), wantErr: false},
		{name: "empty key rejected even with empty baseURL", baseURL: "", key: "", policy: testPinnedPolicy(), wantErr: true, wantAuthErr: true},
		{name: "empty baseURL self-defaults with valid key", baseURL: "", key: "sk-live-xyz", policy: testPinnedPolicy(), wantErr: false},
		{name: "unpinned policy forwarded as typed error", baseURL: "https://inference.phala.com", key: "sk-live-xyz", policy: aci.Policy{}, wantErr: true, wantUnpinned: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			client, err := phala.New(tt.baseURL, tt.key, tt.policy)
			if (err != nil) != tt.wantErr {
				t.Fatalf("New() error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				if client != nil {
					t.Errorf("New() returned non-nil client alongside error")
				}
				if tt.wantAuthErr {
					var are *llm.AuthRequiredError
					if !errors.As(err, &are) {
						t.Fatalf("New() error = %T (%v), want *llm.AuthRequiredError", err, err)
					}
					if are.Provider != llm.ProviderPhala || are.Kind != llm.AuthAPIKey {
						t.Errorf("AuthRequiredError = {Provider:%s Kind:%s}, want {phala api_key}", are.Provider, are.Kind)
					}
				}
				if tt.wantUnpinned {
					var upe *aci.UnpinnedPolicyError
					if !errors.As(err, &upe) {
						t.Fatalf("New() error = %T (%v), want *aci.UnpinnedPolicyError", err, err)
					}
				}
				return
			}
			if client == nil {
				t.Errorf("New() = nil client, want non-nil llm.LLM")
			}
		})
	}
}
