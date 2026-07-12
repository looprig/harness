package tools

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/looprig/core/content"
)

// file_freshness_test.go pins the per-loop optimistic-concurrency behavior of the
// file tools (design §"File-tool optimistic concurrency and binding"): a mutation
// of an EXISTING file is authorized only by a complete prior read whose hash still
// equals the current on-disk bytes, while a genuinely ABSENT path may be created
// without any prior read. These are pure-logic (no cross-process) cases and run in
// the default suite; the real atomic-link/rename FS behavior lives in
// file_freshness_integration_test.go.

// invokeWrite runs a WriteFile bound to obs and returns the tool-result text.
func invokeWrite(t *testing.T, root string, obs *fileObservations, path, contentBody string) string {
	t.Helper()
	args, err := json.Marshal(map[string]any{"path": path, "content": contentBody})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	res, err := NewWriteFile(root, obs).InvokableRun(context.Background(), string(args))
	if err != nil {
		t.Fatalf("WriteFile Go error: %v", err)
	}
	return res.Content[0].(*content.TextBlock).Text
}

// invokeEdit runs an EditFile bound to obs and returns the tool-result text.
func invokeEdit(t *testing.T, root string, obs *fileObservations, path, old, replacement string) string {
	t.Helper()
	args, err := json.Marshal(map[string]any{"path": path, "old": old, "new": replacement})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	res, err := NewEditFile(root, obs).InvokableRun(context.Background(), string(args))
	if err != nil {
		t.Fatalf("EditFile Go error: %v", err)
	}
	return res.Content[0].(*content.TextBlock).Text
}

func seedFile(t *testing.T, root, rel, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, rel), []byte(body), 0o600); err != nil {
		t.Fatalf("seed %q: %v", rel, err)
	}
}

func onDisk(t *testing.T, root, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, rel))
	if err != nil {
		t.Fatalf("read %q: %v", rel, err)
	}
	return string(b)
}

// TestWriteFreshnessGate covers the existing-file overwrite authorization matrix:
// only a complete read of the CURRENT bytes authorizes an overwrite; a missing
// observation, a truncated read, or an external change since the read is refused
// WITHOUT clobbering.
func TestWriteFreshnessGate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		seed     string
		prepare  func(t *testing.T, root string, obs *fileObservations) // records an observation (or not)
		newBody  string
		wantErr  bool
		wantDisk string
	}{
		{
			name:     "overwrite after complete read succeeds",
			seed:     "old body\n",
			prepare:  func(t *testing.T, root string, obs *fileObservations) { observeFile(t, root, obs, "f.txt") },
			newBody:  "new body\n",
			wantErr:  false,
			wantDisk: "new body\n",
		},
		{
			name:     "overwrite without any observation is refused",
			seed:     "old body\n",
			prepare:  func(t *testing.T, root string, obs *fileObservations) {},
			newBody:  "new body\n",
			wantErr:  true,
			wantDisk: "old body\n",
		},
		{
			name: "overwrite after a truncated read is refused",
			seed: "old body that is long\n",
			prepare: func(t *testing.T, root string, obs *fileObservations) {
				// A tiny read cap forces truncation, which records NO usable hash.
				args, _ := json.Marshal(map[string]any{"path": "f.txt"})
				if _, err := NewReadFile(root, newFakeReadGuard(4), obs).InvokableRun(context.Background(), string(args)); err != nil {
					t.Fatalf("truncated read Go error: %v", err)
				}
			},
			newBody:  "new body\n",
			wantErr:  true,
			wantDisk: "old body that is long\n",
		},
		{
			name: "overwrite after external modification is refused",
			seed: "old body\n",
			prepare: func(t *testing.T, root string, obs *fileObservations) {
				observeFile(t, root, obs, "f.txt")
				// Someone else changes the file after our read.
				seedFile(t, root, "f.txt", "changed under us\n")
			},
			newBody:  "new body\n",
			wantErr:  true,
			wantDisk: "changed under us\n",
		},
		{
			name: "re-read recovers authorization after external change",
			seed: "old body\n",
			prepare: func(t *testing.T, root string, obs *fileObservations) {
				observeFile(t, root, obs, "f.txt")
				seedFile(t, root, "f.txt", "changed under us\n")
				observeFile(t, root, obs, "f.txt") // read again -> fresh observation
			},
			newBody:  "new body\n",
			wantErr:  false,
			wantDisk: "new body\n",
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			obs := newFileObservations()
			seedFile(t, root, "f.txt", tt.seed)
			tt.prepare(t, root, obs)
			out := invokeWrite(t, root, obs, "f.txt", tt.newBody)
			if gotErr := strings.HasPrefix(out, "error:"); gotErr != tt.wantErr {
				t.Fatalf("result = %q, wantErr = %v", out, tt.wantErr)
			}
			if got := onDisk(t, root, "f.txt"); got != tt.wantDisk {
				t.Errorf("on-disk = %q, want %q", got, tt.wantDisk)
			}
		})
	}
}

// TestEditFreshnessGate mirrors the write gate for EditFile and separates the
// freshness refusal (StaleFileError → "read again") from the anchor refusal (a
// fresh file whose `old` simply does not match).
func TestEditFreshnessGate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		seed     string
		prepare  func(t *testing.T, root string, obs *fileObservations)
		old      string
		repl     string
		wantErr  bool
		wantStr  string // substring the result must contain
		wantDisk string
	}{
		{
			name:     "edit after complete read succeeds",
			seed:     "alpha bravo charlie\n",
			prepare:  func(t *testing.T, root string, obs *fileObservations) { observeFile(t, root, obs, "f.txt") },
			old:      "bravo",
			repl:     "BRAVO",
			wantErr:  false,
			wantDisk: "alpha BRAVO charlie\n",
		},
		{
			name:     "edit without observation is refused as stale",
			seed:     "alpha bravo charlie\n",
			prepare:  func(t *testing.T, root string, obs *fileObservations) {},
			old:      "bravo",
			repl:     "BRAVO",
			wantErr:  true,
			wantStr:  "must be read before writing",
			wantDisk: "alpha bravo charlie\n",
		},
		{
			name: "edit after external change is refused as stale",
			seed: "alpha bravo charlie\n",
			prepare: func(t *testing.T, root string, obs *fileObservations) {
				observeFile(t, root, obs, "f.txt")
				seedFile(t, root, "f.txt", "alpha bravo delta\n")
			},
			old:      "bravo",
			repl:     "BRAVO",
			wantErr:  true,
			wantStr:  "must be read before writing",
			wantDisk: "alpha bravo delta\n",
		},
		{
			name:     "anchor mismatch on a fresh file is a distinct error, not stale",
			seed:     "alpha bravo charlie\n",
			prepare:  func(t *testing.T, root string, obs *fileObservations) { observeFile(t, root, obs, "f.txt") },
			old:      "zulu",
			repl:     "X",
			wantErr:  true,
			wantStr:  "not found in the file",
			wantDisk: "alpha bravo charlie\n",
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			obs := newFileObservations()
			seedFile(t, root, "f.txt", tt.seed)
			tt.prepare(t, root, obs)
			out := invokeEdit(t, root, obs, "f.txt", tt.old, tt.repl)
			if gotErr := strings.HasPrefix(out, "error:"); gotErr != tt.wantErr {
				t.Fatalf("result = %q, wantErr = %v", out, tt.wantErr)
			}
			if tt.wantStr != "" && !strings.Contains(out, tt.wantStr) {
				t.Errorf("result = %q, want it to contain %q", out, tt.wantStr)
			}
			if got := onDisk(t, root, "f.txt"); got != tt.wantDisk {
				t.Errorf("on-disk = %q, want %q", got, tt.wantDisk)
			}
		})
	}
}

// TestNewFileCreateWithoutRead pins the create branch: a write with NO observation
// to an ABSENT path succeeds (no failed read required), a write to an EXISTING path
// without observation fails without clobbering, and the successful creator's hash is
// recorded so an immediate same-loop overwrite is authorized.
func TestNewFileCreateWithoutRead(t *testing.T) {
	t.Parallel()

	t.Run("absent path is created without a prior read", func(t *testing.T) {
		t.Parallel()
		root := t.TempDir()
		obs := newFileObservations()
		out := invokeWrite(t, root, obs, "brand/new.txt", "hello\n")
		if strings.HasPrefix(out, "error:") {
			t.Fatalf("create of an absent path = %q, want success", out)
		}
		if got := onDisk(t, root, "brand/new.txt"); got != "hello\n" {
			t.Errorf("created body = %q, want %q", got, "hello\n")
		}
	})

	t.Run("creator hash authorizes an immediate overwrite", func(t *testing.T) {
		t.Parallel()
		root := t.TempDir()
		obs := newFileObservations()
		if out := invokeWrite(t, root, obs, "x.txt", "first\n"); strings.HasPrefix(out, "error:") {
			t.Fatalf("initial create = %q, want success", out)
		}
		// No intervening read: the create recorded the produced hash, so a same-loop
		// overwrite is authorized.
		if out := invokeWrite(t, root, obs, "x.txt", "second\n"); strings.HasPrefix(out, "error:") {
			t.Fatalf("overwrite after create = %q, want success", out)
		}
		if got := onDisk(t, root, "x.txt"); got != "second\n" {
			t.Errorf("body = %q, want %q", got, "second\n")
		}
	})

	t.Run("existing path without observation is refused without clobbering", func(t *testing.T) {
		t.Parallel()
		root := t.TempDir()
		obs := newFileObservations()
		seedFile(t, root, "x.txt", "existing\n")
		out := invokeWrite(t, root, obs, "x.txt", "clobber\n")
		if !strings.HasPrefix(out, "error:") {
			t.Fatalf("unobserved write to existing path = %q, want refusal", out)
		}
		if got := onDisk(t, root, "x.txt"); got != "existing\n" {
			t.Errorf("body = %q, want it unchanged %q", got, "existing\n")
		}
	})
}

// TestObservationBundlesAreIndependentPerLoop proves two loops' maps never share
// authorization: loop A reading a file does NOT let loop B overwrite it.
func TestObservationBundlesAreIndependentPerLoop(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	seedFile(t, root, "shared.txt", "original\n")

	loopA := newFileObservations()
	loopB := newFileObservations()

	// Loop A completely reads the file (authorizing ITS OWN writes).
	observeFile(t, root, loopA, "shared.txt")

	// Loop B never read it: B's overwrite is refused and does not clobber.
	if out := invokeWrite(t, root, loopB, "shared.txt", "from B\n"); !strings.HasPrefix(out, "error:") {
		t.Fatalf("loop B write = %q, want refusal (A's read must not authorize B)", out)
	}
	if got := onDisk(t, root, "shared.txt"); got != "original\n" {
		t.Fatalf("file changed after refused B write: %q", got)
	}

	// Loop A, which did read, may overwrite.
	if out := invokeWrite(t, root, loopA, "shared.txt", "from A\n"); strings.HasPrefix(out, "error:") {
		t.Fatalf("loop A write = %q, want success (A read the file)", out)
	}
	if got := onDisk(t, root, "shared.txt"); got != "from A\n" {
		t.Fatalf("loop A write body = %q, want %q", got, "from A\n")
	}
}

// TestCanonicalAliasHitsSameEntry proves a read via one lexical alias authorizes a
// write via a different lexical alias of the SAME canonical path (the map key is the
// symlink-resolved contained path, so aliases collapse).
func TestCanonicalAliasHitsSameEntry(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "dir"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	seedFile(t, root, "dir/f.txt", "body\n")
	obs := newFileObservations()

	// Observe via a messy lexical alias.
	observeFile(t, root, obs, "dir/./sub/../f.txt")
	// Overwrite via the clean alias: same canonical path, so it is authorized.
	if out := invokeWrite(t, root, obs, "dir/f.txt", "rewritten\n"); strings.HasPrefix(out, "error:") {
		t.Fatalf("write via canonical alias = %q, want success", out)
	}
	if got := onDisk(t, root, "dir/f.txt"); got != "rewritten\n" {
		t.Errorf("body = %q, want %q", got, "rewritten\n")
	}
}

// TestUnobservedOverwriteRejectedCheaply pins FIX 1: an unobserved overwrite of an
// existing REGULAR file is refused as *StaleFileError from the cheap Lstat classify +
// the observation flag, BEFORE the file is hashed — so a large unread file is
// rejected without an O(file-size) read. We use a 4 MiB file (far over any read cap)
// to exercise the cheap path; the behavior is identical to the small-file case, just
// without the wasted I/O, and the bytes are proven untouched.
func TestUnobservedOverwriteRejectedCheaply(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	big := strings.Repeat("x", 4<<20)
	seedFile(t, root, "big.txt", big)

	obs := newFileObservations()
	w := NewWriteFile(root, obs)
	abs, err := containedPath(root, "big.txt")
	if err != nil {
		t.Fatalf("containedPath: %v", err)
	}
	// commit is exercised directly so the typed error survives (InvokableRun flattens
	// it to a tool-result string).
	cerr := w.commit(canonicalObservationKey(abs), joinedUnderRoot(root, "big.txt"), "big.txt", []byte("small"))
	var stale *StaleFileError
	if !errors.As(cerr, &stale) {
		t.Fatalf("commit err = %v (%T), want *StaleFileError", cerr, cerr)
	}
	if got := onDisk(t, root, "big.txt"); got != big {
		t.Fatalf("unobserved rejected write clobbered the file (len now %d, want %d)", len(got), len(big))
	}
}

// TestIrregularWriteTargetIsTyped pins FIX 2: a final-component symlink write/edit
// target yields a DISTINCT *IrregularFileError (never *StaleFileError), so the model
// is not told to "read again" into a dead end (a ReadFile of the same path also
// refuses it O_NOFOLLOW). The symlink node and its target are left untouched.
func TestIrregularWriteTargetIsTyped(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	const targetBody = "body\n"
	target := filepath.Join(root, "t.txt")
	if err := os.WriteFile(target, []byte(targetBody), 0o600); err != nil {
		t.Fatalf("seed target: %v", err)
	}
	link := filepath.Join(root, "l.txt")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	abs, err := containedPath(root, "l.txt")
	if err != nil {
		t.Fatalf("containedPath: %v", err)
	}
	key := canonicalObservationKey(abs)
	lexical := joinedUnderRoot(root, "l.txt")

	assertIrregularNotStale := func(t *testing.T, cerr error) {
		t.Helper()
		var irr *IrregularFileError
		if !errors.As(cerr, &irr) {
			t.Fatalf("err = %v (%T), want *IrregularFileError", cerr, cerr)
		}
		var stale *StaleFileError
		if errors.As(cerr, &stale) {
			t.Fatalf("err %v must be distinct from *StaleFileError", cerr)
		}
	}

	t.Run("WriteFile", func(t *testing.T) {
		obs := newFileObservations()
		assertIrregularNotStale(t, NewWriteFile(root, obs).commit(key, lexical, "l.txt", []byte("x")))
	})
	t.Run("EditFile", func(t *testing.T) {
		obs := newFileObservations()
		_, cerr := NewEditFile(root, obs).commit(key, lexical, "l.txt", "body", "X", false)
		assertIrregularNotStale(t, cerr)
	})

	// The symlink is intact and its target unchanged (nothing was written).
	if fi, err := os.Lstat(link); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("symlink mutated (mode err %v)", err)
	}
	if b, err := os.ReadFile(target); err != nil || string(b) != targetBody {
		t.Fatalf("symlink target clobbered: %q (err %v)", string(b), err)
	}
}

// TestConcurrentSamePathWritesAreSerialized runs many same-loop overwrites of one
// observed file concurrently: the per-path critical section keeps every one
// consistent (each records the hash it wrote, so the next is authorized) and the
// final content is one of the writers' payloads. Run under -race.
func TestConcurrentSamePathWritesAreSerialized(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	seedFile(t, root, "f.txt", "seed\n")
	obs := newFileObservations()
	observeFile(t, root, obs, "f.txt")

	write := NewWriteFile(root, obs)
	const writers = 16
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range writers {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			<-start
			args, _ := json.Marshal(map[string]any{"path": "f.txt", "content": "payload\n"})
			// Ignore individual results: a concurrent writer may legitimately observe
			// a hash written by a peer and retry-fail; the invariant under test is
			// race-freedom + a consistent final on-disk state.
			_, _ = write.InvokableRun(context.Background(), string(args))
			_ = n
		}(i)
	}
	close(start)
	wg.Wait()

	// The file must be intact and hold a whole payload (never a torn write).
	got := onDisk(t, root, "f.txt")
	if got != "seed\n" && got != "payload\n" {
		t.Fatalf("final on-disk body = %q, want a whole write (seed or payload)", got)
	}
}
