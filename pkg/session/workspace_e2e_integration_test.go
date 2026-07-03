//go:build integration

package session

import (
	"context"
	"testing"

	"github.com/ciram-co/fsstore"
	"github.com/ciram-co/looprig/pkg/event"
	"github.com/ciram-co/looprig/pkg/sessionstore"
	"github.com/ciram-co/looprig/pkg/workspacestore"
)

// This is the end-to-end suspend/resume proof for the workspace-store feature against a
// REAL filesystem backend — the property the memstore round-trip tests in
// restore_workspace_test.go structurally CANNOT prove.
//
// # Why approach (A): session-level, over fsstore
//
// The plan offered two shapes. (A) drives the actual production APIs the workspace
// feature added — Session.CheckpointWorkspace (B1) on the suspend leg and
// session.Restore's materialize-on-restore path (B2) on the resume leg — reusing the
// existing session harness (newOriginalHub/stampCheckpoint/handOver/restoreCfg and the
// wsBuildTree/wsSnapshotTree/wsAssertTreesEqual tree helpers). (B) would drive
// workspacestore + sessionstore directly and re-discover the journal-append path by hand.
// (A) is the higher-fidelity e2e and reuses a proven harness, and it introduces NO import
// cycle: this test file is in package session (which already depends on sessionstore +
// workspacestore), and fsstore imports only storekit — never looprig. So (A) is chosen.
//
// # Why fsstore is the load-bearing difference
//
// memstore is in-memory: a second memstore.New() is a fresh empty universe, so a memstore
// "resume" always shares the live process's heap to see the prior session's journal + blobs.
// fsstore persists to disk. Opening a SECOND fsstore.Store over the SAME root — after the
// first is Closed, dropping every byte of its in-process state — sees the earlier session's
// durable ledger and blobs and nothing else. That is a genuine "resume on a new process /
// host": the ONLY channel between the two legs is the on-disk store root.

// openFSSessionStore opens a fresh fsstore.Store rooted at fsRoot and wraps it in a
// sessionstore.Store over the four-primitive field-bundle fsstore exposes (its embedded
// *storekit.Composite, via Backend). It returns both so the caller can address the durable
// Blobs (fs.Blobs) for the workspace store and Close the underlying store to simulate a
// process ending. Each call is an INDEPENDENT instance — that independence is the whole
// point of the resume leg.
func openFSSessionStore(t *testing.T, fsRoot string) (*sessionstore.Store, *fsstore.Store) {
	t.Helper()
	fs, err := fsstore.Open(fsstore.Options{Root: fsRoot})
	if err != nil {
		t.Fatalf("fsstore.Open(%q): %v", fsRoot, err)
	}
	store, err := sessionstore.Open(fs.Backend())
	if err != nil {
		t.Fatalf("sessionstore.Open over fsstore: %v", err)
	}
	return store, fs
}

// TestWorkspaceSuspendResumeAcrossFreshFSStore is the holistic proof that a session's
// workspace survives a process/host change against a real filesystem backend. It runs the
// full spec cycle: leg 1 builds a workspace tree, Snapshots it through workspacestore, and
// records a WorkspaceCheckpointed in the fsstore-backed session journal, then CLOSES the
// leg-1 store (process death); leg 2 opens a brand-new fsstore over the SAME on-disk root,
// Restores the session (which materializes the last checkpoint into a fresh empty root), and
// asserts the materialized tree equals the original down to file modes (incl. the 0755
// executable bit) and the symlink target. The last-checkpoint-wins case additionally proves
// the resume selects the most recent durable snapshot.
func TestWorkspaceSuspendResumeAcrossFreshFSStore(t *testing.T) {
	tests := []struct {
		name    string
		markers []string // one source tree per marker, snapshotted + checkpointed in order; the LAST must land
	}{
		{name: "single checkpoint resumes on a fresh store instance", markers: []string{"alpha"}},
		{name: "last checkpoint wins across the fresh instance", markers: []string{"alpha", "beta"}},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			fsRoot := t.TempDir() // the on-disk store shared by BOTH legs — the only channel between them
			fp := FingerprintFrom(restoreCfg(&stubLLM{}, "model-x", "be helpful"))

			// --- Leg 1: the original process. Snapshot the workspace, checkpoint it, then die. ---
			store1, fs1 := openFSSessionStore(t, fsRoot)
			ws1, err := workspacestore.Open(fs1.Blobs)
			if err != nil {
				t.Fatalf("workspacestore.Open (leg 1): %v", err)
			}

			var refs []string
			var lastSrc string
			for _, m := range tt.markers {
				src := t.TempDir()
				wsBuildTree(t, src, m) // nested dir + regular file + 0755 executable + relative symlink
				ref, err := ws1.Snapshot(context.Background(), src)
				if err != nil {
					t.Fatalf("Snapshot(%s): %v", m, err)
				}
				refs = append(refs, string(ref))
				lastSrc = src
			}

			// Durably record the checkpoint(s) in the fsstore-backed journal, then release the
			// single-writer lease (the suspend/handover boundary).
			orig := stampCheckpoint(t, store1, fp, refs...)
			handOver(t, orig.lease)

			// Process death: close the leg-1 store so NONE of its in-process state (the ledger's
			// cached name->file registry, any held lease fd, warm blob handles) can survive into
			// leg 2. After this line the checkpoint exists ONLY on disk under fsRoot.
			if err := fs1.Close(); err != nil {
				t.Fatalf("close leg-1 fsstore: %v", err)
			}

			// --- Leg 2: a fresh process / host. A brand-new fsstore over the SAME directory. ---
			store2, fs2 := openFSSessionStore(t, fsRoot)
			t.Cleanup(func() { _ = fs2.Close() })
			// Sanity sub-check: leg 2 is a genuinely distinct instance. The real guarantee that no
			// in-memory state is shared is fs1.Close() ABOVE (leg 1's heap state is gone before
			// leg 2 opens); these distinct pointers just make that structurally explicit.
			if fs2 == fs1 {
				t.Fatal("leg-2 fsstore must be a distinct instance from leg 1")
			}
			if store2 == store1 {
				t.Fatal("leg-2 sessionstore must be a distinct instance from leg 1")
			}
			ws2, err := workspacestore.Open(fs2.Blobs)
			if err != nil {
				t.Fatalf("workspacestore.Open (leg 2): %v", err)
			}

			freshRoot := t.TempDir() // empty → the truth path (extract from the durable blob)
			s, err := Restore(context.Background(), restoreCfg(&stubLLM{}, "model-x", "be helpful"),
				orig.sessionID, store2, WithWorkspaceStore(ws2, freshRoot))
			if err != nil {
				t.Fatalf("Restore on fresh fsstore: %v", err)
			}
			t.Cleanup(func() { _ = s.Shutdown(context.Background()) })

			// The whole point: a fresh store instance materialized the LAST checkpointed tree —
			// contents, permission modes (incl. the 0755 executable bit), and the symlink target —
			// purely from what leg 1 persisted to disk.
			wsAssertTreesEqual(t, lastSrc, freshRoot)

			// A clean restore tail: RestoreStarted → RestoreDone (the workspace restored, so the
			// restore was allowed to declare done), replayed from the durable fsstore journal.
			assertTail(t, restoreEventTail(t, store2, orig.sessionID, orig.primaryLoopID),
				[]event.Event{event.RestoreStarted{}, event.RestoreDone{}})
		})
	}
}
