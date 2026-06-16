package eval

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestLoadCases(t *testing.T) {
	t.Parallel()
	t.Run("loads json sorted by filename, ignoring non-json", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		mustWrite(t, filepath.Join(dir, "b.json"), `{"name":"second","input":"i2","expectedOutput":"o2"}`)
		mustWrite(t, filepath.Join(dir, "a.json"), `{"name":"first","input":"i1","expectedOutput":"o1"}`)
		mustWrite(t, filepath.Join(dir, "notes.txt"), `ignored`)
		cases, err := LoadCases(dir)
		if err != nil {
			t.Fatalf("LoadCases() error = %v", err)
		}
		if len(cases) != 2 {
			t.Fatalf("len = %d, want 2", len(cases))
		}
		if cases[0].Name != "first" || cases[1].Name != "second" {
			t.Errorf("order = %q,%q, want first,second", cases[0].Name, cases[1].Name)
		}
	})
	t.Run("missing dir is a LoadError", func(t *testing.T) {
		t.Parallel()
		_, err := LoadCases(filepath.Join(t.TempDir(), "nope"))
		var le *LoadError
		if !errors.As(err, &le) {
			t.Fatalf("error = %v, want *LoadError", err)
		}
	})
	t.Run("malformed json is a LoadError", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		mustWrite(t, filepath.Join(dir, "bad.json"), `{not json`)
		_, err := LoadCases(dir)
		var le *LoadError
		if !errors.As(err, &le) {
			t.Fatalf("error = %v, want *LoadError", err)
		}
	})
	t.Run("symlink escaping the dir is not followed", func(t *testing.T) {
		t.Parallel()
		outside := t.TempDir()
		secret := filepath.Join(outside, "secret.json")
		mustWrite(t, secret, `{"name":"leaked","input":"secret"}`)

		dir := t.TempDir()
		// A *.json entry that symlinks to a file outside dir must be rejected
		// by os.Root containment, not silently read.
		if err := os.Symlink(secret, filepath.Join(dir, "escape.json")); err != nil {
			t.Skipf("symlink unsupported on this platform: %v", err)
		}
		_, err := LoadCases(dir)
		var le *LoadError
		if !errors.As(err, &le) {
			t.Fatalf("error = %v, want *LoadError for escaping symlink", err)
		}
	})
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
