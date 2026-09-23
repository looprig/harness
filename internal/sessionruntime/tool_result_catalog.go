package sessionruntime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"unicode/utf8"

	sessionwire "github.com/looprig/core/sessionwire/v1"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/internal/loopruntime"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/tool"
)

// tool_result_catalog.go is the session's side of READABLE tool-result
// retention: the store binding every loop publishes through, and the capture
// index read_tool_result is answered from.
//
// The index maps a capture's ToolExecutionID to the loop that produced it and
// the committed capture record. It is filled from COMMITTED StepDone events only
// — live, by the hub's commit observer, and on restore by folding every replayed
// StepDone — so a capture is readable exactly when the journal says it exists.
// It lives beside the loops' histories rather than in them, so compaction, which
// rewrites a history, never makes a capture unreadable.

// maxBinaryPageBytes keeps a binary page's base64 rendering within
// tool.MaxToolResultPageBytes: base64 spends four bytes for every three. A
// fitted page limit is scaled the same way (see ReadToolResult).
const maxBinaryPageBytes = tool.MaxToolResultPageBytes / 4 * 3

// toolResultPageFooterReserve is the room a page leaves for its rendered footer
// when it is fitted under a loop's model preview budget.
const toolResultPageFooterReserve = 512

// toolResultCatalog is one session's readable retention state. A nil catalog
// means no readable store is wired; every method is safe on it.
type toolResultCatalog struct {
	objects loop.ToolResultObjects
	session uuid.UUID

	mu       sync.RWMutex
	captures map[uuid.UUID]indexedToolResultCapture
}

type indexedToolResultCapture struct {
	loopID  uuid.UUID
	capture event.ToolResultCapture
}

func newToolResultCatalog(objects loop.ToolResultObjects, session uuid.UUID) *toolResultCatalog {
	if objects == nil {
		return nil
	}
	return &toolResultCatalog{objects: objects, session: session, captures: map[uuid.UUID]indexedToolResultCapture{}}
}

// observe indexes the captures of one committed event. It is idempotent: the
// same StepDone observed twice indexes the same entries.
func (c *toolResultCatalog) observe(ev event.Event) {
	if c == nil {
		return
	}
	done, ok := ev.(event.StepDone)
	if !ok || len(done.Captures) == 0 {
		return
	}
	loopID := done.Coordinates.LoopID
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, capture := range done.Captures {
		if capture.ToolExecutionID.IsZero() {
			continue
		}
		c.captures[capture.ToolExecutionID] = indexedToolResultCapture{loopID: loopID, capture: capture}
	}
}

// fold indexes every capture in a replayed event stream. Restore calls it with
// the full-session replay, so a successor Host can page a capture its
// predecessor retained. Restore reuses each loop's journal LoopID, which is what
// keeps the per-loop scoping intact across failover.
func (c *toolResultCatalog) fold(events []event.Event) {
	for _, ev := range events {
		c.observe(ev)
	}
}

func (c *toolResultCatalog) lookup(id uuid.UUID) (indexedToolResultCapture, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	entry, ok := c.captures[id]
	return entry, ok
}

// publisher returns the session-bound publish seam, or a NIL INTERFACE when no
// readable store is wired — never a typed nil, which the loop would treat as
// configured.
func (c *toolResultCatalog) publisher() loopruntime.ToolResultPublisher {
	if c == nil {
		return nil
	}
	return sessionToolResultPublisher{objects: c.objects, session: c.session}
}

// readerFor returns the reader bound to (this session, loopID), or a nil
// interface when no readable store is wired.
func (c *toolResultCatalog) readerFor(loopID uuid.UUID) tool.ToolResultReader {
	if c == nil {
		return nil
	}
	reader := &loopToolResultReader{catalog: c, loopID: loopID}
	reader.pageLimit.Store(tool.MaxToolResultPageBytes)
	return reader
}

// fitToolResultReader lowers a bound reader's page ceiling so a page plus its
// footer fits the SMALLEST model preview budget any of the loop's modes
// declares. A page that exceeded the budget would be shaped and retained a
// second time instead of read. It is a no-op for a reader this package did not
// build.
func fitToolResultReader(reader tool.ToolResultReader, bound loop.BoundDefinition) {
	r, ok := reader.(*loopToolResultReader)
	if !ok || r == nil || bound == nil {
		return
	}
	smallest := bound.ToolLimits().ResultBytes
	for _, mode := range bound.Modes() {
		if b := mode.ToolLimits.ResultBytes; b > 0 && (smallest <= 0 || b < smallest) {
			smallest = b
		}
	}
	if smallest <= 0 {
		return
	}
	limit := smallest - toolResultPageFooterReserve
	if limit <= 0 {
		limit = smallest / 2
	}
	if limit < int(r.pageLimit.Load()) {
		r.pageLimit.Store(int64(limit))
	}
}

// sessionToolResultPublisher binds loop.ToolResultObjects to one session, so a
// loop can publish only into its own session's scope.
type sessionToolResultPublisher struct {
	objects loop.ToolResultObjects
	session uuid.UUID
}

func (p sessionToolResultPublisher) PublishToolResultObject(ctx context.Context, content io.Reader, size uint64, sum [32]byte) (sessionwire.ObjectMetadata, error) {
	return p.objects.PublishToolResultObject(ctx, p.session, content, size, sum)
}

// loopToolResultReader is tool.ToolResultReader for one loop in one session.
type loopToolResultReader struct {
	catalog   *toolResultCatalog
	loopID    uuid.UUID
	pageLimit atomic.Int64
}

func (r *loopToolResultReader) ReadToolResult(ctx context.Context, request tool.ToolResultPageRequest) (tool.ToolResultPage, error) {
	id, err := uuid.Parse(request.CaptureID)
	if err != nil || id.IsZero() {
		return tool.ToolResultPage{}, &tool.ToolResultReadError{Kind: tool.ToolResultReadUnknownCapture}
	}
	entry, ok := r.catalog.lookup(id)
	// A capture another loop produced is reported exactly as one that does not
	// exist: the reader must not become an oracle for other loops' captures.
	if !ok || entry.loopID != r.loopID {
		return tool.ToolResultPage{}, &tool.ToolResultReadError{Kind: tool.ToolResultReadUnknownCapture}
	}
	capture := entry.capture
	if capture.Reference == nil {
		return tool.ToolResultPage{}, &tool.ToolResultReadError{Kind: tool.ToolResultReadNotRetained}
	}
	if request.Offset >= capture.CapturedBytes {
		return tool.ToolResultPage{}, &tool.ToolResultReadError{Kind: tool.ToolResultReadOffsetOutOfRange}
	}
	window := uint64(r.pageLimit.Load()) // #nosec G115 -- the limit is positive by construction
	binary := capture.Encoding == event.ToolResultEncodingBinary
	if binary {
		// The page limit is a RENDERED budget, and base64 spends four bytes for
		// every three: the raw window is three quarters of it, and never above
		// the ceiling's own three quarters.
		window = max(min(window/4*3, maxBinaryPageBytes), 3)
	}
	if request.MaxBytes > 0 && request.MaxBytes < window {
		window = request.MaxBytes
	}
	data, start, err := r.readWindow(ctx, capture, request.Offset, window, !binary)
	if err != nil {
		return tool.ToolResultPage{}, err
	}
	original, exact := capture.OriginalSize()
	return tool.ToolResultPage{
		CaptureID:        id,
		Offset:           start,
		Data:             data,
		CapturedBytes:    capture.CapturedBytes,
		OriginalBytes:    original,
		OriginalExact:    exact,
		Truncated:        capture.Truncated,
		TruncationReason: string(capture.TruncationReason),
		Encoding:         string(capture.Encoding),
	}, nil
}

// readWindow opens the object, reads [offset, offset+window) and then DRAINS the
// stream to EOF, so the store's whole-object integrity check runs before any
// byte is returned. It independently re-checks the size and digest the capture
// was published with. For UTF-8 text the window is moved onto rune boundaries.
func (r *loopToolResultReader) readWindow(ctx context.Context, capture event.ToolResultCapture, offset, window uint64, text bool) ([]byte, uint64, error) {
	metadata, stream, err := r.catalog.objects.OpenToolResultObject(ctx, r.catalog.session, *capture.Reference)
	if err != nil {
		return nil, 0, readFailure(err)
	}
	if metadata.SizeBytes != capture.CapturedBytes {
		_ = stream.Close()
		return nil, 0, &tool.ToolResultReadError{Kind: tool.ToolResultReadIntegrity}
	}
	digest := sha256.New()
	tee := io.TeeReader(stream, digest)
	// A UTF-8 window reads up to utf8.UTFMax-1 bytes either side so it can move
	// both ends onto rune boundaries without a second pass.
	lead := uint64(0)
	if text {
		lead = min(offset, utf8.UTFMax-1)
	}
	if _, err := io.CopyN(io.Discard, tee, int64(offset-lead)); err != nil { // #nosec G115 -- offset < CapturedBytes, bounded by the capture ceiling
		_ = stream.Close()
		return nil, 0, readFailure(err)
	}
	span := lead + window
	if text {
		span += utf8.UTFMax - 1
	}
	span = min(span, capture.CapturedBytes-(offset-lead))
	buf := make([]byte, span)
	if _, err := io.ReadFull(tee, buf); err != nil {
		_ = stream.Close()
		return nil, 0, readFailure(err)
	}
	if _, err := io.Copy(io.Discard, tee); err != nil {
		_ = stream.Close()
		return nil, 0, readFailure(err)
	}
	if err := stream.Close(); err != nil {
		return nil, 0, readFailure(err)
	}
	// An absent digest is refused, not skipped: the reader's own check is the
	// one that does not depend on the store's EOF verification.
	if metadata.Digest != "sha256:"+hex.EncodeToString(digest.Sum(nil)) {
		return nil, 0, &tool.ToolResultReadError{Kind: tool.ToolResultReadIntegrity}
	}
	start := lead
	end := min(lead+window, uint64(len(buf)))
	if text {
		start, end = runeWindow(buf, start, end)
	}
	return append([]byte(nil), buf[start:end]...), offset - lead + start, nil
}

// runeWindow moves [start, end) of buf onto UTF-8 rune boundaries: start
// forward past continuation bytes, end back to the start of an incomplete rune.
// A window narrower than one rune is widened to that whole rune, so a page is
// never empty while bytes remain. Invalid UTF-8 is passed through: a boundary is
// only sought within utf8.UTFMax bytes.
func runeWindow(buf []byte, start, end uint64) (uint64, uint64) {
	for i := 0; i < utf8.UTFMax-1 && start < end && !utf8.RuneStart(buf[start]); i++ {
		start++
	}
	if end < uint64(len(buf)) {
		trimmed := end
		for i := 0; i < utf8.UTFMax-1 && trimmed > start && !utf8.RuneStart(buf[trimmed]); i++ {
			trimmed--
		}
		if utf8.RuneStart(buf[trimmed]) {
			end = trimmed
		}
	}
	if end == start && start < uint64(len(buf)) {
		_, size := utf8.DecodeRune(buf[start:])
		end = start + uint64(size) // #nosec G115 -- size is 1..UTFMax
	}
	return start, end
}

// readFailure classifies a store failure. The loop.ErrToolResultObjectIntegrity
// sentinel is how a store says the bytes did not verify; every other failure is
// the store being unable to serve them.
func readFailure(err error) error {
	if errors.Is(err, loop.ErrToolResultObjectIntegrity) {
		return &tool.ToolResultReadError{Kind: tool.ToolResultReadIntegrity, Cause: err}
	}
	return &tool.ToolResultReadError{Kind: tool.ToolResultReadUnavailable, Cause: err}
}
