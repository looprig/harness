package rig

import (
	"path/filepath"

	"github.com/looprig/harness/internal/sessionruntime"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/tool"
)

// capture.go is the PUBLIC wiring for durable tool-result retention and the
// capture-safety descriptor projected from a rig's tool definitions. Every type
// named here comes from pkg/loop or pkg/tool, never from an internal package, so
// a composition root outside github.com/looprig/harness can write the call —
// which is the whole reason the store types are not declared in the loop runtime.

// captureFieldObjects and captureFieldSpillBase are the only two values
// DefinitionError.Name takes for an invalid capture wiring. They are field labels,
// so a caller can branch on which half of the option was wrong without the error
// ever carrying a value the caller supplied.
const (
	captureFieldObjects   = "objects"
	captureFieldSpillBase = "spill_base"
)

// WithToolResultObjects wires READABLE durable tool-result retention: the store
// every session publishes each oversized tool result into, and the ABSOLUTE base
// directory each session's local capture spill root is created under.
//
// The store ISSUES each capture's reference, and that reference is what the
// committed StepDone records, so a capture is readable afterwards — by a loop
// through read_tool_result (a tool definition declaring
// tool.RequiresToolResultReader), and by any reader of the same store, such as
// an object route. (*sessionstore.Store).ToolResultObjects() is the reference
// implementation; wire the SAME store the rig journals into, so the objects
// live beside the journal that references them and outlive every workspace.
//
// The spill base follows exactly the rules of WithToolResultCapture. The two
// options are exclusive: supplying both is a DefinitionDuplicateOption.
//
// A composition should also bound ToolLimits.ResultBytes. With it zero the
// model preview is unbounded, so nothing is ever elided and no object is written.
func WithToolResultObjects(objects loop.ToolResultObjects, spillBase string) Option {
	return func(state *definitionState) error {
		if state.seen[keyToolResultCapture] {
			return &DefinitionError{Kind: DefinitionDuplicateOption, Name: string(keyToolResultCapture)}
		}
		if nilToolResultObjects(objects) {
			return &DefinitionError{Kind: DefinitionInvalidToolResultCapture, Name: captureFieldObjects}
		}
		if !filepath.IsAbs(spillBase) {
			return &DefinitionError{Kind: DefinitionInvalidToolResultCapture, Name: captureFieldSpillBase}
		}
		state.seen[keyToolResultCapture] = true
		state.toolResultReadable = objects
		state.toolResultSpillBase = spillBase
		return nil
	}
}

// WithToolResultCapture wires durable tool-result retention: the session object
// store each loop retains an oversized tool result into, and the ABSOLUTE base
// directory each session's local capture spill root is created under.
//
// Both are required together. A store with no spill base would hold every
// capture's retained prefix in host memory up to the capture ceiling, which is
// exactly the residency a pooled host cannot budget; a spill base with no store
// would write local files nothing ever uploads. The base must be absolute
// because a relative one resolves against the process working directory, which
// is neither session-scoped nor stable for a pooled host, and because it must be
// comparable against the workspace region at Define time.
//
// The base must ALREADY EXIST, be owner-writable only, and be a directory rather
// than a symlink, by the time a session is created — harness refuses to create it,
// because creating it would mean creating through whatever intermediate components
// the path has, and os.MkdirAll follows a symlinked one silently. Define checks
// only that the path is absolute, since the base may legitimately be created after
// the rig is defined; the session's own establishment is the authoritative check
// and a failure there ends the turn at the spill stage.
//
// The base is a BASE, not a root: each session creates <base>/<sessionID>,
// owner-only, and removes it at shutdown. Define refuses a base that overlaps the
// configured workspace region in either direction, which is what keeps a capture
// spill out of every workspace checkpoint — a checkpoint archives the whole
// region, so the exclusion has to be a placement invariant rather than a filter
// some future snapshot path might not consult.
//
// Deprecated: the loop mints each capture's identity itself, and no session
// object store can resolve an identity it did not issue, so a capture retained
// this way can never be read back. Use WithToolResultObjects. This option is
// removed at the next major version.
func WithToolResultCapture(objects loop.ToolResultObjectStore, spillBase string) Option {
	return func(state *definitionState) error {
		if state.seen[keyToolResultCapture] {
			return &DefinitionError{Kind: DefinitionDuplicateOption, Name: string(keyToolResultCapture)}
		}
		if objects == nil {
			return &DefinitionError{Kind: DefinitionInvalidToolResultCapture, Name: captureFieldObjects}
		}
		// One check, not two. filepath.IsAbs is false for the empty string and for
		// a whitespace-only or leading-whitespace path, so "empty", "blank" and
		// "relative" are the same rejection to a caller; splitting them would leave
		// a branch nothing reads and a Name nothing distinguishes. Name is a FIELD
		// LABEL in both branches and never the caller's path: an error value that
		// sometimes carries a filesystem path is one a log or an API response has to
		// treat as sensitive.
		if !filepath.IsAbs(spillBase) {
			return &DefinitionError{Kind: DefinitionInvalidToolResultCapture, Name: captureFieldSpillBase}
		}
		state.seen[keyToolResultCapture] = true
		state.toolResultObjects = objects
		state.toolResultSpillBase = spillBase
		return nil
	}
}

// resolveToolResultCapture canonicalizes the spill base and enforces the
// workspace-overlap invariant. The check runs in BOTH directions — a base
// beneath the region, and a region beneath the base — because either arrangement
// puts one inside the other's tree, and it is the containment that matters, not
// which was declared first. It reuses the same canonicalization the placement
// options use, so a lexical or symlink alias of the region cannot slip past.
func resolveToolResultCapture(state *definitionState, placementConfigured bool, region string) (string, error) {
	if state.toolResultObjects == nil && state.toolResultReadable == nil {
		return "", nil
	}
	base, err := canonicalPath(state.toolResultSpillBase)
	if err != nil {
		return "", err
	}
	if placementConfigured && (pathOverlapsRoot(region, base) || pathOverlapsRoot(base, region)) {
		return "", &DefinitionError{Kind: DefinitionToolResultSpillOverlapsWorkspace, Name: base}
	}
	return base, nil
}

// CaptureSafety reports whether this rig's tools are safe to place where a tool
// result's residency matters: every high-output tool either streams its raw
// result into the capture sink, or the runtime's finite materialized maximum
// bounds the fallback.
//
// It is a plain value naming only pkg/tool types, so a placement decision can be
// taken from it without importing anything that runs a loop — and Harness never
// imports the consumer that reads it. It is projected at Define time from the
// tool DEFINITIONS, not from bound tools, because a placement decision has to be
// takeable before a session exists.
func (r *Rig) CaptureSafety() tool.CaptureSafetyDescriptor { return r.captureSafety }

// projectCaptureSafety collects every loop's tool definitions and projects the
// descriptor against the runtime's declared materialized maximum.
func projectCaptureSafety(loops []loop.Definition) tool.CaptureSafetyDescriptor {
	var defs []tool.Definition
	for _, definition := range loops {
		defs = append(defs, definition.ToolDefinitions()...)
	}
	return tool.ProjectCaptureSafety(defs, loop.DefaultMaterializedToolResultBytes)
}

// toolResultCaptureLifecycleOption forwards the wiring to every session the rig
// creates or restores, or returns nil when no store was wired.
func toolResultCaptureLifecycleOption(state *definitionState, spillBase string) sessionruntime.LifecycleOption {
	if state.toolResultReadable != nil {
		return sessionruntime.WithLifecycleToolResultObjects(state.toolResultReadable, spillBase)
	}
	if state.toolResultObjects == nil {
		return nil
	}
	return sessionruntime.WithLifecycleToolResultCapture(state.toolResultObjects, spillBase)
}

// nilToolResultObjects reports a nil store, including a typed nil pointer
// behind the interface, which would otherwise pass a == nil check and fail at
// the first oversized tool result instead of at Define.
func nilToolResultObjects(objects loop.ToolResultObjects) bool {
	return nilInterfaceValue(objects)
}
