package sessionruntime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/looprig/core/content"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/internal/loopruntime"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/identity"
	"github.com/looprig/harness/pkg/journal"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/sessionstore"
	"github.com/looprig/harness/pkg/tool"
	"github.com/looprig/harness/pkg/workspacestore"
	"github.com/looprig/storage"
	"github.com/looprig/storage/memstore"
)

// --- workspace-restore test wiring (local to package session) -----------------------

// mustWorkspaceStore opens a workspacestore.Store over blobs, failing the test loudly on
// error. The SAME store (hence the same Blobs backend) is used by the original run's
// Snapshot AND the restore-side Materialize, so a snapshot blob survives the handover.
func mustWorkspaceStore(t *testing.T, blobs storage.Blobs) *workspacestore.Store {
	t.Helper()
	ws, err := workspacestore.Open(blobs)
	if err != nil {
		t.Fatalf("workspacestore.Open: %v", err)
	}
	return ws
}

// getCountingBlobs wraps a storage.Blobs and counts Get calls, so a workspace-restore
// test can PROVE the verified-reuse path materializes a warm volume without any fetch.
// Put/List/Delete are promoted from the embedded interface.
type getCountingBlobs struct {
	storage.Blobs
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

func TestFixedRootReconcilePreservesWorkspaceParentDirectoryMode(t *testing.T) {
	root := t.TempDir()
	staging := t.TempDir()
	rollback := t.TempDir()
	source := filepath.Join(staging, "nested", "work.txt")
	if err := os.MkdirAll(filepath.Dir(source), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(source, []byte("work"), 0o600); err != nil {
		t.Fatal(err)
	}

	rec := &reconcile{root: root, staging: staging, rollback: rollback}
	if err := rec.replace(filepath.Join("nested", "work.txt")); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(root, "nested"))
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o755 {
		t.Fatalf("created parent directory mode = %#o, want compatibility mode 0755", got)
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
	h, sessionID, rootLoopID, lease, es := newOriginalHub(t, store, fp)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for _, ref := range refs {
		es.stamp(t, ctx, h, event.WorkspaceCheckpointed{
			Header:      event.Header{Coordinates: identity.Coordinates{SessionID: sessionID}},
			Ref:         ref,
			Consistency: event.SnapshotQuiescent,
			Trigger:     event.SnapshotTriggerManual,
		})
	}
	return persistedStream{sessionID: sessionID, rootLoopID: rootLoopID, lease: lease}
}

// --- unit: the discovery scanner ----------------------------------------------------

// TestEffectiveCurrentWorkspace proves the scanner returns the Ref selected by the last
// checkpoint or restore transition, and false when the session has neither.
func TestEffectiveCurrentWorkspace(t *testing.T) {
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
			ref, ok := effectiveCurrentWorkspace(tt.events)
			if ok != tt.wantOK {
				t.Fatalf("effectiveCurrentWorkspace ok = %v, want %v", ok, tt.wantOK)
			}
			if ref != tt.wantRef {
				t.Errorf("effectiveCurrentWorkspace ref = %q, want %q", ref, tt.wantRef)
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
			fp := fingerprintFromDefinition(restoreCfg(&stubLLM{}, "model-x", "be helpful"))
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
			s, err := restoreTestSession(context.Background(), restoreCfg(&stubLLM{}, "model-x", "be helpful"),
				orig.sessionID, store, WithWorkspaceCheckpointing(ws, freshRoot))
			if err != nil {
				t.Fatalf("Restore: %v", err)
			}
			t.Cleanup(func() { _ = s.Shutdown(context.Background()) })

			// The restored root equals the LAST checkpointed tree (contents + modes + symlinks).
			wsAssertTreesEqual(t, lastSrc, freshRoot)

			// A clean tail: RestoreStarted → RestoreDone (the workspace restored, so the restore
			// was allowed to declare done).
			assertTail(t, restoreEventTail(t, store, orig.sessionID, orig.rootLoopID),
				[]event.Event{event.RestoreStarted{}, event.RestoreDone{}})
		})
	}
}

// TestRestoreMaterializesEffectiveCurrentWorkspace proves restore follows the latest
// workspace transition, not merely the latest checkpoint. A deliberate rewind from B to A
// must survive process restart even though B remains the newest checkpoint for history and
// backup tooling.
func TestRestoreMaterializesEffectiveCurrentWorkspace(t *testing.T) {
	t.Parallel()
	store := newRestoreStore(t)
	fp := fingerprintFromDefinition(restoreCfg(&stubLLM{}, "model-x", "be helpful"))
	ws := mustWorkspaceStore(t, memstore.New().Blobs)

	srcA, srcB := t.TempDir(), t.TempDir()
	wsBuildTree(t, srcA, "alpha")
	wsBuildTree(t, srcB, "beta")
	refA, err := ws.Snapshot(context.Background(), srcA)
	if err != nil {
		t.Fatalf("Snapshot(A): %v", err)
	}
	refB, err := ws.Snapshot(context.Background(), srcB)
	if err != nil {
		t.Fatalf("Snapshot(B): %v", err)
	}

	h, sessionID, rootLoopID, lease, es := newOriginalHub(t, store, fp)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for _, ev := range []event.Event{
		event.WorkspaceCheckpointed{
			Header:      event.Header{Coordinates: identity.Coordinates{SessionID: sessionID}},
			Ref:         string(refA),
			Consistency: event.SnapshotQuiescent,
			Trigger:     event.SnapshotTriggerManual,
		},
		event.WorkspaceCheckpointed{
			Header:      event.Header{Coordinates: identity.Coordinates{SessionID: sessionID}},
			Ref:         string(refB),
			Consistency: event.SnapshotQuiescent,
			Trigger:     event.SnapshotTriggerManual,
		},
		event.WorkspaceRestored{
			Header: event.Header{Coordinates: identity.Coordinates{SessionID: sessionID}},
			Ref:    string(refA),
		},
	} {
		es.stamp(t, ctx, h, ev)
	}
	handOver(t, lease)

	freshRoot := t.TempDir()
	s, err := restoreTestSession(context.Background(), restoreCfg(&stubLLM{}, "model-x", "be helpful"),
		sessionID, store, WithWorkspaceCheckpointing(ws, freshRoot))
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	t.Cleanup(func() { _ = s.Shutdown(context.Background()) })

	wsAssertTreesEqual(t, srcA, freshRoot)
	assertTail(t, restoreEventTail(t, store, sessionID, rootLoopID),
		[]event.Event{event.RestoreStarted{}, event.RestoreDone{}})
}

// TestRestoreSkipsWorkspaceMaterialize proves the two skip cases: a session with NO
// WorkspaceCheckpointed in its journal restores with the wired root untouched, and a session
// that DOES carry a checkpoint but is restored WITHOUT WithWorkspaceCheckpointing (a conversation-only
// restore where the composition root opted out) skips materialize and comes up clean.
func TestRestoreSkipsWorkspaceMaterialize(t *testing.T) {
	tests := []struct {
		name       string
		checkpoint bool // does the journal carry a WorkspaceCheckpointed?
		wireStore  bool // does Restore pass WithWorkspaceCheckpointing?
	}{
		{name: "no checkpoint in journal leaves wired root untouched", checkpoint: false, wireStore: true},
		{name: "checkpoint present but no store wired (conversation-only) skips", checkpoint: true, wireStore: false},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			store := newRestoreStore(t)
			fp := fingerprintFromDefinition(restoreCfg(&stubLLM{}, "model-x", "be helpful"))
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
				opts = append(opts, WithWorkspaceCheckpointing(ws, root))
			}
			s, err := restoreTestSession(context.Background(), restoreCfg(&stubLLM{}, "model-x", "be helpful"),
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
			if tail := restoreEventTail(t, store, orig.sessionID, orig.rootLoopID); !lastIs(tail, event.RestoreDone{}) {
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
	fp := fingerprintFromDefinition(restoreCfg(&stubLLM{}, "model-x", "be helpful"))
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

	s, err := restoreTestSession(context.Background(), restoreCfg(&stubLLM{}, "model-x", "be helpful"),
		orig.sessionID, store, WithWorkspaceCheckpointing(ws, warmRoot))
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
	assertTail(t, restoreEventTail(t, store, orig.sessionID, orig.rootLoopID),
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
				var bnfe *storage.BlobNotFoundError
				if !errors.As(err, &bnfe) {
					t.Fatalf("cause does not unwrap to *storage.BlobNotFoundError: %v", err)
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
			fp := fingerprintFromDefinition(restoreCfg(&stubLLM{}, "model-x", "be helpful"))
			ws := mustWorkspaceStore(t, memstore.New().Blobs)

			ref, root := tt.setup(t, ws)
			orig := stampCheckpoint(t, store, fp, ref)
			handOver(t, orig.lease)

			s, err := restoreTestSession(context.Background(), restoreCfg(&stubLLM{}, "model-x", "be helpful"),
				orig.sessionID, store, WithWorkspaceCheckpointing(ws, root))

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
			tail := restoreEventTail(t, store, orig.sessionID, orig.rootLoopID)
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

// --- workspace identity across residency (H4.3) --------------------------------------

// canonicalTempDir returns a temporary directory with every symlink resolved. A managed
// per-session placement refuses a base parent that does not equal its own canonical form
// (establishCanonicalDirectory), and on darwin t.TempDir() lives under the /var -> /private/var
// symlink, so the raw value would be rejected before any of these assertions ran.
func canonicalTempDir(t *testing.T) string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("EvalSymlinks(TempDir): %v", err)
	}
	return resolved
}

// crossRootLifecycle builds a lifecycle whose per-session placement hangs off baseDir —
// one "Host" runtime root. Two of them over the SAME sessionstore and workspacestore are
// the warm handover this task is about. The snapshot policy is Manual so the only
// checkpoints in the journal are the ones a test asked for.
func crossRootLifecycle(t *testing.T, store *sessionstore.Store, ws *workspacestore.Store, baseDir string) *Lifecycle {
	t.Helper()
	lifecycle, err := newTestLifecycle(cfg(&stubLLM{}), store,
		WithLifecyclePlacement(WorkspacePlacement{Mode: PlacementSession, Store: ws, BaseDir: baseDir}),
		WithLifecycleSnapshotPolicy(SnapshotPolicy{Trigger: SnapshotManual, Priority: SnapshotBestEffort, Timeout: 10 * time.Second}),
	)
	if err != nil {
		t.Fatalf("NewTopologyLifecycle(baseDir=%q): %v", baseDir, err)
	}
	return lifecycle
}

// replaySessionStream reads the whole durable stream for sid, returning each event with
// the journal sequence the store assigned it. Assertions about "the checkpoint's journal
// sequence" need the sequence the JOURNAL knows, not a position in a recorder slice.
func replaySessionStream(t *testing.T, store *sessionstore.Store, sid uuid.UUID) ([]event.Event, []uint64) {
	t.Helper()
	replayer, err := store.OpenEventReplayer(sid, sessionstore.ReplayRequest{FromSeq: 0})
	if err != nil {
		t.Fatalf("OpenEventReplayer: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cursor, err := replayer.Open(ctx, journal.ReplayRequest{Follow: false})
	if err != nil {
		t.Fatalf("replay Open: %v", err)
	}
	defer func() { _ = cursor.Close() }()
	var events []event.Event
	var seqs []uint64
	for {
		ev, seq, err := cursor.Next(ctx)
		if errors.Is(err, io.EOF) {
			return events, seqs
		}
		if err != nil {
			t.Fatalf("replay Next: %v", err)
		}
		events = append(events, ev)
		seqs = append(seqs, seq)
	}
}

// TestRestoreOnADifferentRuntimeRootPreservesTheModelVisibleWorkspacePath is the H4.3
// end-to-end: create, checkpoint, release residency on Host A, restore on Host B whose
// runtime root is a DIFFERENT directory. The physical roots must differ (otherwise the
// test proves nothing about stability) while the model-visible path and the workspace
// contents are the same, and the restored session must report the journal sequence of the
// checkpoint it came up on with no post-checkpoint loss.
func TestRestoreOnADifferentRuntimeRootPreservesTheModelVisibleWorkspacePath(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	store := newRestoreStore(t)
	ws := mustWorkspaceStore(t, memstore.New().Blobs)
	baseA, baseB := canonicalTempDir(t), canonicalTempDir(t)

	first, err := crossRootLifecycle(t, store, ws, baseA).NewSession(ctx, "")
	if err != nil {
		t.Fatalf("NewSession on host A: %v", err)
	}
	sid := first.SessionID()
	rootA := first.wsRoot
	// The LIVE binding host A's loops were bound with, captured before the release, so the
	// comparison below is between two real bindings rather than one real and one synthesized.
	bindingA := first.newWorkspaceBinding()
	if bindingA == nil {
		t.Fatal("host-A session has no workspace binding")
	}
	wsBuildTree(t, rootA, "alpha")
	if _, err := first.CheckpointWorkspace(ctx); err != nil {
		t.Fatalf("CheckpointWorkspace: %v", err)
	}
	if err := first.ReleaseResidency(ctx); err != nil {
		t.Fatalf("ReleaseResidency: %v", err)
	}

	second, err := crossRootLifecycle(t, store, ws, baseB).RestoreSession(ctx, sid)
	if err != nil {
		t.Fatalf("RestoreSession on host B: %v", err)
	}
	t.Cleanup(func() { _ = second.Shutdown(context.Background()) })
	rootB := second.wsRoot

	if rootA == rootB {
		t.Fatalf("both hosts resolved the same physical root %q: the fixture does not cross a runtime root", rootA)
	}
	bindingB := second.newWorkspaceBinding()
	if bindingB == nil {
		t.Fatal("restored session has no workspace binding")
	}
	if bindingA.Root == bindingB.Root {
		t.Errorf("binding.Root = %q on both hosts: the binding did not follow the runtime root", bindingA.Root)
	}
	if bindingA.LogicalRoot != bindingB.LogicalRoot {
		t.Errorf("model-visible workspace path changed across the handover: %q -> %q", bindingA.LogicalRoot, bindingB.LogicalRoot)
	}
	if bindingB.LogicalRoot == "" || bindingB.LogicalRoot == bindingB.Root {
		t.Errorf("restored LogicalRoot = %q (Root = %q): the model-visible path is empty or collapsed onto the physical one", bindingB.LogicalRoot, bindingB.Root)
	}
	wsAssertTreesEqual(t, rootA, rootB)

	// The restored status reports the journal sequence of the checkpoint it materialized,
	// and it agrees with the sequence the release record independently anchored to.
	events, seqs := replaySessionStream(t, store, sid)
	var wantSeq uint64
	for i, ev := range events {
		if _, ok := ev.(event.WorkspaceCheckpointed); ok {
			wantSeq = seqs[i]
		}
	}
	if wantSeq == 0 {
		t.Fatal("no WorkspaceCheckpointed in the durable stream")
	}
	released, _ := countResidencyEvents(events)
	if len(released) != 1 {
		t.Fatalf("SessionResidencyReleased count = %d, want 1", len(released))
	}
	if released[0].CheckpointSeq != wantSeq {
		t.Errorf("SessionResidencyReleased.CheckpointSeq = %d, want the journal sequence %d", released[0].CheckpointSeq, wantSeq)
	}
	status := second.WorkspaceStatus()
	if !status.HasCheckpoint {
		t.Error("restored WorkspaceStatus reports no checkpoint after a clean release")
	}
	if status.CheckpointSeq != wantSeq {
		t.Errorf("restored CheckpointSeq = %d, want %d", status.CheckpointSeq, wantSeq)
	}
	if status.PostCheckpointLoss() {
		t.Errorf("restored status reports post-checkpoint loss after a clean release: %d events after the checkpoint", status.PostCheckpointEvents)
	}
	if status.LogicalRoot != bindingB.LogicalRoot || status.Root != rootB {
		t.Errorf("status roots = (%q, %q), want (%q, %q)", status.LogicalRoot, status.Root, bindingB.LogicalRoot, rootB)
	}
}

// TestRestoredWorkspaceStatusSurfacesPostCheckpointLoss is the DETECTION claim: work that
// the journal records after the last workspace checkpoint did not survive into the
// materialized tree, and the restored session says so rather than presenting the journal
// as though the uncheckpointed files were there. It is the crash shape — a checkpoint,
// then a turn, then no release at all.
func TestRestoredWorkspaceStatusSurfacesPostCheckpointLoss(t *testing.T) {
	t.Parallel()
	store := newRestoreStore(t)
	fp := fingerprintFromDefinition(restoreCfg(&stubLLM{}, "model-x", "be helpful"))
	ws := mustWorkspaceStore(t, memstore.New().Blobs)
	src := t.TempDir()
	wsBuildTree(t, src, "alpha")
	ref, err := ws.Snapshot(context.Background(), src)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}

	h, sid, rootLoopID, lease, es := newOriginalHub(t, store, fp)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	es.stamp(t, ctx, h, event.WorkspaceCheckpointed{
		Header:      event.Header{Coordinates: identity.Coordinates{SessionID: sid}},
		Ref:         string(ref),
		Consistency: event.SnapshotQuiescent,
		Trigger:     event.SnapshotTriggerManual,
	})
	// One loop-scoped record AFTER the checkpoint: the workspace mutations that turn made
	// are not in the snapshot the restore materializes.
	es.stamp(t, ctx, h, event.LoopIdle{
		Header: event.Header{Coordinates: identity.Coordinates{SessionID: sid, LoopID: rootLoopID}},
	})
	handOver(t, lease)

	restored, err := restoreTestSession(context.Background(), restoreCfg(&stubLLM{}, "model-x", "be helpful"),
		sid, store, WithWorkspaceCheckpointing(ws, t.TempDir()))
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	t.Cleanup(func() { _ = restored.Shutdown(context.Background()) })

	status := restored.WorkspaceStatus()
	if !status.HasCheckpoint {
		t.Fatal("restored status reports no checkpoint boundary at all")
	}
	if !status.PostCheckpointLoss() {
		t.Error("restored status reports no post-checkpoint loss, but a loop-scoped record follows the checkpoint")
	}
	if status.PostCheckpointEvents != 1 {
		t.Errorf("PostCheckpointEvents = %d, want 1", status.PostCheckpointEvents)
	}
}

// TestFoldWorkspaceResidency pins the fold the restored status is computed by, including
// the cases the end-to-end fixtures cannot cheaply reach: a stream with no checkpoint at
// all, a rewind that moves the pointer forward, and session-scoped records (the release
// itself, the restore lifecycle) that must NOT be counted as lost work.
func TestFoldWorkspaceResidency(t *testing.T) {
	t.Parallel()
	rec := func(ev event.Event) journal.JournalRecord { return journal.NewEventRecord(ev) }
	tests := []struct {
		name      string
		records   []journal.JournalRecord
		seqs      []uint64
		wantSeq   uint64
		wantHas   bool
		wantAfter int
		// wantLoss is stated per case rather than derived from wantAfter>0, because the
		// two are deliberately NOT the same predicate: with no checkpoint there is no
		// transition to have lost anything since, however many loop records follow.
		wantLoss bool
	}{
		{
			name:      "no checkpoint: loop records counted, but nothing was lost",
			records:   []journal.JournalRecord{rec(event.SessionStarted{}), rec(event.LoopStarted{}), rec(event.LoopIdle{})},
			seqs:      []uint64{1, 2, 3},
			wantAfter: 2,
			wantLoss:  false,
		},
		{
			name:      "checkpoint at its journal sequence, nothing after",
			records:   []journal.JournalRecord{rec(event.SessionStarted{}), rec(event.LoopIdle{}), rec(event.WorkspaceCheckpointed{})},
			seqs:      []uint64{7, 8, 9},
			wantSeq:   9,
			wantHas:   true,
			wantAfter: 0,
		},
		{
			name:      "loop work after the checkpoint counts",
			records:   []journal.JournalRecord{rec(event.WorkspaceCheckpointed{}), rec(event.LoopIdle{}), rec(event.LoopIdle{})},
			seqs:      []uint64{4, 5, 6},
			wantSeq:   4,
			wantHas:   true,
			wantAfter: 2,
			wantLoss:  true,
		},
		{
			name:      "a rewind moves the boundary and clears the count",
			records:   []journal.JournalRecord{rec(event.WorkspaceCheckpointed{}), rec(event.LoopIdle{}), rec(event.WorkspaceRestored{})},
			seqs:      []uint64{2, 3, 4},
			wantSeq:   4,
			wantHas:   true,
			wantAfter: 0,
		},
		{
			name:      "session-scoped tail is not lost work",
			records:   []journal.JournalRecord{rec(event.WorkspaceCheckpointed{}), rec(event.SessionResidencyReleased{}), rec(event.RestoreStarted{}), rec(event.RestoreDone{})},
			seqs:      []uint64{11, 12, 13, 14},
			wantSeq:   11,
			wantHas:   true,
			wantAfter: 0,
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := foldWorkspaceResidency(tt.records, tt.seqs)
			if got.CheckpointSeq != tt.wantSeq {
				t.Errorf("CheckpointSeq = %d, want %d", got.CheckpointSeq, tt.wantSeq)
			}
			if got.HasCheckpoint != tt.wantHas {
				t.Errorf("HasCheckpoint = %v, want %v", got.HasCheckpoint, tt.wantHas)
			}
			if got.PostCheckpointEvents != tt.wantAfter {
				t.Errorf("PostCheckpointEvents = %d, want %d", got.PostCheckpointEvents, tt.wantAfter)
			}
			if got.PostCheckpointLoss() != tt.wantLoss {
				t.Errorf("PostCheckpointLoss() = %v, want %v", got.PostCheckpointLoss(), tt.wantLoss)
			}
		})
	}
}

// TestNeverCheckpointedRestoreReportsNoLossAndLeavesTheTreeIntact is the false-positive
// guard on the detection predicate. With no workspace transition in the stream the restore
// materializes NOTHING — the fixed/per-session root is left exactly as it was found — so a
// warm restart of a never-checkpointed session has lost nothing, however much loop work the
// journal holds. Reporting loss there would fire an operator alert on every such restart.
//
// The count is still asserted non-zero: the fix is the PREDICATE's gate, not suppressing
// the records, and a fix that zeroed the count instead would hide the crash-shape signal
// this test's sibling depends on. The intact-tree clause is asserted on a file written
// BEFORE the restore and read AFTER it, so it is a real observation of the tree the restore
// left behind rather than a negative assertion after a destructive step.
func TestNeverCheckpointedRestoreReportsNoLossAndLeavesTheTreeIntact(t *testing.T) {
	t.Parallel()
	store := newRestoreStore(t)
	fp := fingerprintFromDefinition(restoreCfg(&stubLLM{}, "model-x", "be helpful"))
	ws := mustWorkspaceStore(t, memstore.New().Blobs)

	h, sid, rootLoopID, lease, es := newOriginalHub(t, store, fp)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	// Loop work and NO checkpoint at all.
	es.stamp(t, ctx, h, event.LoopIdle{
		Header: event.Header{Coordinates: identity.Coordinates{SessionID: sid, LoopID: rootLoopID}},
	})
	handOver(t, lease)

	// A warm root the previous residency left behind, written before the restore runs.
	warm := t.TempDir()
	const payload = "uncheckpointed but entirely present\n"
	if err := os.WriteFile(filepath.Join(warm, "work.txt"), []byte(payload), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	restored, err := restoreTestSession(context.Background(), restoreCfg(&stubLLM{}, "model-x", "be helpful"),
		sid, store, WithWorkspaceCheckpointing(ws, warm))
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	t.Cleanup(func() { _ = restored.Shutdown(context.Background()) })

	// The tree the restore left behind, read after it ran.
	got, err := os.ReadFile(filepath.Join(warm, "work.txt")) // #nosec G304 -- test-controlled tree under t.TempDir
	if err != nil {
		t.Fatalf("warm root file is gone after restore: %v", err)
	}
	if string(got) != payload {
		t.Fatalf("warm root file changed: %q, want %q", got, payload)
	}

	status := restored.WorkspaceStatus()
	if status.HasCheckpoint {
		t.Fatalf("HasCheckpoint = true with no workspace transition in the stream (seq %d)", status.CheckpointSeq)
	}
	if status.PostCheckpointEvents == 0 {
		t.Error("PostCheckpointEvents = 0: the count was suppressed rather than the predicate gated")
	}
	if status.PostCheckpointLoss() {
		t.Errorf("PostCheckpointLoss() = true on a never-checkpointed restore whose tree is intact (%d records counted)", status.PostCheckpointEvents)
	}
}

// --- eviction-safe tool-result retrieval (H5.4) --------------------------------------

// evictionCaptureCeiling is the per-result retention ceiling every fixture below
// declares, and the ONLY thing that decides which side of the truncation boundary a
// payload lands on. The two payload sizes are derived from it rather than picked:
// one strictly under it and one strictly over it, so the pair spans both arms of the
// sink's ceiling branch instead of sampling one of them twice.
const evictionCaptureCeiling = 4096

// evictionPreviewBytes bounds the model-visible preview. It is far below the ceiling
// so a capture is never the "below threshold" case in which the committed message
// already carries the whole retained prefix and no separate object is written: every
// arm here must actually produce an object, or the retrieval assertion would be
// asserting nothing.
const evictionPreviewBytes = 512

// evictionPayload builds n bytes of ASCII whose value varies with position. A repeated
// single byte would make every prefix of the payload equal to every other prefix of
// the same length, so a store that returned the WRONG 4096 bytes would still compare
// equal; this payload makes the retrieved bytes identify their own offset.
func evictionPayload(n int) string {
	buf := make([]byte, n)
	for i := range buf {
		buf[i] = byte('a' + i%26)
	}
	return string(buf)
}

// evictionObjectStore is the SessionObjectStore fake the loop retains into.
//
// Harness ships NO implementation of loop.ToolResultObjectStore — every implementation
// in the tree is a test fake — so this one stands in for something with no in-tree
// referent and is written against the INTERFACE CONTRACT in pkg/loop/tool_capture.go,
// audited in both directions:
//
//   - Put's content slice is "owned by the loop and valid only for the duration of the
//     call", so this copies it. A fake that retained the slice would be LOOSER than the
//     contract and would silently pass for a producer that reused its buffer.
//   - "The object is immutable: writing the same identity twice must either be a no-op
//     or store identical bytes." A second Put with different bytes is a producer defect,
//     so this records it rather than overwriting — a fake that just overwrote would hide
//     it. It is not STRICTER than the contract: it accepts the repeat, it only refuses
//     to let differing bytes pass unnoticed.
//   - Stat's size and digest are COMPUTED from the stored bytes, never echoed from what
//     Put was told. A fake that echoed would make the loop's size/digest verification
//     stages unfalsifiable: a store that truncated would still verify.
//   - The streaming variant reads through a plain io.Reader only. The contract forbids a
//     store from type-asserting io.ReaderAt, io.Seeker, io.WriterTo or a Size method,
//     because the dynamic type varies with the host's spill configuration; this fake
//     therefore uses io.ReadFull and one extra Read to prove the stream ended, which is
//     also what "a store that reads fewer than size bytes must report an error" requires.
//
// What it deliberately does NOT model is latency, partial durability or an ambiguous
// Put. Those are H5.3's fault tests; this fixture is about what survives eviction.
type evictionObjectStore struct {
	// streaming makes the store advertise loop.ToolResultObjectStreamStore. It is a
	// FIELD rather than a second type so the two arms differ in exactly one bit.
	streaming bool
	// forgetful accepts and verifies exactly like the retaining store but keeps no
	// bytes. It is the control that makes the retrieval assertion falsifiable: see
	// TestToolResultObjectsAreUnreachableWithoutAStoreThatKeptTheBytes.
	forgetful bool

	mu        sync.Mutex
	objects   map[string][]byte
	sizes     map[string]uint64
	digests   map[string]string
	puts      int
	streams   int
	conflicts []string
}

func (s *evictionObjectStore) record(id string, body []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.objects == nil {
		s.objects = map[string][]byte{}
		s.sizes = map[string]uint64{}
		s.digests = map[string]string{}
	}
	sum := sha256.Sum256(body)
	digest := hex.EncodeToString(sum[:])
	if prior, ok := s.digests[id]; ok && prior != digest {
		s.conflicts = append(s.conflicts, id)
	}
	s.sizes[id] = uint64(len(body))
	s.digests[id] = digest
	if !s.forgetful {
		s.objects[id] = append([]byte(nil), body...)
	}
}

func (s *evictionObjectStore) PutToolResultObject(_ context.Context, objectID string, body []byte) error {
	s.mu.Lock()
	s.puts++
	s.mu.Unlock()
	s.record(objectID, body)
	return nil
}

func (s *evictionObjectStore) StatToolResultObject(_ context.Context, objectID string) (loopruntime.ToolResultObjectStat, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	size, ok := s.sizes[objectID]
	if !ok {
		return loopruntime.ToolResultObjectStat{}, fmt.Errorf("no object %q", objectID)
	}
	return loopruntime.ToolResultObjectStat{SizeBytes: size, Digest: s.digests[objectID]}, nil
}

// get returns the stored bytes. A forgetful store always reports absent, which is the
// whole point of it.
func (s *evictionObjectStore) get(objectID string) ([]byte, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	body, ok := s.objects[objectID]
	return body, ok
}

func (s *evictionObjectStore) counts() (puts, streams int, conflicts []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.puts, s.streams, append([]string(nil), s.conflicts...)
}

// ids returns every identity the store holds bytes for, sorted, so a test can assert
// what the store contains rather than only whether one expected identity is present.
func (s *evictionObjectStore) ids() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.objects))
	for id := range s.objects {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// evictionStreamStore is the streaming half. It is a separate type because a Go value
// either implements the optional capability or does not, and the loop selects the path
// by type assertion; evictionObjectStore.streaming records which one a fixture built so
// an arm cannot silently take the other path.
type evictionStreamStore struct{ *evictionObjectStore }

func (s evictionStreamStore) PutToolResultObjectStream(_ context.Context, objectID string, body io.Reader, size uint64) error {
	s.mu.Lock()
	s.streams++
	s.mu.Unlock()
	buf := make([]byte, size)
	if _, err := io.ReadFull(body, buf); err != nil {
		return fmt.Errorf("stream %q: %w", objectID, err)
	}
	// The contract says the store is handed exactly size bytes. Proving the stream is
	// exhausted here is what makes "a store that reads fewer than size bytes must report
	// an error" a two-sided check rather than a truncating read that always succeeds.
	var overflow [1]byte
	if n, err := body.Read(overflow[:]); n != 0 || !errors.Is(err, io.EOF) {
		return fmt.Errorf("stream %q offered more than the declared %d bytes", objectID, size)
	}
	s.record(objectID, buf)
	return nil
}

// evictionTool is the large fake tool. It is materialized (a plain InvokableTool), which
// is the fallback producer path — the one a host has no control over, and therefore the
// one whose bytes most need to survive.
type evictionTool struct{ payload string }

func (e *evictionTool) Info(context.Context) (*tool.ToolInfo, error) {
	return &tool.ToolInfo{Name: "Big", Desc: "produces a large result", Schema: []byte(`{"type":"object"}`)}, nil
}

func (e *evictionTool) PrepareCall(context.Context, uuid.UUID, string) (tool.Request, tool.PreparedArtifact, error) {
	return tool.Request{ToolName: "Big", Summary: "produce a large result"}, nil, nil
}

func (e *evictionTool) InvokableRun(context.Context, string) (*tool.ToolResult, error) {
	return tool.TextResult(e.payload), nil
}

// evictionRun is one whole lifecycle: a journal-backed session with a materialized
// workspace root and a capture spill base, one turn that runs the large tool, and the
// facts needed to interrogate what survived.
type evictionRun struct {
	session    *Session
	store      *sessionstore.Store
	sessionID  uuid.UUID
	rootLoopID uuid.UUID
	wsRoot     string
	spillBase  string
	spillRoot  string
	payload    string
	// stepDoneRefusals counts the durable StepDone appends the fixture refused, or is
	// zero for a fixture that refuses none.
	stepDoneRefusals atomic.Int64
}

// newEvictionRun builds the session and drives one turn to its terminal. The session is
// wired to a REAL sessionstore journal rather than an in-memory recorder because the
// claim under test is about what survives the process's local state: a capture list held
// only in this process's memory would still be there after the roots are deleted, so
// reading it back through the store's replayer is the layer that has the reader.
func newEvictionRun(t *testing.T, objects *evictionObjectStore, payloadBytes int) *evictionRun {
	t.Helper()
	run := buildEvictionRunWith(t, objects, evictionOptions{payloadBytes: payloadBytes})
	run.drive(t)
	return run
}

// evictionOptions carries the one thing an arm may vary beyond the payload: a durable
// append that refuses the StepDone, which is how the commit is failed AFTER a fully
// verified retention.
type evictionOptions struct {
	payloadBytes   int
	refuseStepDone error
}

// stepDoneRefusingAppender fails the durable append of a StepDone and passes everything
// else through. It sits at the session's REQUIRED durable event tap, which is the layer
// the loop's commit handshake actually reports from: the actor calls the durable commit
// and acks its error, so a refusal here is a commitStep failure at the one production
// cause the ordering exists to survive — the append.
type stepDoneRefusingAppender struct {
	inner eventAppender
	err   error
	// refusals counts the StepDone appends actually refused. Without it a run in which
	// no StepDone was ever attempted — a tool that never ran, a step that never
	// completed — would satisfy every "nothing was committed" assertion for the wrong
	// reason.
	refusals *atomic.Int64
}

func (a stepDoneRefusingAppender) AppendEvent(ctx context.Context, ev event.Event) (uint64, error) {
	switch ev.(type) {
	case event.StepDone, *event.StepDone:
		a.refusals.Add(1)
		return 0, a.err
	}
	return a.inner.AppendEvent(ctx, ev)
}

func buildEvictionRunWith(t *testing.T, objects *evictionObjectStore, opts evictionOptions) *evictionRun {
	t.Helper()
	ctx := context.Background()
	store := newRestoreStore(t)
	sessionID := mustSessionID(t)
	lease := mustAcquireLease(t, store, sessionID)
	j, err := store.OpenJournal(ctx, sessionID, lease)
	if err != nil {
		t.Fatalf("OpenJournal: %v", err)
	}
	ws := mustWorkspaceStore(t, memstore.New().Blobs)
	wsRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(wsRoot, "work.txt"), []byte("work"), 0o600); err != nil {
		t.Fatalf("seed workspace root: %v", err)
	}
	spillBase := t.TempDir()
	payload := evictionPayload(opts.payloadBytes)

	var wired loopruntime.ToolResultObjectStore = objects
	if objects.streaming {
		wired = evictionStreamStore{evictionObjectStore: objects}
	}

	definition := mustDefine(
		loop.WithName("agent"),
		loop.WithInference(&scriptedToolLLM{toolName: "Big"}, validModel("base")),
		loop.WithSystem("base"),
		loop.WithTools(tool.NewDefinition("Big", 0, func(context.Context, tool.Bindings) ([]tool.InvokableTool, error) {
			return []tool.InvokableTool{&evictionTool{payload: payload}}, nil
		})),
		loop.WithAccessGate(allowAllAccessGate{}),
		loop.WithPolicyRevision("eviction"),
		loop.WithToolLimits(loop.ToolLimits{ResultBytes: evictionPreviewBytes, CaptureBytes: evictionCaptureCeiling}),
		loop.WithDrainTimeout(200*time.Millisecond),
	)
	run := &evictionRun{
		store:     store,
		sessionID: sessionID,
		wsRoot:    wsRoot,
		spillBase: spillBase,
		spillRoot: filepath.Join(spillBase, sessionID.String()),
		payload:   payload,
	}
	var appender eventAppender = journal.NewJournalEventAppender(j)
	if opts.refuseStepDone != nil {
		appender = stepDoneRefusingAppender{inner: appender, err: opts.refuseStepDone, refusals: &run.stepDoneRefusals}
	}
	s, err := newTestSession(ctx, definition,
		WithSessionID(sessionID),
		WithEventAppender(appender),
		WithLeaseRelease(lease.Release),
		WithWorkspaceCheckpointing(ws, wsRoot),
		WithSnapshotPolicy(SnapshotPolicy{Trigger: SnapshotOnIdle, Priority: SnapshotRequired, Timeout: 30 * time.Second}),
		WithToolResultCapture(wired, spillBase),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	run.session = s
	run.rootLoopID = s.ActiveLoopID()
	t.Cleanup(func() { _ = s.Shutdown(context.Background()) })
	return run
}

// drive submits the one turn that calls the large tool and waits for its terminal.
func (r *evictionRun) drive(t *testing.T) {
	t.Helper()
	submitAndDrain(t, r.session, []content.Block{&content.TextBlock{Text: "run the big tool"}})
}

// driveForTerminal is drive plus the turn terminal it ended on. A test that arranges a
// failure part way through a turn has to name WHICH ending it produced: "the object is an
// orphan" reads the same whether the commit was interrupted or the tool never ran.
func (r *evictionRun) driveForTerminal(t *testing.T) event.Event {
	t.Helper()
	sub, err := r.session.SubscribeEvents(event.EventFilter{Enduring: event.LoopScope{All: true}})
	if err != nil {
		t.Fatalf("SubscribeEvents: %v", err)
	}
	defer func() { _ = sub.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := r.session.Submit(ctx, []content.Block{&content.TextBlock{Text: "run the big tool"}}); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	timeout := time.After(30 * time.Second)
	for {
		select {
		case delivery, ok := <-sub.Events():
			if !ok {
				t.Fatal("subscription closed before a turn terminal")
			}
			switch delivery.Event.(type) {
			case event.TurnDone, event.TurnFailed, event.TurnInterrupted:
				return delivery.Event
			}
		case <-timeout:
			t.Fatal("no turn terminal within deadline")
		}
	}
}

// evict is the state-destroying action the task names: release this process's residency,
// then delete the materialized workspace root and the whole capture spill base. It
// asserts both are really gone, because every assertion after it is a claim about
// retrievability WITHOUT them and would be vacuous if either survived.
func (r *evictionRun) evict(t *testing.T) {
	t.Helper()
	if _, err := os.Lstat(r.spillRoot); err != nil {
		t.Fatalf("lstat session spill root before eviction = %v, want the root a loop established", err)
	}
	if err := r.session.ReleaseResidency(context.Background()); err != nil {
		t.Fatalf("ReleaseResidency: %v", err)
	}
	// Giving up residency gives up the local capture spill: the release runs the same
	// teardown Shutdown does, and that teardown removes this session's whole spill root.
	// This is asserted here rather than assumed because ReleaseResidency is a DIFFERENT
	// public entry point from Shutdown — the nonterminal one a pooled host actually calls
	// — and the existing spill-release readers only ever drove Shutdown. The deletions
	// below would mask it: they would remove the root whether the release had or not.
	if _, err := os.Lstat(r.spillRoot); !os.IsNotExist(err) {
		t.Fatalf("lstat session spill root after ReleaseResidency = %v, want not-exist: a released session left its complete tool output on local disk", err)
	}
	for _, root := range []string{r.wsRoot, r.spillBase} {
		if err := os.RemoveAll(root); err != nil {
			t.Fatalf("delete %q: %v", root, err)
		}
		if _, err := os.Lstat(root); !os.IsNotExist(err) {
			t.Fatalf("lstat %q after deletion = %v, want not-exist: the eviction did not happen", root, err)
		}
	}
}

// replayCaptures reads the tool-result captures back out of the DURABLE journal, after
// the session has given up residency. Nothing in this process's memory is consulted.
func (r *evictionRun) replayCaptures(t *testing.T) []event.ToolResultCapture {
	t.Helper()
	replayer, err := r.store.OpenEventReplayer(r.sessionID, sessionstore.ReplayRequest{FromSeq: 0})
	if err != nil {
		t.Fatalf("OpenEventReplayer: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cursor, err := replayer.Open(ctx, journal.ReplayRequest{LoopID: r.rootLoopID, Follow: false})
	if err != nil {
		t.Fatalf("replay Open: %v", err)
	}
	defer func() { _ = cursor.Close() }()
	var captures []event.ToolResultCapture
	for {
		ev, _, err := cursor.Next(ctx)
		if errors.Is(err, io.EOF) {
			return captures
		}
		if err != nil {
			t.Fatalf("replay Next: %v", err)
		}
		if done, ok := ev.(event.StepDone); ok {
			captures = append(captures, done.Captures...)
		}
	}
}

// onlyCapture fails unless the replayed journal carries exactly one capture. One tool
// ran, so one capture is the whole list; "the first of several" would let a second,
// contradictory record pass unread.
func (r *evictionRun) onlyCapture(t *testing.T) event.ToolResultCapture {
	t.Helper()
	captures := r.replayCaptures(t)
	if len(captures) != 1 {
		t.Fatalf("replayed captures = %d, want exactly 1", len(captures))
	}
	return captures[0]
}

// TestToolResultObjectsSurviveResidencyReleaseAndRootDeletion is step 1 of H5.4: a large
// tool runs, the step commits, this process releases residency, the materialized
// workspace root and the whole capture spill base are DELETED, and the complete captured
// bytes plus the declared truncation metadata are still retrievable — the metadata from
// the durable journal, the bytes from the SessionObjectStore.
//
// The space is derived from the two mechanisms that decide the outcome rather than
// picked: the sink's ceiling branch (a payload under it and one over it) and
// putCapturedObject's type assertion (a store that implements the optional streaming
// capability and one that does not). Every arm asserts which path was actually taken, so
// an arm cannot silently collapse into another.
func TestToolResultObjectsSurviveResidencyReleaseAndRootDeletion(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name          string
		streaming     bool
		payloadBytes  int
		wantCaptured  int
		wantTruncated bool
		wantReason    event.ToolResultTruncationReason
	}{
		{name: "under the ceiling, materialized put", payloadBytes: evictionCaptureCeiling / 2, wantCaptured: evictionCaptureCeiling / 2},
		{name: "over the ceiling, materialized put", payloadBytes: evictionCaptureCeiling * 4, wantCaptured: evictionCaptureCeiling, wantTruncated: true, wantReason: event.ToolResultTruncatedCaptureCeiling},
		{name: "under the ceiling, streaming put", streaming: true, payloadBytes: evictionCaptureCeiling / 2, wantCaptured: evictionCaptureCeiling / 2},
		{name: "over the ceiling, streaming put", streaming: true, payloadBytes: evictionCaptureCeiling * 4, wantCaptured: evictionCaptureCeiling, wantTruncated: true, wantReason: event.ToolResultTruncatedCaptureCeiling},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			objects := &evictionObjectStore{streaming: tt.streaming}
			run := newEvictionRun(t, objects, tt.payloadBytes)
			run.evict(t)

			capture := run.onlyCapture(t)

			// The declared metadata, read back from the journal after eviction.
			if capture.Reference == nil {
				t.Fatal("the replayed capture carries no object reference: the elided bytes are named by nothing")
			}
			if got, want := capture.CapturedBytes, uint64(tt.wantCaptured); got != want {
				t.Errorf("CapturedBytes = %d, want %d", got, want)
			}
			original, exact := capture.OriginalSize()
			if !exact {
				t.Error("OriginalSize is a lower bound: a materialized producer's total is exactly known")
			}
			if got, want := original, uint64(tt.payloadBytes); got != want {
				t.Errorf("OriginalBytes = %d, want the producer's %d", got, want)
			}
			if capture.Truncated != tt.wantTruncated {
				t.Errorf("Truncated = %v, want %v", capture.Truncated, tt.wantTruncated)
			}
			if capture.TruncationReason != tt.wantReason {
				t.Errorf("TruncationReason = %q, want %q", capture.TruncationReason, tt.wantReason)
			}
			if capture.Encoding != event.ToolResultEncodingUTF8 {
				t.Errorf("Encoding = %q, want %q for an ASCII payload cut on a byte boundary", capture.Encoding, event.ToolResultEncodingUTF8)
			}

			// The bytes, reopened through the SessionObjectStore with every local root gone.
			body, ok := objects.get(capture.Reference.ObjectID)
			if !ok {
				t.Fatalf("object %q is absent after eviction", capture.Reference.ObjectID)
			}
			if uint64(len(body)) != capture.CapturedBytes {
				t.Fatalf("retrieved %d bytes, want the declared CapturedBytes %d", len(body), capture.CapturedBytes)
			}
			if want := run.payload[:tt.wantCaptured]; string(body) != want {
				t.Errorf("retrieved bytes are not the producer's prefix (first difference at %d)", evictionFirstDifference(string(body), want))
			}
			// The identity is content-addressed, so the retrieved bytes must reproduce it.
			// This is what makes the object reference usable by a reader that has only the
			// journal: it can verify what it fetched without trusting the store.
			sum := sha256.Sum256(body)
			if want := "v1:sha256:" + hex.EncodeToString(sum[:]); capture.Reference.ObjectID != want {
				t.Errorf("ObjectID = %q, want the content address of the retrieved bytes %q", capture.Reference.ObjectID, want)
			}

			puts, streams, conflicts := objects.counts()
			if len(conflicts) != 0 {
				t.Errorf("the same object identity was written with different bytes: %v", conflicts)
			}
			if tt.streaming && (streams != 1 || puts != 0) {
				t.Errorf("streaming store saw %d streams / %d materialized puts, want 1/0: this arm did not exercise the streaming path", streams, puts)
			}
			if !tt.streaming && (puts != 1 || streams != 0) {
				t.Errorf("materialized store saw %d puts / %d streams, want 1/0", puts, streams)
			}
		})
	}
}

// evictionFirstDifference reports the index of the first differing byte, for a failure
// message that says WHERE two 4 KiB strings diverge instead of printing both.
func evictionFirstDifference(got, want string) int {
	for i := 0; i < len(got) && i < len(want); i++ {
		if got[i] != want[i] {
			return i
		}
	}
	return min(len(got), len(want))
}

// TestToolResultObjectsAreUnreachableWithoutAStoreThatKeptTheBytes is the reason the test
// above is not vacuous, and it is a §5-class-9 obligation: an assertion made AFTER a
// state-destroying action asserts nothing unless the probe can observe the failure.
//
// It runs the identical sequence against a store that accepts and VERIFIES exactly like
// the retaining one — Put succeeds, Stat answers with the size and digest it computed at
// Put time, so every stage of the retention pipeline passes and the committed capture is
// byte-identical — but keeps no bytes. After the same eviction the bytes are reachable
// from nowhere: not from the store, and not from any surviving file on disk.
//
// The two claims together are what make the previous test meaningful. The metadata is
// identical, so the difference between the runs is ONLY whether the store retained the
// bytes; and the byte sweep shows no local copy is quietly satisfying the retrieval.
func TestToolResultObjectsAreUnreachableWithoutAStoreThatKeptTheBytes(t *testing.T) {
	t.Parallel()
	const payloadBytes = evictionCaptureCeiling * 4

	retaining := &evictionObjectStore{}
	kept := newEvictionRun(t, retaining, payloadBytes)
	kept.evict(t)
	keptCapture := kept.onlyCapture(t)

	forgetful := &evictionObjectStore{forgetful: true}
	lost := newEvictionRun(t, forgetful, payloadBytes)
	// The parent of both roots is swept for the payload after the eviction, so the
	// sweep must start from paths that still exist: capture them first.
	sweepRoots := []string{filepath.Dir(lost.wsRoot), filepath.Dir(lost.spillBase)}
	lost.evict(t)
	lostCapture := lost.onlyCapture(t)

	// Same committed metadata: the runs differ in one property only.
	if keptCapture.Reference == nil || lostCapture.Reference == nil {
		t.Fatal("a run committed no object reference")
	}
	if keptCapture.Reference.ObjectID != lostCapture.Reference.ObjectID {
		t.Fatalf("object identities differ (%q vs %q): the two runs are not comparable",
			keptCapture.Reference.ObjectID, lostCapture.Reference.ObjectID)
	}
	if keptCapture.CapturedBytes != lostCapture.CapturedBytes || keptCapture.Truncated != lostCapture.Truncated ||
		keptCapture.TruncationReason != lostCapture.TruncationReason || keptCapture.Encoding != lostCapture.Encoding {
		t.Fatalf("committed capture metadata differs between the runs: %+v vs %+v", keptCapture, lostCapture)
	}

	// The retaining run yields the bytes; the forgetful one cannot.
	if _, ok := retaining.get(keptCapture.Reference.ObjectID); !ok {
		t.Fatal("the retaining store lost the object: the control has nothing to contrast with")
	}
	if body, ok := forgetful.get(lostCapture.Reference.ObjectID); ok {
		t.Fatalf("the forgetful store returned %d bytes: the control is not a control", len(body))
	}

	// And nothing on disk is standing in for it. The prefix is long enough that a match
	// could not be coincidental and short enough to scan cheaply.
	needle := lost.payload[:256]
	for _, root := range sweepRoots {
		if path, found := evictionFindPayload(t, root, needle); found {
			t.Fatalf("the captured bytes are still on local disk at %q after eviction: the retrieval assertion could be satisfied without the object store", path)
		}
	}

	// The sweep is itself a negative assertion, so it is proved in both directions: over
	// a directory holding the payload it must find it, and over one that does not it must
	// not. A sweep that silently found nothing anywhere would report success for a run
	// whose bytes were sitting in plain view. It runs AFTER the sweep above, because the
	// planted copy would otherwise be inside the swept tree.
	planted := t.TempDir()
	if err := os.WriteFile(filepath.Join(planted, "leaked.capture"), []byte("prefix"+needle+"suffix"), 0o600); err != nil {
		t.Fatalf("plant a payload copy: %v", err)
	}
	if _, found := evictionFindPayload(t, planted, needle); !found {
		t.Fatal("the on-disk sweep did not find a planted copy of the payload: it cannot observe the failure it asserts the absence of")
	}
	if _, found := evictionFindPayload(t, t.TempDir(), needle); found {
		t.Fatal("the on-disk sweep found the payload in an empty directory")
	}
}

// evictionFindPayload walks root for a file containing needle. A missing root is not a
// failure — it is the expected state of a deleted one — but any other walk error is,
// because a walk that silently gave up would report "not found" for the wrong reason.
func evictionFindPayload(t *testing.T, root, needle string) (string, bool) {
	t.Helper()
	var hit string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if d.IsDir() || !d.Type().IsRegular() {
			return nil
		}
		data, readErr := os.ReadFile(path) // #nosec G304 -- test-controlled tree under t.TempDir
		if readErr != nil {
			if os.IsNotExist(readErr) {
				return nil
			}
			return readErr
		}
		if strings.Contains(string(data), needle) {
			hit = path
			return filepath.SkipAll
		}
		return nil
	})
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("sweep %q: %v", root, err)
	}
	return hit, hit != ""
}

// TestCommitFailureAfterVerifiedRetentionLeavesOnlyAnUnreferencedObject closes the
// conjunction H5.3 left open: the ORDER "retain, then commit" is correct by construction,
// but nothing had ever failed the commit AFTER a successful retention, so the sentence
// "an append failure leaves only an object-store orphan eligible for safe GC" described a
// state no test had ever produced.
//
// It is failed at the append, which is the cause that sentence names. The session's
// required durable event tap refuses the StepDone; the loop actor's commit is the durable
// commit, so it commits nothing and acks the error, and the turn ends on it. Retention has
// already run to completion by then — Put, Stat, size and digest all verified — and the
// deferred release has already deleted the local spill.
//
// An interrupt was tried first and rejected as a mechanism: cfg.commit selects on the
// actor channel AND the cancelled context, and Go picks a ready case uniformly at random,
// so a cancelled step commits its StepDone about half the time. A test built on it passes
// or fails by coin flip — measured directly, one run of this scenario reported zero
// captures and the next reported one.
//
// The state that must hold is a conjunction, and each half is worthless alone:
//   - the object EXISTS, with exactly the bytes and the content-addressed identity the
//     capture would have referenced; and
//   - NOTHING references it — no StepDone reached the durable journal, so no committed
//     record names the identity, which is what "eligible for safe GC" means; and
//   - the local spill is gone, so the bytes exist in exactly one place.
func TestCommitFailureAfterVerifiedRetentionLeavesOnlyAnUnreferencedObject(t *testing.T) {
	t.Parallel()
	const payloadBytes = evictionCaptureCeiling * 4
	refused := errors.New("durable append refused")
	objects := &evictionObjectStore{}
	run := buildEvictionRunWith(t, objects, evictionOptions{payloadBytes: payloadBytes, refuseStepDone: refused})

	terminal := run.driveForTerminal(t)
	if got := run.stepDoneRefusals.Load(); got != 1 {
		t.Fatalf("refused StepDone appends = %d, want exactly 1: the commit this test fails must have been attempted", got)
	}
	if _, ok := terminal.(event.TurnInterrupted); !ok {
		t.Fatalf("turn terminal = %T, want event.TurnInterrupted: the commit handshake is what had to fail here", terminal)
	}

	// The object was written and verified: the retention pipeline ran to completion
	// before the commit was attempted. Its identity is the content address of the
	// retained prefix, computed here from the payload rather than read from the store,
	// so a store that wrote the wrong bytes could not satisfy it.
	wantBytes := run.payload[:evictionCaptureCeiling]
	sum := sha256.Sum256([]byte(wantBytes))
	wantID := "v1:sha256:" + hex.EncodeToString(sum[:])
	if got := objects.ids(); len(got) != 1 || got[0] != wantID {
		t.Fatalf("stored object identities = %v, want exactly [%s]", got, wantID)
	}
	if body, _ := objects.get(wantID); string(body) != wantBytes {
		t.Errorf("the orphaned object holds the wrong bytes (first difference at %d)", evictionFirstDifference(string(body), wantBytes))
	}

	// Nothing references it. The durable journal carries no capture at all — not one
	// naming this identity, and not one naming any other.
	if captures := run.replayCaptures(t); len(captures) != 0 {
		t.Fatalf("the journal carries %d captures after a refused StepDone append: %+v", len(captures), captures)
	}

	// And the local spill is gone, so the orphan is the only copy: the retention
	// pipeline releases every sink on the way out, including this path.
	entries, err := os.ReadDir(run.spillRoot)
	if err != nil {
		t.Fatalf("read session spill root: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("session spill root still holds %d entries after a failed commit: the capture was left on local disk", len(entries))
	}
}
