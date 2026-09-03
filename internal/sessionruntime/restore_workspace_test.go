package sessionruntime

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

	"github.com/looprig/core/content"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/identity"
	"github.com/looprig/harness/pkg/journal"
	"github.com/looprig/harness/pkg/sessionstore"
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
	}{
		{
			name:      "no checkpoint: every loop record is unaccounted work",
			records:   []journal.JournalRecord{rec(event.SessionStarted{}), rec(event.LoopStarted{}), rec(event.LoopIdle{})},
			seqs:      []uint64{1, 2, 3},
			wantAfter: 2,
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
			if want := tt.wantAfter > 0; got.PostCheckpointLoss() != want {
				t.Errorf("PostCheckpointLoss() = %v, want %v", got.PostCheckpointLoss(), want)
			}
		})
	}
}
