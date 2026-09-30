package gate

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"

	"github.com/looprig/harness/pkg/tool"
)

// WorkspaceAccess is an AccessSource for an agent that reads freely within a
// fixed set of directory roots and writes there only with approval:
//
//   - filesystem.read at or beneath a root is Allow;
//   - filesystem.write at or beneath a root is Gated, so an interactive
//     evaluator asks its Approver (or finds a remembered "always" rule);
//   - every read or write outside the roots, the whole-host scope "host:*",
//     command.execute and network are Deny;
//   - an unknown kind or a malformed scope fails closed with an error.
//
// It shares ReadOnlyAccess's root canonicalization and its LEXICAL contract:
// roots are resolved once, at construction, to absolute symlink-free paths,
// and each decision judges only the Scope string it is given, touching no
// filesystem. It is sound only for canonical scopes supplied by trusted tools,
// which must resolve symlinks before preparing (the standard tools' EditFile
// and WriteFile report the resolved target) and must confine their own I/O to
// what was approved, for example through an os.Root bound to the root, so a
// path swapped for a symlink after approval is not followed. Comparison is
// byte-exact, so on a case- or Unicode-normalization-insensitive filesystem an
// alias spelling of an in-root path may be over-denied, never widened.
//
// Because Deny is decided before any stored rule is consulted, a remembered
// "always" answer can pre-approve a write only inside the roots; it can never
// widen access past them.
//
// Workspace access decides tool calls; it is not an OS sandbox and does not
// confine processes. Command execution and network stay denied because it
// issues no execution grant; to route those kinds elsewhere, compose
// NewInteractiveEvaluator yourself with Bindings() minus the kinds you
// re-route.
type WorkspaceAccess struct {
	roots []string
}

// NewWorkspaceAccess validates and canonicalizes roots exactly as
// NewReadOnlyAccess does: each must name an existing directory other than the
// filesystem root, a relative root is resolved against the current working
// directory now, symlinks are resolved, and duplicates are compacted. At
// least one root is required.
func NewWorkspaceAccess(roots ...string) (*WorkspaceAccess, error) {
	canonical, err := canonicalRoots("workspace", roots)
	if err != nil {
		return nil, err
	}
	return &WorkspaceAccess{roots: canonical}, nil
}

// Roots returns a copy of the canonical roots, sorted and deduplicated.
func (a *WorkspaceAccess) Roots() []string {
	if a == nil {
		return nil
	}
	return slices.Clone(a.roots)
}

// Bindings routes every standard capability kind (filesystem.read,
// filesystem.write, command.execute, network) to a. Requirements of any other
// kind stay unrouted, which the evaluator fails closed.
func (a *WorkspaceAccess) Bindings() []AccessBinding {
	return []AccessBinding{
		{Kind: kindFilesystemRead, Source: a},
		{Kind: kindFilesystemWrite, Source: a},
		{Kind: tool.CapabilityCommandExecute, Source: a},
		{Kind: kindNetwork, Source: a},
	}
}

// AccessVersion reports the access ABI. An unconstructed WorkspaceAccess
// reports zero, which NewAccessBindings rejects.
func (a *WorkspaceAccess) AccessVersion() uint16 {
	if a == nil || len(a.roots) == 0 {
		return 0
	}
	return CurrentAccessVersion
}

// AccessFor answers AccessAllow for a filesystem.read and AccessGated for a
// filesystem.write whose scope lies at or beneath a root, and AccessDeny for
// every other known kind and scope.
func (a *WorkspaceAccess) AccessFor(kind, scope string) (uint8, error) {
	if a.AccessVersion() != CurrentAccessVersion {
		return AccessDeny, errors.New("gate: workspace access is not constructed")
	}
	switch kind {
	case kindFilesystemRead, kindFilesystemWrite:
		within, err := scopeWithinRoots(a.roots, scope)
		if err != nil {
			return AccessDeny, err
		}
		switch {
		case !within:
			return AccessDeny, nil
		case kind == kindFilesystemRead:
			return AccessAllow, nil
		default:
			return AccessGated, nil
		}
	case tool.CapabilityCommandExecute, kindNetwork:
		return AccessDeny, nil
	default:
		return AccessDeny, fmt.Errorf("gate: workspace access does not know kind %q", kind)
	}
}

// RuleStore is what an interactive evaluator needs of "Approve always"
// storage: it matches stored rules and persists newly approved ones.
type RuleStore interface {
	RuleMatcher
	RuleWriter
}

// PolicyRevisioner is implemented by a rule store whose contents are part of
// a loop's policy identity, such as a durable rule file. PolicyRevision
// returns a stable, secret-free digest of that identity; it changes when the
// stored policy changes.
type PolicyRevisioner interface {
	PolicyRevision() string
}

// SessionRules is an in-memory, allow-only RuleStore. WriteRules remembers
// the exact displayed candidates by Kind and Match; MatchesAllow reports
// whether a requirement's Kind and Match equal a remembered candidate (an
// exact string match, never a prefix or pattern); MatchesDeny is always
// false. It never persists: remembered rules last only as long as the value,
// so a new SessionRules starts empty. It is safe for concurrent use and does
// not implement PolicyRevisioner, so an "always" answer never changes a
// loop's policy identity.
type SessionRules struct {
	mu    sync.RWMutex
	allow map[sessionRule]struct{}
}

type sessionRule struct{ kind, match string }

// NewSessionRules returns an empty SessionRules.
func NewSessionRules() *SessionRules {
	return &SessionRules{allow: make(map[sessionRule]struct{})}
}

var errSessionRulesUnconstructed = errors.New("gate: session rules are not constructed")

// WriteRules remembers every candidate, or none: a batch containing a
// candidate with an empty Kind or Match is refused whole.
func (r *SessionRules) WriteRules(_ context.Context, candidates []tool.RuleCandidate) error {
	if r == nil || r.allow == nil {
		return errSessionRulesUnconstructed
	}
	for i, candidate := range candidates {
		if candidate.Kind == "" || candidate.Match == "" {
			return fmt.Errorf("gate: session rule candidate %d needs a kind and a match", i)
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, candidate := range candidates {
		r.allow[sessionRule{kind: candidate.Kind, match: candidate.Match}] = struct{}{}
	}
	return nil
}

// MatchesAllow reports whether requirement's Kind and Match were remembered.
func (r *SessionRules) MatchesAllow(_ context.Context, requirement tool.Requirement) (bool, error) {
	if r == nil || r.allow == nil {
		return false, errSessionRulesUnconstructed
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	_, ok := r.allow[sessionRule{kind: requirement.Kind, match: requirement.Match}]
	return ok, nil
}

// MatchesDeny is always false: SessionRules stores only allow rules.
func (r *SessionRules) MatchesDeny(context.Context, tool.Requirement) (bool, error) {
	if r == nil || r.allow == nil {
		return false, errSessionRulesUnconstructed
	}
	return false, nil
}

// ApproverFunc adapts a function to Approver, for headless compositions, CI
// and tests. A nil ApproverFunc fails closed.
type ApproverFunc func(context.Context, ApprovalPrompt) (ApprovalAction, error)

// RequestApproval calls f.
func (f ApproverFunc) RequestApproval(ctx context.Context, prompt ApprovalPrompt) (ApprovalAction, error) {
	if f == nil {
		return "", errors.New("gate: nil ApproverFunc")
	}
	return f(ctx, prompt)
}

// NewWorkspaceEvaluator returns the interactive Evaluator for access: reads
// within its roots run, writes within its roots are approved by approver or
// by a rule remembered in rules, and everything else is denied. rules serves
// as both the matcher and the "Approve always" writer. No GrantIssuer is
// configured: the file tools enforce the approved path through their own
// root-bound handle, and command execution, which would need a grant, is
// denied. A nil approver or rules fails construction.
func NewWorkspaceEvaluator(access *WorkspaceAccess, approver Approver, rules RuleStore) (*Evaluator, error) {
	if access == nil {
		return nil, &AccessError{Kind: AccessSourceNil, Requirement: kindFilesystemWrite}
	}
	if rules == nil {
		return nil, &EvaluationError{Kind: EvaluationWriterMissing, Cause: errors.New("workspace access requires a rule store")}
	}
	return NewInteractiveEvaluator(access.Bindings(), rules, approver, rules, nil)
}
