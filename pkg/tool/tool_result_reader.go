package tool

import (
	"context"
	"encoding/base64"
	"reflect"
	"strconv"
	"strings"

	"github.com/looprig/core/uuid"
)

// tool_result_reader.go is the capability a tool is handed to page back through
// a retained tool result: the complete bytes the loop captured and stored
// durably when the model was shown only a shaped preview.
//
// The capability is scoped by the RUNTIME, never by the model. Harness binds one
// reader per (session, calling loop), so a model can name only a capture id —
// never a session, tenant, object reference, backend key or path — and it can
// name only a capture its OWN loop produced. A parent loop cannot page a child's
// captures and a child cannot page its parent's.

// MaxToolResultPageBytes is the hard ceiling on the RETAINED bytes one page
// returns, whatever a caller asks for. A page is itself a tool result that enters
// the model's history, so a page larger than the model preview would be shaped —
// and retained a second time — rather than read.
const MaxToolResultPageBytes = 64 << 10

// Capture encodings, mirroring the durable ToolResultCapture vocabulary. They are
// restated here as strings because pkg/event depends on this package and not the
// other way around.
const (
	ToolResultEncodingUTF8   = "utf-8"
	ToolResultEncodingBinary = "binary"
)

// ToolResultPageRequest names one window of a retained capture.
//
// CaptureID is the capture's ToolExecutionID in canonical UUID form — the value
// the loop's retention marker prints. Offset is the first retained byte to
// return. MaxBytes bounds the RETAINED bytes returned; zero means the reader's
// own ceiling, and any value is clamped to that ceiling.
type ToolResultPageRequest struct {
	CaptureID string
	Offset    uint64
	MaxBytes  uint64
}

// ToolResultPage is one window of a retained capture's bytes plus the capture's
// size description.
//
// Data holds retained bytes [Offset, Offset+len(Data)) verbatim. For a UTF-8
// capture the window ends on a rune boundary, so a page never splits a
// character; for a binary capture it is raw bytes, which Render encodes as
// base64.
type ToolResultPage struct {
	CaptureID uuid.UUID
	Offset    uint64
	Data      []byte

	// CapturedBytes is how many bytes were retained. Bytes at and beyond it do
	// not exist anywhere.
	CapturedBytes uint64
	// OriginalBytes is the producer's byte count; OriginalExact is false when
	// the producer was stopped and the count is only a lower bound.
	OriginalBytes uint64
	OriginalExact bool
	// Truncated reports that the producer emitted more than was retained, and
	// TruncationReason says why ("capture_ceiling" or "source_limit").
	Truncated        bool
	TruncationReason string
	// Encoding is ToolResultEncodingUTF8 or ToolResultEncodingBinary.
	Encoding string
}

// End is the offset one past the last byte in this page.
func (p ToolResultPage) End() uint64 { return p.Offset + uint64(len(p.Data)) }

// NextOffset reports the offset of the next page, or false when this page
// reached the end of the retained bytes.
func (p ToolResultPage) NextOffset() (uint64, bool) {
	end := p.End()
	if end >= p.CapturedBytes {
		return 0, false
	}
	return end, true
}

// Render returns the model-visible text of this page: the window (verbatim for a
// UTF-8 capture, base64 for a binary one) followed by a one-line footer naming
// the byte range, the continuation offset, and — for a truncated capture — how
// many bytes were never retained and why.
//
// It is PROMPT TEXT. No consumer may parse it; the machine-readable form of a
// page is the ToolResultPage itself.
func (p ToolResultPage) Render() string {
	var b strings.Builder
	if p.Encoding == ToolResultEncodingBinary {
		b.WriteString(base64.StdEncoding.EncodeToString(p.Data))
	} else {
		b.Write(p.Data)
	}
	b.WriteString("\n[capture ")
	b.WriteString(p.CaptureID.String())
	b.WriteString(": ")
	if len(p.Data) == 0 {
		b.WriteString("no bytes")
	} else {
		b.WriteString("bytes ")
		b.WriteString(strconv.FormatUint(p.Offset, 10))
		b.WriteString("-")
		b.WriteString(strconv.FormatUint(p.End()-1, 10))
	}
	b.WriteString(" of ")
	b.WriteString(strconv.FormatUint(p.CapturedBytes, 10))
	b.WriteString(" retained (original ")
	if !p.OriginalExact {
		b.WriteString("at least ")
	}
	b.WriteString(strconv.FormatUint(p.OriginalBytes, 10))
	b.WriteString(")")
	if p.Encoding == ToolResultEncodingBinary {
		b.WriteString("; encoding=base64")
	}
	if next, ok := p.NextOffset(); ok {
		b.WriteString("; next_offset=")
		b.WriteString(strconv.FormatUint(next, 10))
	} else {
		b.WriteString("; end")
	}
	if p.Truncated && p.OriginalBytes > p.CapturedBytes {
		b.WriteString("; truncated: ")
		if !p.OriginalExact {
			b.WriteString("at least ")
		}
		b.WriteString(strconv.FormatUint(p.OriginalBytes-p.CapturedBytes, 10))
		b.WriteString(" bytes not retained (")
		b.WriteString(p.TruncationReason)
		b.WriteString(")")
	}
	b.WriteString("]")
	return b.String()
}

// ToolResultReader pages through the retained captures of ONE loop in ONE
// session. It is safe for concurrent use.
//
// Every page is verified: the reader streams the whole stored object through its
// integrity check before returning any window of it, so a page is never served
// from an object whose stored bytes no longer match what was captured. The cost
// of a page is therefore the capture's size, which the capture ceiling bounds.
type ToolResultReader interface {
	ReadToolResult(ctx context.Context, request ToolResultPageRequest) (ToolResultPage, error)
}

// ToolResultReadErrorKind classifies a ToolResultReader failure. Every kind is
// safe to show the model: none carries a session, tenant, reference or backend
// detail.
type ToolResultReadErrorKind string

const (
	// ToolResultReadUnknownCapture means the capture id is malformed, or names
	// no capture this loop produced — including one another loop or another
	// session produced. The two are deliberately indistinguishable.
	ToolResultReadUnknownCapture ToolResultReadErrorKind = "unknown_capture"
	// ToolResultReadNotRetained means the capture exists but no object was
	// stored for it, because the model already saw the complete result.
	ToolResultReadNotRetained ToolResultReadErrorKind = "not_retained"
	// ToolResultReadOffsetOutOfRange means the offset is at or past the end of
	// the retained bytes.
	ToolResultReadOffsetOutOfRange ToolResultReadErrorKind = "offset_out_of_range"
	// ToolResultReadIntegrity means the stored object did not verify.
	ToolResultReadIntegrity ToolResultReadErrorKind = "integrity"
	// ToolResultReadUnavailable means the store could not serve the object.
	ToolResultReadUnavailable ToolResultReadErrorKind = "unavailable"
)

// ToolResultReadError is the typed ToolResultReader failure. Cause may carry a
// store diagnostic and must not be shown to the model; Error does not include it.
type ToolResultReadError struct {
	Kind  ToolResultReadErrorKind
	Cause error
}

func (e *ToolResultReadError) Error() string {
	switch e.Kind {
	case ToolResultReadUnknownCapture:
		return "read tool result: unknown capture_id"
	case ToolResultReadNotRetained:
		return "read tool result: the complete result is already in the conversation; nothing further was retained"
	case ToolResultReadOffsetOutOfRange:
		return "read tool result: offset is past the end of the retained bytes"
	case ToolResultReadIntegrity:
		return "read tool result: the retained object failed its integrity check"
	default:
		return "read tool result: the retained object is unavailable"
	}
}

func (e *ToolResultReadError) Unwrap() error { return e.Cause }

func nilToolResultReader(value ToolResultReader) bool {
	return value == nil || nilReflectValue(reflect.ValueOf(value))
}
