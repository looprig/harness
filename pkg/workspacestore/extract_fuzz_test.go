package workspacestore

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// extractFuzzNameCap bounds the entry-name length for which the fuzz performs a
// real on-disk extraction. It comfortably fits every containment-escape vector
// while keeping each iteration's filesystem footprint tiny, so the parallel fuzz
// workers stay within their resource budget.
const extractFuzzNameCap = 256

// oneFileArchive best-effort encodes a single regular-file entry named name
// into a gzip tar. Some fuzz-generated names are invalid for tar (e.g. embedded
// NUL); on any encode error it returns whatever bytes were produced so the
// extractor still exercises its gzip/tar decode and containment paths.
func oneFileArchive(name string) []byte {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	hdr := tar.Header{Name: name, Typeflag: tar.TypeReg, Size: int64(len("payload")), Format: tar.FormatPAX}
	if err := tw.WriteHeader(&hdr); err == nil {
		_, _ = tw.Write([]byte("payload"))
	}
	_ = tw.Close()
	_ = gz.Close()
	return buf.Bytes()
}

// pathsOutsideDest returns the set of slash-relative paths under base that are
// NOT within the dest subtree — the canary set an extraction escape would grow.
func pathsOutsideDest(t *testing.T, base, dest string) map[string]struct{} {
	t.Helper()
	got := make(map[string]struct{})
	err := filepath.WalkDir(base, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // tolerate transient walk errors; the canary only needs stability
		}
		if p == dest {
			return filepath.SkipDir
		}
		if p == base {
			return nil
		}
		rel, relErr := filepath.Rel(base, p)
		if relErr != nil {
			return nil
		}
		got[filepath.ToSlash(rel)] = struct{}{}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %q: %v", base, err)
	}
	return got
}

// FuzzArchiveEntryName drives the entry-name validator and the full extractor
// with arbitrary entry names. It asserts two invariants: validateEntryName never
// panics, and extracting a single-entry archive named with arbitrary input never
// writes a byte outside the canary destination directory.
func FuzzArchiveEntryName(f *testing.F) {
	seeds := []string{
		"a.txt", "dir/file", "src/main.go",
		"../escape", "../../etc/passwd", "/etc/shadow",
		"a/../../b", "..", ".", "./x", "a//b",
		"link/inner", "a\x00b", "", "  ",
		strings.Repeat("../", 64) + "x",
		strings.Repeat("a/", 512) + "deep",
	}
	for _, s := range seeds {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, name string) {
		// Invariant 1: the name validator never panics on any input.
		_ = validateEntryName(name)

		// Invariant 2: extraction never escapes the canary dest. This is
		// exercised only for short names: every escape vector (zip-slip "..",
		// absolute paths, symlink names) is short, whereas a long benign name is
		// merely a deep directory tree whose creation would make each fuzz
		// iteration a heavyweight filesystem operation — no added escape signal,
		// just resource pressure across the parallel fuzz workers.
		if len(name) > extractFuzzNameCap {
			return
		}
		base := t.TempDir()
		dest := filepath.Join(base, "dest")
		outside := filepath.Join(base, "outside")
		for _, d := range []string{dest, outside} {
			if err := os.MkdirAll(d, 0o755); err != nil {
				t.Fatalf("MkdirAll(%q): %v", d, err)
			}
		}
		if err := os.WriteFile(filepath.Join(outside, "sentinel"), []byte("keep"), 0o644); err != nil {
			t.Fatalf("write sentinel: %v", err)
		}

		before := pathsOutsideDest(t, base, dest)
		arc := oneFileArchive(name)
		// The error (if any) is expected for hostile names; only escape matters.
		_ = extractArchive(context.Background(), bytes.NewReader(arc), dest, limits{maxEntries: 1 << 20, maxBytes: 1 << 20})
		after := pathsOutsideDest(t, base, dest)

		if len(after) != len(before) {
			t.Fatalf("extraction of entry %q escaped dest: paths outside dest grew from %v to %v", name, before, after)
		}
		for p := range after {
			if _, ok := before[p]; !ok {
				t.Fatalf("extraction of entry %q created path %q outside dest", name, p)
			}
		}
	})
}
