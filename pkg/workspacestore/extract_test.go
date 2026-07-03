package workspacestore

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// generousLimits are extraction bounds far larger than any test tree, used by
// happy-path tests that must never trip the decompression-bomb guards.
func generousLimits() limits {
	return limits{maxEntries: 1 << 20, maxBytes: 8 << 30}
}

// rawEntry is one hand-built tar entry: a header plus (for regular files) its
// data. Building the hostile corpus by hand keeps each attack byte-precise —
// writeArchive can never emit a "../escape" name or a device node.
type rawEntry struct {
	hdr  tar.Header
	data string
}

// gzTar encodes entries as a gzip-compressed tar (the exact wrapper
// extractArchive consumes). Entry names and types are written verbatim in PAX
// format so an absolute or ".."-bearing name survives to the extractor intact.
func gzTar(t *testing.T, entries []rawEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		h := e.hdr
		h.Format = tar.FormatPAX
		if h.Typeflag == tar.TypeReg && h.Size == 0 && e.data != "" {
			h.Size = int64(len(e.data))
		}
		if err := tw.WriteHeader(&h); err != nil {
			t.Fatalf("WriteHeader(%q): %v", h.Name, err)
		}
		if len(e.data) > 0 {
			if _, err := tw.Write([]byte(e.data)); err != nil {
				t.Fatalf("Write(%q): %v", h.Name, err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("tar close: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	return buf.Bytes()
}

// treeNode is a location-independent snapshot of one filesystem node used to
// assert two trees are identical. perm is zeroed for symlinks (link perms are
// platform noise); content applies to files; target applies to symlinks.
type treeNode struct {
	kind    nodeKind
	perm    fs.FileMode
	content string
	target  string
}

// snapshotTree walks root and returns a slash-relative map of its nodes (the
// root itself excluded), reading file contents and symlink targets so two
// snapshots compare by value, independent of where each tree lives on disk.
func snapshotTree(t *testing.T, root string) map[string]treeNode {
	t.Helper()
	out := make(map[string]treeNode)
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p == root {
			return nil
		}
		rel, relErr := filepath.Rel(root, p)
		if relErr != nil {
			return relErr
		}
		rel = filepath.ToSlash(rel)
		info, infoErr := d.Info()
		if infoErr != nil {
			return infoErr
		}
		switch {
		case info.Mode()&fs.ModeSymlink != 0:
			target, rlErr := os.Readlink(p)
			if rlErr != nil {
				return rlErr
			}
			out[rel] = treeNode{kind: kindSymlink, target: target}
		case info.IsDir():
			out[rel] = treeNode{kind: kindDir, perm: info.Mode().Perm()}
		default:
			data, rdErr := os.ReadFile(p) // #nosec G304 -- test-controlled tree under t.TempDir
			if rdErr != nil {
				return rdErr
			}
			out[rel] = treeNode{kind: kindFile, perm: info.Mode().Perm(), content: string(data)}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("snapshotTree(%q): %v", root, err)
	}
	return out
}

// assertDestEmpty fails unless dest exists and contains no entries — the
// "partial output removed" contract after a mid-extraction failure.
func assertDestEmpty(t *testing.T, dest string) {
	t.Helper()
	entries, err := os.ReadDir(dest)
	if err != nil {
		t.Fatalf("ReadDir(%q): %v", dest, err)
	}
	if len(entries) != 0 {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("dest %q not emptied after failed extraction; leftover: %v", dest, names)
	}
}

func TestExtractArchiveRoundTrip(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		nodes []node
	}{
		{name: "canonical tree", nodes: canonicalNodes()},
		{name: "single file", nodes: []node{{path: "only.txt", kind: kindFile, content: "x\n", mode: 0o644}}},
		{name: "empty directory", nodes: []node{{path: "emptydir", kind: kindDir}}},
		{name: "executable bit preserved", nodes: []node{{path: "run.sh", kind: kindFile, content: "#!/bin/sh\n", mode: 0o755}}},
		{name: "private file mode preserved", nodes: []node{{path: "secret", kind: kindFile, content: "s\n", mode: 0o600}}},
		{name: "nested dir with in-tree symlink", nodes: []node{
			{path: "a", kind: kindDir},
			{path: "a/f.txt", kind: kindFile, content: "hi\n", mode: 0o600},
			{path: "a/link", kind: kindSymlink, target: "f.txt"},
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			src := t.TempDir()
			buildTree(t, src, tt.nodes)

			var buf bytes.Buffer
			if err := writeArchive(&buf, src); err != nil {
				t.Fatalf("writeArchive(%q): %v", src, err)
			}

			dest := t.TempDir()
			if err := extractArchive(context.Background(), bytes.NewReader(buf.Bytes()), dest, generousLimits()); err != nil {
				t.Fatalf("extractArchive: %v", err)
			}

			want := snapshotTree(t, src)
			got := snapshotTree(t, dest)
			if len(want) != len(got) {
				t.Fatalf("node count mismatch: extracted %d, want %d\n want=%v\n got=%v", len(got), len(want), want, got)
			}
			for name, w := range want {
				g, ok := got[name]
				if !ok {
					t.Errorf("extracted tree missing node %q", name)
					continue
				}
				if g != w {
					t.Errorf("node %q mismatch:\n got  %+v\n want %+v", name, g, w)
				}
			}
		})
	}
}

// chmodTreeWritable makes every directory under root owner-writable so a test's
// t.TempDir cleanup can remove a tree that contains restrictive (e.g. 0o555)
// directories. WalkDir does not follow symlinks, so it never escapes root.
func chmodTreeWritable(root string) {
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			_ = os.Chmod(p, 0o755)
		}
		return nil
	})
}

func TestExtractArchiveRestoresReadOnlyDir(t *testing.T) {
	t.Parallel()

	src := t.TempDir()
	// The file node precedes the directory node so buildTree writes the child
	// into the directory BEFORE chmod'ing it read-only (0o555). writeArchive then
	// emits entries parent-before-child in sorted order, which is exactly the
	// order that would break a naive extractor that chmods a dir before writing
	// its children.
	buildTree(t, src, []node{
		{path: "ro/inside.txt", kind: kindFile, content: "locked\n", mode: 0o444},
		{path: "ro", kind: kindDir, mode: 0o555},
	})
	t.Cleanup(func() { chmodTreeWritable(src) })

	var buf bytes.Buffer
	if err := writeArchive(&buf, src); err != nil {
		t.Fatalf("writeArchive: %v", err)
	}

	dest := t.TempDir()
	t.Cleanup(func() { chmodTreeWritable(dest) })
	if err := extractArchive(context.Background(), bytes.NewReader(buf.Bytes()), dest, generousLimits()); err != nil {
		t.Fatalf("extractArchive of a read-only dir with a child: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(dest, "ro", "inside.txt")) // #nosec G304 -- test-controlled path under t.TempDir
	if err != nil {
		t.Fatalf("child of read-only dir missing: %v", err)
	}
	if string(got) != "locked\n" {
		t.Errorf("child content = %q, want %q", got, "locked\n")
	}
	info, err := os.Lstat(filepath.Join(dest, "ro"))
	if err != nil {
		t.Fatalf("Lstat(dest/ro): %v", err)
	}
	if info.Mode().Perm() != 0o555 {
		t.Errorf("restored dir mode = %o, want %o (read-only mode must round-trip)", info.Mode().Perm(), 0o555)
	}
}

func TestExtractArchiveRejectsDeepSymlinkAncestor(t *testing.T) {
	t.Parallel()

	// The spec asks for containment on EVERY path component, not just the
	// immediate parent. Here the escaping symlink is at depth 2 ("a/b") and the
	// write targets "a/b/c/d" — the write must be rejected as a hostile entry and
	// nothing may land at the symlink target.
	tests := []struct {
		name   string
		target string
		abs    bool
	}{
		{name: "absolute target", abs: true},
		{name: "relative parent-escape target", target: "../../../../escape"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			outside := t.TempDir()
			target := tt.target
			if tt.abs {
				target = outside
			}

			arc := gzTar(t, []rawEntry{
				{hdr: tar.Header{Name: "a", Typeflag: tar.TypeDir, Mode: 0o755}},
				{hdr: tar.Header{Name: "a/b", Typeflag: tar.TypeSymlink, Linkname: target}},
				{hdr: tar.Header{Name: "a/b/c/d", Typeflag: tar.TypeReg, Mode: 0o644}, data: "must not escape"},
			})

			dest := t.TempDir()
			err := extractArchive(context.Background(), bytes.NewReader(arc), dest, generousLimits())
			if err == nil {
				t.Fatal("extractArchive returned nil; want rejection of a write through a deep symlink ancestor")
			}
			var aee *ArchiveEntryError
			if !errors.As(err, &aee) {
				t.Fatalf("extractArchive error = %v (%T), want *ArchiveEntryError", err, err)
			}
			// Nothing may have been written through the symlink into its target.
			if _, statErr := os.Lstat(filepath.Join(outside, "c")); !errors.Is(statErr, fs.ErrNotExist) {
				t.Errorf("escaped into symlink target %q: Lstat(c) err = %v, want NotExist", outside, statErr)
			}
			assertDestEmpty(t, dest)
		})
	}
}

func TestExtractArchiveRejectsHostileEntries(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		entries    []rawEntry
		wantReason string // optional substring the ArchiveEntryError.Reason must contain
	}{
		{
			name:    "parent-escape file name",
			entries: []rawEntry{{hdr: tar.Header{Name: "../escape", Typeflag: tar.TypeReg}, data: "pwn"}},
		},
		{
			name:    "absolute file name",
			entries: []rawEntry{{hdr: tar.Header{Name: "/etc/cron.d/x", Typeflag: tar.TypeReg}, data: "pwn"}},
		},
		{
			name:    "dotdot mid path",
			entries: []rawEntry{{hdr: tar.Header{Name: "sub/../../escape", Typeflag: tar.TypeReg}, data: "pwn"}},
		},
		{
			name:    "symlink name escapes",
			entries: []rawEntry{{hdr: tar.Header{Name: "../evil", Typeflag: tar.TypeSymlink, Linkname: "whatever"}}},
		},
		{
			name:    "hard link entry",
			entries: []rawEntry{{hdr: tar.Header{Name: "hard", Typeflag: tar.TypeLink, Linkname: "some/target"}}},
		},
		{
			name:    "character device entry",
			entries: []rawEntry{{hdr: tar.Header{Name: "chardev", Typeflag: tar.TypeChar, Devmajor: 1, Devminor: 3}}},
		},
		{
			name:    "block device entry",
			entries: []rawEntry{{hdr: tar.Header{Name: "blockdev", Typeflag: tar.TypeBlock, Devmajor: 8, Devminor: 0}}},
		},
		{
			name:    "fifo entry",
			entries: []rawEntry{{hdr: tar.Header{Name: "pipe", Typeflag: tar.TypeFifo}}},
		},
		{
			name: "duplicate entry name",
			entries: []rawEntry{
				{hdr: tar.Header{Name: "dup.txt", Typeflag: tar.TypeReg, Mode: 0o644}, data: "first"},
				{hdr: tar.Header{Name: "dup.txt", Typeflag: tar.TypeReg, Mode: 0o644}, data: "second"},
			},
			wantReason: "duplicate",
		},
		{
			name:       "unknown typeflag",
			entries:    []rawEntry{{hdr: tar.Header{Name: "weird", Typeflag: tar.TypeCont}}},
			wantReason: "typeflag",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			dest := t.TempDir()
			arc := gzTar(t, tt.entries)

			err := extractArchive(context.Background(), bytes.NewReader(arc), dest, generousLimits())
			if err == nil {
				t.Fatal("extractArchive returned nil; want *ArchiveEntryError")
			}
			var aee *ArchiveEntryError
			if !errors.As(err, &aee) {
				t.Fatalf("extractArchive error = %v (%T), want *ArchiveEntryError", err, err)
			}
			if aee.Name == "" {
				t.Errorf("ArchiveEntryError.Name is empty; want the offending entry name")
			}
			if aee.Reason == "" {
				t.Errorf("ArchiveEntryError.Reason is empty; want the rule broken")
			}
			if tt.wantReason != "" && !strings.Contains(aee.Reason, tt.wantReason) {
				t.Errorf("ArchiveEntryError.Reason = %q, want it to contain %q", aee.Reason, tt.wantReason)
			}
			assertDestEmpty(t, dest)
		})
	}
}

func TestExtractArchiveSymlinkTargetCreatedButNotFollowed(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		target string // symlink target: absolute (outside dir) or relative escape
		abs    bool
	}{
		{name: "absolute target", abs: true},
		{name: "relative parent-escape target", target: "../../../../../../etc"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			outside := t.TempDir()
			target := tt.target
			if tt.abs {
				target = outside
			}

			// Entry 1 plants a hostile symlink "link" -> escaping target.
			// Entry 2 tries to write "link/inner" THROUGH that symlink; it must
			// be rejected because "link" is a symlink component, so nothing is
			// ever written to the symlink's target.
			arc := gzTar(t, []rawEntry{
				{hdr: tar.Header{Name: "link", Typeflag: tar.TypeSymlink, Linkname: target}},
				{hdr: tar.Header{Name: "link/inner", Typeflag: tar.TypeReg}, data: "should never land outside dest"},
			})

			dest := t.TempDir()
			err := extractArchive(context.Background(), bytes.NewReader(arc), dest, generousLimits())
			if err == nil {
				t.Fatal("extractArchive returned nil; want rejection of write through a symlink")
			}
			var aee *ArchiveEntryError
			if !errors.As(err, &aee) {
				t.Fatalf("extractArchive error = %v (%T), want *ArchiveEntryError", err, err)
			}
			// The escape must not have produced a file at the symlink target.
			if _, statErr := os.Lstat(filepath.Join(outside, "inner")); !errors.Is(statErr, fs.ErrNotExist) {
				t.Errorf("file escaped into symlink target dir %q: Lstat err = %v, want NotExist", outside, statErr)
			}
			assertDestEmpty(t, dest)
		})
	}
}

func TestExtractArchiveEscapingSymlinkStoredVerbatim(t *testing.T) {
	t.Parallel()

	// A symlink whose target escapes the destination, with no entry that tries to
	// traverse it, is a harmless entry: it must be stored verbatim (never resolved
	// or rejected) so a faithful snapshot round-trips, while remaining inert
	// because extraction never followed it.
	tests := []struct {
		name   string
		target string
	}{
		{name: "absolute target", target: "/etc/passwd"},
		{name: "relative parent-escape target", target: "../../../../secret"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			arc := gzTar(t, []rawEntry{
				{hdr: tar.Header{Name: "link", Typeflag: tar.TypeSymlink, Linkname: tt.target}},
			})

			dest := t.TempDir()
			if err := extractArchive(context.Background(), bytes.NewReader(arc), dest, generousLimits()); err != nil {
				t.Fatalf("extractArchive of a lone escaping symlink: %v", err)
			}

			got, err := os.Readlink(filepath.Join(dest, "link"))
			if err != nil {
				t.Fatalf("Readlink(dest/link): %v", err)
			}
			if got != tt.target {
				t.Errorf("stored symlink target = %q, want %q (verbatim, unresolved)", got, tt.target)
			}
		})
	}
}

func TestExtractArchiveEnforcesLimits(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		lim       limits
		entries   []rawEntry
		wantLimit ArchiveLimit
		wantCap   int64
	}{
		{
			name: "entry count bomb",
			lim:  limits{maxEntries: 3, maxBytes: 1 << 30},
			entries: []rawEntry{
				{hdr: tar.Header{Name: "d1", Typeflag: tar.TypeDir}},
				{hdr: tar.Header{Name: "d2", Typeflag: tar.TypeDir}},
				{hdr: tar.Header{Name: "d3", Typeflag: tar.TypeDir}},
				{hdr: tar.Header{Name: "d4", Typeflag: tar.TypeDir}},
			},
			wantLimit: ArchiveLimitEntries,
			wantCap:   3,
		},
		{
			name: "cumulative byte bomb",
			lim:  limits{maxEntries: 1 << 20, maxBytes: 10},
			entries: []rawEntry{
				{hdr: tar.Header{Name: "a", Typeflag: tar.TypeReg}, data: "AAAAAAAA"}, // 8 bytes
				{hdr: tar.Header{Name: "b", Typeflag: tar.TypeReg}, data: "BBBBBBBB"}, // +8 = 16 > 10
			},
			wantLimit: ArchiveLimitBytes,
			wantCap:   10,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			dest := t.TempDir()
			arc := gzTar(t, tt.entries)

			err := extractArchive(context.Background(), bytes.NewReader(arc), dest, tt.lim)
			if err == nil {
				t.Fatal("extractArchive returned nil; want *ArchiveLimitError")
			}
			var ale *ArchiveLimitError
			if !errors.As(err, &ale) {
				t.Fatalf("extractArchive error = %v (%T), want *ArchiveLimitError", err, err)
			}
			if ale.Limit != tt.wantLimit {
				t.Errorf("ArchiveLimitError.Limit = %q, want %q", ale.Limit, tt.wantLimit)
			}
			if ale.Cap != tt.wantCap {
				t.Errorf("ArchiveLimitError.Cap = %d, want %d", ale.Cap, tt.wantCap)
			}
			if ale.Observed <= ale.Cap {
				t.Errorf("ArchiveLimitError.Observed = %d, want > Cap %d", ale.Observed, ale.Cap)
			}
			assertDestEmpty(t, dest)
		})
	}
}

func TestExtractArchiveHonorsContextCancellation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		entries []rawEntry
	}{
		{
			name: "small entries",
			entries: []rawEntry{
				{hdr: tar.Header{Name: "a.txt", Typeflag: tar.TypeReg}, data: "a"},
				{hdr: tar.Header{Name: "b.txt", Typeflag: tar.TypeReg}, data: "b"},
			},
		},
		{
			// A large entry proves a cancelled context aborts extraction rather
			// than blocking on a long copy.
			name:    "large entry",
			entries: []rawEntry{{hdr: tar.Header{Name: "big.bin", Typeflag: tar.TypeReg}, data: strings.Repeat("x", 4<<20)}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			ctx, cancel := context.WithCancel(context.Background())
			cancel() // cancelled before extraction begins

			arc := gzTar(t, tt.entries)
			dest := t.TempDir()
			err := extractArchive(ctx, bytes.NewReader(arc), dest, generousLimits())
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("extractArchive error = %v, want context.Canceled", err)
			}
			assertDestEmpty(t, dest)
		})
	}
}

// cancelAfterFirstRead cancels a context as a side effect of its first delegated
// Read, so a ctxReader wrapping it aborts on its NEXT read (the pre-check) after
// partial progress — a deterministic stand-in for mid-copy cancellation.
type cancelAfterFirstRead struct {
	r      io.Reader
	cancel context.CancelFunc
	fired  bool
}

func (c *cancelAfterFirstRead) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	if !c.fired {
		c.fired = true
		c.cancel()
	}
	return n, err
}

func TestCopyCappedHonorsCancellationMidCopy(t *testing.T) {
	t.Parallel()

	const total = 512 * 1024
	ctx, cancel := context.WithCancel(context.Background())
	src := &cancelAfterFirstRead{r: bytes.NewReader(make([]byte, total)), cancel: cancel}
	cr := &ctxReader{ctx: ctx, r: src}

	n, exceeded, err := copyCapped(io.Discard, cr, 1<<30)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("copyCapped error = %v, want context.Canceled", err)
	}
	if exceeded {
		t.Errorf("copyCapped exceeded = true, want false on cancellation")
	}
	if n <= 0 || n >= total {
		t.Errorf("copyCapped copied n = %d, want partial progress in (0, %d)", n, total)
	}
}

func TestExtractArchiveEmptyArchive(t *testing.T) {
	t.Parallel()

	arc := gzTar(t, nil) // a valid gzip tar with no entries
	dest := t.TempDir()
	if err := extractArchive(context.Background(), bytes.NewReader(arc), dest, generousLimits()); err != nil {
		t.Fatalf("extractArchive of an empty archive: %v", err)
	}
	assertDestEmpty(t, dest)
}

func TestExtractArchiveMalformedInput(t *testing.T) {
	t.Parallel()

	valid := gzTar(t, []rawEntry{{hdr: tar.Header{Name: "a.txt", Typeflag: tar.TypeReg}, data: "hello"}})

	tests := []struct {
		name  string
		input []byte
	}{
		{name: "not gzip", input: []byte("this is plainly not a gzip stream at all")},
		{name: "empty input", input: nil},
		{name: "truncated gzip", input: valid[:len(valid)/2]},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			dest := t.TempDir()
			err := extractArchive(context.Background(), bytes.NewReader(tt.input), dest, generousLimits())
			if err == nil {
				t.Fatal("extractArchive returned nil; want a decode error")
			}
			assertDestEmpty(t, dest)
		})
	}
}

func TestValidateEntryName(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		entry   string
		wantErr bool
	}{
		{name: "simple file", entry: "readme.txt"},
		{name: "nested file", entry: "src/main.go"},
		{name: "deeply nested", entry: "a/b/c/d.txt"},
		{name: "dot-prefixed segment is fine", entry: ".config/settings"},

		{name: "empty", entry: "", wantErr: true},
		{name: "absolute", entry: "/etc/passwd", wantErr: true},
		{name: "leading dotdot", entry: "../escape", wantErr: true},
		{name: "trailing dotdot", entry: "a/..", wantErr: true},
		{name: "mid dotdot", entry: "a/../../b", wantErr: true},
		{name: "bare dotdot", entry: "..", wantErr: true},
		{name: "dot resolves to root", entry: ".", wantErr: true},
		{name: "dotslash resolves to root", entry: "./", wantErr: true},
		{name: "nul byte", entry: "a\x00b", wantErr: true},
		{name: "many dotdot", entry: "../../../../../../etc/shadow", wantErr: true},
		{name: "over length name", entry: strings.Repeat("a/", 4096) + "deep", wantErr: true},
		{name: "exactly max length name accepted", entry: strings.Repeat("a/", 2047) + "de"}, // 2*2047 + 2 = 4096
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			err := validateEntryName(tt.entry)
			if (err != nil) != tt.wantErr {
				t.Fatalf("validateEntryName(%q) error = %v, wantErr %v", tt.entry, err, tt.wantErr)
			}
			if tt.wantErr {
				var aee *ArchiveEntryError
				if !errors.As(err, &aee) {
					t.Fatalf("validateEntryName(%q) error = %v (%T), want *ArchiveEntryError", tt.entry, err, err)
				}
				if aee.Reason == "" {
					t.Errorf("validateEntryName(%q) *ArchiveEntryError.Reason is empty", tt.entry)
				}
			}
		})
	}
}
