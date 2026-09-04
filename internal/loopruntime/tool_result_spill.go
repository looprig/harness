package loopruntime

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"hash"
	"unicode/utf8"

	"github.com/looprig/harness/pkg/event"
)

// captureSink is the session spill one tool result's raw bytes are encoded into
// on the way to the SessionObjectStore. It is the single place the declared
// capture ceiling, the byte counts and the content digest are applied, so the
// streaming producer path (which writes to it incrementally) and the
// materialized fallback path (which writes to it once) cannot disagree about
// what "captured" means.
//
// It is an io.Writer and never reports a short write: a producer that keeps
// going past the ceiling must be able to finish rather than see an I/O error it
// would report as a tool failure. Bytes past the ceiling are counted but not
// retained, which is exactly the difference between offeredBytes and
// capturedBytes and the reason truncated reports true.
//
// A sink is owned by one result and is not safe for concurrent use.
type captureSink struct {
	ceiling  int
	retained bytes.Buffer
	digest   hash.Hash
	offered  uint64
	captured uint64
}

// newCaptureSink builds a sink that retains at most ceiling bytes. A ceiling of
// zero or less retains nothing, which is why resolveToolSetCaps never produces
// one: the runtime's ceiling is a reviewed positive default, and a caller that
// asks for zero gets a sink that records every byte as elided rather than a sink
// that silently retains everything.
func newCaptureSink(ceiling int) *captureSink {
	return &captureSink{ceiling: ceiling, digest: sha256.New()}
}

// Write accepts every byte, counts it, and retains the prefix that still fits
// under the ceiling. The digest covers exactly the RETAINED bytes, because the
// digest exists to verify the stored object and the stored object is the
// retained prefix.
func (s *captureSink) Write(p []byte) (int, error) {
	s.offered += uint64(len(p))
	room := s.ceiling - s.retained.Len()
	if room > 0 {
		kept := p
		if len(kept) > room {
			kept = kept[:room]
		}
		s.retained.Write(kept)
		s.digest.Write(kept)
		s.captured += uint64(len(kept))
	}
	return len(p), nil
}

// bytes returns the retained prefix. The slice aliases the sink's buffer and is
// valid until the next Write.
func (s *captureSink) bytes() []byte { return s.retained.Bytes() }

// capturedBytes is how many bytes were retained; offeredBytes is how many the
// producer supplied in total. They differ exactly when the ceiling bound.
func (s *captureSink) capturedBytes() uint64 { return s.captured }

func (s *captureSink) offeredBytes() uint64 { return s.offered }

// truncated reports that the producer supplied more than the sink retained.
func (s *captureSink) truncated() bool { return s.offered > s.capturedBytes() }

// digestHex is the lowercase-hex SHA-256 of the retained bytes.
func (s *captureSink) digestHex() string { return hex.EncodeToString(s.digest.Sum(nil)) }

// encoding classifies the RETAINED bytes, not the producer's intent. Cutting a
// text result at the ceiling can land mid-rune, and such a prefix is reported as
// binary because that is what the stored object contains: the sink does not trim
// back to a rune boundary, so capturedBytes always names the exact object size.
func (s *captureSink) encoding() event.ToolResultEncoding {
	if utf8.Valid(s.retained.Bytes()) {
		return event.ToolResultEncodingUTF8
	}
	return event.ToolResultEncodingBinary
}
