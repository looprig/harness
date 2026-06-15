package tools

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestContainedPath(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		// setup builds the fixture under the per-test temp dir and returns the
		// (root, input) pair to feed containedPath. It may create files and
		// symlinks. Any sibling dir it needs (e.g. an "outside" target) is
		// created under tmp too so the test never touches host paths.
		setup func(t *testing.T, tmp string) (root, input string)
		// wantRel, when wantErr is false, is the expected path RELATIVE to the
		// resolved root (so the assertion is independent of the temp dir name).
		wantRel string
		wantErr bool
	}{
		{
			name: "in-workspace existing file ok",
			setup: func(t *testing.T, tmp string) (string, string) {
				root := mkdir(t, tmp, "ws")
				writeFile(t, filepath.Join(root, "a.go"), "x")
				return root, "a.go"
			},
			wantRel: "a.go",
		},
		{
			name: "in-workspace nested existing file ok",
			setup: func(t *testing.T, tmp string) (string, string) {
				root := mkdir(t, tmp, "ws")
				mkdir(t, root, "src")
				writeFile(t, filepath.Join(root, "src", "main.go"), "x")
				return root, "src/main.go"
			},
			wantRel: "src/main.go",
		},
		{
			name: "root itself is contained",
			setup: func(t *testing.T, tmp string) (string, string) {
				return mkdir(t, tmp, "ws"), "."
			},
			wantRel: ".",
		},
		{
			name: "dotdot escape rejected",
			setup: func(t *testing.T, tmp string) (string, string) {
				root := mkdir(t, tmp, "ws")
				writeFile(t, filepath.Join(tmp, "secret.txt"), "x")
				return root, "../secret.txt"
			},
			wantErr: true,
		},
		{
			name: "deep dotdot escape rejected",
			setup: func(t *testing.T, tmp string) (string, string) {
				root := mkdir(t, tmp, "ws")
				return root, "a/b/../../../etc/passwd"
			},
			wantErr: true,
		},
		{
			name: "symlink inside workspace pointing outside rejected",
			setup: func(t *testing.T, tmp string) (string, string) {
				root := mkdir(t, tmp, "ws")
				outside := mkdir(t, tmp, "outside")
				writeFile(t, filepath.Join(outside, "loot.txt"), "x")
				// link lives inside ws but resolves to the outside dir.
				link := filepath.Join(root, "link")
				if err := os.Symlink(outside, link); err != nil {
					t.Fatalf("symlink: %v", err)
				}
				return root, "link/loot.txt"
			},
			wantErr: true,
		},
		{
			name: "symlink inside workspace pointing inside ok",
			setup: func(t *testing.T, tmp string) (string, string) {
				root := mkdir(t, tmp, "ws")
				target := mkdir(t, root, "real")
				writeFile(t, filepath.Join(target, "f.txt"), "x")
				link := filepath.Join(root, "link")
				if err := os.Symlink(target, link); err != nil {
					t.Fatalf("symlink: %v", err)
				}
				return root, "link/f.txt"
			},
			wantRel: "real/f.txt",
		},
		{
			name: "non-existent write target under real parent ok",
			setup: func(t *testing.T, tmp string) (string, string) {
				root := mkdir(t, tmp, "ws")
				mkdir(t, root, "out")
				return root, "out/new.txt"
			},
			wantRel: "out/new.txt",
		},
		{
			name: "non-existent nested write target with non-existent parents ok",
			setup: func(t *testing.T, tmp string) (string, string) {
				root := mkdir(t, tmp, "ws")
				return root, "a/b/c/new.txt"
			},
			wantRel: "a/b/c/new.txt",
		},
		{
			name: "non-existent target whose existing parent is a symlink outside rejected",
			setup: func(t *testing.T, tmp string) (string, string) {
				root := mkdir(t, tmp, "ws")
				outside := mkdir(t, tmp, "outside")
				link := filepath.Join(root, "link")
				if err := os.Symlink(outside, link); err != nil {
					t.Fatalf("symlink: %v", err)
				}
				// new.txt does not exist; its parent "link" resolves outside.
				return root, "link/new.txt"
			},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			tmp := t.TempDir()
			root, input := tt.setup(t, tmp)

			got, err := containedPath(root, input)

			if tt.wantErr {
				if err == nil {
					t.Fatalf("containedPath(%q, %q) = %q, want error", root, input, got)
				}
				var ce *ContainmentError
				if !errors.As(err, &ce) {
					t.Fatalf("error is not *ContainmentError: %T %v", err, err)
				}
				if got != "" {
					t.Errorf("fail-secure violated: got non-empty path %q with error", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("containedPath(%q, %q) unexpected error: %v", root, input, err)
			}
			// The returned path must always be inside the resolved root.
			assertInside(t, root, got)
			// And, for the success cases, it must equal the expected
			// root-relative location.
			if tt.wantRel != "" {
				gotRel := relToRoot(t, root, got)
				if gotRel != tt.wantRel {
					t.Errorf("containedPath(%q, %q) rel = %q, want %q", root, input, gotRel, tt.wantRel)
				}
			}
		})
	}
}

// TestContainedPathAbsoluteAnchoredUnderRoot verifies the documented treatment
// of absolute inputs: they are anchored under root (not honoured as absolute),
// so an absolute path "outside" root becomes a contained would-be path rather
// than escaping. This is the deliberate, fail-secure interpretation.
func TestContainedPathAbsoluteAnchoredUnderRoot(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	root := mkdir(t, tmp, "ws")
	abs := filepath.Join(tmp, "outside", "x.txt")

	got, err := containedPath(root, abs)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	assertInside(t, root, got)
}

// assertInside fails the test unless got is inside the EvalSymlinks-resolved
// root (re-checked independently with filepath.Rel, mirroring the production
// containment check).
func assertInside(t *testing.T, root, got string) {
	t.Helper()
	rRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatalf("EvalSymlinks(root): %v", err)
	}
	rRoot, err = filepath.Abs(rRoot)
	if err != nil {
		t.Fatalf("Abs(root): %v", err)
	}
	rel, err := filepath.Rel(rRoot, got)
	if err != nil {
		t.Fatalf("Rel(%q, %q): %v", rRoot, got, err)
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		t.Fatalf("returned path %q escapes root %q (rel=%q)", got, rRoot, rel)
	}
}

func mkdir(t *testing.T, parent, name string) string {
	t.Helper()
	p := filepath.Join(parent, name)
	if err := os.MkdirAll(p, 0o700); err != nil {
		t.Fatalf("mkdir %q: %v", p, err)
	}
	return p
}

func writeFile(t *testing.T, p, content string) {
	t.Helper()
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatalf("write %q: %v", p, err)
	}
}

// relToRoot returns got's path relative to the EvalSymlinks-resolved root,
// using forward slashes so table expectations stay OS-independent.
func relToRoot(t *testing.T, root, got string) string {
	t.Helper()
	rRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatalf("EvalSymlinks(root): %v", err)
	}
	rRoot, err = filepath.Abs(rRoot)
	if err != nil {
		t.Fatalf("Abs(root): %v", err)
	}
	rel, err := filepath.Rel(rRoot, got)
	if err != nil {
		t.Fatalf("Rel(%q, %q): %v", rRoot, got, err)
	}
	return filepath.ToSlash(rel)
}
