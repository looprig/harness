package loopruntime

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/loop"
)

// TestCaptureSinkCountsEveryOfferedByteAndRetainsTheCeiling enumerates a space of
// (ceiling, payload size) pairs rather than pinning one pair: the property is
// "for all sizes, captured = min(size, ceiling) and offered = size", and a single
// fixture would let a mutant that returned the ceiling, the payload length, or a
// constant agree with it.
func TestCaptureSinkCountsEveryOfferedByteAndRetainsTheCeiling(t *testing.T) {
	t.Parallel()
	for _, ceiling := range []int{1, 2, 7, 16, 64} {
		for _, size := range []int{0, 1, 6, 7, 8, 15, 16, 17, 100} {
			payload := strings.Repeat("a", size)
			sink := newCaptureSink(ceiling)
			n, err := sink.Write([]byte(payload))
			if err != nil || n != size {
				t.Fatalf("ceiling=%d size=%d: Write = (%d, %v), want (%d, nil)", ceiling, size, n, err, size)
			}
			wantCaptured := min(size, ceiling)
			if got := sink.capturedBytes(); got != uint64(wantCaptured) {
				t.Errorf("ceiling=%d size=%d: capturedBytes = %d, want %d", ceiling, size, got, wantCaptured)
			}
			if got := sink.offeredBytes(); got != uint64(size) {
				t.Errorf("ceiling=%d size=%d: offeredBytes = %d, want %d", ceiling, size, got, size)
			}
			if got, want := sink.truncated(), size > ceiling; got != want {
				t.Errorf("ceiling=%d size=%d: truncated = %v, want %v", ceiling, size, got, want)
			}
			retained, err := sink.materialize()
			if err != nil {
				t.Fatalf("ceiling=%d size=%d: materialize: %v", ceiling, size, err)
			}
			if got := string(retained); got != payload[:wantCaptured] {
				t.Errorf("ceiling=%d size=%d: bytes = %q, want %q", ceiling, size, got, payload[:wantCaptured])
			}
			sum := sha256.Sum256([]byte(payload[:wantCaptured]))
			if got, want := sink.digestHex(), hex.EncodeToString(sum[:]); got != want {
				t.Errorf("ceiling=%d size=%d: digestHex = %s, want %s", ceiling, size, got, want)
			}
		}
	}
}

// TestCaptureSinkAccumulatesAcrossWrites pins that the ceiling applies to the
// running total, not to each Write: an incremental producer must not be able to
// exceed the ceiling by writing in pieces.
func TestCaptureSinkAccumulatesAcrossWrites(t *testing.T) {
	t.Parallel()
	sink := newCaptureSink(5)
	for i := 0; i < 4; i++ {
		if _, err := sink.Write([]byte("abc")); err != nil {
			t.Fatalf("Write %d: %v", i, err)
		}
	}
	retained, err := sink.materialize()
	if err != nil {
		t.Fatalf("materialize: %v", err)
	}
	if got := string(retained); got != "abcab" {
		t.Fatalf("bytes = %q, want %q", got, "abcab")
	}
	if got := sink.offeredBytes(); got != 12 {
		t.Fatalf("offeredBytes = %d, want 12", got)
	}
	if !sink.truncated() {
		t.Fatal("truncated = false, want true")
	}
}

// TestCaptureSinkEncodingClassifiesTheRetainedBytes covers both directions,
// including the boundary the doc comment calls out: a valid text result cut
// mid-rune by the ceiling is reported as binary, because that is what the stored
// object contains.
func TestCaptureSinkEncodingClassifiesTheRetainedBytes(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		ceiling int
		payload string
		want    event.ToolResultEncoding
	}{
		{"empty is utf-8", 8, "", event.ToolResultEncodingUTF8},
		{"ascii is utf-8", 8, "hello", event.ToolResultEncodingUTF8},
		{"multibyte kept whole is utf-8", 8, "界界", event.ToolResultEncodingUTF8},
		{"multibyte cut on a boundary is utf-8", 3, "界界", event.ToolResultEncodingUTF8},
		{"multibyte cut mid-rune is binary", 4, "界界", event.ToolResultEncodingBinary},
		{"invalid bytes are binary", 8, "\xff\xfe", event.ToolResultEncodingBinary},
		{"invalid bytes past the ceiling do not count", 2, "ab\xff", event.ToolResultEncodingUTF8},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			sink := newCaptureSink(tt.ceiling)
			if _, err := sink.Write([]byte(tt.payload)); err != nil {
				t.Fatalf("Write: %v", err)
			}
			if got := sink.encoding(); got != tt.want {
				t.Fatalf("encoding = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestToolResultCaptureCeilingTakesTheSmallerBound enumerates both orderings
// and the tie. A fixture that only ever set the two bounds equal could not tell
// min from either projection.
func TestToolResultCaptureCeilingTakesTheSmallerBound(t *testing.T) {
	t.Parallel()
	for _, capture := range []int{256, 1024, 4096} {
		for _, materialized := range []int{256, 1024, 4096} {
			ts := ToolSet{MaxToolResultCaptureBytes: capture, MaxMaterializedToolResultBytes: materialized}
			want := min(capture, materialized)
			if got := materializedCaptureCeiling(ts); got != want {
				t.Errorf("materializedCaptureCeiling(capture=%d, materialized=%d) = %d, want %d", capture, materialized, got, want)
			}
		}
	}
}

// TestToolResultCaptureBoundsAreDefaulted pins that neither capture bound
// can resolve to zero, which would make an oversized result unreachable while a
// shaped preview still committed.
func TestToolResultCaptureBoundsAreDefaulted(t *testing.T) {
	t.Parallel()
	for _, in := range []int{-1, 0} {
		got := resolveToolSetCaps(ToolSet{MaxToolResultCaptureBytes: in, MaxMaterializedToolResultBytes: in})
		if got.MaxToolResultCaptureBytes != loop.DefaultToolResultCaptureBytes {
			t.Errorf("in=%d MaxToolResultCaptureBytes = %d, want %d", in, got.MaxToolResultCaptureBytes, loop.DefaultToolResultCaptureBytes)
		}
		if got.MaxMaterializedToolResultBytes != defaultMaxMaterializedToolResultBytes {
			t.Errorf("in=%d MaxMaterializedToolResultBytes = %d, want %d", in, got.MaxMaterializedToolResultBytes, defaultMaxMaterializedToolResultBytes)
		}
	}
	got := resolveToolSetCaps(ToolSet{MaxToolResultCaptureBytes: 4096, MaxMaterializedToolResultBytes: 8192})
	if got.MaxToolResultCaptureBytes != 4096 || got.MaxMaterializedToolResultBytes != 8192 {
		t.Fatalf("declared bounds were overwritten: %+v", got)
	}
}

// --- session-workspace spill (H5.3) ---

// TestCaptureSpillDirectoryRejectsAnUnusableBase enumerates the base paths that
// cannot be a session-scoped spill root. A relative base resolves against the
// process working directory, which is neither session-scoped nor stable across a
// pooled Host's lifetime, so it is refused rather than silently anchored.
func TestCaptureSpillDirectoryRejectsAnUnusableBase(t *testing.T) {
	t.Parallel()
	for _, base := range []string{"", " ", "relative/spills", "./spills", "../spills"} {
		if _, err := newCaptureSpillDirectory(base, uuid.UUID{1}); err == nil {
			t.Errorf("base %q was accepted, want a typed rejection", base)
		}
	}
}

// TestCaptureSpillDirectoryIsSessionScopedAndOwnerOnly pins both halves of step
// 1's directory requirement at once: the root is keyed by the session id (so two
// sessions sharing a base cannot see each other's spills) and it is owner-only
// regardless of the process umask.
func TestCaptureSpillDirectoryIsSessionScopedAndOwnerOnly(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	session := uuid.UUID{7, 7, 7}
	dir, err := newCaptureSpillDirectory(base, session)
	if err != nil {
		t.Fatalf("newCaptureSpillDirectory: %v", err)
	}
	defer dir.Release()
	want := filepath.Join(base, session.String())
	if dir.root != want {
		t.Fatalf("root = %q, want %q", dir.root, want)
	}
	info, err := os.Lstat(dir.root)
	if err != nil {
		t.Fatalf("lstat root: %v", err)
	}
	if !info.IsDir() {
		t.Fatal("root is not a directory")
	}
	if perm := info.Mode().Perm(); perm != 0o700 {
		t.Fatalf("root permissions = %o, want 0700", perm)
	}
	other, err := newCaptureSpillDirectory(base, uuid.UUID{9})
	if err != nil {
		t.Fatalf("second session: %v", err)
	}
	defer other.Release()
	if other.root == dir.root {
		t.Fatal("two sessions share one spill root")
	}
}

// TestCaptureSpillDirectoryRejectsASymlinkedRoot covers the symlink refusal in
// both places a symlink can appear: as the base itself, and as an already
// existing session root. A symlinked root would let a spill be written outside
// the session's own directory, which is exactly what the checkpoint exclusion
// and the owner-only permissions are protecting.
func TestCaptureSpillDirectoryRejectsASymlinkedRoot(t *testing.T) {
	t.Parallel()
	real := t.TempDir()
	base := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(real, base); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	assertSpillRejection(t, "symlinked base", func() error {
		_, err := newCaptureSpillDirectory(base, uuid.UUID{1})
		return err
	}, "spill directory is a symlink")

	plain := t.TempDir()
	session := uuid.UUID{2}
	if err := os.Symlink(real, filepath.Join(plain, session.String())); err != nil {
		t.Fatalf("symlink session root: %v", err)
	}
	assertSpillRejection(t, "symlinked session root", func() error {
		_, err := newCaptureSpillDirectory(plain, session)
		return err
	}, "spill directory is a symlink")

	// A plain FILE where the root belongs is the neighbouring refusal, and it must
	// carry a DIFFERENT reason: if both arrangements reported the same cause the
	// symlink branch would have no reader and could be deleted unnoticed.
	notADirectory := t.TempDir()
	fileSession := uuid.UUID{3}
	if err := os.WriteFile(filepath.Join(notADirectory, fileSession.String()), []byte("x"), 0o600); err != nil {
		t.Fatalf("write blocking file: %v", err)
	}
	assertSpillRejection(t, "file where the root belongs", func() error {
		_, err := newCaptureSpillDirectory(notADirectory, fileSession)
		return err
	}, "spill directory is not a directory")
}

// assertSpillRejection requires a typed *captureSpillError carrying exactly the
// expected reason, which is what gives each refusal branch its own reader.
func assertSpillRejection(t *testing.T, name string, call func() error, reason string) {
	t.Helper()
	err := call()
	var spillErr *captureSpillError
	if !errors.As(err, &spillErr) {
		t.Errorf("%s: error = %v, want a *captureSpillError", name, err)
		return
	}
	if spillErr.Reason != reason {
		t.Errorf("%s: reason = %q, want %q", name, spillErr.Reason, reason)
	}
}

// TestCaptureSpillFileIsOwnerOnlyAndKeyedByExecutionID pins the per-spill file:
// owner-only, named by the ToolExecutionID, and created exclusively so an
// existing file — or a symlink planted at that name — is refused rather than
// followed.
func TestCaptureSpillFileIsOwnerOnlyAndKeyedByExecutionID(t *testing.T) {
	t.Parallel()
	dir, err := newCaptureSpillDirectory(t.TempDir(), uuid.UUID{3})
	if err != nil {
		t.Fatalf("newCaptureSpillDirectory: %v", err)
	}
	defer dir.Release()
	execution := uuid.UUID{4, 5, 6}
	sink, err := dir.openSink(execution, 16)
	if err != nil {
		t.Fatalf("openSink: %v", err)
	}
	path := filepath.Join(dir.root, execution.String()+captureSpillSuffix)
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("lstat spill: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("spill permissions = %o, want 0600", perm)
	}
	if _, err := dir.openSink(execution, 16); err == nil {
		t.Error("a second spill for the same execution id was accepted")
	}
	if err := sink.release(); err != nil {
		t.Fatalf("release: %v", err)
	}
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatalf("lstat after release = %v, want not-exist", err)
	}
	if err := os.Symlink(filepath.Join(t.TempDir(), "elsewhere"), path); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := dir.openSink(execution, 16); err == nil {
		t.Error("a symlink planted at the spill path was followed")
	}
}

// TestCaptureSpillNameCannotEscapeItsDirectory derives the space from the
// mechanism rather than picking one id: every ToolExecutionID is a uuid.UUID, so
// the guard must hold for the whole 16-byte space. The extremes plus a sweep of
// random values stand in for it, and each is checked as a PATH containment fact,
// not merely as a string shape.
func TestCaptureSpillNameCannotEscapeItsDirectory(t *testing.T) {
	t.Parallel()
	dir, err := newCaptureSpillDirectory(t.TempDir(), uuid.UUID{5})
	if err != nil {
		t.Fatalf("newCaptureSpillDirectory: %v", err)
	}
	defer dir.Release()
	ids := []uuid.UUID{{}, {}}
	for i := range ids[1] {
		ids[1][i] = 0xff
	}
	for i := 0; i < 32; i++ {
		id, err := uuid.New()
		if err != nil {
			t.Fatalf("uuid.New: %v", err)
		}
		ids = append(ids, id)
	}
	for _, id := range ids {
		path, err := dir.spillPath(id)
		if err != nil {
			t.Fatalf("spillPath(%v): %v", id, err)
		}
		if filepath.Dir(path) != dir.root {
			t.Errorf("spillPath(%v) = %q, which is not directly under %q", id, path, dir.root)
		}
		if filepath.Clean(path) != path {
			t.Errorf("spillPath(%v) = %q is not already clean", id, path)
		}
	}
}

// TestCaptureSpillDirectoryUsesNoProcessGlobalTempPath is a derived guard for
// step 1's "no process-global temp path". It reads the package's own production
// files rather than naming this file, so a temp-path helper introduced later —
// in any file of the package — is caught without editing the test.
func TestCaptureSpillDirectoryUsesNoProcessGlobalTempPath(t *testing.T) {
	t.Parallel()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package directory: %v", err)
	}
	banned := []string{"os.TempDir", "os.MkdirTemp", "os.CreateTemp", "ioutil.TempDir", "ioutil.TempFile"}
	scanned := 0
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		data, readErr := os.ReadFile(name)
		if readErr != nil {
			t.Fatalf("read %s: %v", name, readErr)
		}
		scanned++
		for _, call := range banned {
			if strings.Contains(string(data), call) {
				t.Errorf("%s calls %s; a capture spill must live under the session-scoped root, not a process-global temp path", name, call)
			}
		}
	}
	if scanned < 10 {
		t.Fatalf("scanned %d production files; the guard is vacuous", scanned)
	}
}

// TestSpillBackedSinkMatchesTheMemorySinkExactly holds the single-place claim in
// captureSink's doc comment to the layer that has the reader: the ceiling, the
// counts, the digest and the encoding must be identical whichever backing holds
// the retained prefix, or the streaming and materialized paths disagree about
// what "captured" means.
func TestSpillBackedSinkMatchesTheMemorySinkExactly(t *testing.T) {
	t.Parallel()
	dir, err := newCaptureSpillDirectory(t.TempDir(), uuid.UUID{6})
	if err != nil {
		t.Fatalf("newCaptureSpillDirectory: %v", err)
	}
	defer dir.Release()
	payloads := []string{"", "a", "hello world", "界界界", "\xff\xfe\x00", strings.Repeat("x", 300)}
	execution := 0
	for _, ceiling := range []int{1, 4, 7, 64, 4096} {
		for _, payload := range payloads {
			for _, chunk := range []int{1, 3, len(payload) + 1} {
				execution++
				id := uuid.UUID{byte(execution), byte(execution >> 8)}
				spill, openErr := dir.openSink(id, ceiling)
				if openErr != nil {
					t.Fatalf("openSink: %v", openErr)
				}
				memory := newCaptureSink(ceiling)
				for offset := 0; offset < len(payload); offset += chunk {
					end := min(offset+chunk, len(payload))
					if _, err := spill.Write([]byte(payload[offset:end])); err != nil {
						t.Fatalf("spill Write: %v", err)
					}
					if _, err := memory.Write([]byte(payload[offset:end])); err != nil {
						t.Fatalf("memory Write: %v", err)
					}
				}
				if spill.spillErr() != nil {
					t.Fatalf("spill error: %v", spill.spillErr())
				}
				if spill.capturedBytes() != memory.capturedBytes() || spill.offeredBytes() != memory.offeredBytes() {
					t.Errorf("ceiling=%d payload=%q chunk=%d: counts (%d,%d) want (%d,%d)",
						ceiling, payload, chunk, spill.capturedBytes(), spill.offeredBytes(), memory.capturedBytes(), memory.offeredBytes())
				}
				if spill.truncated() != memory.truncated() {
					t.Errorf("ceiling=%d payload=%q chunk=%d: truncated %v want %v", ceiling, payload, chunk, spill.truncated(), memory.truncated())
				}
				if spill.digestHex() != memory.digestHex() {
					t.Errorf("ceiling=%d payload=%q chunk=%d: digest %s want %s", ceiling, payload, chunk, spill.digestHex(), memory.digestHex())
				}
				if spill.encoding() != memory.encoding() {
					t.Errorf("ceiling=%d payload=%q chunk=%d: encoding %q want %q", ceiling, payload, chunk, spill.encoding(), memory.encoding())
				}
				materialized, matErr := spill.materialize()
				if matErr != nil {
					t.Fatalf("materialize: %v", matErr)
				}
				wantBytes, wantErr := memory.materialize()
				if wantErr != nil {
					t.Fatalf("memory materialize: %v", wantErr)
				}
				if string(materialized) != string(wantBytes) {
					t.Errorf("ceiling=%d payload=%q chunk=%d: materialized %q want %q", ceiling, payload, chunk, materialized, wantBytes)
				}
				reader, readerErr := spill.reader()
				if readerErr != nil {
					t.Fatalf("reader: %v", readerErr)
				}
				streamed, streamErr := io.ReadAll(reader)
				closeErr := reader.Close()
				if streamErr != nil || closeErr != nil {
					t.Fatalf("stream spill: %v / %v", streamErr, closeErr)
				}
				if string(streamed) != string(wantBytes) {
					t.Errorf("ceiling=%d payload=%q chunk=%d: streamed %q want %q", ceiling, payload, chunk, streamed, wantBytes)
				}
				if err := spill.release(); err != nil {
					t.Fatalf("release: %v", err)
				}
			}
		}
	}
}

// TestSpillWriteFailureLatchesAndStillLetsTheProducerFinish covers the short
// write and disk-full faults together, because to a writer they are the same
// event: the backing accepted fewer bytes than asked, or refused outright. The
// producer must still see a full, error-free Write — a tool told its output
// failed would report a tool error for a loop-side retention problem — while the
// sink latches the FIRST cause for the retention pipeline to fail on.
func TestSpillWriteFailureLatchesAndStillLetsTheProducerFinish(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		backing *faultyBacking
	}{
		{name: "short write", backing: &faultyBacking{shortAfter: 2}},
		{name: "disk full", backing: &faultyBacking{err: errFakeDiskFull}},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			sink := newCaptureSinkWithBacking(64, tt.backing)
			n, err := sink.Write([]byte("abcdefgh"))
			if n != 8 || err != nil {
				t.Fatalf("Write = (%d, %v), want (8, nil): a producer must be able to finish", n, err)
			}
			if _, err := sink.Write([]byte("ijkl")); err != nil {
				t.Fatalf("second Write = %v, want nil", err)
			}
			if sink.spillErr() == nil {
				t.Fatal("spillErr = nil, want the latched backing failure")
			}
			if sink.offeredBytes() != 12 {
				t.Fatalf("offeredBytes = %d, want 12: every producer byte is counted", sink.offeredBytes())
			}
			tt.backing.err = nil
			tt.backing.shortAfter = -1
			if sink.spillErr() == nil {
				t.Fatal("a later healthy write cleared the latched failure")
			}
		})
	}
}

// TestCaptureSpillDirectoryReleaseRemovesEveryLocalSpill pins the workspace/
// session release path: after Release nothing the session wrote is left on disk,
// and Release is idempotent so a shutdown that runs it twice is not an error.
func TestCaptureSpillDirectoryReleaseRemovesEveryLocalSpill(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	dir, err := newCaptureSpillDirectory(base, uuid.UUID{8})
	if err != nil {
		t.Fatalf("newCaptureSpillDirectory: %v", err)
	}
	for i := 1; i <= 3; i++ {
		sink, openErr := dir.openSink(uuid.UUID{byte(i)}, 64)
		if openErr != nil {
			t.Fatalf("openSink: %v", openErr)
		}
		if _, err := sink.Write([]byte("payload")); err != nil {
			t.Fatalf("Write: %v", err)
		}
	}
	if err := dir.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if _, err := os.Lstat(dir.root); !os.IsNotExist(err) {
		t.Fatalf("lstat root after Release = %v, want not-exist", err)
	}
	if err := dir.Release(); err != nil {
		t.Fatalf("second Release = %v, want nil", err)
	}
	if _, err := dir.openSink(uuid.UUID{9}, 64); err == nil {
		t.Error("a spill was opened after the session's spill root was released")
	}
	entries, err := os.ReadDir(base)
	if err != nil {
		t.Fatalf("read base: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("base still holds %d entries after Release", len(entries))
	}
}

// errFakeDiskFull stands in for ENOSPC.
var errFakeDiskFull = errors.New("no space left on device")

// faultyBacking fails the way a full or failing disk does: it either refuses the
// write outright or accepts a prefix. shortAfter < 0 disables the short write.
type faultyBacking struct {
	shortAfter int
	err        error
	written    int
	accepted   []byte
}

func (f *faultyBacking) write(p []byte) (int, error) {
	if f.err != nil {
		return 0, f.err
	}
	if f.shortAfter >= 0 && f.written+len(p) > f.shortAfter {
		room := max(f.shortAfter-f.written, 0)
		f.accepted = append(f.accepted, p[:room]...)
		f.written += room
		return room, io.ErrShortWrite
	}
	f.accepted = append(f.accepted, p...)
	f.written += len(p)
	return len(p), nil
}

// materialize and reader deliberately SUCCEED, returning exactly the bytes the
// backing accepted. A real partial disk failure leaves a readable prefix, and a
// fake that refused to read it back would hide the difference between "the
// pipeline noticed the latched failure" and "the pipeline failed later for
// another reason".
func (f *faultyBacking) materialize() ([]byte, error) { return f.accepted, nil }

func (f *faultyBacking) reader() (io.ReadCloser, error) {
	return io.NopCloser(bytes.NewReader(f.accepted)), nil
}

func (f *faultyBacking) release() error { return nil }

// TestToolResultStoreTypesAreThePublicOnes is the mechanical form of "the store
// seam is usable from outside the module". A DEFINED type here with the same
// shape as pkg/loop's would satisfy every assignment an interface test could
// make, so the property is checked as type IDENTITY: reflect reports one type,
// which is only true of an alias.
func TestToolResultStoreTypesAreThePublicOnes(t *testing.T) {
	t.Parallel()
	pairs := []struct {
		name     string
		internal reflect.Type
		public   reflect.Type
	}{
		{"stat", reflect.TypeOf(ToolResultObjectStat{}), reflect.TypeOf(loop.ToolResultObjectStat{})},
		{"store", reflect.TypeOf((*ToolResultObjectStore)(nil)).Elem(), reflect.TypeOf((*loop.ToolResultObjectStore)(nil)).Elem()},
		{"stream store", reflect.TypeOf((*ToolResultObjectStreamStore)(nil)).Elem(), reflect.TypeOf((*loop.ToolResultObjectStreamStore)(nil)).Elem()},
	}
	for _, pair := range pairs {
		if pair.internal != pair.public {
			t.Errorf("%s: internal type %v is not the public type %v; a consumer outside the module could not name it",
				pair.name, pair.internal, pair.public)
		}
		if got := pair.public.PkgPath(); strings.Contains(got, "/internal/") {
			t.Errorf("%s: public type lives at %s, which no consumer outside the module may import", pair.name, got)
		}
	}
}
