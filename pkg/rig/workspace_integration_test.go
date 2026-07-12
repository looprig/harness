//go:build integration

package rig

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/looprig/harness/pkg/workspacestore"
)

// TestRigSessionWorkspaceSeedAndRewind drives the whole placement path through
// rig.Define → NewSession for a per-session workspace: it proves the seed is materialized
// into the injective root (population resolution ran), that CheckpointWorkspace works
// (the session's workspace store + coordinator are live), and that RestoreWorkspace rewinds
// the live tree back to a prior ref via the verified whole-root swap.
func TestRigSessionWorkspaceSeedAndRewind(t *testing.T) {
	spool := t.TempDir()
	ws := wsStoreT(t, workspacestore.WithSpoolDir(spool))

	// Build a seed tree and snapshot it into the store.
	seedDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(seedDir, "seed.txt"), []byte("hello-seed"), 0o644); err != nil {
		t.Fatalf("write seed: %v", err)
	}
	seedRef, err := ws.Snapshot(context.Background(), seedDir)
	if err != nil {
		t.Fatalf("Snapshot seed: %v", err)
	}

	baseDir := t.TempDir()
	rig, err := defineWith(t, sessionStoreT(t), WithSessionWorkspaces(ws, baseDir), WithSnapshots(SnapshotPolicy{Trigger: SnapshotManual}))
	if err != nil {
		t.Fatalf("Define: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	sess, err := rig.NewSeededSession(ctx, WithSeedSnapshot(seedRef))
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	defer func() {
		shutCtx, shutCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer shutCancel()
		_ = sess.Shutdown(shutCtx)
	}()

	// The per-session root (baseDir/<sid>) must contain the seed file.
	root := onlySubdir(t, baseDir)
	if got := readFile(t, filepath.Join(root, "seed.txt")); got != "hello-seed" {
		t.Fatalf("seed.txt = %q, want hello-seed", got)
	}

	// A checkpoint of the live tree succeeds (proves ws store + root + coordinator live).
	afterSeedRef, err := sess.CheckpointWorkspace(ctx)
	if err != nil {
		t.Fatalf("CheckpointWorkspace: %v", err)
	}

	// Mutate the live tree, then rewind to the seed ref: the swap must restore the seed
	// contents and drop the mutation.
	if err := os.WriteFile(filepath.Join(root, "extra.txt"), []byte("mutation"), 0o644); err != nil {
		t.Fatalf("write mutation: %v", err)
	}
	if err := sess.RestoreWorkspace(ctx, seedRef); err != nil {
		t.Fatalf("RestoreWorkspace(seed): %v", err)
	}
	if pathExistsT(filepath.Join(root, "extra.txt")) {
		t.Fatalf("extra.txt survived rewind to seed")
	}
	if got := readFile(t, filepath.Join(root, "seed.txt")); got != "hello-seed" {
		t.Fatalf("after rewind seed.txt = %q, want hello-seed", got)
	}

	// Rewinding forward to the post-seed checkpoint round-trips cleanly too.
	if err := sess.RestoreWorkspace(ctx, afterSeedRef); err != nil {
		t.Fatalf("RestoreWorkspace(afterSeed): %v", err)
	}
}

func onlySubdir(t *testing.T, dir string) string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir %s: %v", dir, err)
	}
	var subs []string
	for _, e := range entries {
		if e.IsDir() {
			subs = append(subs, e.Name())
		}
	}
	if len(subs) != 1 {
		t.Fatalf("baseDir has %d subdirs, want exactly 1 (the session root): %v", len(subs), subs)
	}
	return filepath.Join(dir, subs[0])
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile %s: %v", path, err)
	}
	return string(b)
}

func pathExistsT(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}
