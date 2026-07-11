package loop

import (
	"context"
	"encoding/json"
	"strconv"

	"github.com/looprig/harness/pkg/tool"
)

// PermissionFactory creates the permission gate private to one bound loop. It may
// be called concurrently for separate Bind calls and must synchronize captured state.
type PermissionFactory func(context.Context, tool.Bindings) (PermissionGate, error)

// DelegationStyle selects the model-facing delegation action set.
type DelegationStyle uint8

const (
	DelegationSyncOnly DelegationStyle = iota
	DelegationManaged
)

// Delegation is the immutable delegation policy copied into a Definition.
type Delegation struct{ Style DelegationStyle }

// deps.go is the runner's CONSUMER surface for the tool subsystem (design §3b).
// The loop depends only on these interfaces and value types; it never imports
// the concrete `tools/` package. The composition root wires concrete
// implementations (e.g. *tools.PermissionChecker) into a ToolSet on loop.Config.

// Effect is the permission outcome the PermissionGate yields for a tool call.
//
// ZERO-VALUE SEMANTICS (fail-secure, per CLAUDE.md "Fail secure"):
// EffectAsk is deliberately the zero value (iota == 0). A zero-value Effect —
// produced by an uninitialized field, a struct literal that omits Effect, or a
// map miss — therefore means "ask the user", never "auto-approve". Making
// EffectAutoApprove the zero value would be a fail-OPEN bug: an accidental zero
// would silently grant a tool call. No code may rely on an implicit zero meaning
// auto-approve. (The design comment in §3b lists the names in the order
// "AutoApprove | Ask | Deny" but does not pin integer values; we order the
// consts so the zero value is safe.)
type Effect uint8

const (
	// EffectAsk (0) is the safe default: prompt the user. It is intentionally the
	// zero value so an uninitialized Effect never auto-approves.
	EffectAsk Effect = iota
	// EffectAutoApprove runs the tool call without prompting.
	EffectAutoApprove
	// EffectDeny blocks the tool call.
	EffectDeny
)

// The user-facing wire strings for Effect in approvals.json. They read naturally
// (not opaque 0/1/2) so a human editing the file understands the policy.
const (
	effectStringAllow = "allow"
	effectStringAsk   = "ask"
	effectStringDeny  = "deny"
)

// InvalidEffectError is the typed failure for an Effect that cannot be mapped to
// or from a wire string. It is fail-secure: callers (and json.Unmarshal) get an
// error rather than a silently-defaulted effect, so a malformed approval is never
// treated as auto-approve. Either Wire (bad/unknown input string or raw token)
// or Value (out-of-range numeric Effect) is set depending on the direction.
type InvalidEffectError struct {
	Wire  string // the offending JSON token/string, when unmarshalling
	Value Effect // the offending numeric Effect, when marshalling an unknown value
}

func (e *InvalidEffectError) Error() string {
	if e.Wire != "" {
		return "loop: invalid Effect: unknown approval value " + strconv.Quote(e.Wire) +
			" (want \"allow\", \"ask\", or \"deny\")"
	}
	return "loop: invalid Effect: out-of-range value " + strconv.Itoa(int(e.Value))
}

// MarshalJSON encodes an Effect as its user-facing wire string. An out-of-range
// Effect returns an *InvalidEffectError rather than emitting a bogus token.
func (e Effect) MarshalJSON() ([]byte, error) {
	var s string
	switch e {
	case EffectAutoApprove:
		s = effectStringAllow
	case EffectAsk:
		s = effectStringAsk
	case EffectDeny:
		s = effectStringDeny
	default:
		return nil, &InvalidEffectError{Value: e}
	}
	return json.Marshal(s)
}

// UnmarshalJSON decodes a wire string into an Effect. Any non-string token, or a
// string other than the three known values, yields an *InvalidEffectError
// (fail-secure: an unrecognized approval is never decoded as auto-approve). The
// receiver is left unchanged on error.
func (e *Effect) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		// Non-string JSON (number, bool, null, object, or malformed): record the
		// raw token so the user can see what was rejected, never silently accept.
		return &InvalidEffectError{Wire: string(b)}
	}
	switch s {
	case effectStringAllow:
		*e = EffectAutoApprove
	case effectStringAsk:
		*e = EffectAsk
	case effectStringDeny:
		*e = EffectDeny
	default:
		return &InvalidEffectError{Wire: s}
	}
	return nil
}

// ToolPolicy is a single user-editable approval rule. Match is tool-interpreted
// (path glob for file tools, the EXACT command for Bash, or "METHOD scheme://host"
// for Fetch); an empty Match means "all calls to this tool".
//
// GrantDeltas are the MAC-verified escalation delta DESCRIPTIONS (sorted, deduped)
// bound to this approval when it was granted for a grant-carrying call (SPEC §9.3).
// They are DESCRIPTIONS, never the single-mint tokens. A policy WITH GrantDeltas
// matches only a grant-bearing call whose delta set equals it; a policy WITHOUT
// them matches only grant-free calls. Empty for the common (grant-free) grant.
type ToolPolicy struct {
	Tool        string
	Effect      Effect
	Match       []string
	GrantDeltas []string
}

// PermissionDecision is the richer permission result optionally exposed by a
// gate that can explain non-gated approve/deny outcomes. It preserves Check's
// fail-secure Effect contract while letting the runner emit a redacted audit
// event without importing a concrete checker.
type PermissionDecision struct {
	Effect Effect
	Reason string
}

// PermissionGate is the runner's view of permission checking. It is satisfied by
// the concrete checker in `tools/` (wired at the composition root). The runner
// retains the toolName+argsJSON it passed to Check so it can later pass the same
// values to Grant for an open gate.
type PermissionGate interface {
	// Check returns the Effect for a prospective tool call. It must be
	// fail-secure: on any ambiguity or internal error it returns EffectAsk or
	// EffectDeny, never EffectAutoApprove.
	Check(ctx context.Context, t tool.InvokableTool, toolName, argsJSON string) Effect
	// Grant persists an approval at the chosen scope. ScopeSession appends an
	// in-memory ToolPolicy; ScopeWorkspace writes an approval record to the
	// out-of-repo policy store. ScopeOnce is never passed (it persists nothing).
	Grant(ctx context.Context, toolName, argsJSON string, scope tool.ApprovalScope) error
}

// ReadGuard is the narrow read-side policy the read tools enforce themselves
// (Interface Segregation: read tools depend only on this, not the full gate).
// DeniedRead filters denied paths during Glob/Grep traversal and results;
// MaxReadBytes is the per-file cap ReadFile/Grep apply via io.LimitReader.
//
// This is the §10.5 read-adaptation SEAM: it is deliberately stdlib-typed (no
// import of any sandbox package) so a sandbox consumer (swe) can build ONE
// ReadGuard from the sandbox Policy's read rules and bind the native ReadFile/
// Grep/Glob tools IDENTICALLY to a sandboxed `sh -c cat` — a single source of
// truth, with no drift between the in-process guards and OS enforcement. The
// concrete PermissionChecker already satisfies ReadGuard (via DeniedRead/
// MaxReadBytes), so the bare harness and a sandboxed harness share one read-deny
// contract; the swe adapter simply wraps the sandbox resolver's Resolve behind the
// same two methods.
type ReadGuard interface {
	// DeniedRead reports whether reading absPath is denied by policy (e.g. the
	// §5.3 secret deny-reads such as "**/.env*", or a zerotrust restricted-read).
	//
	// CANONICAL-PATH CONTRACT (fail-secure): absPath MUST be an ABSOLUTE,
	// filepath.Clean'ed, SYMLINK-RESOLVED path. The guard is purely LEXICAL — it
	// matches the string it is handed and performs NO filesystem resolution of its
	// own. Resolving symlinks (and, on a case-insensitive volume such as default
	// macOS/APFS, canonicalising case) BEFORE the call is the CALLER's (the tool's)
	// responsibility: a guard fed a non-canonical path can be bypassed by a symlink
	// or a case variant that resolves to the denied file. The native read tools
	// honour this — ReadFile passes the containedPath-resolved abs, Grep/Glob pass
	// the EvalSymlinks'd path via denyFilteredRel. This mirrors the sandbox Resolve
	// contract, so the swe adapter and the native tools MUST both feed canonical
	// paths or a deny is trivially evaded.
	DeniedRead(absPath string) bool
	// MaxReadBytes is the per-file read cap (bytes) ReadFile/Grep apply via
	// io.LimitReader.
	MaxReadBytes() int64
}

// ToolSet is the RUNNER's view of the tool subsystem — the only thing
// loop.Config carries. Tools never see it: they are not handed
// Permission/Registry/Middlewares (they do not call them). nil
// Permission/Registry/Middlewares are valid; the composition root sets them.
type ToolSet struct {
	Permission  PermissionGate
	Registry    []tool.InvokableTool // runner looks up by Info().Name
	Middlewares []tool.ToolMiddleware

	// Runaway guards. loop.New applies the defaults below when a field is zero
	// (or negative — treated as unset), mirroring how it defaults DrainTimeout.
	MaxToolIterations    int // max LLM<->tool round-trips per turn (default 25)
	MaxToolCallsPerTurn  int // max total tool executions per turn (default 100)
	MaxParallelToolCalls int // semaphore width for the parallel batch (default 8)
}

const (
	defaultMaxToolIterations    = 25
	defaultMaxToolCallsPerTurn  = 100
	defaultMaxParallelToolCalls = 8
)

// resolveMaxToolIterations applies the default when the caller leaves the field
// unset (zero or negative), mirroring resolveDrainTimeout.
func resolveMaxToolIterations(n int) int {
	if n <= 0 {
		return defaultMaxToolIterations
	}
	return n
}

// resolveMaxToolCallsPerTurn applies the default when unset (zero or negative).
func resolveMaxToolCallsPerTurn(n int) int {
	if n <= 0 {
		return defaultMaxToolCallsPerTurn
	}
	return n
}

// resolveMaxParallelToolCalls applies the default when unset (zero or negative).
func resolveMaxParallelToolCalls(n int) int {
	if n <= 0 {
		return defaultMaxParallelToolCalls
	}
	return n
}

// resolveToolSetCaps returns ts with each zero (or negative) runaway-guard field
// replaced by its default. Permission/Registry/Middlewares are left untouched.
func resolveToolSetCaps(ts ToolSet) ToolSet {
	ts.MaxToolIterations = resolveMaxToolIterations(ts.MaxToolIterations)
	ts.MaxToolCallsPerTurn = resolveMaxToolCallsPerTurn(ts.MaxToolCallsPerTurn)
	ts.MaxParallelToolCalls = resolveMaxParallelToolCalls(ts.MaxParallelToolCalls)
	return ts
}
