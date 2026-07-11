package pathutil

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"
)

func TestCanonicalize(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		setup   func(t *testing.T) (paths []string, want []string)
		wantErr bool
	}{
		{
			name: "existing paths are sorted and deduplicated",
			setup: func(t *testing.T) ([]string, []string) {
				first := t.TempDir()
				second := t.TempDir()
				wantFirst, err := filepath.EvalSymlinks(first)
				if err != nil {
					t.Fatalf("EvalSymlinks(%q): %v", first, err)
				}
				wantSecond, err := filepath.EvalSymlinks(second)
				if err != nil {
					t.Fatalf("EvalSymlinks(%q): %v", second, err)
				}
				want := []string{wantFirst, wantSecond}
				sort.Strings(want)
				return []string{second, first, first, ""}, want
			},
		},
		{
			name: "missing tail below symlink ancestor is canonicalized",
			setup: func(t *testing.T) ([]string, []string) {
				target := t.TempDir()
				alias := filepath.Join(t.TempDir(), "alias")
				if err := os.Symlink(target, alias); err != nil {
					t.Fatalf("Symlink(%q, %q): %v", target, alias, err)
				}
				path := filepath.Join(alias, "missing", "tail")
				wantTarget, err := filepath.EvalSymlinks(target)
				if err != nil {
					t.Fatalf("EvalSymlinks(%q): %v", target, err)
				}
				return []string{path}, []string{filepath.Join(wantTarget, "missing", "tail")}
			},
		},
		{
			name: "broken symlink fails closed",
			setup: func(t *testing.T) ([]string, []string) {
				base := t.TempDir()
				alias := filepath.Join(base, "broken")
				if err := os.Symlink(filepath.Join(base, "absent"), alias); err != nil {
					t.Fatalf("Symlink(broken): %v", err)
				}
				return []string{filepath.Join(alias, "tail")}, nil
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			paths, want := tt.setup(t)
			got, err := Canonicalize(paths)
			if (err != nil) != tt.wantErr {
				t.Fatalf("Canonicalize() err = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				var pathErr *CanonicalPathError
				if !errors.As(err, &pathErr) {
					t.Fatalf("Canonicalize() err = %T %v, want *CanonicalPathError", err, err)
				}
				if !errors.Is(err, fs.ErrNotExist) {
					t.Errorf("errors.Is(err, fs.ErrNotExist) = false, want true")
				}
				return
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("Canonicalize() = %v, want %v", got, want)
			}
		})
	}
}
