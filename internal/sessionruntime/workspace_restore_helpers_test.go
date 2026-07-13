package sessionruntime

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestWithinRoot(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		root string
		path string
		want bool
	}{
		{name: "equal", root: "/a/b", path: "/a/b", want: true},
		{name: "child", root: "/a/b", path: "/a/b/c", want: true},
		{name: "deep child", root: "/a/b", path: "/a/b/c/d", want: true},
		{name: "parent escapes", root: "/a/b", path: "/a", want: false},
		{name: "sibling escapes", root: "/a/b", path: "/a/c", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := withinRoot(tt.root, tt.path); got != tt.want {
				t.Fatalf("withinRoot(%q,%q) = %v, want %v", tt.root, tt.path, got, tt.want)
			}
		})
	}
}

func TestRefuseSymlink(t *testing.T) {
	t.Parallel()
	base := t.TempDir()

	// Absent path is allowed.
	if err := refuseSymlink(filepath.Join(base, "absent")); err != nil {
		t.Fatalf("absent path refused: %v", err)
	}
	// Real dir is allowed.
	real := filepath.Join(base, "real")
	mustMkdir(t, real)
	if err := refuseSymlink(real); err != nil {
		t.Fatalf("real dir refused: %v", err)
	}
	// Symlink is refused.
	link := filepath.Join(base, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatalf("Symlink: %v", err)
	}
	var target *WorkspaceRestoreError
	if err := refuseSymlink(link); err == nil || !errors.As(err, &target) || target.Kind != WorkspaceRestoreSymlinkRoot {
		t.Fatalf("symlink refused err = %v, want symlink_root", err)
	}
}

func TestRelRegularFiles(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	mustWriteFile(t, filepath.Join(root, "a.txt"), "a")
	mustMkdir(t, filepath.Join(root, "sub"))
	mustWriteFile(t, filepath.Join(root, "sub", "b.txt"), "b")

	got, err := relRegularFiles(root)
	if err != nil {
		t.Fatalf("relRegularFiles: %v", err)
	}
	want := []string{"a.txt", filepath.Join("sub", "b.txt")}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("relRegularFiles = %v, want %v (sorted)", got, want)
	}

	// Absent root yields empty set.
	empty, err := relRegularFiles(filepath.Join(root, "nope"))
	if err != nil {
		t.Fatalf("relRegularFiles(absent): %v", err)
	}
	if len(empty) != 0 {
		t.Fatalf("relRegularFiles(absent) = %v, want empty", empty)
	}
}

// TestReconcileRollback proves a mid-commit failure undoes applied replacements/deletions
// in reverse: we manually apply, then rollback, and assert the original tree returns.
func TestReconcileRollback(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	root := filepath.Join(base, "root")
	staging := siblingScratch(root, "restore-stage", "tok")
	rollback := siblingScratch(root, "restore-rollback", "tok")
	mustMkdir(t, root)
	mustMkdir(t, staging)
	mustMkdir(t, rollback)

	// Original: keep.txt="orig", drop.txt="bye"
	mustWriteFile(t, filepath.Join(root, "keep.txt"), "orig")
	mustWriteFile(t, filepath.Join(root, "drop.txt"), "bye")
	// Staging replacement for keep.txt.
	mustWriteFile(t, filepath.Join(staging, "keep.txt"), "new")

	rec := &reconcile{root: root, staging: staging, rollback: rollback}
	if err := rec.replace("keep.txt"); err != nil {
		t.Fatalf("replace: %v", err)
	}
	if err := rec.delete("drop.txt"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	// Mid-commit changes are visible.
	if got := readFileT(t, filepath.Join(root, "keep.txt")); got != "new" {
		t.Fatalf("keep.txt = %q, want new", got)
	}
	if pathExists(filepath.Join(root, "drop.txt")) {
		t.Fatalf("drop.txt should be deleted before rollback")
	}
	// Rollback restores the original tree.
	if err := rec.rollbackAll(); err != nil {
		t.Fatalf("rollbackAll: %v", err)
	}
	if got := readFileT(t, filepath.Join(root, "keep.txt")); got != "orig" {
		t.Fatalf("after rollback keep.txt = %q, want orig", got)
	}
	if got := readFileT(t, filepath.Join(root, "drop.txt")); got != "bye" {
		t.Fatalf("after rollback drop.txt = %q, want bye", got)
	}
}

// TestSiblingScratchSessionUnique proves two distinct session tokens derive distinct
// scratch paths for the SAME shared root, so concurrent shared-root rewinds never collide.
func TestSiblingScratchSessionUnique(t *testing.T) {
	t.Parallel()
	root := "/srv/shared/project"
	a := siblingScratch(root, "restore-stage", "session-a")
	b := siblingScratch(root, "restore-stage", "session-b")
	if a == b {
		t.Fatalf("distinct tokens produced identical scratch path %q", a)
	}
	// Same token is stable (same session's sequential restores reuse one path, serialized
	// by the exclusive permit).
	if a != siblingScratch(root, "restore-stage", "session-a") {
		t.Fatalf("same token produced non-stable scratch path")
	}
	// Staging and rollback within one session are distinct.
	if siblingScratch(root, "restore-stage", "t") == siblingScratch(root, "restore-rollback", "t") {
		t.Fatalf("stage and rollback scratch paths collide")
	}
}

// TestRollbackAllReportsUndoFailure proves a failing rollback undo surfaces (not swallowed)
// so the caller can escalate to a session fault, while still attempting every undo.
func TestRollbackAllReportsUndoFailure(t *testing.T) {
	t.Parallel()
	boom := errors.New("undo failed")
	var ran int
	rec := &reconcile{undo: []func() error{
		func() error { ran++; return nil },
		func() error { ran++; return boom },
		func() error { ran++; return nil },
	}}
	err := rec.rollbackAll()
	if !errors.Is(err, boom) {
		t.Fatalf("rollbackAll err = %v, want boom", err)
	}
	if ran != 3 {
		t.Fatalf("rollbackAll ran %d undos, want all 3 attempted", ran)
	}
}

func readFileT(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile %s: %v", path, err)
	}
	return string(b)
}
