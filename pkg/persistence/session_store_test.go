package persistence

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"
)

func TestSessionDataRootFrom(t *testing.T) {
	tests := []struct {
		name        string
		xdgDataHome string
		home        string
		wantRoot    string
		wantApp     string
		wantErr     bool
	}{
		{
			name:        "xdg data home takes precedence",
			xdgDataHome: "/tmp/looprig-xdg",
			home:        "/tmp/looprig-home",
			wantRoot:    "/tmp/looprig-xdg",
			wantApp:     "looprig",
		},
		{
			name:     "home fallback uses looprig dot directory",
			home:     "/tmp/looprig-home",
			wantRoot: "/tmp/looprig-home",
			wantApp:  defaultDirName,
		},
		{
			name:        "empty roots are rejected",
			xdgDataHome: " ",
			home:        "",
			wantErr:     true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			gotRoot, gotApp, err := sessionDataRootFrom(tt.xdgDataHome, tt.home)
			if (err != nil) != tt.wantErr {
				t.Fatalf("sessionDataRootFrom() error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}
			if gotRoot != tt.wantRoot {
				t.Errorf("root = %q, want %q", gotRoot, tt.wantRoot)
			}
			if gotApp != tt.wantApp {
				t.Errorf("app = %q, want %q", gotApp, tt.wantApp)
			}
		})
	}
}

func TestOpenSessionStoreRootAt(t *testing.T) {
	tests := []struct {
		name      string
		appDir    string
		configure func(t *testing.T, dataRoot string) string
		wantErr   bool
	}{
		{
			name:   "creates private xdg session root",
			appDir: "looprig",
			configure: func(t *testing.T, dataRoot string) string {
				t.Helper()
				return dataRoot
			},
		},
		{
			name:   "creates a nonexistent configured xdg root",
			appDir: "looprig",
			configure: func(t *testing.T, dataRoot string) string {
				t.Helper()
				return filepath.Join(dataRoot, "not-created-yet")
			},
		},
		{
			name:   "normalizes existing app and sessions modes",
			appDir: "looprig",
			configure: func(t *testing.T, dataRoot string) string {
				t.Helper()
				appDir := filepath.Join(dataRoot, "looprig")
				if err := os.Mkdir(appDir, 0o755); err != nil {
					t.Fatalf("Mkdir(%q): %v", appDir, err)
				}
				if err := os.Mkdir(filepath.Join(appDir, sessionRootDirName), 0o755); err != nil {
					t.Fatalf("Mkdir(sessions): %v", err)
				}
				if err := os.Chmod(appDir, 0o755); err != nil {
					t.Fatalf("Chmod(%q): %v", appDir, err)
				}
				if err := os.Chmod(filepath.Join(appDir, sessionRootDirName), 0o755); err != nil {
					t.Fatalf("Chmod(sessions): %v", err)
				}
				return dataRoot
			},
		},
		{
			name:   "symlinked xdg root is rejected",
			appDir: "looprig",
			configure: func(t *testing.T, dataRoot string) string {
				t.Helper()
				link := filepath.Join(dataRoot, "xdg-link")
				if err := os.Symlink(t.TempDir(), link); err != nil {
					t.Fatalf("Symlink(): %v", err)
				}
				return link
			},
			wantErr: true,
		},
		{
			name:   "symlinked xdg root component is rejected",
			appDir: "looprig",
			configure: func(t *testing.T, dataRoot string) string {
				t.Helper()
				link := filepath.Join(dataRoot, "xdg-link")
				if err := os.Symlink(t.TempDir(), link); err != nil {
					t.Fatalf("Symlink(): %v", err)
				}
				return filepath.Join(link, "nested")
			},
			wantErr: true,
		},
		{
			name:   "symlinked home root is rejected",
			appDir: defaultDirName,
			configure: func(t *testing.T, dataRoot string) string {
				t.Helper()
				link := filepath.Join(dataRoot, "home-link")
				if err := os.Symlink(t.TempDir(), link); err != nil {
					t.Fatalf("Symlink(): %v", err)
				}
				return link
			},
			wantErr: true,
		},
		{
			name:   "root that is a regular file is rejected",
			appDir: "looprig",
			configure: func(t *testing.T, dataRoot string) string {
				t.Helper()
				path := filepath.Join(dataRoot, "file-root")
				if err := os.WriteFile(path, []byte("not a directory"), 0o600); err != nil {
					t.Fatalf("WriteFile(%q): %v", path, err)
				}
				return path
			},
			wantErr: true,
		},
		{
			name:   "traversal app directory is rejected",
			appDir: "../outside",
			configure: func(t *testing.T, dataRoot string) string {
				t.Helper()
				return dataRoot
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			dataRoot := t.TempDir()
			rootPath := tt.configure(t, dataRoot)
			root, err := openSessionStoreRootAt(rootPath, tt.appDir)
			if (err != nil) != tt.wantErr {
				t.Fatalf("openSessionStoreRootAt() error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				assertSessionStoreError(t, err)
				return
			}

			wantAppDir := filepath.Join(rootPath, tt.appDir)
			wantSessionsDir := filepath.Join(wantAppDir, sessionRootDirName)
			if root.appDir != wantAppDir {
				t.Errorf("appDir = %q, want %q", root.appDir, wantAppDir)
			}
			if root.sessionsDir != wantSessionsDir {
				t.Errorf("sessionsDir = %q, want %q", root.sessionsDir, wantSessionsDir)
			}
			assertPrivateDirectory(t, wantAppDir)
			assertPrivateDirectory(t, wantSessionsDir)
		})
	}
}

func TestConfinedChild(t *testing.T) {
	tests := []struct {
		name    string
		root    string
		child   string
		want    string
		wantErr bool
	}{
		{
			name:  "valid child remains beneath root",
			root:  "/tmp/looprig",
			child: "sessions",
			want:  "/tmp/looprig/sessions",
		},
		{
			name:    "parent traversal is rejected",
			root:    "/tmp/looprig",
			child:   "../outside",
			wantErr: true,
		},
		{
			name:    "absolute child is rejected",
			root:    "/tmp/looprig",
			child:   "/tmp/outside",
			wantErr: true,
		},
		{
			name:    "empty root is rejected",
			root:    "",
			child:   "sessions",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := confinedChild(tt.root, tt.child)
			if (err != nil) != tt.wantErr {
				t.Fatalf("confinedChild(%q, %q) error = %v, wantErr %v", tt.root, tt.child, err, tt.wantErr)
			}
			if !tt.wantErr && got != tt.want {
				t.Errorf("confinedChild(%q, %q) = %q, want %q", tt.root, tt.child, got, tt.want)
			}
		})
	}
}

func TestSessionDir(t *testing.T) {
	canonicalID := uuid.MustParse("5d65ceae-6ec4-4232-a4c6-628820c2ae31")

	tests := []struct {
		name      string
		id        uuid.UUID
		configure func(t *testing.T, root *SessionStoreRoot, id uuid.UUID)
		root      func(t *testing.T) *SessionStoreRoot
		wantErr   bool
	}{
		{
			name: "canonical uuid resolves beneath sessions root",
			id:   canonicalID,
		},
		{
			name:    "zero uuid is rejected",
			id:      uuid.UUID{},
			wantErr: true,
		},
		{
			name: "symlinked session directory root escape is rejected",
			id:   canonicalID,
			configure: func(t *testing.T, root *SessionStoreRoot, id uuid.UUID) {
				t.Helper()
				if err := os.Symlink(t.TempDir(), filepath.Join(root.sessionsDir, id.String())); err != nil {
					t.Fatalf("Symlink(): %v", err)
				}
			},
			wantErr: true,
		},
		{
			name: "nil root is rejected",
			id:   canonicalID,
			root: func(t *testing.T) *SessionStoreRoot {
				t.Helper()
				return nil
			},
			wantErr: true,
		},
		{
			name: "sessions root outside app root is rejected",
			id:   canonicalID,
			root: func(t *testing.T) *SessionStoreRoot {
				t.Helper()
				return &SessionStoreRoot{appDir: t.TempDir(), sessionsDir: t.TempDir()}
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			root := newTestSessionStoreRoot(t)
			if tt.root != nil {
				root = tt.root(t)
			}
			if tt.configure != nil {
				tt.configure(t, root, tt.id)
			}

			got, err := root.SessionDir(tt.id)
			if (err != nil) != tt.wantErr {
				t.Fatalf("SessionDir(%q) error = %v, wantErr %v", tt.id, err, tt.wantErr)
			}
			if tt.wantErr {
				assertSessionStoreError(t, err)
				return
			}
			want := filepath.Join(root.sessionsDir, canonicalID.String())
			if got != want {
				t.Errorf("SessionDir(%q) = %q, want %q", tt.id, got, want)
			}
		})
	}
}

func TestCreateSessionDir(t *testing.T) {
	canonicalID := uuid.MustParse("76759057-6978-44d1-ae52-1582430fd117")

	tests := []struct {
		name      string
		id        uuid.UUID
		configure func(t *testing.T, root *SessionStoreRoot, id uuid.UUID)
		root      func(t *testing.T) *SessionStoreRoot
		wantErr   bool
	}{
		{
			name: "creates canonical private session directory",
			id:   canonicalID,
		},
		{
			name:    "zero uuid is rejected",
			id:      uuid.UUID{},
			wantErr: true,
		},
		{
			name: "symlinked session directory traversal is rejected",
			id:   canonicalID,
			configure: func(t *testing.T, root *SessionStoreRoot, id uuid.UUID) {
				t.Helper()
				if err := os.Symlink(t.TempDir(), filepath.Join(root.sessionsDir, id.String())); err != nil {
					t.Fatalf("Symlink(): %v", err)
				}
			},
			wantErr: true,
		},
		{
			name: "nil root is rejected",
			id:   canonicalID,
			root: func(t *testing.T) *SessionStoreRoot {
				t.Helper()
				return nil
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			root := newTestSessionStoreRoot(t)
			if tt.root != nil {
				root = tt.root(t)
			}
			if tt.configure != nil {
				tt.configure(t, root, tt.id)
			}

			got, err := root.CreateSessionDir(tt.id)
			if (err != nil) != tt.wantErr {
				t.Fatalf("CreateSessionDir(%q) error = %v, wantErr %v", tt.id, err, tt.wantErr)
			}
			if tt.wantErr {
				assertSessionStoreError(t, err)
				return
			}
			want := filepath.Join(root.sessionsDir, canonicalID.String())
			if got != want {
				t.Errorf("CreateSessionDir(%q) = %q, want %q", tt.id, got, want)
			}
			assertPrivateDirectory(t, got)
		})
	}
}

func newTestSessionStoreRoot(t *testing.T) *SessionStoreRoot {
	t.Helper()

	root, err := openSessionStoreRootAt(t.TempDir(), "looprig")
	if err != nil {
		t.Fatalf("openSessionStoreRootAt(): %v", err)
	}
	return root
}

func assertPrivateDirectory(t *testing.T, path string) {
	t.Helper()

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat(%q): %v", path, err)
	}
	if !info.IsDir() {
		t.Fatalf("%q is not a directory", path)
	}
	if got := info.Mode().Perm(); got != storeDirPerm {
		t.Errorf("directory mode = %#o, want %#o", got, storeDirPerm)
	}
}

func assertSessionStoreError(t *testing.T, err error) {
	t.Helper()

	var storeErr *SessionStoreError
	if !errors.As(err, &storeErr) {
		t.Fatalf("error = %T %v, want *SessionStoreError", err, err)
	}
}

func TestPurgeLegacyStore(t *testing.T) {
	t.Run("removes legacy directory with files", func(t *testing.T) {
		root := newTestSessionStoreRoot(t)
		legacy := filepath.Join(root.appDir, jetstreamDirName)
		if err := os.MkdirAll(filepath.Join(legacy, "streams"), 0o700); err != nil {
			t.Fatalf("seed legacy: %v", err)
		}
		if err := os.WriteFile(filepath.Join(legacy, "streams", "data"), []byte("x"), 0o600); err != nil {
			t.Fatalf("seed file: %v", err)
		}

		result, err := root.PurgeLegacyStore()
		if err != nil {
			t.Fatalf("PurgeLegacyStore: %v", err)
		}
		if !result.Removed {
			t.Errorf("Removed = false, want true")
		}
		if result.Path != legacy {
			t.Errorf("Path = %q, want %q", result.Path, legacy)
		}
		if _, err := os.Lstat(legacy); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("legacy dir still present: err = %v", err)
		}
	})

	t.Run("absent legacy directory is a no-op", func(t *testing.T) {
		root := newTestSessionStoreRoot(t)

		result, err := root.PurgeLegacyStore()
		if err != nil {
			t.Fatalf("PurgeLegacyStore: %v", err)
		}
		if result.Removed {
			t.Errorf("Removed = true, want false for absent legacy store")
		}
	})

	t.Run("symlinked legacy path is rejected", func(t *testing.T) {
		root := newTestSessionStoreRoot(t)
		target := t.TempDir()
		sentinel := filepath.Join(target, "keep")
		if err := os.WriteFile(sentinel, []byte("survive"), 0o600); err != nil {
			t.Fatalf("seed sentinel: %v", err)
		}
		legacy := filepath.Join(root.appDir, jetstreamDirName)
		if err := os.Symlink(target, legacy); err != nil {
			t.Fatalf("Symlink: %v", err)
		}

		if _, err := root.PurgeLegacyStore(); err == nil {
			t.Fatal("PurgeLegacyStore on a symlink succeeded, want error")
		} else {
			assertSessionStoreError(t, err)
		}
		if _, err := os.Lstat(sentinel); err != nil {
			t.Errorf("symlink target was followed and deleted: %v", err)
		}
	})

	t.Run("sessions root and log sibling survive", func(t *testing.T) {
		root := newTestSessionStoreRoot(t)
		legacy := filepath.Join(root.appDir, jetstreamDirName)
		if err := os.MkdirAll(legacy, 0o700); err != nil {
			t.Fatalf("seed legacy: %v", err)
		}
		logSibling := filepath.Join(root.appDir, "looprig.log")
		if err := os.WriteFile(logSibling, []byte("log"), 0o600); err != nil {
			t.Fatalf("seed log: %v", err)
		}

		if _, err := root.PurgeLegacyStore(); err != nil {
			t.Fatalf("PurgeLegacyStore: %v", err)
		}
		if _, err := os.Stat(root.sessionsDir); err != nil {
			t.Errorf("sessions root removed: %v", err)
		}
		if _, err := os.Stat(logSibling); err != nil {
			t.Errorf("log sibling removed: %v", err)
		}
	})

	t.Run("malformed root is refused", func(t *testing.T) {
		root := &SessionStoreRoot{appDir: t.TempDir(), sessionsDir: t.TempDir()}
		if _, err := root.PurgeLegacyStore(); err == nil {
			t.Fatal("PurgeLegacyStore on a malformed root succeeded, want error")
		} else {
			assertSessionStoreError(t, err)
		}
	})
}
