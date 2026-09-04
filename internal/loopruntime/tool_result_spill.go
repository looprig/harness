package loopruntime

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"hash"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/event"
)

// captureSink is the session spill one tool result's raw bytes are encoded into
// on the way to the SessionObjectStore. It is the single place the declared
// capture ceiling, the byte counts, the content digest and the encoding
// classification are applied, so the streaming producer path (which writes to it
// incrementally, from inside the tool) and the materialized fallback path (which
// writes to it once, after the tool returned) cannot disagree about what
// "captured" means.
//
// It is an io.Writer and never reports a short write or an error: a producer
// that keeps going past the ceiling — or whose spill file has failed — must be
// able to finish rather than see an I/O error it would report as a TOOL failure,
// which is a different outcome from "the loop could not retain the result".
// Bytes past the ceiling are counted but not retained, which is exactly the
// difference between offeredBytes and capturedBytes and the reason truncated
// reports true. A backing failure is latched on spillErr instead, and the
// retention pipeline fails the step on it.
//
// Where the retained prefix LIVES is the backing's business. A memory backing
// holds it in a bytes.Buffer, which is what a caller with no configured spill
// directory gets; a file backing holds it in an owner-only file under the
// session-scoped spill root and is what keeps a large capture out of Host
// memory. Everything else in this type is identical for both, which is the
// property TestSpillBackedSinkMatchesTheMemorySinkExactly measures directly.
//
// A sink is owned by one result and is not safe for concurrent use.
type captureSink struct {
	ceiling int
	backing captureBacking
	digest  hash.Hash
	text    utf8Scanner
	// retained is capturedBytes as an int. It exists so the ceiling arithmetic
	// never converts between int and uint64: retained is bounded by ceiling,
	// which is an int, while the two exported counts are uint64 because a
	// producer's total is not bounded by anything the loop chose.
	retained int
	offered  uint64
	captured uint64
	failure  error
}

// captureBacking is where a sink's retained prefix is kept. write returns the
// number of bytes it durably accepted, so a short write is reported as such
// rather than being rounded up to success; the sink treats a short write and an
// outright error identically, because both mean the retained prefix on the
// backing no longer matches what the sink counted.
type captureBacking interface {
	write(p []byte) (int, error)
	materialize() ([]byte, error)
	reader() (io.ReadCloser, error)
	release() error
}

// newCaptureSink builds a memory-backed sink that retains at most ceiling bytes.
// A ceiling of zero or less retains nothing, which is why resolveToolSetCaps
// never produces one: the runtime's ceiling is a reviewed positive default, and a
// caller that asks for zero gets a sink that records every byte as elided rather
// than a sink that silently retains everything.
//
// This is the backing a loop uses when no spill directory is configured. It is
// bounded by the same ceiling as the file backing, so it is not unbounded
// residency; it is simply residency, which is what a spill directory exists to
// remove.
func newCaptureSink(ceiling int) *captureSink {
	return newCaptureSinkWithBacking(ceiling, &memoryBacking{})
}

func newCaptureSinkWithBacking(ceiling int, backing captureBacking) *captureSink {
	return &captureSink{ceiling: ceiling, backing: backing, digest: sha256.New(), text: newUTF8Scanner()}
}

// newFailedCaptureSink is the sink a caller gets when the spill could not be
// opened at all. It accepts and counts every producer byte — the tool still runs
// and must still be able to finish — but retains nothing and reports the open
// failure from spillErr, so the retention pipeline fails the step at the spill
// stage rather than committing a capture that references an object nobody wrote.
func newFailedCaptureSink(ceiling int, cause error) *captureSink {
	sink := newCaptureSinkWithBacking(ceiling, failedBacking{cause: cause})
	sink.failure = cause
	return sink
}

// Write accepts every byte, counts it, and retains the prefix that still fits
// under the ceiling. The digest covers exactly the RETAINED bytes, because the
// digest exists to verify the stored object and the stored object is the
// retained prefix.
//
// A backing that fails or short-writes latches the FIRST cause and stops
// retaining, while offeredBytes keeps rising: the producer is never interrupted,
// and the count it reports remains the true original size.
func (s *captureSink) Write(p []byte) (int, error) {
	s.offered += uint64(len(p))
	room := s.ceiling - s.retained
	if room <= 0 || s.failure != nil {
		return len(p), nil
	}
	kept := p
	if len(kept) > room {
		kept = kept[:room]
	}
	n, err := s.backing.write(kept)
	if n > 0 {
		s.digest.Write(kept[:n])
		s.text.write(kept[:n])
		s.captured += uint64(n)
		s.retained += n
	}
	if err == nil && n < len(kept) {
		err = io.ErrShortWrite
	}
	if err != nil {
		s.failure = err
	}
	return len(p), nil
}

// spillErr reports the latched backing failure, if any. It is nil for every sink
// whose retained prefix is exactly what the counts and the digest describe.
func (s *captureSink) spillErr() error { return s.failure }

// materialize returns the retained prefix as bytes, reading it back from the
// backing when the backing is a file. For a memory backing the returned slice
// ALIASES the sink's buffer and is valid until the next Write, which is why
// PutToolResultObject's contract states the content slice is call-scoped. It is bounded by the ceiling, never by the
// producer's output, but it IS resident: prefer reader when the whole prefix does
// not have to be in memory at once.
func (s *captureSink) materialize() ([]byte, error) { return s.backing.materialize() }

// reader returns a rewound stream over the retained prefix. The caller closes it.
func (s *captureSink) reader() (io.ReadCloser, error) { return s.backing.reader() }

// release discards the local spill. It is idempotent and safe to call on a sink
// whose bytes were never uploaded.
func (s *captureSink) release() error { return s.backing.release() }

// capturedBytes is how many bytes were retained; offeredBytes is how many the
// producer supplied in total. They differ exactly when the ceiling bound — or
// when the backing failed, which spillErr reports and the pipeline refuses.
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
//
// It is computed incrementally as bytes are retained rather than by validating a
// materialized copy, so classifying a file-backed capture costs no read and no
// residency.
func (s *captureSink) encoding() event.ToolResultEncoding {
	if s.text.valid() {
		return event.ToolResultEncodingUTF8
	}
	return event.ToolResultEncodingBinary
}

// memoryBacking keeps the retained prefix in memory. bytes.Buffer never fails
// and never short-writes, so a memory-backed sink never latches a failure.
type memoryBacking struct{ buf bytes.Buffer }

func (m *memoryBacking) write(p []byte) (int, error) { return m.buf.Write(p) }

func (m *memoryBacking) materialize() ([]byte, error) { return m.buf.Bytes(), nil }

func (m *memoryBacking) reader() (io.ReadCloser, error) {
	return io.NopCloser(bytes.NewReader(m.buf.Bytes())), nil
}

func (m *memoryBacking) release() error { return nil }

// failedBacking is the backing of a sink whose spill could not be opened. Every
// operation reports the original cause, so the failure cannot be mistaken for an
// empty capture.
type failedBacking struct{ cause error }

func (f failedBacking) write([]byte) (int, error) { return 0, f.cause }

func (f failedBacking) materialize() ([]byte, error) { return nil, f.cause }

func (f failedBacking) reader() (io.ReadCloser, error) { return nil, f.cause }

func (f failedBacking) release() error { return nil }

// utf8Scanner decides UTF-8 validity of a byte stream incrementally, holding back
// at most the three bytes that could still be the start of a longer rune. It
// answers exactly what utf8.Valid would answer over the concatenation of every
// byte written, which is the property the sink's encoding contract needs and
// which TestSpillBackedSinkMatchesTheMemorySinkExactly compares directly against
// a memory-backed sink over the same payloads.
type utf8Scanner struct {
	ok      bool
	pending []byte
}

func newUTF8Scanner() utf8Scanner { return utf8Scanner{ok: true} }

func (s *utf8Scanner) write(p []byte) {
	if !s.ok {
		return
	}
	buf := p
	if len(s.pending) > 0 {
		buf = append(append([]byte(nil), s.pending...), p...)
		s.pending = nil
	}
	for len(buf) > 0 {
		if buf[0] < utf8.RuneSelf {
			buf = buf[1:]
			continue
		}
		if !utf8.FullRune(buf) {
			// A trailing partial rune is neither valid nor invalid yet: hold it
			// until the next write completes it, or until valid() is asked while
			// it is still incomplete, which is an invalid stream.
			s.pending = append([]byte(nil), buf...)
			return
		}
		r, size := utf8.DecodeRune(buf)
		if r == utf8.RuneError && size == 1 {
			s.ok = false
			s.pending = nil
			return
		}
		buf = buf[size:]
	}
}

// valid reports whether every byte written so far forms a complete, valid UTF-8
// stream. A held-back partial rune counts as invalid, because the retained
// prefix really does end mid-rune.
func (s *utf8Scanner) valid() bool { return s.ok && len(s.pending) == 0 }

// captureSpillSuffix names a spill file. It is a fixed suffix on the
// ToolExecutionID so a directory listing is self-describing and so the file name
// carries no tool-, argument- or output-derived text.
const captureSpillSuffix = ".capture"

// captureSpillDirectory is one session's spill root: <base>/<sessionID>, created
// owner-only, holding at most one file per ToolExecutionID.
//
// Three properties are structural rather than conventional. The base must be an
// ABSOLUTE path, so the root never depends on the process working directory and
// no process-global temp path can supply it — a pooled Host places the base
// itself, and TestCaptureSpillDirectoryUsesNoProcessGlobalTempPath holds the
// package to it. The root is refused if it, or the base, is a symlink, so a
// planted link cannot redirect spills out of the session's own directory. And
// the root is deliberately NOT inside a checkpointed workspace: pkg/rig rejects a
// spill base that overlaps the workspace region, which is what keeps a spill out
// of every workspace checkpoint (see rig's WithToolResultCapture).
type captureSpillDirectory struct {
	root string

	// failure is set on a directory that could never be established. Such a
	// directory is still handed to the loop rather than dropped, so retention
	// fails at the spill stage — visibly, on the turn — instead of silently
	// degrading to no retention at all.
	failure error

	mu       sync.Mutex
	released bool
}

// ToolResultSpillDirectory is the exported name for one session's spill root, so
// internal/sessionruntime can hold and release it. It is an alias rather than a
// wrapper: there is one type, and its only exported method is Release, so no
// package outside this one can open a spill.
type ToolResultSpillDirectory = captureSpillDirectory

// NewToolResultSpillDirectory establishes a session's spill root under base.
func NewToolResultSpillDirectory(base string, sessionID uuid.UUID) (*ToolResultSpillDirectory, error) {
	return newCaptureSpillDirectory(base, sessionID)
}

// UnavailableToolResultSpillDirectory is the directory a session holds when its
// spill root could not be established. Every openSink reports cause, so a loop
// wired for retention fails its turn at the spill stage rather than quietly
// retaining nothing — the composition asked for durable retention, and this is
// the honest report that it is unavailable.
func UnavailableToolResultSpillDirectory(cause error) *ToolResultSpillDirectory {
	return &captureSpillDirectory{failure: cause}
}

// captureSpillError is the typed cause when a spill directory or file cannot be
// established. It never carries tool output or argument text — only the path the
// loop itself derived.
type captureSpillError struct {
	Path   string
	Reason string
	Cause  error
}

func (e *captureSpillError) Error() string {
	if e.Path == "" {
		return "loop: tool result spill: " + e.Reason
	}
	return "loop: tool result spill: " + e.Reason + ": " + e.Path
}

func (e *captureSpillError) Unwrap() error { return e.Cause }

// newCaptureSpillDirectory establishes <base>/<sessionID> as this session's spill
// root. The base is created if missing; both it and the root are then verified to
// be real, owner-only directories rather than symlinks.
func newCaptureSpillDirectory(base string, sessionID uuid.UUID) (*captureSpillDirectory, error) {
	if strings.TrimSpace(base) == "" {
		return nil, &captureSpillError{Reason: "spill base is empty"}
	}
	if !filepath.IsAbs(base) {
		return nil, &captureSpillError{Path: base, Reason: "spill base is not an absolute path"}
	}
	if err := os.MkdirAll(base, 0o700); err != nil {
		return nil, &captureSpillError{Path: base, Reason: "create spill base", Cause: err}
	}
	if err := verifyOwnerOnlyDirectory(base); err != nil {
		return nil, err
	}
	root := filepath.Join(base, sessionID.String())
	if err := os.Mkdir(root, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return nil, &captureSpillError{Path: root, Reason: "create spill root", Cause: err}
	}
	if err := verifyOwnerOnlyDirectory(root); err != nil {
		return nil, err
	}
	return &captureSpillDirectory{root: root}, nil
}

// verifyOwnerOnlyDirectory rejects a path that is a symlink or not a directory,
// and forces owner-only permissions regardless of the process umask. Mkdir's mode
// argument is masked by the umask, so the chmod is what actually establishes the
// permission rather than requesting it.
func verifyOwnerOnlyDirectory(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return &captureSpillError{Path: path, Reason: "stat spill directory", Cause: err}
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return &captureSpillError{Path: path, Reason: "spill directory is a symlink"}
	}
	if !info.IsDir() {
		return &captureSpillError{Path: path, Reason: "spill directory is not a directory"}
	}
	// #nosec G302 -- 0700 is owner-only for a DIRECTORY: the execute bit is what
	// permits traversal, so 0600 would make the directory unusable by its owner.
	if err := os.Chmod(path, 0o700); err != nil {
		return &captureSpillError{Path: path, Reason: "restrict spill directory", Cause: err}
	}
	return nil
}

// spillPath derives the file path for one ToolExecutionID and re-checks
// containment.
//
// The containment check cannot fail today and no test can make it fail: the only
// input is a uuid.UUID, which is a [16]byte whose String is 36 fixed-format
// characters of lowercase hex and dashes, so no value in the space can contain a
// separator or a dot segment — TestCaptureSpillNameCannotEscapeItsDirectory
// establishes the containment over that space rather than over this branch. It is
// therefore an ASSERTION on the caller's contract, not a guard with a reader, and
// deleting it is an equivalent mutation. It is kept because the contract it
// asserts ("the name is derived, never supplied") is what makes the O_EXCL open
// below safe, and it costs one comparison per tool call.
func (d *captureSpillDirectory) spillPath(executionID uuid.UUID) (string, error) {
	name := executionID.String() + captureSpillSuffix
	path := filepath.Join(d.root, name)
	if filepath.Dir(path) != d.root || path != filepath.Clean(path) {
		return "", &captureSpillError{Path: name, Reason: "spill name escapes the session spill root"}
	}
	return path, nil
}

// openSink creates this execution's spill file and returns the sink writing into
// it. The file is created O_EXCL, so an existing file — or a symlink planted at
// the path — is refused rather than truncated or followed.
func (d *captureSpillDirectory) openSink(executionID uuid.UUID, ceiling int) (*captureSink, error) {
	if d.failure != nil {
		return nil, d.failure
	}
	d.mu.Lock()
	released := d.released
	d.mu.Unlock()
	if released {
		return nil, &captureSpillError{Path: d.root, Reason: "spill root was released"}
	}
	path, err := d.spillPath(executionID)
	if err != nil {
		return nil, err
	}
	// #nosec G304 -- path is not caller-supplied: it is the session-scoped root
	// joined with a uuid.UUID's fixed-format string, re-checked for containment by
	// spillPath, and opened O_EXCL so an existing file or symlink is refused.
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err != nil {
		return nil, &captureSpillError{Path: path, Reason: "create spill file", Cause: err}
	}
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		_ = os.Remove(path)
		return nil, &captureSpillError{Path: path, Reason: "restrict spill file", Cause: err}
	}
	return newCaptureSinkWithBacking(ceiling, &fileBacking{file: file, path: path}), nil
}

// Release removes this session's whole spill root. It is idempotent, and after it
// returns no further spill can be opened: a session that has released its
// workspace must not start writing new local files under it.
func (d *captureSpillDirectory) Release() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.released = true
	if d.root == "" {
		return nil
	}
	if err := os.RemoveAll(d.root); err != nil {
		return &captureSpillError{Path: d.root, Reason: "remove spill root", Cause: err}
	}
	return nil
}

// fileBacking keeps the retained prefix in one owner-only file. Writes append at
// the file's own offset; reader rewinds a SEPARATE descriptor so a read cannot
// disturb the write position or race a concurrent writer's offset.
type fileBacking struct {
	file *os.File
	path string
}

func (f *fileBacking) write(p []byte) (int, error) { return f.file.Write(p) }

func (f *fileBacking) materialize() ([]byte, error) {
	reader, err := f.reader()
	if err != nil {
		return nil, err
	}
	defer func() { _ = reader.Close() }()
	return io.ReadAll(reader)
}

func (f *fileBacking) reader() (io.ReadCloser, error) {
	if err := f.file.Sync(); err != nil {
		return nil, &captureSpillError{Path: f.path, Reason: "flush spill file", Cause: err}
	}
	// #nosec G304 -- f.path is the spill this backing itself created under the
	// session-scoped root; it is never derived from tool input.
	file, err := os.OpenFile(f.path, os.O_RDONLY, 0)
	if err != nil {
		return nil, &captureSpillError{Path: f.path, Reason: "reopen spill file", Cause: err}
	}
	return file, nil
}

// release closes and deletes the spill. It is idempotent: a second call finds the
// descriptor already closed and the path already gone, and reports neither.
func (f *fileBacking) release() error {
	closeErr := f.file.Close()
	if closeErr != nil && !errors.Is(closeErr, os.ErrClosed) {
		return &captureSpillError{Path: f.path, Reason: "close spill file", Cause: closeErr}
	}
	if err := os.Remove(f.path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return &captureSpillError{Path: f.path, Reason: "remove spill file", Cause: err}
	}
	return nil
}
