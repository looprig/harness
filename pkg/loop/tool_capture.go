package loop

import (
	"context"
	"io"
)

// tool_capture.go is the PUBLIC durable tool-result retention seam. It lives here
// rather than in internal/loopruntime for one structural reason: a composition
// root outside this module has to be able to NAME the store it wires, and an
// internal package's types cannot be named from outside github.com/looprig/harness.
// The loop runtime aliases these declarations, so there is exactly one type in
// each case and no conversion at the boundary.
//
// It sits beside ToolLimits.CaptureBytes on purpose. The ceiling is the agent's
// declarative policy and belongs to a loop definition; the store is the
// composition root's placement decision and is wired as runtime dependency. Both
// nevertheless describe one mechanism, so a reader of either finds the other.

// DefaultMaterializedToolResultBytes is the declared hard maximum for one
// MATERIALIZED tool result: 32 MiB. It is a memory figure, not a retention one —
// a materialized result is resident in full before the loop can bound it, so
// this multiplied by the loop's parallel tool-call limit is what a pooled host
// must budget for one tool batch. It sits above DefaultToolResultCaptureBytes so
// the retention ceiling, not the memory declaration, is what ordinarily bounds a
// capture; both remain reachable because the retention ceiling is configurable
// per definition.
//
// It is exported because it is the finite maximum a capture-safety descriptor is
// projected against (tool.ProjectCaptureSafety): a placement decision that has to
// know whether a materialized fallback is bounded needs the bound, not just the
// fact that one exists.
const DefaultMaterializedToolResultBytes = 32 << 20

// ToolResultObjectStat is what a session object store reports back about an
// object the loop has just written: the stored byte count and the lowercase-hex
// SHA-256 of the stored bytes. Both are compared against what the capture sink
// computed before the referencing StepDone is allowed to commit, so a store that
// silently truncated or rewrote the payload cannot be recorded as a successful
// retention.
type ToolResultObjectStat struct {
	SizeBytes uint64
	Digest    string
}

// ToolResultObjectStore is the narrow session-object-store surface the loop
// runtime needs in order to retain a tool result the committed model message
// cannot carry in full. It is deliberately two methods wide: the loop mints the
// opaque object identity itself and never asks the store for a name, a URL, a
// credential or a backend path, so no such value can reach the public journal
// through this seam.
//
// A nil store means retention is not configured: the loop then commits exactly
// what it committed before this seam existed. Requiring durable retention is the
// composition root's decision, taken by wiring a store.
type ToolResultObjectStore interface {
	// PutToolResultObject stores content under the caller-minted opaque objectID.
	// The object is immutable: writing the same identity twice must either be a
	// no-op or store identical bytes.
	//
	// The content slice is owned by the loop and is valid only for the duration
	// of the call. An implementation that keeps it must copy it.
	PutToolResultObject(ctx context.Context, objectID string, content []byte) error
	// StatToolResultObject reports the size and digest of a stored object.
	StatToolResultObject(ctx context.Context, objectID string) (ToolResultObjectStat, error)
}

// ToolResultObjectStreamStore is the optional streaming-upload capability, probed
// by type assertion exactly as the tool capabilities are. A store that implements
// it receives the capture as a stream, so a capture larger than the loop cares to
// hold is never resident in the host at all; a store that does not gets the
// bounded materialized Put, which reads the local spill back into memory up to
// the capture ceiling.
//
// It embeds ToolResultObjectStore because the streaming upload is an alternative
// to PutToolResultObject, never a replacement for verification: StatToolResultObject
// is still what proves the stored object matches what the sink captured.
type ToolResultObjectStreamStore interface {
	ToolResultObjectStore
	// PutToolResultObjectStream stores exactly size bytes read from content under
	// the caller-minted opaque objectID. content is valid only for the duration
	// of the call. A store that reads fewer than size bytes must report an error
	// rather than storing a short object; the loop verifies the result either
	// way, so a store that does not will be caught at the size or digest stage
	// instead.
	PutToolResultObjectStream(ctx context.Context, objectID string, content io.Reader, size uint64) error
}
