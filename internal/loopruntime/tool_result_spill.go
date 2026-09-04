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
// byte written, which is the property the sink's encoding contract needs.
//
// That equivalence is measured by TestUTF8ScannerAgreesWithUTF8ValidOnEveryShortString,
// which runs every string of length 1..4 over a byte alphabet covering each UTF-8
// byte class, in every chunking, against utf8.Valid itself.
// TestSpillBackedSinkMatchesTheMemorySinkExactly deliberately does NOT establish
// it: both of its sinks run this scanner, so it compares the scanner to itself and
// could not fail for a scanner bug. What that test measures is that the BACKING
// makes no difference.
//
// The incremental form is chosen over validating a materialized copy because the
// retained prefix may be a file: this keeps the classification to O(1) retained
// state and no second pass over the capture.
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
// root. It does NOT create the base: where a host may write local bytes is the
// host's placement decision, and creating it would mean creating THROUGH whatever
// intermediate components the path happens to have. os.MkdirAll follows a
// symlinked interior component silently, so a link planted anywhere above the
// final component would be traversed and the final Lstat would then see a
// perfectly real directory inside somebody else's tree. Requiring the base to
// exist removes that step entirely: harness creates exactly one component, the
// session root, directly inside a directory it has already lstat-checked.
//
// What is and is not checked, precisely. The BASE must be an absolute path, must
// already exist, must be a directory rather than a symlink AS ITS FINAL COMPONENT,
// and must not be writable by group or other — a base anyone else can write to is
// a base anyone else can plant the session root in. Harness does not resolve the
// base's interior components and does not repair its permissions; pkg/rig's public
// option canonicalizes the base with EvalSymlinks before wiring it, which is the
// layer that owns the whole path. The ROOT is created by harness, so it is held to
// the full property: one component, created 0700, then lstat-checked as a
// non-symlink directory.
func newCaptureSpillDirectory(base string, sessionID uuid.UUID) (*captureSpillDirectory, error) {
	if strings.TrimSpace(base) == "" {
		return nil, &captureSpillError{Reason: "spill base is empty"}
	}
	if !filepath.IsAbs(base) {
		return nil, &captureSpillError{Path: base, Reason: "spill base is not an absolute path"}
	}
	if err := verifySpillBase(base); err != nil {
		return nil, err
	}
	root := filepath.Join(base, sessionID.String())
	if err := os.Mkdir(root, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return nil, &captureSpillError{Path: root, Reason: "create spill root", Cause: err}
	}
	if err := verifySpillRoot(root); err != nil {
		return nil, err
	}
	return &captureSpillDirectory{root: root}, nil
}

// verifySpillBase checks the operator-supplied base WITHOUT mutating it. It is a
// pure check by design: the base may be a long-lived directory shared by every
// session on a host, so silently re-permissioning it would change something
// harness does not own — and a group- or world-writable base is a condition to
// refuse, not to repair, because by the time the repair ran an entry could already
// have been planted.
func verifySpillBase(base string) error {
	info, err := lstatDirectory(base)
	if err != nil {
		return err
	}
	if info.Mode().Perm()&0o022 != 0 {
		return &captureSpillError{Path: base, Reason: "spill base is writable by group or other"}
	}
	return nil
}

// verifySpillRoot checks the session root harness just created and restores the
// owner bits a restrictive umask cleared.
//
// The chmod is a USABILITY fix, not a security control, and the distinction
// matters because the opposite claim would be a false one: a umask can only REMOVE
// permission bits, so os.Mkdir(root, 0o700) can never produce a mode wider than
// 0700 and this call can never narrow anything. What it can do is repair the
// unusable case — under a umask of 0700 the new directory is mode 0000 and its own
// owner cannot traverse it. No test drives that path, because the umask is
// process-global and setting it would race every parallel test in the package.
func verifySpillRoot(root string) error {
	if _, err := lstatDirectory(root); err != nil {
		return err
	}
	// #nosec G302 -- 0700 is owner-only for a DIRECTORY: the execute bit is what
	// permits traversal, so 0600 would make the directory unusable by its owner.
	if err := os.Chmod(root, 0o700); err != nil {
		return &captureSpillError{Path: root, Reason: "restrict spill directory", Cause: err}
	}
	return nil
}

// lstatDirectory rejects a path that does not exist, is a symlink, or is not a
// directory. It lstats rather than stats, so a symlink is seen as a symlink
// instead of as whatever it points at.
func lstatDirectory(path string) (os.FileInfo, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, &captureSpillError{Path: path, Reason: "stat spill directory", Cause: err}
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return nil, &captureSpillError{Path: path, Reason: "spill directory is a symlink"}
	}
	if !info.IsDir() {
		return nil, &captureSpillError{Path: path, Reason: "spill directory is not a directory"}
	}
	return info, nil
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
	// The chmod restores owner bits a restrictive umask cleared; it can never
	// widen the mode, because a umask only removes bits from the 0o600 requested
	// above. Like verifySpillRoot's, it is a usability repair rather than a
	// security control, and no test drives it for the same process-global reason.
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
// the file's own offset; reader hands out an io.SectionReader over the SAME
// descriptor, which reads with ReadAt and therefore never disturbs the write
// offset.
//
// Reading through the open descriptor rather than reopening the path is what makes
// this correct as well as cheap: a reopen would need the path to still resolve to
// the same file, and it would need a flush to look coherent even though a second
// open on the same host already reads through the page cache. A SectionReader
// needs neither, so there is no fsync per capture upload and no second variable
// path for gosec to flag.
type fileBacking struct {
	file    *os.File
	path    string
	written int64
}

func (f *fileBacking) write(p []byte) (int, error) {
	n, err := f.file.Write(p)
	f.written += int64(n)
	return n, err
}

func (f *fileBacking) materialize() ([]byte, error) {
	reader, err := f.reader()
	if err != nil {
		return nil, err
	}
	defer func() { _ = reader.Close() }()
	return io.ReadAll(reader)
}

// reader returns a rewound stream over exactly the bytes this backing ACCEPTED.
// The bound is the backing's own running count of accepted bytes, which is the
// same quantity the sink reports as capturedBytes and digests — so the section's
// length, the recorded size and the digest cannot disagree, whatever the file on
// disk happens to be. That equality is what
// TestSpillBackedSinkMatchesTheMemorySinkExactly measures, by comparing the
// streamed bytes against a memory-backed sink over the same 90 payload/chunking
// pairs.
func (f *fileBacking) reader() (io.ReadCloser, error) {
	return spillSection{SectionReader: io.NewSectionReader(f.file, 0, f.written)}, nil
}

// spillSection is a read-only view of the bytes a spill file accepted. It is a
// NAMED type rather than an io.NopCloser so that "the upload streamed off the
// local spill" is observable: a materialized copy comes back as a reader over a
// byte slice, and the two are otherwise indistinguishable to a caller. Close is a
// no-op because the section borrows the backing's descriptor, which release owns.
type spillSection struct{ *io.SectionReader }

func (spillSection) Close() error { return nil }

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
