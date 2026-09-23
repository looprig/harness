package loop

import (
	"reflect"

	"github.com/looprig/harness/pkg/tool"
	model "github.com/looprig/inference/model"
)

const (
	defaultMaxToolIterations    = 25
	defaultMaxToolCallsPerTurn  = 100
	defaultMaxParallelToolCalls = 8
	minToolResultBytes          = 256
)

// DefaultToolResultCaptureBytes is the per-result durable retention ceiling a
// loop applies when ToolLimits.CaptureBytes is left zero: 8 MiB.
//
// It is deliberately NOT derived from ToolLimits.ResultBytes, whose zero means
// "do not bound the model-visible text at all". A retention ceiling that
// defaulted to unbounded would make an oversized tool result an unbounded
// durable object, and a retention ceiling that defaulted to the model budget
// would retain no more than the preview it exists to complete.
//
// 8 MiB is chosen against what a retained object is FOR: it holds a complete
// build log, test output or file read that the model only sees a preview of, and
// a later paging read is served from it by byte range. A session that retains a
// hundred such results stays under a gigabyte, which is an object store's
// ordinary working set rather than a capacity decision an operator must make per
// agent. Above this a result is better served by the streaming capture path,
// which reaches the same ceiling without ever holding the result in memory.
const DefaultToolResultCaptureBytes = 8 << 20

// minToolResultCaptureBytes is the smallest capture ceiling a definition may
// declare. It coincides with minToolResultBytes because a capture ceiling below
// the smallest legal model preview would retain strictly less than the message
// it exists to complete, which inverts the purpose of retaining anything.
const minToolResultCaptureBytes = minToolResultBytes

// ModeName identifies a predeclared loop mode. The empty name identifies the base mode.
type ModeName string

// ToolLimits bounds tool activity during one turn.
type ToolLimits struct {
	Iterations  int
	Calls       int
	Parallel    int
	ResultBytes int

	// CaptureBytes bounds how many bytes of one tool result the loop retains
	// durably in the SessionObjectStore. It is a SEPARATE knob from ResultBytes:
	// ResultBytes bounds the text the model sees and its zero means unbounded,
	// while CaptureBytes bounds durable retention and its zero means
	// DefaultToolResultCaptureBytes. A declared value below
	// minToolResultCaptureBytes is rejected.
	CaptureBytes int
}

// Mode declares a validated alternative to a definition's base inference settings.
// Define defensively copies Tools and the model's sampling values.
type Mode struct {
	Name         ModeName
	Model        model.Model
	Effort       model.Effort
	Tools        []tool.Definition
	ToolLimits   ToolLimits
	Instructions string
}

// BoundMode is one immutable definition mode resolved to runtime tool instances.
// Values returned by BoundDefinition are defensive copies.
type BoundMode struct {
	Name         ModeName
	Model        model.Model
	Effort       model.Effort
	Tools        []tool.InvokableTool
	ToolLimits   ToolLimits
	Instructions string

	// toolResultReader records, at Bind, whether this mode's tools include a
	// ReadToolResultToolName tool built by a definition that declares
	// tool.RequiresToolResultReader. It is unexported so only Bind can set it.
	toolResultReader bool
}

// ToolResultReaderBound reports whether this mode has a read_tool_result tool
// that was actually handed a tool-result reader: its name is
// ReadToolResultToolName AND its definition declares
// tool.RequiresToolResultReader. A same-named tool without the requirement (an
// MCP tool, say) does not count, so the loop's retention marker never tells the
// model to call a tool with no reader behind it.
func (m BoundMode) ToolResultReaderBound() bool { return m.toolResultReader }

func cloneModel(value model.Model) model.Model {
	value.Sampling = value.Sampling.Clone()
	return value
}

func cloneMode(mode Mode) Mode {
	mode.Model = cloneModel(mode.Model)
	mode.Tools = append([]tool.Definition(nil), mode.Tools...)
	return mode
}

func cloneBoundMode(mode BoundMode) BoundMode {
	mode.Model = cloneModel(mode.Model)
	mode.Tools = append([]tool.InvokableTool(nil), mode.Tools...)
	return mode
}

func zeroModel(value model.Model) bool {
	return reflect.DeepEqual(value, model.Model{})
}

func resolveLimits(base, override ToolLimits) ToolLimits {
	result := base
	if override.Iterations > 0 {
		result.Iterations = override.Iterations
	}
	if override.Calls > 0 {
		result.Calls = override.Calls
	}
	if override.Parallel > 0 {
		result.Parallel = override.Parallel
	}
	if override.ResultBytes > 0 {
		result.ResultBytes = override.ResultBytes
	}
	if override.CaptureBytes > 0 {
		result.CaptureBytes = override.CaptureBytes
	}
	return result
}

func defaultLimits(limits ToolLimits) ToolLimits {
	if limits.Iterations == 0 {
		limits.Iterations = defaultMaxToolIterations
	}
	if limits.Calls == 0 {
		limits.Calls = defaultMaxToolCallsPerTurn
	}
	if limits.Parallel == 0 {
		limits.Parallel = defaultMaxParallelToolCalls
	}
	if limits.CaptureBytes == 0 {
		limits.CaptureBytes = DefaultToolResultCaptureBytes
	}
	return limits
}

func invalidLimits(limits ToolLimits) bool {
	return limits.Iterations < 0 || limits.Calls < 0 || limits.Parallel < 0 ||
		limits.ResultBytes < 0 || (limits.ResultBytes > 0 && limits.ResultBytes < minToolResultBytes) ||
		limits.CaptureBytes < 0 || (limits.CaptureBytes > 0 && limits.CaptureBytes < minToolResultCaptureBytes)
}
