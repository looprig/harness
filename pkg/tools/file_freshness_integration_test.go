//go:build integration

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

// file_freshness_integration_test.go exercises the REAL filesystem publication
// primitives behind the file tools' optimistic concurrency: the atomic no-replace
// CREATE (os.Link) that lets a genuinely-new file be written without a prior read,
// and its concurrent-creator conflict path (exactly one winner). It also proves the
// hash-gated overwrite uses a temp+rename that leaves no litter. Tagged
// `integration` (run: `go test -tags integration -race ./pkg/tools`).

func itWrite(t *testing.T, root string, obs *fileObservations, path, body string) string {
	t.Helper()
	args, err := json.Marshal(map[string]any{"path": path, "content": body})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	res, err := NewWriteFile(root, obs).InvokableRun(context.Background(), string(args))
	if err != nil {
		t.Fatalf("WriteFile Go error: %v", err)
	}
	return res.Content[0].(*content.TextBlock).Text
}

// TestFSCreateWithoutReadAtomicLink proves a WriteFile with NO observation creates a
// currently-absent file via the atomic no-replace link publication, at the owner-
// only mode, leaving no temp litter — and that the created inode is a single regular
// file (the temp link was removed).
func TestFSCreateWithoutReadAtomicLink(t *testing.T) {
	root := fsWorkspace(t)
	obs := newFileObservations()

	rel := "created/without/read.txt"
	if out := itWrite(t, root, obs, rel, "genuinely new\n"); !strings.HasPrefix(out, "wrote ") {
		t.Fatalf("no-read create = %q, want success", out)
	}
	abs := filepath.Join(root, rel)
	body, err := os.ReadFile(abs)
	if err != nil {
		t.Fatalf("read created file: %v", err)
	}
	if string(body) != "genuinely new\n" {
		t.Errorf("created body = %q, want %q", string(body), "genuinely new\n")
	}
	fi, err := os.Lstat(abs)
	if err != nil {
		t.Fatalf("lstat created file: %v", err)
	}
	if !fi.Mode().IsRegular() {
		t.Errorf("created node mode = %v, want a regular file", fi.Mode())
	}
	if perm := fi.Mode().Perm(); perm != newFilePerm {
		t.Errorf("created file perm = %o, want %o", perm, newFilePerm)
	}
	assertNoTempLitter(t, filepath.Dir(abs))
}

// TestFSCreateConflictWithoutClobber proves the no-replace publication REFUSES to
// overwrite an existing file when there is no observation: the create link hits the
// existing inode, fails, and the original bytes are intact.
func TestFSCreateConflictWithoutClobber(t *testing.T) {
	root := fsWorkspace(t)
	obs := newFileObservations()

	rel := "keep.txt"
	fsWrite(t, root, rel, "original bytes\n")
	out := itWrite(t, root, obs, rel, "should not land\n")
	if !strings.HasPrefix(out, "error:") {
		t.Fatalf("unobserved write to existing file = %q, want refusal", out)
	}
	body, err := os.ReadFile(filepath.Join(root, rel))
	if err != nil {
		t.Fatalf("read file: %v", err)
	}
	if string(body) != "original bytes\n" {
		t.Errorf("file was clobbered: %q, want %q", string(body), "original bytes\n")
	}
	assertNoTempLitter(t, root)
}

// TestFSConcurrentCreatorsExactlyOneWinner runs many DISTINCT loops (separate
// observation maps, so their per-path mutexes are independent) all creating the SAME
// absent path at once. The atomic os.Link is the cross-loop arbiter: exactly one
// succeeds; every other is TYPED-REFUSED without clobbering. A loser is refused via
// one of two non-clobbering paths depending on timing — it either lost the os.Link
// race (FileCreateConflictError) or observed the winner's file already present with
// no observation of its own (StaleFileError) — and the spec authorizes both ("if the
// path exists OR another writer wins, it fails typed without clobbering"). The
// deterministic link-EEXIST path itself is pinned by TestFSCreateConflictErrorIsTyped.
// Run under -race.
func TestFSConcurrentCreatorsExactlyOneWinner(t *testing.T) {
	root := fsWorkspace(t)
	const rel = "race/target.txt"
	const creators = 24

	var (
		wg      sync.WaitGroup
		start   = make(chan struct{})
		mu      sync.Mutex
		wins    int
		refused int
		other   []string
		bodies  = make([]string, creators)
	)
	for i := range creators {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			body := "winner-" + string(rune('A'+n)) + "\n"
			bodies[n] = body
			obs := newFileObservations() // a fresh loop per creator
			args, _ := json.Marshal(map[string]any{"path": rel, "content": body})
			<-start
			res, err := NewWriteFile(root, obs).InvokableRun(context.Background(), string(args))
			if err != nil {
				mu.Lock()
				other = append(other, "go error: "+err.Error())
				mu.Unlock()
				return
			}
			out := res.Content[0].(*content.TextBlock).Text
			mu.Lock()
			switch {
			case strings.HasPrefix(out, "wrote "):
				wins++
			case strings.Contains(out, "already exists") || strings.Contains(out, "must be read before writing"):
				refused++ // a typed, non-clobbering refusal (conflict or stale)
			default:
				other = append(other, out)
			}
			mu.Unlock()
		}(i)
	}
	close(start)
	wg.Wait()

	if len(other) != 0 {
		t.Fatalf("unexpected non-win/non-refusal results: %v", other)
	}
	if wins != 1 {
		t.Fatalf("winners = %d, want exactly 1", wins)
	}
	if refused != creators-1 {
		t.Fatalf("typed refusals = %d, want %d", refused, creators-1)
	}

	// The file exists exactly once and holds one creator's whole payload.
	got, err := os.ReadFile(filepath.Join(root, rel))
	if err != nil {
		t.Fatalf("read race target: %v", err)
	}
	matched := false
	for _, b := range bodies {
		if string(got) == b {
			matched = true
			break
		}
	}
	if !matched {
		t.Fatalf("race target body = %q, want exactly one creator's whole payload", string(got))
	}
	assertNoTempLitter(t, filepath.Dir(filepath.Join(root, rel)))
}

// TestFSCreateConflictErrorIsTyped confirms the direct create helper surfaces the
// errCreateConflict leaf on an existing destination, so WriteFile.commit can map it
// to a typed *FileCreateConflictError.
func TestFSCreateConflictErrorIsTyped(t *testing.T) {
	root := fsWorkspace(t)
	target := filepath.Join(root, "exists.txt")
	if err := os.WriteFile(target, []byte("here\n"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	err := atomicCreateFile(target, []byte("new\n"))
	if !errors.Is(err, errCreateConflict) {
		t.Fatalf("atomicCreateFile over existing = %v, want errCreateConflict", err)
	}
	// The original bytes are intact and no temp remains.
	body, rerr := os.ReadFile(target)
	if rerr != nil || string(body) != "here\n" {
		t.Fatalf("target after conflict = %q (err %v), want %q intact", string(body), rerr, "here\n")
	}
	assertNoTempLitter(t, root)
}
