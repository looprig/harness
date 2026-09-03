package sessionruntime

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/looprig/core/uuid"
)

func TestPlacementRootFor(t *testing.T) {
	t.Parallel()
	sid, err := uuid.New()
	if err != nil {
		t.Fatalf("uuid.New: %v", err)
	}
	tests := []struct {
		name      string
		placement WorkspacePlacement
		sid       uuid.UUID
		wantErr   bool
		check     func(t *testing.T, root string)
	}{
		{
			name:      "exclusive returns fixed root",
			placement: WorkspacePlacement{Mode: PlacementExclusive, Root: "/fixed/root"},
			sid:       sid,
			check:     func(t *testing.T, root string) { mustEqual(t, root, "/fixed/root") },
		},
		{
			name:      "shared returns fixed root",
			placement: WorkspacePlacement{Mode: PlacementShared, Root: "/shared/root"},
			sid:       sid,
			check:     func(t *testing.T, root string) { mustEqual(t, root, "/shared/root") },
		},
		{
			name:      "per-session is injective baseDir/sid",
			placement: WorkspacePlacement{Mode: PlacementSession, BaseDir: "/base"},
			sid:       sid,
			check:     func(t *testing.T, root string) { mustEqual(t, root, filepath.Join("/base", sid.String())) },
		},
		{
			name:      "per-session zero sid rejected",
			placement: WorkspacePlacement{Mode: PlacementSession, BaseDir: "/base"},
			sid:       uuid.UUID{},
			wantErr:   true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := tt.placement.rootFor(tt.sid)
			if (err != nil) != tt.wantErr {
				t.Fatalf("rootFor err = %v, wantErr %v", err, tt.wantErr)
			}
			if !tt.wantErr {
				tt.check(t, got)
			}
		})
	}
}

func mustEqual(t *testing.T, got, want string) {
	t.Helper()
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestRecoverSessionRoot(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name          string
		setup         func(t *testing.T, root string)
		wantRootFiles []string
		wantStaging   bool
	}{
		{
			name:  "brand-new root is created empty",
			setup: func(*testing.T, string) {},
		},
		{
			name: "abandoned staging is removed",
			setup: func(t *testing.T, root string) {
				mustMkdir(t, sessionStagingPath(root))
				mustWriteFile(t, filepath.Join(sessionStagingPath(root), "junk"), "x")
				mustMkdir(t, root)
			},
		},
		{
			name: "orphaned backup restored when root absent",
			setup: func(t *testing.T, root string) {
				mustMkdir(t, sessionBackupPath(root))
				mustWriteFile(t, filepath.Join(sessionBackupPath(root), "kept"), "y")
			},
			wantRootFiles: []string{"kept"},
		},
		{
			name: "stale backup removed when root present",
			setup: func(t *testing.T, root string) {
				mustMkdir(t, root)
				mustWriteFile(t, filepath.Join(root, "live"), "z")
				mustMkdir(t, sessionBackupPath(root))
			},
			wantRootFiles: []string{"live"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			base := realTempDir(t)
			root := filepath.Join(base, "ws")
			tt.setup(t, root)
			if err := recoverSessionRoot(root); err != nil {
				t.Fatalf("recoverSessionRoot: %v", err)
			}
			if pathExists(sessionStagingPath(root)) {
				t.Fatalf("staging dir not removed")
			}
			if pathExists(sessionBackupPath(root)) {
				t.Fatalf("backup dir not removed")
			}
			if !pathExists(root) {
				t.Fatal("live root was not recovered or created")
			}
			for _, want := range tt.wantRootFiles {
				if !pathExists(filepath.Join(root, want)) {
					t.Fatalf("expected root file %q missing", want)
				}
			}
		})
	}
}

func TestRecoverSessionRootCreatesFinalDirectoryExclusively(t *testing.T) {
	base := realTempDir(t)
	root := filepath.Join(base, "session-id")
	if err := recoverSessionRoot(root); err != nil {
		t.Fatalf("recoverSessionRoot: %v", err)
	}
	info, err := os.Lstat(root)
	if err != nil {
		t.Fatal(err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0o700 {
		t.Fatalf("created root mode = %v, want real directory 0700", info.Mode())
	}
}

func TestRecoverSessionRootRejectsUnsafeDestinationsWithoutOutsideMutation(t *testing.T) {
	tests := []struct {
		name  string
		setup func(t *testing.T) (root, protected string)
	}{
		{
			name: "existing root symlink",
			setup: func(t *testing.T) (string, string) {
				base, outside := realTempDir(t), realTempDir(t)
				protected := filepath.Join(outside, "sentinel")
				mustWriteFile(t, protected, "outside")
				root := filepath.Join(base, "session-id")
				if err := os.Symlink(outside, root); err != nil {
					t.Fatal(err)
				}
				return root, protected
			},
		},
		{
			name: "existing root regular file",
			setup: func(t *testing.T) (string, string) {
				root := filepath.Join(realTempDir(t), "session-id")
				mustWriteFile(t, root, "do not replace")
				return root, root
			},
		},
		{
			name: "orphan backup symlink",
			setup: func(t *testing.T) (string, string) {
				base, outside := realTempDir(t), realTempDir(t)
				protected := filepath.Join(outside, "sentinel")
				mustWriteFile(t, protected, "outside")
				root := filepath.Join(base, "session-id")
				if err := os.Symlink(outside, sessionBackupPath(root)); err != nil {
					t.Fatal(err)
				}
				return root, protected
			},
		},
		{
			name: "orphan backup regular file",
			setup: func(t *testing.T) (string, string) {
				root := filepath.Join(realTempDir(t), "session-id")
				backup := sessionBackupPath(root)
				mustWriteFile(t, backup, "do not rename")
				return root, backup
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root, protected := tt.setup(t)
			err := recoverSessionRoot(root)
			var recovery *WorkspaceRecoveryError
			if !errors.As(err, &recovery) || recovery.Path == "" || recovery.Reason == "" {
				t.Fatalf("recover error = %T %v, want typed unsafe recovery error", err, err)
			}
			if _, err := os.Lstat(protected); err != nil {
				t.Fatalf("protected outside/path mutated: %v", err)
			}
		})
	}
}

func TestRecoverSessionRootRejectsBaseParentSymlinkSubstitution(t *testing.T) {
	holder, outside := realTempDir(t), realTempDir(t)
	base := filepath.Join(holder, "base")
	if err := os.Symlink(outside, base); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(base, "session-id")
	err := recoverSessionRoot(root)
	var recovery *WorkspaceRecoveryError
	if !errors.As(err, &recovery) || recovery.Path != base || recovery.Reason == "" {
		t.Fatalf("recover error = %T %v, want parent substitution refusal", err, err)
	}
	if _, err := os.Lstat(filepath.Join(outside, "session-id")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("outside session path was created: %v", err)
	}
}

func TestRecoverSessionRootRemovesStagingSymlinkOnly(t *testing.T) {
	base, outside := realTempDir(t), realTempDir(t)
	root := filepath.Join(base, "session-id")
	mustMkdir(t, root)
	protected := filepath.Join(outside, "sentinel")
	mustWriteFile(t, protected, "outside")
	if err := os.Symlink(outside, sessionStagingPath(root)); err != nil {
		t.Fatal(err)
	}
	if err := recoverSessionRoot(root); err != nil {
		t.Fatalf("recoverSessionRoot: %v", err)
	}
	if pathExists(sessionStagingPath(root)) {
		t.Fatal("staging symlink survived recovery")
	}
	if _, err := os.Lstat(protected); err != nil {
		t.Fatalf("staging symlink target mutated: %v", err)
	}
}

func realTempDir(t *testing.T) string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return resolved
}

func TestRootIsEmpty(t *testing.T) {
	t.Parallel()
	base := t.TempDir()

	absent := filepath.Join(base, "absent")
	if empty, err := rootIsEmpty(absent); err != nil || !empty {
		t.Fatalf("absent: empty=%v err=%v, want true nil", empty, err)
	}

	emptyDir := filepath.Join(base, "empty")
	mustMkdir(t, emptyDir)
	if empty, err := rootIsEmpty(emptyDir); err != nil || !empty {
		t.Fatalf("empty: empty=%v err=%v, want true nil", empty, err)
	}

	full := filepath.Join(base, "full")
	mustMkdir(t, full)
	mustWriteFile(t, filepath.Join(full, "f"), "x")
	if empty, err := rootIsEmpty(full); err != nil || empty {
		t.Fatalf("full: empty=%v err=%v, want false nil", empty, err)
	}
}

func TestRootLeaseHealth(t *testing.T) {
	t.Parallel()
	// nil health is always healthy.
	var nilHealth *rootLeaseHealth
	if err := nilHealth.Healthy(); err != nil {
		t.Fatalf("nil health = %v, want nil", err)
	}
	open := make(chan struct{})
	live := &rootLeaseHealth{lost: open}
	if err := live.Healthy(); err != nil {
		t.Fatalf("open lease health = %v, want nil", err)
	}
	closed := make(chan struct{})
	close(closed)
	lost := &rootLeaseHealth{lost: closed}
	var target *WorkspaceRootLeaseLostError
	if err := lost.Healthy(); err == nil {
		t.Fatalf("lost lease health = nil, want error")
	} else if !errors.As(err, &target) {
		t.Fatalf("lost lease health = %T, want *WorkspaceRootLeaseLostError", err)
	}
}

func mustMkdir(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("MkdirAll %s: %v", dir, err)
	}
}

func mustWriteFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile %s: %v", path, err)
	}
}

// TestLogicalWorkspaceRootIsDerivedFromSessionIdentityAlone pins the identity half of
// spec §11.1: the model-visible workspace path is a function of the session id and of
// NOTHING else, so two Hosts whose runtime roots differ still expose one path. It is a
// derivation test on purpose — a derivation that consulted the physical root would be
// indistinguishable in an end-to-end probe that happened to reuse a root.
func TestLogicalWorkspaceRootIsDerivedFromSessionIdentityAlone(t *testing.T) {
	t.Parallel()
	first, err := uuid.New()
	if err != nil {
		t.Fatalf("uuid.New: %v", err)
	}
	second, err := uuid.New()
	if err != nil {
		t.Fatalf("uuid.New: %v", err)
	}
	if got, want := logicalWorkspaceRoot(first), "/sessions/"+first.String()+"/workspace"; got != want {
		t.Errorf("logicalWorkspaceRoot = %q, want %q", got, want)
	}
	if logicalWorkspaceRoot(first) == logicalWorkspaceRoot(second) {
		t.Error("two distinct sessions share one logical workspace root: the path is not session-derived")
	}
	if got := logicalWorkspaceRoot(uuid.UUID{}); got != "" {
		t.Errorf("logicalWorkspaceRoot(zero id) = %q, want empty: a session with no identity has no logical path", got)
	}
}

// TestWorkspaceBindingCarriesTheLogicalRootOnEveryRuntimeRoot proves the binding every
// loop bind site receives separates the two paths: Root is THIS process's physical root
// and varies with the runtime root, while LogicalRoot is the same on both. The two
// clauses are asserted together because either alone is satisfiable by collapsing the
// fields onto each other.
func TestWorkspaceBindingCarriesTheLogicalRootOnEveryRuntimeRoot(t *testing.T) {
	t.Parallel()
	sid, err := uuid.New()
	if err != nil {
		t.Fatalf("uuid.New: %v", err)
	}
	roots := []string{filepath.FromSlash("/hostA/runtime/") + sid.String(), filepath.FromSlash("/hostB/elsewhere/") + sid.String()}
	logical := make([]string, 0, len(roots))
	for _, root := range roots {
		s := &Session{sessionID: sid, wsRoot: root, wsCoordinator: newWorkspaceCoordinator(nil)}
		binding := s.newWorkspaceBinding()
		if binding == nil {
			t.Fatalf("newWorkspaceBinding() = nil for root %q", root)
		}
		if binding.Root != root {
			t.Errorf("binding.Root = %q, want the physical root %q", binding.Root, root)
		}
		if binding.LogicalRoot == binding.Root {
			t.Errorf("binding.LogicalRoot = binding.Root = %q: the model-visible path collapsed onto the physical one", binding.Root)
		}
		logical = append(logical, binding.LogicalRoot)
	}
	if logical[0] != logical[1] {
		t.Errorf("LogicalRoot differs across runtime roots: %q vs %q", logical[0], logical[1])
	}
	if logical[0] != logicalWorkspaceRoot(sid) {
		t.Errorf("binding.LogicalRoot = %q, want %q", logical[0], logicalWorkspaceRoot(sid))
	}
}

// TestWorkspaceStatusIsZeroWithoutAManagedWorkspace pins the fail-quiet edge: a session
// with no workspace store has no boundary to name and no tree to have lost anything from,
// so it must report the zero value rather than a logical path for a workspace that does
// not exist. Asserting the WHOLE struct means a field added later is covered by default.
func TestWorkspaceStatusIsZeroWithoutAManagedWorkspace(t *testing.T) {
	t.Parallel()
	sid, err := uuid.New()
	if err != nil {
		t.Fatalf("uuid.New: %v", err)
	}
	s := &Session{sessionID: sid, wsResidency: WorkspaceResidencyStatus{CheckpointSeq: 9, HasCheckpoint: true, PostCheckpointEvents: 3}}
	if got := s.WorkspaceStatus(); got != (WorkspaceResidencyStatus{}) {
		t.Errorf("WorkspaceStatus() = %+v on a session with no managed workspace, want the zero value", got)
	}
}
