package sessionruntime

import (
	"strings"

	"github.com/looprig/harness/pkg/event"
)

// workspace_fingerprint.go owns the one encoding of a managed-workspace placement into the
// config fingerprint's WorkspaceRoot field — "<mode>:<canonical region>" — and the rule
// restore uses to compare two of them. Both halves live here so the format and its
// comparison cannot drift apart across packages.
//
// What the field is FOR is detecting a restore under a DIFFERENT workspace: a different
// placement contract (mode) or a different fixed tree (an exclusive or shared root names
// WHICH repository — and whose .skills/ — the session ran against). A per-session base is
// neither. It is the host-local directory the session's tree is materialized under
// (baseDir/<sessionID>); the tree's content comes back from the durable snapshot and the
// model-visible path is WorkspaceBinding.LogicalRoot, derived from the session id alone.
// So two per-session fingerprints are compatible whatever their bases: a pooled Host with a
// pod-specific base, a rollout that changed the mount, or a symlink that resolves
// differently on another node restores the session instead of refusing it.
//
// The encoding is deliberately UNCHANGED from v0.37.0 — the base is still written — so a
// journal written by either version compares the same way under both, and a mixed fleet
// or a rollback to v0.37.0 sees exactly the format it always wrote. Only the comparison is
// relaxed, and only for the per-session mode.

const (
	placementNameNone      = "none"
	placementNameExclusive = "exclusive"
	placementNameSession   = "session"
	placementNameShared    = "shared"

	// placementFingerprintSeparator joins the mode name and the canonical region. A
	// canonical region is absolute, so a fingerprint can never be confused with a plain
	// absolute workspace root a caller supplied through ConfigFingerprintFields.
	placementFingerprintSeparator = ":"
)

// PlacementFingerprint is the WorkspaceRoot fingerprint value for a resolved placement and
// its canonical region (the fixed root, or the per-session base).
func PlacementFingerprint(mode WorkspacePlacementMode, region string) string {
	return placementModeName(mode) + placementFingerprintSeparator + region
}

func placementModeName(mode WorkspacePlacementMode) string {
	switch mode {
	case PlacementExclusive:
		return placementNameExclusive
	case PlacementSession:
		return placementNameSession
	case PlacementShared:
		return placementNameShared
	default:
		return placementNameNone
	}
}

// isSessionPlacementFingerprint reports whether a WorkspaceRoot fingerprint names a
// per-session placement, whose region is a host-local base.
func isSessionPlacementFingerprint(value string) bool {
	return strings.HasPrefix(value, placementNameSession+placementFingerprintSeparator)
}

// relocatedWorkspaceRoot returns the WorkspaceRoot restore should compare the persisted
// value as: the live value when both name a per-session placement (a relocated base is not
// a workspace change), and the persisted value unchanged otherwise — a mode change, a
// fixed-root change, or a caller-supplied root still compares verbatim.
func relocatedWorkspaceRoot(persisted, live string) string {
	if isSessionPlacementFingerprint(persisted) && isSessionPlacementFingerprint(live) {
		return live
	}
	return persisted
}

// relocatedFingerprint is relocatedWorkspaceRoot applied to a legacy fingerprint.
func relocatedFingerprint(persisted, live event.ConfigFingerprint) event.ConfigFingerprint {
	persisted.WorkspaceRoot = relocatedWorkspaceRoot(persisted.WorkspaceRoot, live.WorkspaceRoot)
	return persisted
}

// relocatedManifest is relocatedWorkspaceRoot applied to a drift baseline. It copies the
// manifest by value; the only field it replaces is a string.
func relocatedManifest(baseline, candidate event.ConfigManifest) event.ConfigManifest {
	baseline.WorkspaceRoot = relocatedWorkspaceRoot(baseline.WorkspaceRoot, candidate.WorkspaceRoot)
	return baseline
}
