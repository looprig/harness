package tools

import (
	"os"
	"slices"
	"sync"

	"github.com/inventivepotter/urvi/internal/agent/loop"
	"github.com/inventivepotter/urvi/internal/hashcache"
)

// permission.go defines the policy data structures, fail-secure default
// hard-deny rules, and the PermissionChecker type (its construction, the
// home-dir seam, and the ReadGuard surface) for the tools subsystem (design
// §3c). The seven-stage Check + per-tool extraction live in check.go; the
// out-of-repo store path resolution lives in store.go.

// EffectChecker decides the Effect for a tool call from its raw arguments,
// independent of any persisted approval. It is consulted as an early evaluation
// stage (e.g. a tool that is intrinsically read-only can auto-approve itself).
// handled=false means "this checker has no opinion" — evaluation continues to
// the next stage; handled=true pins the returned Effect.
type EffectChecker interface {
	CheckEffect(argsJSON string) (effect loop.Effect, handled bool)
}

// HardApproveRules names tools that are auto-approved unconditionally (after
// containment + hard-deny). These are intrinsically safe, side-effect-free tools
// (e.g. a within-workspace search) that never need a prompt.
type HardApproveRules struct {
	// Tools is the set of tool names that hard-approve. An empty slice approves
	// nothing.
	Tools []string
}

// HardDenyRules are the non-negotiable denials evaluated before any approval
// stage (containment aside). They are fail-secure: a call matching any entry is
// denied regardless of persisted approvals, session policies, or an
// EffectChecker. Path entries are globs matched by MatchFileGlob; Bash entries
// are normalized command prefixes.
type HardDenyRules struct {
	// DeniedReadPaths globs that ReadFile/Glob/Grep may never read (secrets).
	DeniedReadPaths []string
	// DeniedWritePaths globs that WriteFile/EditFile may never write. By
	// construction this is a superset of DeniedReadPaths plus write-only entries
	// (VCS/build integrity files and the .urvi policy store).
	DeniedWritePaths []string
	// DeniedBashPrefixes normalized command prefixes Bash may never run.
	DeniedBashPrefixes []string
	// MaxReadBytes is the per-file read cap (bytes) the ReadGuard enforces.
	MaxReadBytes int64
}

// ApprovalRecord is one persisted approval (the on-disk approvals.json shape).
// Match is tool-interpreted: a path glob (file tools), the EXACT normalized
// command (Bash), or the "METHOD scheme://host[path]" grammar (Fetch). An empty
// Match means "all calls of this tool". Prefix is the Bash-only, hand-edited,
// risky opt-in to prefix (rather than exact) command matching.
type ApprovalRecord struct {
	Tool   string      `json:"tool"`
	Match  string      `json:"match,omitempty"`
	Prefix bool        `json:"prefix,omitempty"`
	Effect loop.Effect `json:"effect"`
}

// ApprovalsFile is the on-disk approvals document. Version pins the schema so a
// future format change is detectable; Approvals is the ordered rule list.
type ApprovalsFile struct {
	Version   int              `json:"version"`
	Approvals []ApprovalRecord `json:"approvals"`

	// SkippedRecords is the count of individually-malformed records that
	// parseApprovalsFile dropped while keeping the valid ones (fail-secure). It is
	// IN-MEMORY ONLY (json:"-") — never serialized — and is used solely to emit a
	// single aggregate operability warning. It never holds record contents.
	SkippedRecords int `json:"-"`
}

// PermissionPolicy is the immutable-at-construction configuration the
// PermissionChecker evaluates against. WorkspaceRoot is the resolved root for
// containment + path relativisation; Policies holds session-scope ToolPolicy
// grants (extended in place by a ScopeSession Grant).
type PermissionPolicy struct {
	WorkspaceRoot string
	HardApprove   HardApproveRules
	HardDeny      HardDenyRules
	Policies      []loop.ToolPolicy
}

// defaultMaxReadBytes is the per-file read cap default: 1 MiB. A larger file is
// truncated by the ReadGuard rather than streamed unbounded into the model.
const defaultMaxReadBytes int64 = 1 << 20

// The default paths a generic file tool may never READ. These are secret-path
// globs (ssh keys, dotenv, PEM, id_rsa, the .urvi store) PLUS the workspace
// .skills/ source tree, which is reachable only through the gated Skill tool and
// must never be slurped by an auto-approved ReadFile/Glob/Grep (P2b §7a/§10).
// The whole set is also (a subset of) the write-deny set: you may never write
// what you may not read.
var defaultDeniedReadPaths = []string{
	"~/.ssh/**",     // private keys + known_hosts + config
	"**/.env",       // dotenv secrets anywhere in the tree
	"**/*.pem",      // PEM-encoded keys/certs anywhere
	"**/id_rsa",     // bare SSH private key anywhere
	"~/.urvi/**",    // the urvi policy/config store (approvals, identity)
	"**/.skills/**", // workspace skill source: reachable ONLY via the gated Skill tool, never slurped/written by generic file tools (gate-bypass prevention)
}

// The write-only additions on top of the read-deny set. These protect VCS and
// build-integrity files and — security-critically — the .urvi policy store, so
// the tool system can NEVER mutate its own approvals via WriteFile/EditFile.
// "**/.urvi/**" covers any in-repo .urvi directory; "~/.urvi/**" the user store.
// Only PermissionChecker.Grant may ever write the policy store.
var defaultDeniedWriteOnlyPaths = []string{
	"**/.git/config", // git remote/hook config (RCE-via-hook surface)
	"**/go.sum",      // module checksum integrity
	"**/.urvi/**",    // in-repo policy store: deny-write (defense in depth)
	"~/.urvi/**",     // user policy store: deny-write (only Grant writes it)
}

// The default dangerous Bash command prefixes that may never run.
var defaultDeniedBashPrefixes = []string{
	"rm -rf /",    // catastrophic recursive delete from root
	"sudo",        // privilege escalation
	"curl | bash", // pipe-to-shell remote execution
	"dd if=",      // raw device/disk overwrite
}

// DefaultHardDeny returns the fail-secure default HardDenyRules from design §3c.
// The write-deny set is the read-deny set PLUS the write-only additions (so it
// is always a superset), guaranteeing the .urvi policy store and every secret
// glob are deny-write. MaxReadBytes defaults to 1 MiB.
//
// Each call returns fresh slices (no shared backing array) so a caller may
// append workspace-specific entries without mutating the package defaults.
func DefaultHardDeny() HardDenyRules {
	read := slices.Clone(defaultDeniedReadPaths)
	// Write set = read set + write-only additions, in a fresh slice.
	write := make([]string, 0, len(read)+len(defaultDeniedWriteOnlyPaths))
	write = append(write, read...)
	write = append(write, defaultDeniedWriteOnlyPaths...)
	return HardDenyRules{
		DeniedReadPaths:    read,
		DeniedWritePaths:   write,
		DeniedBashPrefixes: slices.Clone(defaultDeniedBashPrefixes),
		MaxReadBytes:       defaultMaxReadBytes,
	}
}

// homeDirFunc resolves the user's home directory. It is a seam: the default is
// os.UserHomeDir, overridable in tests (and by Grant, which uses the SAME seam
// to write the out-of-repo store). Returning an error makes the persisted stage
// fail secure (treat both store files as absent — contribute no approvals).
type homeDirFunc func() (string, error)

// PermissionChecker is the seven-stage, fail-secure permission decision engine
// (design §3c). It satisfies BOTH loop.PermissionGate (Check/Grant) and
// loop.ReadGuard (DeniedRead/MaxReadBytes).
//
// Concurrency: a single mutex guards every mutable field. Check takes the lock
// for its whole duration (it reads policy.Policies — mutated by a session-scope
// Grant — and drives the two approval caches, which it must not race). The
// hashcache instances are themselves concurrency-safe, but they are only ever
// touched under mu here, which keeps the whole decision atomic w.r.t. a
// concurrent session grant.
//
// I/O under the lock — an ACCEPTED trade-off: the lock is intentionally held
// across Stage 5's os.ReadFile of the two approvals files. This serializes
// concurrent Check calls across that disk I/O — notably under the runner's
// parallel tool batch (MaxParallelToolCalls, default 8), where up to that many
// Check calls can be in flight at once. It is accepted because the atomicity it
// buys (a Check never observes a half-applied concurrent session grant, and the
// two-file deny-beats-allow reduction is computed against a single consistent
// policy snapshot) outweighs the cost: the approvals files are small and the
// hashcache returns memoized parses for unchanged content, so the held I/O is a
// cheap, cache-fast read in the common case, and this is an interactive agent,
// not a high-QPS service. If lock contention ever does matter, the noted future
// option is a lock-free snapshot refactor — snapshot the policy fields under the
// lock, release it, then do the file I/O + matching outside the lock — exactly
// as DeniedRead already does for its read-filter check. Until then the locking
// stays as-is; the security core (the decision + the single-mutex structure) is
// verified and must not regress.
type PermissionChecker struct {
	mu      sync.Mutex
	policy  PermissionPolicy
	homeDir homeDirFunc

	// Two caches memoize the JSON parse of the workspace and user approvals files
	// keyed by content hash, so an unchanged file is not re-parsed on every Check.
	wsCache   *hashcache.Cache[ApprovalsFile]
	userCache *hashcache.Cache[ApprovalsFile]
}

// NewPermissionChecker builds a PermissionChecker for the given policy. The
// home-dir seam defaults to os.UserHomeDir; tests (and the future Grant store
// hardening) override it via SetHomeDir. Each approvals cache parses bytes into
// an ApprovalsFile via parseApprovalsFile (strict, fail-secure).
func NewPermissionChecker(policy PermissionPolicy) *PermissionChecker {
	return &PermissionChecker{
		policy:    policy,
		homeDir:   os.UserHomeDir,
		wsCache:   hashcache.New(parseApprovalsFile),
		userCache: hashcache.New(parseApprovalsFile),
	}
}

// SetHomeDir overrides the home-dir resolution seam. It is intended for tests
// (and the composition root) to point the policy store at a controlled location;
// production code uses the os.UserHomeDir default from NewPermissionChecker.
func (c *PermissionChecker) SetHomeDir(fn homeDirFunc) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.homeDir = fn
}

// appendSessionPolicy appends an in-memory ToolPolicy under the lock. It is the
// Stage-6 mutation point a ScopeSession Grant uses; exported behavior is via
// Grant (added in a later task). Kept here so the lock that guards Check also
// guards the slice mutation, making concurrent Check + grant -race clean.
func (c *PermissionChecker) appendSessionPolicy(p loop.ToolPolicy) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.policy.Policies = append(c.policy.Policies, p)
}

// DeniedRead reports whether absPath matches any DeniedReadPaths glob via the
// absolute hard-deny matcher (~/ expanded to the resolved home dir). It is the
// ReadGuard hook the read tools call to filter denied paths during traversal and
// before emitting results. It is fail-secure: an unresolvable home dir does not
// disable the non-home globs (e.g. **/.env still matches), and any matcher error
// is a no-match within a single glob but never a panic.
//
// CONTRACT: callers MUST pass a containedPath-resolved ABSOLUTE path (the
// cleaned, symlink-resolved, workspace-contained output — mirroring the input
// MatchFileGlob requires). matchHardDenyAbs strips a single leading "/" to align
// the glob segments, so a relative or unresolved path would silently mis-match;
// the Phase-6 read tools must honour this so their traversal filter is sound.
func (c *PermissionChecker) DeniedRead(absPath string) bool {
	c.mu.Lock()
	denied := c.policy.HardDeny.DeniedReadPaths
	homeFn := c.homeDir
	c.mu.Unlock()

	home := resolveHomeOrEmpty(homeFn)
	for _, pat := range denied {
		if matchHardDenyAbs(pat, absPath, home) {
			return true
		}
	}
	return false
}

// MaxReadBytes returns the per-file read cap from the policy's HardDenyRules. The
// read tools apply it via io.LimitReader.
func (c *PermissionChecker) MaxReadBytes() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.policy.HardDeny.MaxReadBytes
}

// resolveHomeOrEmpty resolves the home dir, returning "" on error. A "" home
// makes the ~/ expansion in matchHardDenyAbs a no-op for home-relative globs
// (those globs then cannot match — fail-secure for the read filter: a non-home
// glob like **/.env is unaffected, a ~/ glob simply does not contribute a match
// rather than matching something wrong).
func resolveHomeOrEmpty(fn homeDirFunc) string {
	if fn == nil {
		return ""
	}
	home, err := fn()
	if err != nil {
		return ""
	}
	return home
}
