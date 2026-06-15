package tools

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// FuzzContainedPath drives containedPath with arbitrary input strings while
// PINNING the root to a per-run t.TempDir(). The fuzzer NEVER passes a host
// path: root is always the temp dir, and the fuzz bytes are only ever used as
// the (untrusted) input argument — exactly how a tool would receive a
// caller-supplied relative path. This means the only filesystem the resolver
// can ever traverse out of is the temp dir, and the invariant we assert
// (result is inside root, or a typed error) is what production relies on.
//
// To exercise the symlink-resolution paths without escaping into the host, the
// fixture plants, inside the temp root: a regular file, a nested dir, an
// in-root symlink, and an escaping symlink that points to a sibling temp dir.
func FuzzContainedPath(f *testing.F) {
	seeds := []string{
		"",
		".",
		"a.go",
		"src/main.go",
		"../escape",
		"a/b/../../../etc/passwd",
		"link/x",        // through an in-root symlink (planted below)
		"escape/loot",   // through an escaping symlink (planted below)
		"out/new.txt",   // non-existent write target under real parent
		"a/b/c/new.txt", // deep non-existent target
		"/absolute/path",
		"////",
		strings.Repeat("../", 64) + "etc",
		"a/\x00/b", // embedded NUL
	}
	for _, s := range seeds {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, input string) {
		// Fresh, isolated fixture per execution. t.TempDir() is auto-removed.
		root := t.TempDir()
		// A sibling temp dir that an escaping symlink can target — still under
		// the test's own temp space, never a host path.
		sibling := t.TempDir()

		// Plant fixtures (best-effort; ignore errors so a hostile input never
		// blocks the run, but the planting inputs here are fixed and safe).
		_ = os.WriteFile(filepath.Join(root, "a.go"), []byte("x"), 0o600)
		_ = os.MkdirAll(filepath.Join(root, "src"), 0o700)
		_ = os.WriteFile(filepath.Join(root, "src", "main.go"), []byte("x"), 0o600)
		_ = os.MkdirAll(filepath.Join(root, "out"), 0o700)
		realDir := filepath.Join(root, "real")
		_ = os.MkdirAll(realDir, 0o700)
		_ = os.Symlink(realDir, filepath.Join(root, "link"))   // in-root link
		_ = os.Symlink(sibling, filepath.Join(root, "escape")) // escaping link

		got, err := containedPath(root, input)

		if err != nil {
			// Every error must be the typed ContainmentError (fail-secure) and
			// carry no path.
			var ce *ContainmentError
			if !errors.As(err, &ce) {
				t.Fatalf("error is not *ContainmentError: %T %v", err, err)
			}
			if got != "" {
				t.Fatalf("fail-secure violated: non-empty path %q with error %v", got, err)
			}
			return
		}

		// Success: the returned path MUST be inside the resolved root.
		rRoot, evErr := filepath.EvalSymlinks(root)
		if evErr != nil {
			t.Fatalf("EvalSymlinks(root): %v", evErr)
		}
		rRoot, evErr = filepath.Abs(rRoot)
		if evErr != nil {
			t.Fatalf("Abs(root): %v", evErr)
		}
		rel, relErr := filepath.Rel(rRoot, got)
		if relErr != nil {
			t.Fatalf("Rel(%q, %q): %v", rRoot, got, relErr)
		}
		if rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
			t.Fatalf("containedPath returned escaping path: input=%q got=%q rel=%q root=%q",
				input, got, rel, rRoot)
		}
	})
}
