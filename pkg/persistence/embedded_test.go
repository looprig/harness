package persistence

import (
	"errors"
	"path/filepath"
	"testing"
	"time"
)

// TestResolveStoreDir covers the security-sensitive StoreDir resolution: a path must be
// cleaned and confined within the home root (no traversal escape), and an empty/invalid
// input fails closed with a typed error. This is the pure, server-free half of the
// embedded-engine wiring.
func TestResolveStoreDir(t *testing.T) {
	t.Parallel()

	home := "/home/user"
	tests := []struct {
		name    string
		home    string
		dataDir string
		want    string
		wantErr bool
	}{
		{
			name:    "default under home",
			home:    home,
			dataDir: filepath.Join(home, defaultDirName, jetstreamDirName),
			want:    filepath.Join(home, defaultDirName, jetstreamDirName),
		},
		{
			name:    "nested under home is allowed",
			home:    home,
			dataDir: filepath.Join(home, defaultDirName, jetstreamDirName, "sub"),
			want:    filepath.Join(home, defaultDirName, jetstreamDirName, "sub"),
		},
		{
			name:    "uncleaned path is cleaned",
			home:    home,
			dataDir: home + "/.urvi/./jetstream/../jetstream",
			want:    filepath.Join(home, defaultDirName, jetstreamDirName),
		},
		{
			name:    "traversal escaping home is rejected",
			home:    home,
			dataDir: filepath.Join(home, "..", "..", "etc", "urvi"),
			wantErr: true,
		},
		{
			name:    "empty data dir is rejected",
			home:    home,
			dataDir: "",
			wantErr: true,
		},
		{
			name:    "empty home is rejected",
			home:    "",
			dataDir: filepath.Join(home, defaultDirName),
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := resolveStoreDir(tt.home, tt.dataDir)
			if (err != nil) != tt.wantErr {
				t.Fatalf("resolveStoreDir(%q, %q) err = %v, wantErr %v", tt.home, tt.dataDir, err, tt.wantErr)
			}
			if tt.wantErr {
				var pe *StoreDirError
				if !errors.As(err, &pe) {
					t.Fatalf("err = %v, want *StoreDirError", err)
				}
				return
			}
			if got != tt.want {
				t.Errorf("resolveStoreDir = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestDefaultEngineOptions proves the defaults are sane: a non-empty data dir under the
// user's .urvi dir and a positive, explicit SyncInterval (the power-loss durability knob
// the design's round 5 requires be set, not left to the server default).
func TestDefaultEngineOptions(t *testing.T) {
	t.Parallel()

	opts, err := DefaultEngineOptions()
	if err != nil {
		t.Fatalf("DefaultEngineOptions: %v", err)
	}
	if opts.DataDir == "" {
		t.Error("DataDir is empty")
	}
	if filepath.Base(opts.DataDir) != jetstreamDirName {
		t.Errorf("DataDir base = %q, want %q", filepath.Base(opts.DataDir), jetstreamDirName)
	}
	if opts.SyncInterval <= 0 {
		t.Errorf("SyncInterval = %v, want a positive explicit value", opts.SyncInterval)
	}
	// Conservative: a few seconds, not sub-second (we are not optimizing throughput) and
	// not minutes (power-loss window must stay small).
	if opts.SyncInterval < time.Second || opts.SyncInterval > time.Minute {
		t.Errorf("SyncInterval = %v, want a conservative few-seconds value", opts.SyncInterval)
	}
}
