package gate

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/looprig/harness/pkg/tool"
)

// AccessRootInvalid classifies a read-only or workspace root that cannot be confined to:
// empty, missing, not a directory, unresolvable, or the filesystem root.
const AccessRootInvalid AccessErrorKind = "root_invalid"

// The normalized capability kinds the standard tools prepare. Read-only access
// binds all four, so a write, command or network requirement is a structural
// deny ("out of scope") rather than a missing-source failure.
const (
	kindFilesystemRead  = "filesystem.read"
	kindFilesystemWrite = "filesystem.write"
	kindNetwork         = "network"
)

// ReadOnlyAccess is an AccessSource that allows filesystem reads within a fixed
// set of directory roots and denies everything else: reads outside every root,
// whole-host reads, every filesystem write, command execution and network
// access. It never answers Gated, so it needs no approver, rule store or grant
// issuer. Unknown kinds and malformed scopes fail closed with an error.
//
// Roots are resolved once, at construction, to absolute symlink-free paths.
// The gate then judges only the Scope string it is given, by LEXICAL
// containment: it touches no filesystem at decision time. It is sound only for
// canonical scopes supplied by trusted tools. A scope of "/repo/link" is
// allowed even if /repo/link is a symlink leading out of /repo, so a tool must
// resolve symlinks before preparing its request (the standard tools do) and
// must confine its own execution to what was approved, for example by reading
// through an os.Root bound to its root, so that a path swapped for a symlink
// after approval is not followed. Comparison is byte-exact and
// case-sensitive, so on a case- or Unicode-normalization-insensitive
// filesystem an alias spelling of an in-root path may be over-denied, never
// widened.
//
// Read-only access decides tool calls; it is not an OS sandbox and does not
// confine processes.
type ReadOnlyAccess struct {
	roots []string
}

// NewReadOnlyAccess validates and canonicalizes roots. Each root must name an
// existing directory; a relative root is resolved against the current working
// directory now, not at call time. The filesystem root is refused, because
// allowing it would grant every host read. At least one root is required.
func NewReadOnlyAccess(roots ...string) (*ReadOnlyAccess, error) {
	canonical, err := canonicalRoots("read-only", roots)
	if err != nil {
		return nil, err
	}
	return &ReadOnlyAccess{roots: canonical}, nil
}

// canonicalRoots resolves every root with canonicalReadRoot and returns them
// sorted and deduplicated. An empty list or any invalid root fails with an
// AccessRootInvalid error; label names the access flavour in the message.
func canonicalRoots(label string, roots []string) ([]string, error) {
	if len(roots) == 0 {
		return nil, &AccessError{Kind: AccessRootInvalid, Cause: fmt.Errorf("at least one %s root is required", label)}
	}
	canonical := make([]string, 0, len(roots))
	for _, root := range roots {
		resolved, err := canonicalReadRoot(root)
		if err != nil {
			return nil, &AccessError{Kind: AccessRootInvalid, Cause: fmt.Errorf("root %q: %w", root, err)}
		}
		canonical = append(canonical, resolved)
	}
	slices.Sort(canonical)
	return slices.Compact(canonical), nil
}

func canonicalReadRoot(root string) (string, error) {
	if root == "" || strings.TrimSpace(root) != root {
		return "", errors.New("must be a non-empty path without surrounding whitespace")
	}
	absolute, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", errors.New("not a directory")
	}
	if filepath.Dir(resolved) == resolved {
		return "", errors.New("the filesystem root would allow every host read")
	}
	return resolved, nil
}

// Roots returns a copy of the canonical roots, sorted and deduplicated.
func (a *ReadOnlyAccess) Roots() []string {
	if a == nil {
		return nil
	}
	return slices.Clone(a.roots)
}

// Bindings routes every standard capability kind (filesystem.read,
// filesystem.write, command.execute, network) to a. Requirements of any other
// kind stay unrouted, which the evaluator fails closed.
func (a *ReadOnlyAccess) Bindings() []AccessBinding {
	return []AccessBinding{
		{Kind: kindFilesystemRead, Source: a},
		{Kind: kindFilesystemWrite, Source: a},
		{Kind: tool.CapabilityCommandExecute, Source: a},
		{Kind: kindNetwork, Source: a},
	}
}

// AccessVersion reports the access ABI. An unconstructed ReadOnlyAccess reports
// zero, which NewAccessBindings rejects.
func (a *ReadOnlyAccess) AccessVersion() uint16 {
	if a == nil || len(a.roots) == 0 {
		return 0
	}
	return CurrentAccessVersion
}

// AccessFor answers AccessAllow only for a filesystem.read whose scope lies at
// or beneath a root, and AccessDeny for every other known kind.
func (a *ReadOnlyAccess) AccessFor(kind, scope string) (uint8, error) {
	if a.AccessVersion() != CurrentAccessVersion {
		return AccessDeny, errors.New("gate: read-only access is not constructed")
	}
	switch kind {
	case kindFilesystemRead:
		within, err := scopeWithinRoots(a.roots, scope)
		if err != nil {
			return AccessDeny, err
		}
		if within {
			return AccessAllow, nil
		}
		return AccessDeny, nil
	case kindFilesystemWrite, tool.CapabilityCommandExecute, kindNetwork:
		return AccessDeny, nil
	default:
		return AccessDeny, fmt.Errorf("gate: read-only access does not know kind %q", kind)
	}
}

// scopeWithinRoots reports whether a filesystem scope lies at or beneath one
// of roots by lexical containment. The whole-host scope "host:*" is never
// within. A scope must be an absolute, clean path, optionally prefixed with
// "tree:"; anything else is malformed and fails closed with an error. A path
// that merely shares a string prefix with a root (root+"-evil") is outside.
func scopeWithinRoots(roots []string, scope string) (bool, error) {
	if scope == "host:*" {
		return false, nil
	}
	path := strings.TrimPrefix(scope, "tree:")
	if path == "" || !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return false, fmt.Errorf("gate: malformed filesystem scope %q", scope)
	}
	for _, root := range roots {
		if path == root || strings.HasPrefix(path, root+string(filepath.Separator)) {
			return true, nil
		}
	}
	return false, nil
}

// NewReadOnlyEvaluator returns a headless Evaluator that approves a tool call
// only when every requirement is a filesystem read within roots. It is the
// evaluator loop.WithReadOnlyAccess installs; use it directly where an
// AccessGate value is needed (for example a rig-level access override), and
// give the loop a policy revision that changes whenever roots change.
func NewReadOnlyEvaluator(roots ...string) (*Evaluator, error) {
	access, err := NewReadOnlyAccess(roots...)
	if err != nil {
		return nil, err
	}
	return NewHeadlessEvaluator(access.Bindings(), nil, nil)
}
