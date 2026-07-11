package workspacestore

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"

	"github.com/looprig/storage"
	"github.com/looprig/storage/memstore"
)

type reportingBlobs struct {
	storage.Blobs
	paths []string
}

func (b reportingBlobs) StoragePaths() []string { return b.paths }

func sortedPaths(paths ...string) []string {
	sort.Strings(paths)
	return paths
}

// TestOpenResolvesLimitDefaults guards the wiring the plan called out explicitly:
// Open must resolve the extraction bounds to their defaults when the caller
// leaves them unset (or passes a non-positive value), and must honor an explicit
// override. A regression here would leave the decompression-bomb guards at zero
// — i.e. rejecting every archive — so the defaults must actually be applied.
func TestOpenResolvesLimitDefaults(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		opts           []Option
		wantMaxEntries int64
		wantMaxBytes   int64
	}{
		{
			name:           "no options resolves both defaults",
			wantMaxEntries: defaultMaxEntries,
			wantMaxBytes:   defaultMaxBytes,
		},
		{
			name:           "explicit positive values are kept",
			opts:           []Option{WithMaxEntries(7), WithMaxBytes(4096)},
			wantMaxEntries: 7,
			wantMaxBytes:   4096,
		},
		{
			name:           "zero values resolve to defaults",
			opts:           []Option{WithMaxEntries(0), WithMaxBytes(0)},
			wantMaxEntries: defaultMaxEntries,
			wantMaxBytes:   defaultMaxBytes,
		},
		{
			name:           "negative values resolve to defaults",
			opts:           []Option{WithMaxEntries(-1), WithMaxBytes(-1)},
			wantMaxEntries: defaultMaxEntries,
			wantMaxBytes:   defaultMaxBytes,
		},
		{
			name:           "only entries overridden keeps byte default",
			opts:           []Option{WithMaxEntries(3)},
			wantMaxEntries: 3,
			wantMaxBytes:   defaultMaxBytes,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			s, err := Open(memstore.New().Blobs, tt.opts...)
			if err != nil {
				t.Fatalf("Open(): %v", err)
			}
			if s.opts.MaxEntries != tt.wantMaxEntries {
				t.Errorf("opts.MaxEntries = %d, want %d", s.opts.MaxEntries, tt.wantMaxEntries)
			}
			if s.opts.MaxBytes != tt.wantMaxBytes {
				t.Errorf("opts.MaxBytes = %d, want %d", s.opts.MaxBytes, tt.wantMaxBytes)
			}
		})
	}
}

func TestPersistencePaths(t *testing.T) {
	t.Parallel()

	base := t.TempDir()
	first := filepath.Join(base, "a")
	second := filepath.Join(base, "b")
	for _, path := range []string{first, second} {
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatalf("Mkdir(%q): %v", path, err)
		}
	}
	alias := filepath.Join(base, "alias")
	if err := os.Symlink(first, alias); err != nil {
		t.Fatalf("Symlink(%q, %q): %v", first, alias, err)
	}
	wantFirst, err := filepath.EvalSymlinks(first)
	if err != nil {
		t.Fatalf("EvalSymlinks(%q): %v", first, err)
	}
	wantSecond, err := filepath.EvalSymlinks(second)
	if err != nil {
		t.Fatalf("EvalSymlinks(%q): %v", second, err)
	}
	wantDefaultSpool, err := filepath.EvalSymlinks(os.TempDir())
	if err != nil {
		t.Fatalf("EvalSymlinks(default spool): %v", err)
	}

	tests := []struct {
		name  string
		blobs storage.Blobs
		want  []string
	}{
		{
			name:  "remote provider reports default spool",
			blobs: memstore.New().Blobs,
			want:  []string{wantDefaultSpool},
		},
		{
			name:  "empty reporter paths report default spool",
			blobs: reportingBlobs{Blobs: memstore.New().Blobs, paths: []string{""}},
			want:  []string{wantDefaultSpool},
		},
		{
			name: "reporter paths are canonical sorted and deduplicated",
			blobs: reportingBlobs{
				Blobs: memstore.New().Blobs,
				paths: []string{
					second,
					alias,
					filepath.Join(first, "."),
					filepath.Join(base, "a", "..", "b"),
					"",
				},
			},
			want: sortedPaths(wantFirst, wantSecond, wantDefaultSpool),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			st, err := Open(tt.blobs)
			if err != nil {
				t.Fatalf("Open() err = %v", err)
			}
			got, err := st.PersistencePaths()
			if err != nil {
				t.Fatalf("PersistencePaths() err = %v", err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("PersistencePaths() = %v, want %v", got, tt.want)
			}
			if len(got) == 0 {
				return
			}
			got[0] = filepath.Join(base, "mutated")
			next, err := st.PersistencePaths()
			if err != nil {
				t.Fatalf("PersistencePaths() after caller mutation err = %v", err)
			}
			if !reflect.DeepEqual(next, tt.want) {
				t.Errorf("PersistencePaths() after caller mutation = %v, want %v", next, tt.want)
			}
		})
	}
}

func TestPersistencePathsIncludesSpool(t *testing.T) {
	t.Parallel()

	explicit := t.TempDir()
	wantExplicit, err := filepath.EvalSymlinks(explicit)
	if err != nil {
		t.Fatalf("EvalSymlinks(explicit spool): %v", err)
	}
	wantDefault, err := filepath.EvalSymlinks(os.TempDir())
	if err != nil {
		t.Fatalf("EvalSymlinks(default spool): %v", err)
	}
	target := t.TempDir()
	wantTarget, err := filepath.EvalSymlinks(target)
	if err != nil {
		t.Fatalf("EvalSymlinks(alias target): %v", err)
	}
	alias := filepath.Join(t.TempDir(), "spool-alias")
	if err := os.Symlink(target, alias); err != nil {
		t.Fatalf("Symlink(%q, %q): %v", target, alias, err)
	}
	missingTail := filepath.Join(alias, "missing", "tail")
	wantMissingTail := filepath.Join(wantTarget, "missing", "tail")

	tests := []struct {
		name  string
		blobs storage.Blobs
		opts  []Option
		want  []string
	}{
		{
			name:  "explicit spool",
			blobs: memstore.New().Blobs,
			opts:  []Option{WithSpoolDir(explicit)},
			want:  []string{wantExplicit},
		},
		{
			name:  "default spool",
			blobs: memstore.New().Blobs,
			want:  []string{wantDefault},
		},
		{
			name: "spool deduplicates with provider",
			blobs: reportingBlobs{
				Blobs: memstore.New().Blobs,
				paths: []string{explicit},
			},
			opts: []Option{WithSpoolDir(explicit)},
			want: []string{wantExplicit},
		},
		{
			name:  "spool symlink alias resolves to target",
			blobs: memstore.New().Blobs,
			opts:  []Option{WithSpoolDir(alias)},
			want:  []string{wantTarget},
		},
		{
			name:  "missing tail below spool symlink ancestor is canonicalized",
			blobs: memstore.New().Blobs,
			opts:  []Option{WithSpoolDir(missingTail)},
			want:  []string{wantMissingTail},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			st, err := Open(tt.blobs, tt.opts...)
			if err != nil {
				t.Fatalf("Open() err = %v", err)
			}
			got, err := st.PersistencePaths()
			if err != nil {
				t.Fatalf("PersistencePaths() err = %v", err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("PersistencePaths() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestOpenRejectsAmbiguousSpool(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		spool func(t *testing.T) string
	}{
		{
			name: "broken spool symlink",
			spool: func(t *testing.T) string {
				base := t.TempDir()
				broken := filepath.Join(base, "broken-spool")
				if err := os.Symlink(filepath.Join(base, "absent"), broken); err != nil {
					t.Fatalf("Symlink(broken spool): %v", err)
				}
				return filepath.Join(broken, "tail")
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			store, err := Open(memstore.New().Blobs, WithSpoolDir(tt.spool(t)))
			var pathErr *PersistencePathError
			if !errors.As(err, &pathErr) {
				t.Fatalf("Open() err = %T %v, want *PersistencePathError", err, err)
			}
			if store != nil {
				t.Errorf("Open() store = %v on error, want nil", store)
			}
		})
	}
}
