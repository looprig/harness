package tools

import (
	"slices"
	"testing"

	"github.com/ciram-co/looprig/pkg/loop"
)

// fingerprintBase returns a fully-populated base policy for the digest tests. It is
// a fixed local (NOT DefaultHardDeny(), which could change under us) so the golden
// pin and the sensitivity table share one deterministic input.
func fingerprintBase() (PermissionPolicy, FingerprintMode) {
	policy := PermissionPolicy{
		HardApprove: HardApproveRules{Tools: []string{"ReadFile", "Glob"}},
		HardDeny: HardDenyRules{
			DeniedReadPaths:    []string{"~/.ssh/**", "**/.env"},
			DeniedWritePaths:   []string{"**/.git/config"},
			DeniedBashPrefixes: []string{"sudo", "rm -rf /"},
			MaxReadBytes:       1 << 20,
		},
		Policies: []loop.ToolPolicy{
			{Tool: "Bash", Effect: loop.EffectAutoApprove, Match: []string{"go test", "go build"}},
			{Tool: "Fetch", Effect: loop.EffectAutoApprove, Match: []string{"GET https://example.com"}},
		},
	}
	mode := FingerprintMode{Wrapped: true, Unattended: true}
	return policy, mode
}

// clonePolicy deep-clones the slice-bearing fields so a table row can mutate its own
// copy without corrupting the shared base.
func clonePolicy(p PermissionPolicy) PermissionPolicy {
	p.HardApprove.Tools = slices.Clone(p.HardApprove.Tools)
	p.HardDeny.DeniedReadPaths = slices.Clone(p.HardDeny.DeniedReadPaths)
	p.HardDeny.DeniedWritePaths = slices.Clone(p.HardDeny.DeniedWritePaths)
	p.HardDeny.DeniedBashPrefixes = slices.Clone(p.HardDeny.DeniedBashPrefixes)
	p.Policies = slices.Clone(p.Policies)
	for i := range p.Policies {
		p.Policies[i].Match = slices.Clone(p.Policies[i].Match)
	}
	return p
}

// TestPolicyFingerprint_StableAndSensitive locks the digest contract field-by-field:
// changing ANY enforcement-relevant field (or a mode bit) changes the digest, while
// reordering any canonicalized list leaves it unchanged. Each row is an independent
// deep-clone of the base so mutations never leak across rows.
func TestPolicyFingerprint_StableAndSensitive(t *testing.T) {
	t.Parallel()
	base, baseMode := fingerprintBase()
	want := PolicyFingerprint(base, baseMode)

	flipWrapped := baseMode
	flipWrapped.Wrapped = false
	flipUnattended := baseMode
	flipUnattended.Unattended = false

	addApprove := clonePolicy(base)
	addApprove.HardApprove.Tools = append(addApprove.HardApprove.Tools, "Bash")
	reorderApprove := clonePolicy(base)
	reorderApprove.HardApprove.Tools = []string{"Glob", "ReadFile"}

	addRead := clonePolicy(base)
	addRead.HardDeny.DeniedReadPaths = append(addRead.HardDeny.DeniedReadPaths, "**/secrets/**")
	addWrite := clonePolicy(base)
	addWrite.HardDeny.DeniedWritePaths = append(addWrite.HardDeny.DeniedWritePaths, "**/.npmrc")
	addBash := clonePolicy(base)
	addBash.HardDeny.DeniedBashPrefixes = append(addBash.HardDeny.DeniedBashPrefixes, "curl")
	changeMaxRead := clonePolicy(base)
	changeMaxRead.HardDeny.MaxReadBytes = (1 << 20) + 1

	addPolicy := clonePolicy(base)
	addPolicy.Policies = append(addPolicy.Policies, loop.ToolPolicy{Tool: "EditFile", Effect: loop.EffectAutoApprove, Match: []string{"**/*.go"}})
	changeEffect := clonePolicy(base)
	changeEffect.Policies[0].Effect = loop.EffectDeny
	changeMatch := clonePolicy(base)
	changeMatch.Policies[0].Match = []string{"go test", "go vet"}
	// Swap the two base policies' slice order — the digest must be unchanged
	// (writePolicies sorts the rendered lines).
	reorderPolicies := clonePolicy(base)
	reorderPolicies.Policies[0], reorderPolicies.Policies[1] = reorderPolicies.Policies[1], reorderPolicies.Policies[0]
	reorderMatch := clonePolicy(base)
	reorderMatch.Policies[0].Match = []string{"go build", "go test"}

	tests := []struct {
		name     string
		policy   PermissionPolicy
		mode     FingerprintMode
		wantSame bool
	}{
		{"identical base is same", clonePolicy(base), baseMode, true},
		{"Wrapped flip changes", clonePolicy(base), flipWrapped, false},
		{"Unattended flip changes", clonePolicy(base), flipUnattended, false},
		{"HardApprove add changes", addApprove, baseMode, false},
		{"HardApprove reorder is same", reorderApprove, baseMode, true},
		{"DeniedReadPaths add changes", addRead, baseMode, false},
		{"DeniedWritePaths add changes", addWrite, baseMode, false},
		{"DeniedBashPrefixes add changes", addBash, baseMode, false},
		{"MaxReadBytes change changes", changeMaxRead, baseMode, false},
		{"Policies add changes", addPolicy, baseMode, false},
		{"Policy Effect change changes", changeEffect, baseMode, false},
		{"Policy Match change changes", changeMatch, baseMode, false},
		{"Policies reorder is same", reorderPolicies, baseMode, true},
		{"Policy Match reorder is same", reorderMatch, baseMode, true},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := PolicyFingerprint(tt.policy, tt.mode)
			if (got == want) != tt.wantSame {
				t.Errorf("PolicyFingerprint same=%v, wantSame=%v (got=%s base=%s)", got == want, tt.wantSame, got, want)
			}
		})
	}
}

// TestPolicyFingerprint_Golden pins the digest of a fixed base policy+mode. If this
// drifts, the digest encoding or a covered field changed — update the constant
// CONSCIOUSLY (a silent change means a durable session could restore under a drifted
// policy).
func TestPolicyFingerprint_Golden(t *testing.T) {
	t.Parallel()
	const want = "a43ef08e25059e77060a47b27f2321d1e3493848159d78c6c78411f8c366da66"
	base, baseMode := fingerprintBase()
	got := PolicyFingerprint(base, baseMode)
	if got != want {
		t.Errorf("golden digest drift: got %s want %s", got, want)
	}
}
