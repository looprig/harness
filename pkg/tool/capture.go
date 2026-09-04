package tool

import (
	"context"
	"io"
	"sort"
)

// capture.go holds the streaming result-capture contract and the capture-safety
// descriptor projected from it. Both are additive: InvokableTool is unchanged, a
// tool implementing neither is still a fully valid tool, and a Definition that
// declares nothing is classified by the same rule as one that declares the
// materialized default.

// ResultCaptureSink is the write-only seam a capturing tool streams its complete
// deterministic raw result into while it runs. It is exactly io.Writer and
// nothing more, so a tool can write to it with io.Copy, fmt.Fprintf or a
// bufio.Writer and never has to name a harness type beyond this alias-thin
// interface.
//
// Three properties a tool implementation may rely on, all of them owned by the
// runner that supplies the sink rather than by the tool:
//
//   - Write never reports a short write and never returns an error, so a
//     producer that has more output than the loop will retain can always run to
//     completion. A capture ceiling is applied inside the sink; the producer is
//     not told, because "the tool failed" and "the loop kept a prefix" are
//     different outcomes and only the second one happened.
//   - The sink is owned by ONE tool call and is not safe for concurrent use. A
//     tool that fans out internally must serialize its own writes.
//   - The sink is valid only for the duration of the InvokableRunCaptured call
//     that received it. Retaining it past the return is a use-after-free.
type ResultCaptureSink interface {
	io.Writer
}

// CapturingInvokableTool is the optional streaming-capture capability (probed by
// type assertion from an InvokableTool, exactly like Sequential, Auditable and
// WriteTarget). A tool that implements it writes its COMPLETE deterministic raw
// result stream to the supplied sink and returns the bounded model-facing
// ToolResult; the runner prefers this path and falls back to InvokableRun for a
// tool that does not implement it.
//
// The two outputs are deliberately different values. The sink receives what a
// later reader retrieves in full — a whole build log, a whole test run — while
// the returned ToolResult is what enters the conversation, which the loop bounds
// again anyway. A tool that streams its output and ALSO materializes the same
// bytes into the returned ToolResult has gained nothing: the point of the
// capability is that the complete stream never has to be resident.
//
// It deliberately does not embed InvokableTool. The runner reaches it by
// asserting on a value it already holds as an InvokableTool, so embedding would
// only let a type satisfy CapturingInvokableTool without being registrable, and
// the registry's element type already carries the fallback requirement.
type CapturingInvokableTool interface {
	InvokableRunCaptured(ctx context.Context, argsJSON string, sink ResultCaptureSink) (*ToolResult, error)
}

// CaptureClass is how one tool Definition's results reach the capture sink.
type CaptureClass string

const (
	// CaptureClassStreaming means the definition's tools write their raw result
	// to the sink as they produce it, so the complete result is never resident.
	CaptureClassStreaming CaptureClass = "streaming"
	// CaptureClassMaterialized means the definition's tools return a fully built
	// ToolResult, which is resident in memory before the loop can bound it. It is
	// the classification of a definition that declares nothing.
	CaptureClassMaterialized CaptureClass = "materialized"
)

// DeclaredCaptureSafety is what one tool Definition declares about the tools it
// builds. Its zero value is the honest default for a definition that declares
// nothing: materialized, and not known to produce high output.
type DeclaredCaptureSafety struct {
	// Streaming declares that every tool this definition builds implements
	// CapturingInvokableTool.
	Streaming bool
	// HighOutput declares that this definition's tools can produce results large
	// enough that residency matters — a build, a test run, a whole-file read.
	// Only a high-output definition can make a descriptor unsafe, because a tool
	// whose results are small is bounded by its own nature rather than by a
	// declared maximum.
	HighOutput bool
}

// CaptureSafetyDeclarer is the optional capability (probed by type assertion,
// mirroring EvidenceKindDeclarer) a tool Definition implements to declare its
// capture safety. A Definition that does not implement it is projected as the
// zero DeclaredCaptureSafety.
//
// It sits on the Definition rather than on the built tool because the descriptor
// is a DESIGN-time projection: a composition root must be able to ask whether an
// assembly is safe to place on a pooled Host before it has bound a session, and
// binding is what builds tools.
type CaptureSafetyDeclarer interface {
	DeclaredCaptureSafety() DeclaredCaptureSafety
}

// ToolCaptureSafety is one definition's projected row: the definition name, the
// model-facing tool names it produces, its class, and whether it declared high
// output.
type ToolCaptureSafety struct {
	Definition string
	Tools      []string
	Class      CaptureClass
	HighOutput bool
}

// CaptureSafetyDescriptor is the capture-safety projection of a whole assembly.
// It is a plain value with no harness runtime in it, so a placement decision can
// be taken from it without importing anything that runs a loop — and Harness
// never has to import the consumer that reads it.
type CaptureSafetyDescriptor struct {
	// Definitions holds one row per distinct definition name, sorted by name so
	// two projections of the same assembly compare equal.
	Definitions []ToolCaptureSafety
	// MaterializedMaxBytes is the finite hard maximum the runtime declares for
	// one materialized tool result. A non-positive value means no finite maximum
	// is declared, which is the only way a high-output materialized definition
	// can be unsafe.
	MaterializedMaxBytes int
}

// Safe reports the rule this descriptor exists to answer: every high-output tool
// is either streaming, or a finite materialized maximum bounds the fallback.
func (d CaptureSafetyDescriptor) Safe() bool { return len(d.Unsafe()) == 0 }

// Unsafe names the definitions that break the rule, in Definitions order. It is
// what a caller reports; Safe is the same fact as a boolean.
func (d CaptureSafetyDescriptor) Unsafe() []string {
	if d.MaterializedMaxBytes > 0 {
		return nil
	}
	var unsafe []string
	for _, row := range d.Definitions {
		if row.HighOutput && row.Class != CaptureClassStreaming {
			unsafe = append(unsafe, row.Definition)
		}
	}
	return unsafe
}

// ProjectCaptureSafety projects the descriptor for a set of tool definitions
// against the runtime's declared materialized maximum.
//
// Two definitions may share a name — the same definition value reached through
// two loops, or two distinct values built with the same name — and they are
// merged into one row CONSERVATIVELY: the row is streaming only if every
// contributor streams, and high-output if any contributor says so. A merge that
// took the first or the last contributor could report an assembly safe on the
// strength of a definition that is not the one a loop will build.
//
// A nil definition is skipped rather than projected as an unnamed materialized
// row, matching how loop.Definition.ToolRequirements treats one.
func ProjectCaptureSafety(defs []Definition, materializedMaxBytes int) CaptureSafetyDescriptor {
	rows := make(map[string]ToolCaptureSafety, len(defs))
	for _, def := range defs {
		if def == nil {
			continue
		}
		declared := DeclaredCaptureSafety{}
		if declarer, ok := def.(CaptureSafetyDeclarer); ok {
			declared = declarer.DeclaredCaptureSafety()
		}
		class := CaptureClassMaterialized
		if declared.Streaming {
			class = CaptureClassStreaming
		}
		row := ToolCaptureSafety{
			Definition: def.Name(),
			Tools:      append([]string(nil), def.ProducedToolNames()...),
			Class:      class,
			HighOutput: declared.HighOutput,
		}
		previous, seen := rows[row.Definition]
		if seen {
			if previous.Class == CaptureClassMaterialized {
				row.Class = CaptureClassMaterialized
			}
			row.HighOutput = row.HighOutput || previous.HighOutput
			row.Tools = mergeToolNames(previous.Tools, row.Tools)
		}
		rows[row.Definition] = row
	}
	descriptor := CaptureSafetyDescriptor{
		Definitions:          make([]ToolCaptureSafety, 0, len(rows)),
		MaterializedMaxBytes: materializedMaxBytes,
	}
	for _, row := range rows {
		descriptor.Definitions = append(descriptor.Definitions, row)
	}
	sort.Slice(descriptor.Definitions, func(i, j int) bool {
		return descriptor.Definitions[i].Definition < descriptor.Definitions[j].Definition
	})
	return descriptor
}

// mergeToolNames unions two produced-name lists, preserving first-seen order so
// the merged row lists every tool either contributor produces exactly once.
func mergeToolNames(a, b []string) []string {
	seen := make(map[string]bool, len(a)+len(b))
	merged := make([]string, 0, len(a)+len(b))
	for _, list := range [][]string{a, b} {
		for _, name := range list {
			if seen[name] {
				continue
			}
			seen[name] = true
			merged = append(merged, name)
		}
	}
	return merged
}
