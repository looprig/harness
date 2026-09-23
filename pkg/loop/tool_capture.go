package loop

import (
	"context"
	"errors"
	"io"

	sessionwire "github.com/looprig/core/sessionwire/v1"
	"github.com/looprig/core/uuid"
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

// ReadToolResultToolName is the model-facing name of the tool that pages through
// a retained tool result. Harness owns the constant because the loop's retention
// marker names the tool: the marker and the tool that answers it must not be
// able to drift. A composition registers the tool itself (a definition declaring
// tool.RequiresToolResultReader); the marker instructs the model to call it only
// when the calling loop actually has a tool of this name bound.
const ReadToolResultToolName = "read_tool_result"

// ToolResultObjects is the SESSION-SCOPED, READABLE durable tool-result
// retention seam, wired with rig.WithToolResultObjects. It supersedes
// ToolResultObjectStore.
//
// The store, not the loop, mints the reference: PublishToolResultObject returns
// the metadata the store issued, and that reference is what the committed
// StepDone records. A reference minted anywhere else is one the store cannot
// resolve, which is exactly the defect of the legacy seam.
//
// The session argument is always supplied by the runtime — the RUNTIME session
// id the journal is filed under — and never by a model or a tool. A tenant is
// not an argument at all: it is fixed by the store the composition wired.
//
// (*sessionstore.Store).ToolResultObjects() is the reference implementation,
// writing SessionStore "tool-result" objects beside the session's journal.
type ToolResultObjects interface {
	// PublishToolResultObject stores exactly size bytes read from content, whose
	// SHA-256 is sum, and returns the store-issued metadata. It must fail rather
	// than store a short or different object. content is io.Reader and nothing
	// more; see ToolResultObjectStreamStore for why an implementation must not
	// type-assert richer capabilities on it. The loop verifies the returned
	// metadata's size, digest and reference before recording it.
	PublishToolResultObject(ctx context.Context, session uuid.UUID, content io.Reader, size uint64, sum [32]byte) (sessionwire.ObjectMetadata, error)
	// OpenToolResultObject resolves a reference this store issued for session
	// and returns its metadata and a stream of its bytes. The stream proves
	// integrity only when read through EOF; a caller that stops early must treat
	// Close's error as a failure rather than a success.
	OpenToolResultObject(ctx context.Context, session uuid.UUID, ref sessionwire.ObjectReference) (sessionwire.ObjectMetadata, io.ReadCloser, error)
}

// ErrToolResultObjectIntegrity is what a ToolResultObjects implementation wraps
// when an object's stored bytes do not verify against the size and digest it
// was published with. A reader distinguishes it from the store merely being
// unavailable: the first is evidence of corruption or substitution, the second
// is worth retrying.
var ErrToolResultObjectIntegrity = errors.New("loop: tool result object failed integrity verification")

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
//
// Deprecated: an identity the loop mints is one no session object store can
// resolve, so a capture retained through this seam can never be read back — not
// by read_tool_result and not by an object route. Use ToolResultObjects, whose
// references the store issues. This seam is removed at the next major version.
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
	//
	// content is io.Reader and NOTHING MORE. Its dynamic type varies with how the
	// loop was composed — a session with a spill directory hands over a view of an
	// open file, one without hands over a view of a byte slice — and those two
	// carry different optional capabilities. A store must therefore not type-assert
	// io.ReaderAt, io.Seeker, io.WriterTo or a Size method and take a different
	// path when it succeeds: that path would be selected by the host's spill
	// configuration rather than by anything about the object, and would go
	// untested in whichever composition the author did not have in mind. A
	// multipart uploader that needs random access must buffer or chunk it itself.
	PutToolResultObjectStream(ctx context.Context, objectID string, content io.Reader, size uint64) error
}
