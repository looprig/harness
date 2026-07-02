package aci

import (
	"errors"
	"testing"
)

// TestPolicyIsPinned verifies that IsPinned reports true iff at least one
// acceptance set is non-empty. Every set counts — including AcceptedWorkloadIDs,
// where a workload-id-only policy is the strictest pin.
func TestPolicyIsPinned(t *testing.T) {
	tests := []struct {
		name string
		p    Policy
		want bool
	}{
		{"empty is unpinned", Policy{}, false},
		{"appid pins", Policy{AcceptedAppIDs: map[string]struct{}{"a": {}}}, true},
		{"provenance pins", Policy{AcceptedSourceProvenance: map[ProvenanceKey]struct{}{{}: {}}}, true},
		{"kms root pins", Policy{AcceptedKMSRootPubKeys: map[string]struct{}{"k": {}}}, true},
		{"workload-id-only pins (strictest)", Policy{AcceptedWorkloadIDs: map[string]struct{}{"sha256:x": {}}}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := tt.p.IsPinned(); got != tt.want {
				t.Errorf("IsPinned() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestPolicyRequireAcceptable verifies the fail-closed gate: an unpinned policy
// is rejected with *UnpinnedPolicyError unless it pins a set or opts in via
// UnpinnedPolicy().
func TestPolicyRequireAcceptable(t *testing.T) {
	tests := []struct {
		name    string
		p       Policy
		wantErr bool
	}{
		{"empty rejected", Policy{}, true},
		{"pinned ok", Policy{AcceptedAppIDs: map[string]struct{}{"a": {}}}, false},
		{"explicit unpinned ok", UnpinnedPolicy(), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := tt.p.requireAcceptable()
			if (err != nil) != tt.wantErr {
				t.Fatalf("requireAcceptable() err=%v wantErr=%v", err, tt.wantErr)
			}
			if tt.wantErr {
				var upe *UnpinnedPolicyError
				if !errors.As(err, &upe) {
					t.Fatalf("want *UnpinnedPolicyError, got %v", err)
				}
			}
		})
	}
}
