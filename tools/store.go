package tools

import (
	"crypto/sha256"
	"encoding/hex"
	"path/filepath"
)

// store.go resolves the OUT-OF-REPO policy-store file paths (design §3c). The
// store deliberately lives under the user's home, NEVER in the repo, so a cloned
// or hostile repo cannot ship an approvals.json that silently auto-approves a
// tool call. The workspace-scoped file is keyed by a sha256 of the RESOLVED
// workspace root so two checkouts of different roots never share approvals.
//
// Only the reading path (workspaceHash + the two file-path resolvers) lives here
// for now; Grant's atomic-write + filesystem hardening (Task 3.6) extends this
// file.

const (
	// urviDirName is the per-user urvi store directory under the home dir.
	urviDirName = ".urvi"
	// workspacesDirName holds one subdirectory per workspace (named by hash).
	workspacesDirName = "workspaces"
	// userApprovalsName is the user-global approvals file (~/.urvi/approvals.json).
	userApprovalsName = "approvals.json"
	// workspaceApprovalsName is the per-workspace approvals file
	// (~/.urvi/workspaces/<hash>/approvals.json).
	workspaceApprovalsName = "approvals.json"
)

// workspaceHash returns the lowercase hex sha256 of the EvalSymlinks-resolved
// workspace root. Resolving symlinks first makes the hash stable across symlink
// aliases of the same directory and matches the containment root resolution, so
// the workspace file is found regardless of which alias the workspace root was
// supplied as. A root that cannot be resolved yields the error so the caller can
// fail secure (treat the workspace store as absent).
func workspaceHash(workspaceRoot string) (string, error) {
	resolved, err := filepath.EvalSymlinks(workspaceRoot)
	if err != nil {
		return "", &PolicyPathError{Root: workspaceRoot, Reason: "workspace root could not be resolved", Err: err}
	}
	resolved, err = filepath.Abs(resolved)
	if err != nil {
		return "", &PolicyPathError{Root: workspaceRoot, Reason: "workspace root could not be made absolute", Err: err}
	}
	sum := sha256.Sum256([]byte(resolved))
	return hex.EncodeToString(sum[:]), nil
}

// userApprovalsPath returns the path to the user-global approvals file given a
// resolved home directory: <home>/.urvi/approvals.json.
func userApprovalsPath(home string) string {
	return filepath.Join(home, urviDirName, userApprovalsName)
}

// workspaceApprovalsPath returns the path to the workspace-scoped approvals file:
// <home>/.urvi/workspaces/<hash>/approvals.json.
func workspaceApprovalsPath(home, hash string) string {
	return filepath.Join(home, urviDirName, workspacesDirName, hash, workspaceApprovalsName)
}

// PolicyPathError is the typed failure for resolving a policy-store path (e.g. an
// unresolvable workspace root). It is fail-secure: the caller treats a non-nil
// PolicyPathError as "this store is absent", contributing no approvals.
type PolicyPathError struct {
	Root   string // the workspace root being hashed (when applicable)
	Reason string // non-secret, human-readable reason
	Err    error  // underlying cause, may be nil
}

func (e *PolicyPathError) Error() string {
	if e.Err != nil {
		return "tools: policy path error: " + e.Reason + " (root=" + e.Root + "): " + e.Err.Error()
	}
	return "tools: policy path error: " + e.Reason + " (root=" + e.Root + ")"
}

func (e *PolicyPathError) Unwrap() error { return e.Err }
