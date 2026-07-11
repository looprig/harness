package workspacestore

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/looprig/storage"
	"github.com/looprig/storage/memstore"
)

// countingBlobs wraps a storage.Blobs and counts Put invocations so a test can
// prove that a content-addressed re-snapshot of an unchanged tree performs zero
// uploads. Get/List/Delete are promoted from the embedded interface unchanged.
type countingBlobs struct {
	storage.Blobs
	puts atomic.Int64
}

func (c *countingBlobs) Put(ctx context.Context, key string, r io.Reader) error {
	c.puts.Add(1)
	return c.Blobs.Put(ctx, key, r)
}

// failPutBlobs wraps a storage.Blobs and fails every Put with a fixed error so a
// test can prove a Blobs upload failure surfaces as an unwrap-able *SnapshotError.
// List (used by the presence check) is promoted from the embedded backend.
type failPutBlobs struct {
	storage.Blobs
	err error
}

func (f *failPutBlobs) Put(ctx context.Context, key string, r io.Reader) error {
	return f.err
}

// getBlob returns the full bytes stored under key, failing the test on any error.
func getBlob(t *testing.T, b storage.Blobs, key string) []byte {
	t.Helper()
	rc, err := b.Get(context.Background(), key)
	if err != nil {
		t.Fatalf("Blobs.Get(%q): %v", key, err)
	}
	defer func() { _ = rc.Close() }()
	data, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read blob %q: %v", key, err)
	}
	return data
}

func TestOpen(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		blobs   storage.Blobs
		opts    []Option
		wantErr bool
	}{
		{name: "valid backend", blobs: memstore.New().Blobs},
		{name: "valid backend with spool dir", blobs: memstore.New().Blobs, opts: []Option{WithSpoolDir(t.TempDir())}},
		{name: "nil backend", blobs: nil, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			s, err := Open(tt.blobs, tt.opts...)
			if (err != nil) != tt.wantErr {
				t.Fatalf("Open() error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				var nbe *NilBlobsError
				if !errors.As(err, &nbe) {
					t.Fatalf("Open() error = %v (%T), want *NilBlobsError", err, err)
				}
				if s != nil {
					t.Errorf("Open() returned non-nil Store on error")
				}
				return
			}
			if s == nil {
				t.Fatal("Open() returned nil Store without error")
			}
		})
	}
}

func TestSnapshotRoundTrip(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		nodes []node
	}{
		{name: "canonical tree", nodes: canonicalNodes()},
		{name: "single file", nodes: []node{{path: "only.txt", kind: kindFile, content: "x\n", mode: 0o644}}},
		{name: "empty directory", nodes: []node{{path: "d", kind: kindDir}}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			root := t.TempDir()
			buildTree(t, root, tt.nodes)

			blobs := memstore.New().Blobs
			s, err := Open(blobs)
			if err != nil {
				t.Fatalf("Open(): %v", err)
			}

			ref, err := s.Snapshot(context.Background(), root)
			if err != nil {
				t.Fatalf("Snapshot(): %v", err)
			}

			// The returned Ref must be grammar-valid.
			if _, perr := ParseRef(string(ref)); perr != nil {
				t.Fatalf("Snapshot returned non-parseable ref %q: %v", ref, perr)
			}

			// The Ref must be exactly "v1:sha256:<hex>" of the deterministic archive.
			want := archiveBytes(t, root)
			wantRef := "v1:sha256:" + digest(want)
			if string(ref) != wantRef {
				t.Errorf("Snapshot ref = %q, want %q", ref, wantRef)
			}

			// The blob must exist under the Ref's key, and its bytes must be the
			// archive whose sha256 matches the Ref.
			keys, err := blobs.List(context.Background(), ref.blobKey())
			if err != nil {
				t.Fatalf("Blobs.List(): %v", err)
			}
			if len(keys) != 1 || keys[0] != ref.blobKey() {
				t.Fatalf("Blobs.List(%q) = %v, want exactly [%q]", ref.blobKey(), keys, ref.blobKey())
			}
			got := getBlob(t, blobs, ref.blobKey())
			if digest(got) != digest(want) {
				t.Errorf("stored blob sha256 = %s, want %s (must match the Ref)", digest(got), digest(want))
			}

			// The stored bytes must decode as the tree's archive.
			entries := readArchive(t, got)
			for _, n := range tt.nodes {
				wantName := n.path
				if n.kind == kindDir {
					wantName += "/"
				}
				if _, ok := entries[wantName]; !ok {
					t.Errorf("archived tree missing entry %q", wantName)
				}
			}
		})
	}
}

func TestSnapshotDedupSkipsSecondPut(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		// second builds the tree the second Snapshot sees; a nil second means
		// "snapshot the exact same root again".
		second func(t *testing.T, nodes []node) string
	}{
		{name: "same root twice", second: nil},
		{
			name: "identical tree at a different dir and creation order",
			second: func(t *testing.T, nodes []node) string {
				other := t.TempDir()
				buildTree(t, other, reversed(nodes))
				return other
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			nodes := canonicalNodes()
			root := t.TempDir()
			buildTree(t, root, nodes)

			cb := &countingBlobs{Blobs: memstore.New().Blobs}
			s, err := Open(cb)
			if err != nil {
				t.Fatalf("Open(): %v", err)
			}

			ref1, err := s.Snapshot(context.Background(), root)
			if err != nil {
				t.Fatalf("first Snapshot(): %v", err)
			}
			if got := cb.puts.Load(); got != 1 {
				t.Fatalf("after first Snapshot Put count = %d, want 1", got)
			}

			secondRoot := root
			if tt.second != nil {
				secondRoot = tt.second(t, nodes)
			}

			ref2, err := s.Snapshot(context.Background(), secondRoot)
			if err != nil {
				t.Fatalf("second Snapshot(): %v", err)
			}
			if ref1 != ref2 {
				t.Errorf("identical trees yielded different refs: %q vs %q", ref1, ref2)
			}
			if got := cb.puts.Load(); got != 1 {
				t.Errorf("after second Snapshot Put count = %d, want 1 (present key must skip upload)", got)
			}
		})
	}
}

func TestSnapshotRootValidation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		root       func(t *testing.T) string
		wantErr    bool
		wantNotDir bool
	}{
		{
			name:    "nonexistent root",
			root:    func(t *testing.T) string { return filepath.Join(t.TempDir(), "does-not-exist") },
			wantErr: true,
		},
		{
			name: "regular file as root",
			root: func(t *testing.T) string {
				f := filepath.Join(t.TempDir(), "file.txt")
				if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
					t.Fatalf("WriteFile: %v", err)
				}
				return f
			},
			wantErr:    true,
			wantNotDir: true,
		},
		{
			name: "existing empty directory is valid",
			root: func(t *testing.T) string { return t.TempDir() },
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			s, err := Open(memstore.New().Blobs)
			if err != nil {
				t.Fatalf("Open(): %v", err)
			}

			_, err = s.Snapshot(context.Background(), tt.root(t))
			if (err != nil) != tt.wantErr {
				t.Fatalf("Snapshot() error = %v, wantErr %v", err, tt.wantErr)
			}
			if !tt.wantErr {
				return
			}
			var se *SnapshotError
			if !errors.As(err, &se) {
				t.Fatalf("Snapshot() error = %v (%T), want *SnapshotError", err, err)
			}
			if tt.wantNotDir {
				var nde *NotDirError
				if !errors.As(err, &nde) {
					t.Errorf("Snapshot() error = %v, want a wrapped *NotDirError", err)
				}
			}
		})
	}
}

func TestSnapshotPutErrorWrapped(t *testing.T) {
	t.Parallel()

	errBoom := errors.New("blobs put failed")

	tests := []struct {
		name    string
		cause   error
		wantErr bool
	}{
		{name: "put failure surfaces as SnapshotError", cause: errBoom, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			root := t.TempDir()
			buildTree(t, root, canonicalNodes())

			fb := &failPutBlobs{Blobs: memstore.New().Blobs, err: tt.cause}
			s, err := Open(fb)
			if err != nil {
				t.Fatalf("Open(): %v", err)
			}

			_, err = s.Snapshot(context.Background(), root)
			if (err != nil) != tt.wantErr {
				t.Fatalf("Snapshot() error = %v, wantErr %v", err, tt.wantErr)
			}
			var se *SnapshotError
			if !errors.As(err, &se) {
				t.Fatalf("Snapshot() error = %v (%T), want *SnapshotError", err, err)
			}
			if !errors.Is(err, tt.cause) {
				t.Errorf("Snapshot() error does not unwrap to the injected cause %v", tt.cause)
			}
		})
	}
}
