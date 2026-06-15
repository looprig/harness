package loop

import (
	"context"

	"github.com/inventivepotter/urvi/internal/tool"
)

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

// ToolPolicy is a single user-editable approval rule. Match is tool-interpreted
// (path glob for file tools, the EXACT command for Bash, or "METHOD scheme://host"
// for Fetch); an empty Match means "all calls to this tool".
type ToolPolicy struct {
	Tool   string
	Effect Effect
	Match  []string
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
type ReadGuard interface {
	DeniedRead(absPath string) bool
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
