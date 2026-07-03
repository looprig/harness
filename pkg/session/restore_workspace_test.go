package session

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ciram-co/looprig/pkg/content"
	"github.com/ciram-co/looprig/pkg/event"
	"github.com/ciram-co/looprig/pkg/identity"
	"github.com/ciram-co/looprig/pkg/sessionstore"
	"github.com/ciram-co/looprig/pkg/workspacestore"
	"github.com/ciram-co/storekit"
	"github.com/ciram-co/storekit/memstore"
)

// --- workspace-restore test wiring (local to package session) -----------------------

// mustWorkspaceStore opens a workspacestore.Store over blobs, failing the test loudly on
// error. The SAME store (hence the same Blobs backend) is used by the original run's
// Snapshot AND the restore-side Materialize, so a snapshot blob survives the handover.
func mustWorkspaceStore(t *testing.T, blobs storekit.Blobs) *workspacestore.Store {
	t.Helper()
	ws, err := workspacestore.Open(blobs)
	if err != nil {
		t.Fatalf("workspacestore.Open: %v", err)
	}
	return ws
}

// getCountingBlobs wraps a storekit.Blobs and counts Get calls, so a workspace-restore
// test can PROVE the verified-reuse path materializes a warm volume without any fetch.
// Put/List/Delete are promoted from the embedded interface.
type getCountingBlobs struct {
	storekit.Blobs
	gets atomic.Int64
}

func (b *getCountingBlobs) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	b.gets.Add(1)
	return b.Blobs.Get(ctx, key)
}

// wsTreeNode is a location-independent snapshot of one filesystem node used to assert two
// trees are logically identical. perm is zeroed for symlinks (link perms are platform
// noise); content applies to files; target applies to symlinks.
type wsTreeNode struct {
	isDir   bool
	perm    os.FileMode
	content string
	target  string
}

// wsBuildTree writes a small representative tree under root — a regular file, an
// executable in a subdirectory, and a relative symlink — so a workspace round-trip
// exercises contents, modes, and symlinks. marker is woven into file contents so distinct
// markers produce content-distinct (and thus Ref-distinct) trees, which the last-checkpoint
// -wins case relies on.
func wsBuildTree(t *testing.T, root, marker string) {
	t.Helper()
	write := func(rel, content string, perm os.FileMode) {
		abs := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			t.Fatalf("MkdirAll(%q): %v", filepath.Dir(abs), err)
		}
		if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
			t.Fatalf("WriteFile(%q): %v", abs, err)
		}
		if err := os.Chmod(abs, perm); err != nil {
			t.Fatalf("Chmod(%q): %v", abs, err)
		}
	}
	write("readme.txt", "hello "+marker+"\n", 0o644)
	write("bin/run.sh", "#!/bin/sh\necho "+marker+"\n", 0o755)
	if err := os.Symlink("readme.txt", filepath.Join(root, "link")); err != nil {
		t.Fatalf("Symlink: %v", err)
	}
}

// wsSnapshotTree walks root and returns a slash-relative map of its nodes (the root itself
// excluded), reading file contents and symlink targets so two snapshots compare by value,
// independent of where each tree lives on disk.
func wsSnapshotTree(t *testing.T, root string) map[string]wsTreeNode {
	t.Helper()
	out := make(map[string]wsTreeNode)
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p == root {
			return nil
		}
		rel, relErr := filepath.Rel(root, p)
		if relErr != nil {
			return relErr
		}
		rel = filepath.ToSlash(rel)
		info, infoErr := d.Info()
		if infoErr != nil {
			return infoErr
		}
		switch {
		case info.Mode()&os.ModeSymlink != 0:
			target, rlErr := os.Readlink(p)
			if rlErr != nil {
				return rlErr
			}
			out[rel] = wsTreeNode{target: target}
		case info.IsDir():
			out[rel] = wsTreeNode{isDir: true, perm: info.Mode().Perm()}
		default:
			data, rdErr := os.ReadFile(p) // #nosec G304 -- test-controlled tree under t.TempDir
			if rdErr != nil {
				return rdErr
			}
			out[rel] = wsTreeNode{perm: info.Mode().Perm(), content: string(data)}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("wsSnapshotTree(%q): %v", root, err)
	}
	return out
}

// wsAssertTreesEqual fails unless the trees rooted at want and got are logically identical
// (names, contents, permission modes, symlink targets) — the property content-addressing
// relies on. It compares by value so the two trees may live at different paths on disk.
func wsAssertTreesEqual(t *testing.T, want, got string) {
	t.Helper()
	w := wsSnapshotTree(t, want)
	g := wsSnapshotTree(t, got)
	if !reflect.DeepEqual(w, g) {
		t.Errorf("materialized tree mismatch:\n want %v\n got  %v", w, g)
	}
}

// stampCheckpoint wires an original run (SessionStarted + root LoopStarted) and stamps a
// WorkspaceCheckpointed for each ref in order through the journal-backed hub — the durable
// record the restore path reads back. Each ref must already be durable in the shared ws
// Blobs (via a prior Snapshot) for the happy/warm paths; the failure paths deliberately
// stamp a ref with no backing blob or a malformed ref. The lease is left held for the
// caller to release (handover).
func stampCheckpoint(t *testing.T, store *sessionstore.Store, fp event.ConfigFingerprint, refs ...string) persistedStream {
	t.Helper()
	h, sessionID, primaryLoopID, lease, es := newOriginalHub(t, store, fp)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for _, ref := range refs {
		es.stamp(t, ctx, h, event.WorkspaceCheckpointed{
			Header: event.Header{Coordinates: identity.Coordinates{SessionID: sessionID}},
			Ref:    ref,
		})
	}
	return persistedStream{sessionID: sessionID, primaryLoopID: primaryLoopID, lease: lease}
}

// --- unit: the discovery scanner ----------------------------------------------------

// TestLastWorkspaceCheckpoint proves the scanner returns the Ref of the LAST
// WorkspaceCheckpointed in the replay (the most recent durable snapshot) and false when the
// session was never checkpointed.
func TestLastWorkspaceCheckpoint(t *testing.T) {
	t.Parallel()
	wc := func(ref string) event.WorkspaceCheckpointed { return event.WorkspaceCheckpointed{Ref: ref} }
	refA := "v1:sha256:" + strings.Repeat("a", 64)
	tests := []struct {
		name    string
		events  []event.Event
		wantRef string
		wantOK  bool
	}{
		{name: "empty replay", events: nil, wantRef: "", wantOK: false},
		{name: "no checkpoint among other events", events: []event.Event{event.SessionStarted{}, event.LoopStarted{}}, wantRef: "", wantOK: false},
		{name: "single checkpoint", events: []event.Event{wc(refA)}, wantRef: refA, wantOK: true},
		{name: "last of several wins", events: []event.Event{wc("ref-A"), event.LoopStarted{}, wc("ref-B"), wc("ref-C")}, wantRef: "ref-C", wantOK: true},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ref, ok := lastWorkspaceCheckpoint(tt.events)
			if ok != tt.wantOK {
				t.Fatalf("lastWorkspaceCheckpoint ok = %v, want %v", ok, tt.wantOK)
			}
			if ref != tt.wantRef {
				t.Errorf("lastWorkspaceCheckpoint ref = %q, want %q", ref, tt.wantRef)
			}
		})
	}
}

// --- end-to-end: materialize on restore ---------------------------------------------

// TestRestoreMaterializesWorkspace is the full-cycle proof: an original run checkpoints a
// workspace tree, the lease hands over, and Restore (pointed at a FRESH EMPTY root)
// materializes the checkpointed snapshot so the root equals the checkpointed tree. When the
// journal carries several checkpoints, the LAST one wins.
func TestRestoreMaterializesWorkspace(t *testing.T) {
	tests := []struct {
		name    string
		markers []string // one source tree per marker, snapshotted+checkpointed in order; the LAST must land
	}{
		{name: "single checkpoint materializes into empty root", markers: []string{"alpha"}},
		{name: "last checkpoint wins over earlier ones", markers: []string{"alpha", "beta"}},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			store := newRestoreStore(t)
			fp := FingerprintFrom(restoreCfg(&stubLLM{}, "model-x", "be helpful"))
			ws := mustWorkspaceStore(t, memstore.New().Blobs)

			var refs []string
			var lastSrc string
			for _, m := range tt.markers {
				src := t.TempDir()
				wsBuildTree(t, src, m)
				ref, err := ws.Snapshot(context.Background(), src)
				if err != nil {
					t.Fatalf("Snapshot(%s): %v", m, err)
				}
				refs = append(refs, string(ref))
				lastSrc = src
			}

			orig := stampCheckpoint(t, store, fp, refs...)
			handOver(t, orig.lease)

			freshRoot := t.TempDir() // empty → truth path (extract)
			s, err := Restore(context.Background(), restoreCfg(&stubLLM{}, "model-x", "be helpful"),
				orig.sessionID, store, WithWorkspaceStore(ws, freshRoot))
			if err != nil {
				t.Fatalf("Restore: %v", err)
			}
			t.Cleanup(func() { _ = s.Shutdown(context.Background()) })

			// The restored root equals the LAST checkpointed tree (contents + modes + symlinks).
			wsAssertTreesEqual(t, lastSrc, freshRoot)

			// A clean tail: RestoreStarted → RestoreDone (the workspace restored, so the restore
			// was allowed to declare done).
			assertTail(t, restoreEventTail(t, store, orig.sessionID, orig.primaryLoopID),
				[]event.Event{event.RestoreStarted{}, event.RestoreDone{}})
		})
	}
}

// TestRestoreSkipsWorkspaceMaterialize proves the two skip cases: a session with NO
// WorkspaceCheckpointed in its journal restores with the wired root untouched, and a session
// that DOES carry a checkpoint but is restored WITHOUT WithWorkspaceStore (a conversation-only
// restore where the composition root opted out) skips materialize and comes up clean.
func TestRestoreSkipsWorkspaceMaterialize(t *testing.T) {
	tests := []struct {
		name       string
		checkpoint bool // does the journal carry a WorkspaceCheckpointed?
		wireStore  bool // does Restore pass WithWorkspaceStore?
	}{
		{name: "no checkpoint in journal leaves wired root untouched", checkpoint: false, wireStore: true},
		{name: "checkpoint present but no store wired (conversation-only) skips", checkpoint: true, wireStore: false},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			store := newRestoreStore(t)
			fp := FingerprintFrom(restoreCfg(&stubLLM{}, "model-x", "be helpful"))
			ws := mustWorkspaceStore(t, memstore.New().Blobs)

			var orig persistedStream
			if tt.checkpoint {
				src := t.TempDir()
				wsBuildTree(t, src, "present")
				ref, err := ws.Snapshot(context.Background(), src)
				if err != nil {
					t.Fatalf("Snapshot: %v", err)
				}
				orig = stampCheckpoint(t, store, fp, string(ref))
			} else {
				orig = buildOriginalRun(t, store, fp,
					restoreCfg(&stubLLM{chunks: []content.Chunk{textChunk("reply")}}, "model-x", "be helpful"), 1)
			}
			handOver(t, orig.lease)

			// A sentinel, NON-EMPTY root: a materialize that wrongly ran would drift or overwrite
			// it. We assert it is left exactly as-is.
			root := t.TempDir()
			if err := os.WriteFile(filepath.Join(root, "sentinel.txt"), []byte("do not touch\n"), 0o600); err != nil {
				t.Fatalf("seed sentinel: %v", err)
			}
			before := wsSnapshotTree(t, root)

			var opts []Option
			if tt.wireStore {
				opts = append(opts, WithWorkspaceStore(ws, root))
			}
			s, err := Restore(context.Background(), restoreCfg(&stubLLM{}, "model-x", "be helpful"),
				orig.sessionID, store, opts...)
			if err != nil {
				t.Fatalf("Restore: %v", err)
			}
			t.Cleanup(func() { _ = s.Shutdown(context.Background()) })

			// The root is untouched (materialize was skipped).
			if after := wsSnapshotTree(t, root); !reflect.DeepEqual(before, after) {
				t.Errorf("workspace root modified on a skipped materialize:\n before %v\n after %v", before, after)
			}
			// The restore completed: RestoreDone present (no RestoreErrored).
			if tail := restoreEventTail(t, store, orig.sessionID, orig.primaryLoopID); !lastIs(tail, event.RestoreDone{}) {
				t.Errorf("restore tail does not end with RestoreDone: %v", tailTypes(tail))
			}
		})
	}
}

// TestRestoreWorkspaceWarmVolumeReuse proves the warm-volume path: when the wired root is
// pre-populated with the EXACT checkpointed tree, Materialize's verified-reuse path returns
// nil WITHOUT fetching from Blobs, so the restore succeeds and the root is left unchanged.
func TestRestoreWorkspaceWarmVolumeReuse(t *testing.T) {
	t.Parallel()
	store := newRestoreStore(t)
	fp := FingerprintFrom(restoreCfg(&stubLLM{}, "model-x", "be helpful"))
	blobs := &getCountingBlobs{Blobs: memstore.New().Blobs}
	ws := mustWorkspaceStore(t, blobs)

	src := t.TempDir()
	wsBuildTree(t, src, "warm")
	ref, err := ws.Snapshot(context.Background(), src)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}

	orig := stampCheckpoint(t, store, fp, string(ref))
	handOver(t, orig.lease)

	// Pre-populate the destination with the EXACT tree (a warm volume) by materializing once.
	warmRoot := t.TempDir()
	if err := ws.Materialize(context.Background(), ref, warmRoot); err != nil {
		t.Fatalf("seed warm volume: %v", err)
	}
	before := wsSnapshotTree(t, warmRoot)
	getsBeforeRestore := blobs.gets.Load()

	s, err := Restore(context.Background(), restoreCfg(&stubLLM{}, "model-x", "be helpful"),
		orig.sessionID, store, WithWorkspaceStore(ws, warmRoot))
	if err != nil {
		t.Fatalf("Restore (warm volume): %v", err)
	}
	t.Cleanup(func() { _ = s.Shutdown(context.Background()) })

	// Verified reuse: the restore performed NO fetch (the warm volume was verified by content).
	if got := blobs.gets.Load(); got != getsBeforeRestore {
		t.Errorf("warm-volume restore fetched from Blobs: gets %d → %d, want no fetch (verified reuse)", getsBeforeRestore, got)
	}
	// The root is unchanged.
	if after := wsSnapshotTree(t, warmRoot); !reflect.DeepEqual(before, after) {
		t.Errorf("warm-volume restore modified the root:\n before %v\n after %v", before, after)
	}
	// The restore completed end to end: clean tail.
	assertTail(t, restoreEventTail(t, store, orig.sessionID, orig.primaryLoopID),
		[]event.Event{event.RestoreStarted{}, event.RestoreDone{}})
}

// TestRestoreWorkspaceMaterializeFailsClosed proves that a Materialize failure fails the
// WHOLE restore closed — identically to any other restore failure: nil session, a typed
// *RestoreError{RestoreMaterializeFailed} whose Cause is the concrete workspacestore error,
// a durably-recorded RestoreErrored with NO RestoreDone, and a released lease (a successor
// can re-acquire). It covers the three deterministic failure mechanisms.
func TestRestoreWorkspaceMaterializeFailsClosed(t *testing.T) {
	tests := []struct {
		name string
		// setup returns the ref to checkpoint and the root to point the restore at.
		setup func(t *testing.T, ws *workspacestore.Store) (ref, root string)
		// assertCause verifies the concrete typed cause is reachable through RestoreError.Unwrap.
		assertCause func(t *testing.T, err error)
	}{
		{
			name: "drifted non-empty root → DestNotEmptyError",
			setup: func(t *testing.T, ws *workspacestore.Store) (string, string) {
				src := t.TempDir()
				wsBuildTree(t, src, "orig")
				ref, err := ws.Snapshot(context.Background(), src)
				if err != nil {
					t.Fatalf("Snapshot: %v", err)
				}
				drift := t.TempDir()
				if err := os.WriteFile(filepath.Join(drift, "different.txt"), []byte("drift\n"), 0o600); err != nil {
					t.Fatalf("seed drift: %v", err)
				}
				return string(ref), drift
			},
			assertCause: func(t *testing.T, err error) {
				var dne *workspacestore.DestNotEmptyError
				if !errors.As(err, &dne) {
					t.Fatalf("cause = %v, want *workspacestore.DestNotEmptyError", err)
				}
			},
		},
		{
			name: "absent blob → MaterializeError/BlobNotFoundError",
			setup: func(t *testing.T, ws *workspacestore.Store) (string, string) {
				// A grammar-valid ref that was never snapshotted; the empty root selects the truth
				// path, whose Blobs.Get fails closed.
				return "v1:sha256:" + strings.Repeat("a", 64), t.TempDir()
			},
			assertCause: func(t *testing.T, err error) {
				var me *workspacestore.MaterializeError
				if !errors.As(err, &me) {
					t.Fatalf("cause = %v, want *workspacestore.MaterializeError", err)
				}
				var bnfe *storekit.BlobNotFoundError
				if !errors.As(err, &bnfe) {
					t.Fatalf("cause does not unwrap to *storekit.BlobNotFoundError: %v", err)
				}
			},
		},
		{
			name: "corrupt journal ref → InvalidRefError",
			setup: func(t *testing.T, ws *workspacestore.Store) (string, string) {
				return "not-a-valid-ref", t.TempDir()
			},
			assertCause: func(t *testing.T, err error) {
				var ire *workspacestore.InvalidRefError
				if !errors.As(err, &ire) {
					t.Fatalf("cause = %v, want *workspacestore.InvalidRefError", err)
				}
			},
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			store := newRestoreStore(t)
			fp := FingerprintFrom(restoreCfg(&stubLLM{}, "model-x", "be helpful"))
			ws := mustWorkspaceStore(t, memstore.New().Blobs)

			ref, root := tt.setup(t, ws)
			orig := stampCheckpoint(t, store, fp, ref)
			handOver(t, orig.lease)

			s, err := Restore(context.Background(), restoreCfg(&stubLLM{}, "model-x", "be helpful"),
				orig.sessionID, store, WithWorkspaceStore(ws, root))

			// (a) No session comes up.
			if s != nil {
				t.Fatalf("Restore returned a non-nil Session on a materialize failure")
			}
			// (b) A typed *RestoreError classifying the materialize failure.
			var re *RestoreError
			if !errors.As(err, &re) {
				t.Fatalf("Restore err = %v, want *RestoreError", err)
			}
			if re.Kind != RestoreMaterializeFailed {
				t.Errorf("RestoreError.Kind = %q, want %q", re.Kind, RestoreMaterializeFailed)
			}
			// (c) The concrete workspacestore cause is reachable through Unwrap.
			tt.assertCause(t, err)
			// (d) A RestoreErrored is durably recorded and NO RestoreDone followed.
			tail := restoreEventTail(t, store, orig.sessionID, orig.primaryLoopID)
			if !lastIs(tail, event.RestoreErrored{}) {
				t.Errorf("restore tail does not end with RestoreErrored: %v", tailTypes(tail))
			}
			for _, ev := range tail {
				if _, ok := ev.(event.RestoreDone); ok {
					t.Errorf("a RestoreDone is present on a failed restore: %v", tailTypes(tail))
				}
			}
			// (e) The lease was released: a successor can re-acquire it.
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			successor, acqErr := store.AcquireLease(ctx, orig.sessionID)
			if acqErr != nil {
				t.Fatalf("successor AcquireLease after failed restore = %v, want success (lease should have been released)", acqErr)
			}
			t.Cleanup(func() {
				rctx, rcancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer rcancel()
				_ = successor.Release(rctx)
			})
		})
	}
}
