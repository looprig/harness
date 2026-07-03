package workspacestore

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/looprig/storekit"
	"github.com/looprig/storekit/memstore"
)

// countingGetBlobs wraps a storekit.Blobs and counts Get invocations so a test
// can prove the verified-reuse path materializes a warm volume without any
// network fetch. Put/List/Delete are promoted from the embedded interface.
type countingGetBlobs struct {
	storekit.Blobs
	gets atomic.Int64
}

func (c *countingGetBlobs) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	c.gets.Add(1)
	return c.Blobs.Get(ctx, key)
}

// assertTreesEqual fails unless the trees rooted at want and got are logically
// identical (names, contents, permission modes, symlink targets), the property
// content-addressing relies on. It compares by value via snapshotTree so the
// two trees may live at different paths on disk.
func assertTreesEqual(t *testing.T, want, got string) {
	t.Helper()
	w := snapshotTree(t, want)
	g := snapshotTree(t, got)
	if len(w) != len(g) {
		t.Fatalf("node count mismatch: got %d, want %d\n want=%v\n got=%v", len(g), len(w), w, g)
	}
	for name, wn := range w {
		gn, ok := g[name]
		if !ok {
			t.Errorf("materialized tree missing node %q", name)
			continue
		}
		if gn != wn {
			t.Errorf("node %q mismatch:\n got  %+v\n want %+v", name, gn, wn)
		}
	}
}

func TestMaterializeRoundTrip(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		nodes []node
		dest  func(t *testing.T) string
	}{
		{
			name:  "empty existing dest extracts canonical tree",
			nodes: canonicalNodes(),
			dest:  func(t *testing.T) string { return t.TempDir() },
		},
		{
			name:  "missing dest is created then extracted",
			nodes: canonicalNodes(),
			dest:  func(t *testing.T) string { return filepath.Join(t.TempDir(), "restore", "here") },
		},
		{
			name:  "single executable file round-trips 0755 mode",
			nodes: []node{{path: "run.sh", kind: kindFile, content: "#!/bin/sh\n", mode: 0o755}},
			dest:  func(t *testing.T) string { return t.TempDir() },
		},
		{
			name: "nested dir with symlink round-trips verbatim",
			nodes: []node{
				{path: "a", kind: kindDir},
				{path: "a/f.txt", kind: kindFile, content: "hi\n", mode: 0o600},
				{path: "a/link", kind: kindSymlink, target: "f.txt"},
			},
			dest: func(t *testing.T) string { return t.TempDir() },
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			src := t.TempDir()
			buildTree(t, src, tt.nodes)

			s, err := Open(memstore.New().Blobs)
			if err != nil {
				t.Fatalf("Open(): %v", err)
			}
			ref, err := s.Snapshot(context.Background(), src)
			if err != nil {
				t.Fatalf("Snapshot(): %v", err)
			}

			dest := tt.dest(t)
			if err := s.Materialize(context.Background(), ref, dest); err != nil {
				t.Fatalf("Materialize(): %v", err)
			}
			assertTreesEqual(t, src, dest)
		})
	}
}

func TestMaterializeVerifiedReuseSkipsFetch(t *testing.T) {
	t.Parallel()

	src := t.TempDir()
	buildTree(t, src, canonicalNodes())

	cb := &countingGetBlobs{Blobs: memstore.New().Blobs}
	s, err := Open(cb)
	if err != nil {
		t.Fatalf("Open(): %v", err)
	}
	ref, err := s.Snapshot(context.Background(), src)
	if err != nil {
		t.Fatalf("Snapshot(): %v", err)
	}

	// First Materialize fetches and extracts (truth path).
	dest := t.TempDir()
	if err := s.Materialize(context.Background(), ref, dest); err != nil {
		t.Fatalf("first Materialize (truth path): %v", err)
	}
	getsAfterFirst := cb.gets.Load()
	if getsAfterFirst == 0 {
		t.Fatal("truth-path Materialize never called Get; want a fetch")
	}

	// Second Materialize into the now-populated dest must prove reuse by content
	// alone: no fetch, no mutation.
	if err := s.Materialize(context.Background(), ref, dest); err != nil {
		t.Fatalf("second Materialize (reuse path): %v", err)
	}
	if got := cb.gets.Load(); got != getsAfterFirst {
		t.Errorf("reuse path called Get: count %d, want unchanged %d", got, getsAfterFirst)
	}
	assertTreesEqual(t, src, dest)
}

func TestMaterializeDestDriftReturnsDestNotEmpty(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		mutate func(t *testing.T, dest string)
	}{
		{
			name: "one byte changed in a file",
			mutate: func(t *testing.T, dest string) {
				p := filepath.Join(dest, "readme.txt")
				if err := os.WriteFile(p, []byte("HELLO world\n"), 0o644); err != nil {
					t.Fatalf("overwrite readme.txt: %v", err)
				}
			},
		},
		{
			name: "extra file added",
			mutate: func(t *testing.T, dest string) {
				p := filepath.Join(dest, "extra.txt")
				if err := os.WriteFile(p, []byte("surprise\n"), 0o644); err != nil {
					t.Fatalf("add extra.txt: %v", err)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			src := t.TempDir()
			buildTree(t, src, canonicalNodes())

			cb := &countingGetBlobs{Blobs: memstore.New().Blobs}
			s, err := Open(cb)
			if err != nil {
				t.Fatalf("Open(): %v", err)
			}
			ref, err := s.Snapshot(context.Background(), src)
			if err != nil {
				t.Fatalf("Snapshot(): %v", err)
			}

			dest := t.TempDir()
			if err := s.Materialize(context.Background(), ref, dest); err != nil {
				t.Fatalf("seed Materialize: %v", err)
			}
			tt.mutate(t, dest)

			before := snapshotTree(t, dest)
			getsBefore := cb.gets.Load()

			err = s.Materialize(context.Background(), ref, dest)
			var dne *DestNotEmptyError
			if !errors.As(err, &dne) {
				t.Fatalf("Materialize on drifted dest = %v (%T), want *DestNotEmptyError", err, err)
			}
			if dne.Want != ref {
				t.Errorf("DestNotEmptyError.Want = %q, want %q", dne.Want, ref)
			}
			if dne.GotDigest == "" || dne.GotDigest == ref.hex() {
				t.Errorf("DestNotEmptyError.GotDigest = %q, want a non-empty digest != ref", dne.GotDigest)
			}
			// A drift is a distinct, caller-actionable outcome, never wrapped as a
			// *MaterializeError.
			var me *MaterializeError
			if errors.As(err, &me) {
				t.Errorf("drift wrongly wrapped in *MaterializeError: %v", err)
			}
			// The reuse path must never fetch...
			if got := cb.gets.Load(); got != getsBefore {
				t.Errorf("drift path called Get: count %d, want unchanged %d", got, getsBefore)
			}
			// ...and must never mutate the destination.
			after := snapshotTree(t, dest)
			if !reflect.DeepEqual(before, after) {
				t.Errorf("drift path modified dest (Materialize must never wipe):\n before %v\n after %v", before, after)
			}
		})
	}
}

func TestMaterializeAbsentBlob(t *testing.T) {
	t.Parallel()

	s, err := Open(memstore.New().Blobs)
	if err != nil {
		t.Fatalf("Open(): %v", err)
	}
	// A grammar-valid Ref that was never stored.
	ref, err := ParseRef(refPrefix + strings.Repeat("a", refHexLen))
	if err != nil {
		t.Fatalf("ParseRef(): %v", err)
	}

	dest := t.TempDir()
	err = s.Materialize(context.Background(), ref, dest)

	var me *MaterializeError
	if !errors.As(err, &me) {
		t.Fatalf("Materialize of absent blob = %v (%T), want *MaterializeError", err, err)
	}
	var bnfe *storekit.BlobNotFoundError
	if !errors.As(err, &bnfe) {
		t.Fatalf("Materialize error does not unwrap to *storekit.BlobNotFoundError: %v", err)
	}
	if bnfe.Key != ref.blobKey() {
		t.Errorf("BlobNotFoundError.Key = %q, want %q", bnfe.Key, ref.blobKey())
	}
	assertDestEmpty(t, dest)
}

func TestMaterializeTamperedBlob(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		tamper        func(t *testing.T) []byte // bytes to store under the ref's blob key
		wantIntegrity bool
	}{
		{
			name:          "garbage bytes fail to decode",
			tamper:        func(t *testing.T) []byte { return []byte("this is not a gzip stream and never will be") },
			wantIntegrity: false, // caught as a decode error inside extractArchive
		},
		{
			name: "valid archive of a different tree mismatches digest",
			tamper: func(t *testing.T) []byte {
				other := t.TempDir()
				buildTree(t, other, []node{{path: "different.txt", kind: kindFile, content: "not the original tree\n", mode: 0o644}})
				return archiveBytes(t, other)
			},
			wantIntegrity: true, // decodes cleanly, but its digest is not the ref's
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			src := t.TempDir()
			buildTree(t, src, canonicalNodes())

			blobs := memstore.New().Blobs
			s, err := Open(blobs)
			if err != nil {
				t.Fatalf("Open(): %v", err)
			}
			ref, err := s.Snapshot(context.Background(), src)
			if err != nil {
				t.Fatalf("Snapshot(): %v", err)
			}

			// Overwrite the stored archive: Delete frees the content-addressed key
			// so a differing Put is accepted.
			payload := tt.tamper(t)
			if err := blobs.Delete(context.Background(), ref.blobKey()); err != nil {
				t.Fatalf("Delete for tamper: %v", err)
			}
			if err := blobs.Put(context.Background(), ref.blobKey(), bytes.NewReader(payload)); err != nil {
				t.Fatalf("Put tampered bytes: %v", err)
			}

			dest := t.TempDir()
			err = s.Materialize(context.Background(), ref, dest)

			var me *MaterializeError
			if !errors.As(err, &me) {
				t.Fatalf("Materialize of tampered blob = %v (%T), want *MaterializeError", err, err)
			}
			var ie *IntegrityError
			gotIntegrity := errors.As(err, &ie)
			if gotIntegrity != tt.wantIntegrity {
				t.Fatalf("IntegrityError present = %v, want %v (err=%v)", gotIntegrity, tt.wantIntegrity, err)
			}
			if tt.wantIntegrity {
				if ie.Ref != ref {
					t.Errorf("IntegrityError.Ref = %q, want %q", ie.Ref, ref)
				}
				if ie.Got == "" || ie.Got == ref.hex() {
					t.Errorf("IntegrityError.Got = %q, want a computed digest != ref", ie.Got)
				}
			}
			// Fail closed: whether the blob broke gzip or merely mismatched, nothing
			// may be left behind in dest.
			assertDestEmpty(t, dest)
		})
	}
}

func TestMaterializeDelete(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		seed    bool // store the blob (via Snapshot) before deleting
		deletes int
	}{
		{name: "present blob is removed", seed: true, deletes: 1},
		{name: "absent blob delete is a no-op", seed: false, deletes: 1},
		{name: "repeated delete stays idempotent", seed: true, deletes: 3},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			blobs := memstore.New().Blobs
			s, err := Open(blobs)
			if err != nil {
				t.Fatalf("Open(): %v", err)
			}

			var ref Ref
			if tt.seed {
				src := t.TempDir()
				buildTree(t, src, canonicalNodes())
				ref, err = s.Snapshot(context.Background(), src)
				if err != nil {
					t.Fatalf("Snapshot(): %v", err)
				}
				keys, err := blobs.List(context.Background(), ref.blobKey())
				if err != nil {
					t.Fatalf("List() before delete: %v", err)
				}
				if len(keys) != 1 {
					t.Fatalf("before Delete List(%q) = %v, want the ref key present", ref.blobKey(), keys)
				}
			} else {
				ref, err = ParseRef(refPrefix + strings.Repeat("b", refHexLen))
				if err != nil {
					t.Fatalf("ParseRef(): %v", err)
				}
			}

			for i := 0; i < tt.deletes; i++ {
				if err := s.Delete(context.Background(), ref); err != nil {
					t.Fatalf("Delete() #%d = %v, want nil", i+1, err)
				}
			}

			keys, err := blobs.List(context.Background(), ref.blobKey())
			if err != nil {
				t.Fatalf("List() after delete: %v", err)
			}
			if len(keys) != 0 {
				t.Errorf("after Delete List(%q) = %v, want empty", ref.blobKey(), keys)
			}
		})
	}
}
