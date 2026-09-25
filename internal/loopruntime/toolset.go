package loopruntime

import (
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/tool"
)

// ToolSet is the actor-private resolved tool bundle. Access is the combined
// prepared-access decision gate; a nil Access denies every tool call (fail
// closed) rather than running ungated.
type ToolSet struct {
	Access      loop.AccessGate
	Registry    []tool.InvokableTool
	Middlewares []tool.ToolMiddleware

	MaxToolIterations    int
	MaxToolCallsPerTurn  int
	MaxParallelToolCalls int
	MaxToolResultBytes   int

	// MaxToolResultCaptureBytes is the durable retention ceiling per tool result,
	// resolved from loop.ToolLimits.CaptureBytes. It is independent of
	// MaxToolResultBytes: see loop.DefaultToolResultCaptureBytes for why the two
	// must not share a knob.
	MaxToolResultCaptureBytes int

	// MaxMaterializedToolResultBytes is the hard maximum the runtime DECLARES for
	// a legacy materialized tool — one that hands back a fully built ToolResult
	// instead of streaming into a capture sink. Its bytes are already resident
	// when the loop sees them, so this bound is what a pooled Host budgets per
	// concurrent materialized call; it is not settable from a loop definition,
	// because it describes the runtime's memory contract rather than an agent's
	// policy. It bounds retention as well as accounting, so a producer that
	// exceeds it still records a durable, observable truncation rather than
	// silently losing its tail.
	MaxMaterializedToolResultBytes int

	// ToolResultReaderBound reports that Registry holds a read_tool_result tool
	// built with a tool-result reader (loop.BoundMode.ToolResultReaderBound).
	// Only then may the retention marker instruct the model to call it.
	ToolResultReaderBound bool
}

const (
	defaultMaxToolIterations    = 25
	defaultMaxToolCallsPerTurn  = 100
	defaultMaxParallelToolCalls = 8
)

// defaultMaxMaterializedToolResultBytes is the declared hard maximum for one
// materialized tool result. It is the package-local name for
// loop.DefaultMaterializedToolResultBytes, which is public because the
// capture-safety descriptor a placement decision reads is projected against it;
// the two must not be able to drift, so this is a reference rather than a copy.
//
// It is a MEMORY figure, not a retention one — a materialized result is resident
// in full before the loop can bound it, so this multiplied by
// MaxParallelToolCalls (8 by default) is the 256 MiB a pooled Host must budget
// for one loop's tool batch. It sits above loop.DefaultToolResultCaptureBytes so
// the retention ceiling, not the memory declaration, is what ordinarily bounds a
// capture; both remain reachable because the retention ceiling is configurable
// per definition.
const defaultMaxMaterializedToolResultBytes = loop.DefaultMaterializedToolResultBytes

// Resolve a zero or invalid negative to the default, preserving loop.Unlimited.
func resolveMaxToolIterations(n int) int {
	if n <= 0 && n != loop.Unlimited {
		return defaultMaxToolIterations
	}
	return n
}

func resolveMaxToolCallsPerTurn(n int) int {
	if n <= 0 && n != loop.Unlimited {
		return defaultMaxToolCallsPerTurn
	}
	return n
}

func resolveMaxParallelToolCalls(n int) int {
	if n <= 0 {
		return defaultMaxParallelToolCalls
	}
	return n
}

func resolveMaxToolResultBytes(n int) int {
	if n < 0 {
		return 0
	}
	return n
}

// resolveMaxToolResultCaptureBytes defaults a non-positive retention ceiling.
// Unlike resolveMaxToolResultBytes it never resolves to zero: zero retention
// would make every oversized result unreachable while still committing a shaped
// preview, which is the exact loss the capture pipeline exists to prevent.
func resolveMaxToolResultCaptureBytes(n int) int {
	if n <= 0 {
		return loop.DefaultToolResultCaptureBytes
	}
	return n
}

func resolveMaxMaterializedToolResultBytes(n int) int {
	if n <= 0 {
		return defaultMaxMaterializedToolResultBytes
	}
	return n
}

func resolveToolSetCaps(ts ToolSet) ToolSet {
	ts.MaxToolIterations = resolveMaxToolIterations(ts.MaxToolIterations)
	ts.MaxToolCallsPerTurn = resolveMaxToolCallsPerTurn(ts.MaxToolCallsPerTurn)
	ts.MaxParallelToolCalls = resolveMaxParallelToolCalls(ts.MaxParallelToolCalls)
	ts.MaxToolResultBytes = resolveMaxToolResultBytes(ts.MaxToolResultBytes)
	ts.MaxToolResultCaptureBytes = resolveMaxToolResultCaptureBytes(ts.MaxToolResultCaptureBytes)
	ts.MaxMaterializedToolResultBytes = resolveMaxMaterializedToolResultBytes(ts.MaxMaterializedToolResultBytes)
	return ts
}
