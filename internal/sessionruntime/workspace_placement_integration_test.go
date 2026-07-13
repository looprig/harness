//go:build integration

package sessionruntime

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/looprig/core/uuid"
	"github.com/looprig/storage/memstore"
)

const testRootLeaseName = "workspace-roots/test"

// TestExclusiveResolveAcquiresAndContends proves resolveForNew acquires the exclusive root
// lease, and a second resolution on the SAME leaser + name contends with a typed
// *WorkspaceRootBusyError (fail closed) rather than double-owning the root.
func TestExclusiveResolveAcquiresAndContends(t *testing.T) {
	leaser := memstore.New()
	ws := mustWorkspaceStore(t, memstore.New().Blobs)
	root := t.TempDir()
	placement := WorkspacePlacement{
		Mode:      PlacementExclusive,
		Store:     ws,
		Root:      root,
		Leaser:    leaser,
		LeaseName: testRootLeaseName,
	}
	sid, _ := uuid.New()

	first, err := placement.resolveForNew(context.Background(), sid)
	if err != nil {
		t.Fatalf("first resolveForNew: %v", err)
	}
	if first.coordinator == nil || first.rootRelease == nil || first.leaseLost == nil {
		t.Fatalf("exclusive resolution missing coordinator/release/leaseLost")
	}

	// A second resolution on the same leaser + name contends.
	sid2, _ := uuid.New()
	_, err = placement.resolveForNew(context.Background(), sid2)
	var busy *WorkspaceRootBusyError
	if !errors.As(err, &busy) {
		t.Fatalf("second resolveForNew err = %T %v, want *WorkspaceRootBusyError", err, err)
	}

	// Release the first lease; a subsequent acquisition then succeeds.
	if err := first.rootRelease(context.Background()); err != nil {
		t.Fatalf("rootRelease: %v", err)
	}
	third, err := placement.resolveForNew(context.Background(), sid2)
	if err != nil {
		t.Fatalf("resolveForNew after release: %v", err)
	}
	_ = third.rootRelease(context.Background())
}

// TestMaterializeSeedValidity proves seed validity: shared placement is rejected, a
// non-empty root is rejected, and a per-session empty root materializes the seed.
func TestMaterializeSeedValidity(t *testing.T) {
	ws := mustWorkspaceStore(t, memstore.New().Blobs)
	seedDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(seedDir, "s.txt"), []byte("seed"), 0o644); err != nil {
		t.Fatalf("write seed: %v", err)
	}
	seedRef, err := ws.Snapshot(context.Background(), seedDir)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}

	// Shared placement rejects seeding.
	shared := WorkspacePlacement{Mode: PlacementShared, Store: ws, Root: t.TempDir()}
	sharedResolved := &resolvedPlacement{mode: PlacementShared, store: ws, root: shared.Root}
	var seedErr *WorkspaceSeedError
	if err := shared.materializeSeed(context.Background(), sharedResolved, seedRef); !errors.As(err, &seedErr) {
		t.Fatalf("shared seed err = %T %v, want *WorkspaceSeedError", err, err)
	}

	// Non-empty root rejects seeding.
	nonEmpty := t.TempDir()
	if err := os.WriteFile(filepath.Join(nonEmpty, "existing"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write existing: %v", err)
	}
	session := WorkspacePlacement{Mode: PlacementSession, Store: ws}
	if err := session.materializeSeed(context.Background(), &resolvedPlacement{mode: PlacementSession, store: ws, root: nonEmpty}, seedRef); !errors.As(err, &seedErr) {
		t.Fatalf("non-empty seed err = %T %v, want *WorkspaceSeedError", err, err)
	}

	// Per-session empty root materializes the seed.
	emptyRoot := filepath.Join(t.TempDir(), "ws")
	if err := session.materializeSeed(context.Background(), &resolvedPlacement{mode: PlacementSession, store: ws, root: emptyRoot}, seedRef); err != nil {
		t.Fatalf("per-session seed: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(emptyRoot, "s.txt"))
	if err != nil || string(b) != "seed" {
		t.Fatalf("seed file = %q err %v, want seed", string(b), err)
	}
}

// TestRestoreFixedRootManifestReconcile proves the fixed-root rewind replaces changed
// files, deletes files absent from the ref, and never renames the configured root.
func TestRestoreFixedRootManifestReconcile(t *testing.T) {
	ws := mustWorkspaceStore(t, memstore.New().Blobs)

	// Reference tree: keep.txt="v1", only.txt="only".
	refDir := t.TempDir()
	mustWriteFile(t, filepath.Join(refDir, "keep.txt"), "v1")
	mustWriteFile(t, filepath.Join(refDir, "only.txt"), "only")
	ref, err := ws.Snapshot(context.Background(), refDir)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}

	// Live root: keep.txt="v2" (changed), extra.txt="extra" (absent from ref).
	root := filepath.Join(t.TempDir(), "fixed")
	mustMkdir(t, root)
	mustWriteFile(t, filepath.Join(root, "keep.txt"), "v2")
	mustWriteFile(t, filepath.Join(root, "extra.txt"), "extra")

	if err := restoreFixedRoot(context.Background(), ws, root, "session-token", ref); err != nil {
		t.Fatalf("restoreFixedRoot: %v", err)
	}
	if got := readFileT(t, filepath.Join(root, "keep.txt")); got != "v1" {
		t.Fatalf("keep.txt = %q, want v1", got)
	}
	if got := readFileT(t, filepath.Join(root, "only.txt")); got != "only" {
		t.Fatalf("only.txt = %q, want only", got)
	}
	if pathExists(filepath.Join(root, "extra.txt")) {
		t.Fatalf("extra.txt not deleted by reconcile")
	}
	// The configured root itself was never renamed away.
	if !pathExists(root) {
		t.Fatalf("configured root missing after reconcile")
	}
}

// TestRestoreFixedRootRefusesSymlinkComponent proves the fixed-root reconcile refuses to
// write THROUGH a symlinked directory component in the live root (which would escape the
// workspace), returning a typed symlink_component error and leaving the outside target
// untouched.
func TestRestoreFixedRootRefusesSymlinkComponent(t *testing.T) {
	ws := mustWorkspaceStore(t, memstore.New().Blobs)

	// Reference tree: sub/inside.txt.
	refDir := t.TempDir()
	mustMkdir(t, filepath.Join(refDir, "sub"))
	mustWriteFile(t, filepath.Join(refDir, "sub", "inside.txt"), "ref-content")
	ref, err := ws.Snapshot(context.Background(), refDir)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}

	// A target OUTSIDE the workspace that a symlink points at.
	outside := t.TempDir()
	outsideFile := filepath.Join(outside, "inside.txt")
	mustWriteFile(t, outsideFile, "SECRET-OUTSIDE")

	// Live root: `sub` is a SYMLINK to the outside dir, so a naive reconcile writing
	// root/sub/inside.txt would truncate the outside file.
	root := filepath.Join(t.TempDir(), "shared")
	mustMkdir(t, root)
	if err := os.Symlink(outside, filepath.Join(root, "sub")); err != nil {
		t.Fatalf("Symlink: %v", err)
	}

	err = restoreFixedRoot(context.Background(), ws, root, "tok", ref)
	var re *WorkspaceRestoreError
	if !errors.As(err, &re) || re.Kind != WorkspaceRestoreSymlinkComponent {
		t.Fatalf("restoreFixedRoot err = %v, want symlink_component", err)
	}
	// The outside target must be UNTOUCHED (never written through the symlink).
	if got := readFileT(t, outsideFile); got != "SECRET-OUTSIDE" {
		t.Fatalf("outside file was written through symlink: %q", got)
	}
}
