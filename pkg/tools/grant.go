package tools

import (
	"context"
	"path/filepath"

	"github.com/ciram-co/looprig/pkg/loop"
	"github.com/ciram-co/looprig/pkg/tool"
)

// grant.go implements the WRITE side of the policy store (design §3c): the
// loop.PermissionGate.Grant method and the per-tool Match derivation. A Grant is
// always an ALLOW (EffectAutoApprove). ScopeSession appends an in-memory
// ToolPolicy under the lock; ScopeWorkspace writes an ApprovalRecord to the
// out-of-repo store (NEVER the repo) with full filesystem hardening; ScopeOnce
// (and any unknown scope) is refused with a typed error and persists nothing.
//
// Compile-time assertion: *PermissionChecker now satisfies BOTH halves of the
// runner's gate (Check from check.go + Grant here).
var _ loop.PermissionGate = (*PermissionChecker)(nil)

// Grant persists an approval at the requested scope. It derives the approval's
// Match from toolName+argsJSON using the SAME extraction the Check path uses, so
// a granted call is matched identically on the next Check.
//
//	ScopeSession   → append an in-memory ToolPolicy (visible to subsequent Checks
//	                 this session); nothing is written to disk.
//	ScopeWorkspace → append an ApprovalRecord to
//	                 <home>/.urvi/workspaces/<sha256(resolvedRoot)>/approvals.json
//	                 (load → append → atomic-write the whole file); NEVER the repo.
//	ScopeOnce      → refused (*UnsupportedScopeError): a once-grant persists
//	                 nothing by definition and the runner never passes it. Any
//	                 out-of-range scope is refused the same way (fail-secure).
func (c *PermissionChecker) Grant(ctx context.Context, toolName, argsJSON string, scope tool.ApprovalScope) error {
	match, err := c.deriveMatch(toolName, argsJSON)
	if err != nil {
		return err
	}

	switch scope {
	case tool.ScopeSession:
		c.appendSessionPolicy(loop.ToolPolicy{
			Tool:   toolName,
			Effect: loop.EffectAutoApprove,
			Match:  matchSlice(match),
		})
		return nil
	case tool.ScopeWorkspace:
		return c.grantWorkspace(ctx, toolName, match)
	default:
		// ScopeOnce or any out-of-range value: never persist (fail-secure).
		return &UnsupportedScopeError{Scope: uint8(scope)}
	}
}

// matchSlice maps a derived Match string to a ToolPolicy.Match slice: an empty
// Match (tool-level grant, e.g. WebSearch) becomes a nil slice (= "all calls"),
// otherwise a single-element slice.
func matchSlice(match string) []string {
	if match == "" {
		return nil
	}
	return []string{match}
}

// grantWorkspace writes the workspace-scoped ApprovalRecord to the out-of-repo
// store with the §3c filesystem hardening: it resolves the home dir via the seam,
// rejects a symlinked component anywhere in the policy path, creates the store
// dirs 0700, loads the existing records, appends the new allow, and atomically
// rewrites the whole file 0600. Any failure is a typed *PolicyStoreError and
// leaves nothing half-written in the wrong place (fail-secure).
func (c *PermissionChecker) grantWorkspace(ctx context.Context, toolName, match string) error {
	home, err := c.resolveHomeForWrite()
	if err != nil {
		return err
	}
	hash, err := workspaceHash(c.policy.WorkspaceRoot)
	if err != nil {
		// Unresolvable workspace root → cannot place the file → fail secure.
		return &PolicyStoreError{Path: c.policy.WorkspaceRoot, Reason: "workspace root could not be resolved for grant", Err: err}
	}
	finalPath := workspaceApprovalsPath(home, hash)
	dir := filepath.Dir(finalPath)

	// Reject a symlinked component anywhere from home down to the target file BEFORE
	// creating anything (don't follow a symlinked ~/.urvi or workspaces/<hash>).
	if err := assertNoSymlinkComponent(home, finalPath); err != nil {
		return err
	}

	// Create + harden the store directories: tighten EVERY store component under
	// home to 0700 (not just the leaf), so a pre-existing loose ~/.urvi or
	// ~/.urvi/workspaces cannot survive as a store-poisoning vector.
	if err := mkdirStoreDir(home, dir); err != nil {
		return err
	}

	// Re-check after creating the dirs: a hostile actor could have swapped a created
	// component for a symlink between the check and the mkdir. (Residual TOCTOU is
	// out of scope per §3c, but this cheap re-check closes the obvious window.)
	if err := assertNoSymlinkComponent(home, finalPath); err != nil {
		return err
	}

	// Load existing records (treat a missing/empty/malformed file as empty so a
	// grant always succeeds; a corrupt file is replaced by a clean one + the new
	// record rather than wedging the user).
	existing := loadApprovalRecordsForGrant(ctx, finalPath)

	af := ApprovalsFile{
		Version:   1,
		Approvals: append(existing, ApprovalRecord{Tool: toolName, Match: match, Effect: loop.EffectAutoApprove}),
	}
	return writeApprovalsFileAtomically(dir, finalPath, af)
}

// resolveHomeForWrite resolves the home dir via the seam for a WRITE. Unlike the
// read path (which fails open to "store absent"), a Grant that cannot resolve home
// must fail with a typed error — there is nowhere safe to write.
func (c *PermissionChecker) resolveHomeForWrite() (string, error) {
	c.mu.Lock()
	homeFn := c.homeDir
	c.mu.Unlock()
	if homeFn == nil {
		return "", &PolicyStoreError{Path: "", Reason: "home dir resolver is nil"}
	}
	home, err := homeFn()
	if err != nil {
		return "", &PolicyStoreError{Path: "", Reason: "home dir could not be resolved for grant", Err: err}
	}
	return home, nil
}

// deriveMatch computes the ApprovalRecord/ToolPolicy Match for a grant from the
// live call's args, reusing the SAME per-tool extraction the Check matchers use:
//   - Bash      → the EXACT normalized command (normalizeWhitespace of "command").
//   - Fetch     → "<METHOD> <scheme>://<host>" (idna-normalized host, no port).
//   - file tools→ the workspace-RELATIVE canonical path glob (containedPath + Rel).
//   - WebSearch → "" (tool-level grant; Match is not a boundary).
//
// A derivation failure (unparseable args, uncontained path, non-normalizable host)
// is a typed error so Grant fails secure rather than persist an empty/wrong Match
// that could over-approve.
func (c *PermissionChecker) deriveMatch(toolName, argsJSON string) (string, error) {
	class := classifyTool(toolName)
	switch class {
	case classBash:
		return c.deriveBashMatch(argsJSON)
	case classNetwork:
		return deriveNetworkMatch(toolName, argsJSON)
	case classRead, classWrite:
		return c.derivePathMatch(toolName, class, argsJSON)
	default:
		// An unknown tool has no stable Match to persist (the runner only offers
		// ScopeOnce for it). Refuse rather than persist an all-calls grant.
		return "", &GrantDerivationError{Tool: toolName, Reason: "unknown tool has no persistable match"}
	}
}

// deriveBashMatch records the exact normalized command. A missing/empty command
// is a derivation error (nothing safe to grant).
func (c *PermissionChecker) deriveBashMatch(argsJSON string) (string, error) {
	cmd, ok, err := extractStringField(argsJSON, fieldCommand)
	if err != nil {
		return "", &GrantDerivationError{Tool: toolBash, Reason: "command could not be extracted", Err: err}
	}
	norm := normalizeWhitespace(cmd)
	if !ok || norm == "" {
		return "", &GrantDerivationError{Tool: toolBash, Reason: "command is empty"}
	}
	return norm, nil
}

// deriveNetworkMatch records the Fetch grammar "METHOD scheme://host" (host
// idna-normalized, lower-cased, port excluded) or, for WebSearch, the empty
// tool-level Match.
func deriveNetworkMatch(toolName, argsJSON string) (string, error) {
	if toolName == toolWebSearch {
		return "", nil // tool-level grant; Match is ignored on the read side.
	}
	// Fetch.
	rawURL, ok, err := extractStringField(argsJSON, fieldURL)
	if err != nil || !ok || rawURL == "" {
		return "", &GrantDerivationError{Tool: toolFetch, Reason: "url could not be extracted", Err: err}
	}
	method, _, mErr := extractStringField(argsJSON, fieldMethod)
	if mErr != nil {
		return "", &GrantDerivationError{Tool: toolFetch, Reason: "method could not be extracted", Err: mErr}
	}
	match, ok := fetchMatchString(method, rawURL)
	if !ok {
		return "", &GrantDerivationError{Tool: toolFetch, Reason: "url/method could not be normalized to a fetch match"}
	}
	return match, nil
}

// derivePathMatch records the workspace-relative canonical path (containedPath +
// Rel, the same form the file-glob matcher compares against). A missing required
// path, an uncontained/escaping path, or an unresolvable root is a derivation
// error.
func (c *PermissionChecker) derivePathMatch(toolName string, class toolClass, argsJSON string) (string, error) {
	field := readOrWriteBoundaryField(toolName, class)
	rel, ok := c.workspaceRelPath(field, argsJSON)
	if !ok {
		return "", &GrantDerivationError{Tool: toolName, Reason: "path could not be contained/relativised for grant"}
	}
	return rel, nil
}

// GrantDerivationError is the typed failure for deriving an approval's Match from
// a grant's tool name + args (unparseable args, an uncontained path, a
// non-normalizable Fetch host, or an unknown tool with no persistable match). It
// is fail-secure: Grant returns it WITHOUT persisting anything, so a call whose
// boundary can't be canonicalized is never granted an over-broad approval.
type GrantDerivationError struct {
	Tool   string // the tool whose match could not be derived
	Reason string // non-secret reason (never the raw args)
	Err    error  // underlying cause, may be nil
}

func (e *GrantDerivationError) Error() string {
	if e.Err != nil {
		return "tools: grant match derivation failed for " + e.Tool + ": " + e.Reason + ": " + e.Err.Error()
	}
	return "tools: grant match derivation failed for " + e.Tool + ": " + e.Reason
}

func (e *GrantDerivationError) Unwrap() error { return e.Err }
