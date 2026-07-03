package workspacestore

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// nodeKind enumerates the filesystem node types a test tree can contain.
type nodeKind int

const (
	kindFile nodeKind = iota
	kindDir
	kindSymlink
)

// node describes one entry in a test tree. path is slash-relative to the tree
// root; content/mode apply to files, mode applies to dirs, target applies to
// symlinks. A zero mode means "leave the umask default".
type node struct {
	path    string
	kind    nodeKind
	content string
	target  string
	mode    fs.FileMode
}

// canonicalNodes is a representative tree exercising every path the archiver
// distinguishes: regular files, an executable (0755), nested directories, a
// relative symlink, and names that sort non-trivially ("a.txt" vs "bin" vs
// "bin/run.sh"). The symlink target is RELATIVE so the archive bytes do not
// depend on the tree's absolute location (the cross-tmpdir determinism test
// relies on that).
func canonicalNodes() []node {
	return []node{
		{path: "readme.txt", kind: kindFile, content: "hello world\n", mode: 0o644},
		{path: "a.txt", kind: kindFile, content: "a\n", mode: 0o644},
		{path: "bin", kind: kindDir},
		{path: "bin/run.sh", kind: kindFile, content: "#!/bin/sh\necho hi\n", mode: 0o755},
		{path: "src", kind: kindDir},
		{path: "src/main.go", kind: kindFile, content: "package main\n", mode: 0o644},
		{path: "src/nested", kind: kindDir},
		{path: "src/nested/deep.txt", kind: kindFile, content: "deep\n", mode: 0o644},
		{path: "link", kind: kindSymlink, target: "readme.txt"},
	}
}

// buildTree materializes nodes under root in the given slice order. Parent
// directories are created on demand (via MkdirAll), so nodes may be supplied in
// any order — this lets a test build the SAME logical tree twice in a different
// creation order to prove the archive does not depend on traversal order.
func buildTree(t *testing.T, root string, nodes []node) {
	t.Helper()
	for _, n := range nodes {
		abs := filepath.Join(root, filepath.FromSlash(n.path))
		switch n.kind {
		case kindDir:
			if err := os.MkdirAll(abs, 0o755); err != nil {
				t.Fatalf("MkdirAll(%q): %v", abs, err)
			}
			if n.mode != 0 {
				if err := os.Chmod(abs, n.mode); err != nil {
					t.Fatalf("Chmod(%q): %v", abs, err)
				}
			}
		case kindFile:
			if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
				t.Fatalf("MkdirAll(%q): %v", filepath.Dir(abs), err)
			}
			if err := os.WriteFile(abs, []byte(n.content), 0o644); err != nil {
				t.Fatalf("WriteFile(%q): %v", abs, err)
			}
			if n.mode != 0 {
				if err := os.Chmod(abs, n.mode); err != nil {
					t.Fatalf("Chmod(%q): %v", abs, err)
				}
			}
		case kindSymlink:
			if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
				t.Fatalf("MkdirAll(%q): %v", filepath.Dir(abs), err)
			}
			if err := os.Symlink(n.target, abs); err != nil {
				t.Fatalf("Symlink(%q -> %q): %v", abs, n.target, err)
			}
		}
	}
}

// shortTempDir returns a short-path temp directory (auto-removed at test end).
// It exists because a unix socket's bind path must fit the OS sun_path limit
// (104 bytes on darwin); t.TempDir embeds the full test name and overflows it,
// which would falsely skip the irregular-node test rather than exercise it.
func shortTempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "ws")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// reversed returns a copy of nodes in reverse order — a cheap way to build an
// identical tree with a different creation order (files before their directory
// nodes; the symlink before its target).
func reversed(nodes []node) []node {
	out := make([]node, len(nodes))
	for i, n := range nodes {
		out[len(nodes)-1-i] = n
	}
	return out
}

// archiveBytes writes root to a deterministic tar.gz and returns the bytes.
func archiveBytes(t *testing.T, root string) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := writeArchive(&buf, root); err != nil {
		t.Fatalf("writeArchive(%q): %v", root, err)
	}
	return buf.Bytes()
}

// digest is the lowercase sha256 hex of b — the content address the Ref carries.
func digest(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// archiveEntry is one decoded tar entry: its header plus (for regular files)
// its data.
type archiveEntry struct {
	header tar.Header
	data   []byte
}

// readArchive gunzips and untars b into a name->entry map, failing the test on
// any decode error.
func readArchive(t *testing.T, b []byte) map[string]archiveEntry {
	t.Helper()
	gz, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		t.Fatalf("gzip.NewReader: %v", err)
	}
	defer func() {
		if cerr := gz.Close(); cerr != nil {
			t.Errorf("gzip close: %v", cerr)
		}
	}()
	tr := tar.NewReader(gz)
	out := make(map[string]archiveEntry)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("tar.Next: %v", err)
		}
		data, err := io.ReadAll(tr)
		if err != nil {
			t.Fatalf("read entry %q: %v", hdr.Name, err)
		}
		out[hdr.Name] = archiveEntry{header: *hdr, data: data}
	}
	return out
}

func TestWriteArchiveByteIdentical(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		nodes []node
	}{
		{name: "flat files", nodes: []node{
			{path: "a", kind: kindFile, content: "a", mode: 0o644},
			{path: "b", kind: kindFile, content: "bb", mode: 0o644},
		}},
		{name: "nested dirs and exec and symlink", nodes: canonicalNodes()},
		{name: "empty dir only", nodes: []node{{path: "d", kind: kindDir}}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			root := t.TempDir()
			buildTree(t, root, tt.nodes)

			first := archiveBytes(t, root)
			second := archiveBytes(t, root)
			if !bytes.Equal(first, second) {
				t.Errorf("two archives of the same tree differ: %d vs %d bytes (digests %s vs %s)",
					len(first), len(second), digest(first), digest(second))
			}
		})
	}
}

func TestWriteArchiveSameDigestAcrossTmpdirs(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		nodes []node
	}{
		{name: "canonical tree", nodes: canonicalNodes()},
		{name: "single file", nodes: []node{{path: "only.txt", kind: kindFile, content: "x\n", mode: 0o644}}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			rootA := t.TempDir()
			buildTree(t, rootA, tt.nodes)

			rootB := t.TempDir()
			buildTree(t, rootB, reversed(tt.nodes)) // same tree, different creation order

			da := digest(archiveBytes(t, rootA))
			db := digest(archiveBytes(t, rootB))
			if da != db {
				t.Errorf("same logical tree built at different tmpdirs/orders yielded different digests: %s vs %s", da, db)
			}
		})
	}
}

func TestWriteArchiveMtimeIndependent(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		touch string // slash-relative path whose mtime is changed
		when  time.Time
	}{
		{name: "file far future", touch: "readme.txt", when: time.Date(2038, 1, 19, 3, 14, 7, 0, time.UTC)},
		{name: "file past", touch: "src/main.go", when: time.Date(1999, 12, 31, 23, 59, 59, 0, time.UTC)},
		{name: "directory", touch: "bin", when: time.Date(2100, 6, 15, 12, 0, 0, 0, time.UTC)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			root := t.TempDir()
			buildTree(t, root, canonicalNodes())

			before := digest(archiveBytes(t, root))

			target := filepath.Join(root, filepath.FromSlash(tt.touch))
			if err := os.Chtimes(target, tt.when, tt.when); err != nil {
				t.Fatalf("Chtimes(%q): %v", target, err)
			}

			after := digest(archiveBytes(t, root))
			if before != after {
				t.Errorf("digest changed after mtime edit of %q: %s -> %s (mtime must be normalized)", tt.touch, before, after)
			}
		})
	}
}

func TestWriteArchiveHeaderNormalization(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	buildTree(t, root, canonicalNodes())
	entries := readArchive(t, archiveBytes(t, root))

	if len(entries) == 0 {
		t.Fatal("archive contained no entries")
	}

	epoch := time.Unix(0, 0)
	for name, e := range entries {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			if e.header.Uid != 0 {
				t.Errorf("entry %q Uid = %d, want 0", name, e.header.Uid)
			}
			if e.header.Gid != 0 {
				t.Errorf("entry %q Gid = %d, want 0", name, e.header.Gid)
			}
			if e.header.Uname != "" {
				t.Errorf("entry %q Uname = %q, want empty", name, e.header.Uname)
			}
			if e.header.Gname != "" {
				t.Errorf("entry %q Gname = %q, want empty", name, e.header.Gname)
			}
			if !e.header.ModTime.Equal(epoch) {
				t.Errorf("entry %q ModTime = %v, want epoch", name, e.header.ModTime)
			}
			if !e.header.AccessTime.IsZero() {
				t.Errorf("entry %q AccessTime = %v, want zero", name, e.header.AccessTime)
			}
			if !e.header.ChangeTime.IsZero() {
				t.Errorf("entry %q ChangeTime = %v, want zero", name, e.header.ChangeTime)
			}
		})
	}
}

func TestWriteArchiveGzipHeaderZeroed(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	buildTree(t, root, canonicalNodes())

	gz, err := gzip.NewReader(bytes.NewReader(archiveBytes(t, root)))
	if err != nil {
		t.Fatalf("gzip.NewReader: %v", err)
	}
	defer func() { _ = gz.Close() }()

	if !gz.Header.ModTime.IsZero() {
		t.Errorf("gzip ModTime = %v, want zero", gz.Header.ModTime)
	}
	if gz.Header.Name != "" {
		t.Errorf("gzip Name = %q, want empty", gz.Header.Name)
	}
	if gz.Header.Comment != "" {
		t.Errorf("gzip Comment = %q, want empty", gz.Header.Comment)
	}
	if gz.Header.OS != gzipOSUnknown {
		t.Errorf("gzip OS = %d, want %d (unknown)", gz.Header.OS, gzipOSUnknown)
	}
}

func TestWriteArchiveModePreserved(t *testing.T) {
	t.Parallel()

	nodes := []node{
		{path: "exec.sh", kind: kindFile, content: "#!/bin/sh\n", mode: 0o755},
		{path: "plain.txt", kind: kindFile, content: "x\n", mode: 0o644},
		{path: "private.txt", kind: kindFile, content: "s\n", mode: 0o600},
	}
	root := t.TempDir()
	buildTree(t, root, nodes)
	entries := readArchive(t, archiveBytes(t, root))

	tests := []struct {
		name string
		want fs.FileMode
	}{
		{name: "exec.sh", want: 0o755},
		{name: "plain.txt", want: 0o644},
		{name: "private.txt", want: 0o600},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			e, ok := entries[tt.name]
			if !ok {
				t.Fatalf("entry %q missing from archive", tt.name)
			}
			if got := e.header.FileInfo().Mode().Perm(); got != tt.want {
				t.Errorf("entry %q mode = %o, want %o", tt.name, got, tt.want)
			}
		})
	}
}

func TestWriteArchiveSymlinkStoredAsSymlink(t *testing.T) {
	t.Parallel()

	const targetContent = "hello world\n"

	tests := []struct {
		name       string
		linkTarget string
	}{
		{name: "relative target", linkTarget: "readme.txt"},
		{name: "relative dotslash target", linkTarget: "./readme.txt"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			root := t.TempDir()
			buildTree(t, root, []node{
				{path: "readme.txt", kind: kindFile, content: targetContent, mode: 0o644},
				{path: "link", kind: kindSymlink, target: tt.linkTarget},
			})
			entries := readArchive(t, archiveBytes(t, root))

			link, ok := entries["link"]
			if !ok {
				t.Fatal("symlink entry \"link\" missing from archive")
			}
			if link.header.Typeflag != tar.TypeSymlink {
				t.Errorf("link Typeflag = %q, want TypeSymlink %q", link.header.Typeflag, tar.TypeSymlink)
			}
			if link.header.Linkname != tt.linkTarget {
				t.Errorf("link Linkname = %q, want %q", link.header.Linkname, tt.linkTarget)
			}
			if len(link.data) != 0 {
				t.Errorf("symlink entry carried %d bytes of data; the target content must NOT be embedded via the link", len(link.data))
			}
			// The real file is archived once, with its content — proving the
			// link did not replace or duplicate it.
			reg, ok := entries["readme.txt"]
			if !ok {
				t.Fatal("regular file \"readme.txt\" missing from archive")
			}
			if string(reg.data) != targetContent {
				t.Errorf("readme.txt data = %q, want %q", reg.data, targetContent)
			}
		})
	}
}

func TestWriteArchiveIrregularFileRejected(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		wantReason string
		make       func(t *testing.T, root string) func()
	}{
		{
			name:       "unix socket",
			wantReason: "socket",
			make: func(t *testing.T, root string) func() {
				ln, err := net.Listen("unix", filepath.Join(root, "sock"))
				if err != nil {
					t.Skipf("unix sockets unavailable on this platform: %v", err)
				}
				return func() { _ = ln.Close() }
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			// A short root path so the socket bind fits the OS sun_path limit.
			root := shortTempDir(t)
			// A companion regular file: the walk must still fail closed even
			// though there is legitimate content alongside the bad node.
			buildTree(t, root, []node{{path: "keep.txt", kind: kindFile, content: "keep\n", mode: 0o644}})

			cleanup := tt.make(t, root)
			defer cleanup()

			err := writeArchive(io.Discard, root)
			if err == nil {
				t.Fatal("writeArchive returned nil; want *ArchiveEntryError for an irregular node")
			}
			var aee *ArchiveEntryError
			if !errors.As(err, &aee) {
				t.Fatalf("writeArchive error = %v (%T), want *ArchiveEntryError", err, err)
			}
			if aee.Name != "sock" {
				t.Errorf("ArchiveEntryError.Name = %q, want \"sock\"", aee.Name)
			}
			if !strings.Contains(aee.Reason, tt.wantReason) {
				t.Errorf("ArchiveEntryError.Reason = %q, want it to mention %q", aee.Reason, tt.wantReason)
			}
		})
	}
}

func TestWriteArchiveRootSymlinkEscapeNotFollowed(t *testing.T) {
	t.Parallel()

	const secret = "TOP SECRET should never be archived\n"

	tests := []struct {
		name     string
		absolute bool // symlink target is the absolute outside dir vs a relative "../.." escape
	}{
		{name: "absolute outside target", absolute: true},
		{name: "relative parent-escape target", absolute: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			outside := t.TempDir()
			if err := os.WriteFile(filepath.Join(outside, "secret.txt"), []byte(secret), 0o644); err != nil {
				t.Fatalf("write secret: %v", err)
			}

			root := t.TempDir()
			if err := os.WriteFile(filepath.Join(root, "keep.txt"), []byte("keep\n"), 0o644); err != nil {
				t.Fatalf("write keep: %v", err)
			}

			var linkTarget string
			if tt.absolute {
				linkTarget = outside
			} else {
				rel, err := filepath.Rel(root, outside)
				if err != nil {
					t.Fatalf("Rel: %v", err)
				}
				linkTarget = rel
			}
			if err := os.Symlink(linkTarget, filepath.Join(root, "out")); err != nil {
				t.Fatalf("Symlink: %v", err)
			}

			entries := readArchive(t, archiveBytes(t, root))

			out, ok := entries["out"]
			if !ok {
				t.Fatal("symlink entry \"out\" missing from archive")
			}
			if out.header.Typeflag != tar.TypeSymlink {
				t.Errorf("out Typeflag = %q, want TypeSymlink", out.header.Typeflag)
			}
			if out.header.Linkname != linkTarget {
				t.Errorf("out Linkname = %q, want %q (target string preserved, not resolved)", out.header.Linkname, linkTarget)
			}
			// WalkDir must not descend through the symlink: no entry under "out/".
			for name, e := range entries {
				if strings.HasPrefix(name, "out/") {
					t.Errorf("archive descended through symlink: found entry %q", name)
				}
				if bytes.Contains(e.data, []byte("TOP SECRET")) {
					t.Errorf("archive entry %q contains outside content; symlink was followed", name)
				}
			}
		})
	}
}
