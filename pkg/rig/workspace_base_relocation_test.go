package rig

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/identity"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/session"
	"github.com/looprig/harness/pkg/sessionstore"
	"github.com/looprig/harness/pkg/tool"
)

// bindingRecorder captures every WorkspaceBinding a RequiresWorkspace tool is built with,
// so a test can read what a loop was actually bound to on create and on restore.
type bindingRecorder struct {
	mu       sync.Mutex
	bindings []tool.WorkspaceBinding
}

func (r *bindingRecorder) definition() tool.Definition {
	return tool.NewDefinition("workspace-probe", tool.RequiresWorkspace, func(_ context.Context, b tool.Bindings) ([]tool.InvokableTool, error) {
		r.mu.Lock()
		defer r.mu.Unlock()
		if b.Workspace != nil {
			r.bindings = append(r.bindings, *b.Workspace)
		}
		return []tool.InvokableTool{fpTool{name: "workspace-probe"}}, nil
	})
}

func (r *bindingRecorder) reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.bindings = nil
}

func (r *bindingRecorder) snapshot() []tool.WorkspaceBinding {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]tool.WorkspaceBinding(nil), r.bindings...)
}

func relocationLoop(t *testing.T, probe *bindingRecorder) loop.Definition {
	t.Helper()
	d, err := loop.Define(
		loop.WithName(identity.AgentName("planner")),
		loop.WithInference(&stubLLM{}, validModel("planner")),
		loop.WithTools(probe.definition()),
	)
	if err != nil {
		t.Fatalf("loop.Define: %v", err)
	}
	return d
}

func relocationRig(t *testing.T, store *sessionstore.Store, def loop.Definition, placement Option) *Rig {
	t.Helper()
	r, err := Define(
		WithLoops(def), WithPrimers("planner"), WithSessionStore(store),
		placement, WithSnapshots(SnapshotPolicy{Trigger: SnapshotManual}),
	)
	if err != nil {
		t.Fatalf("Define: %v", err)
	}
	return r
}

func workspaceStatusOf(t *testing.T, s any) session.WorkspaceStatus {
	t.Helper()
	reporter, ok := s.(session.WorkspaceReporter)
	if !ok {
		t.Fatalf("%T does not report a workspace", s)
	}
	return reporter.WorkspaceStatus()
}

// createCheckpointedSession creates a session under baseDir, writes marker bytes into its
// per-session root, checkpoints, and shuts down — the durable state a Host releases.
func createCheckpointedSession(t *testing.T, r *Rig, marker string) (sessionID uuid.UUID, created session.WorkspaceStatus) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	s, err := r.NewSession(ctx)
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	created = workspaceStatusOf(t, s)
	if err := os.WriteFile(filepath.Join(created.Root, "notes.txt"), []byte(marker), 0o644); err != nil {
		t.Fatalf("write marker: %v", err)
	}
	if _, err := s.CheckpointWorkspace(ctx); err != nil {
		t.Fatalf("CheckpointWorkspace: %v", err)
	}
	id := s.SessionID()
	if err := s.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	return id, created
}

// TestRestoreOnAHostWithADifferentWorkspaceBaseIsCompatible is the pooled-Host
// relocation case: the session was created under one physical base and is restored by a
// rig whose base is a different directory (a pod-specific path, a changed mountPath). The
// physical base is host-local; the durable snapshot carries the content and the
// model-visible logical root is derived from the session id, so the default restore
// policy must accept it — and the restored tree must hold the checkpointed bytes at the
// NEW physical root, with the same logical root.
func TestRestoreOnAHostWithADifferentWorkspaceBaseIsCompatible(t *testing.T) {
	store := sessionStoreT(t)
	ws := wsStoreT(t)
	probe := &bindingRecorder{}
	def := relocationLoop(t, probe)

	baseA, baseB := t.TempDir(), t.TempDir()
	id, created := createCheckpointedSession(t, relocationRig(t, store, def, WithSessionWorkspaces(ws, baseA)), "bytes-from-host-a")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	restoring := relocationRig(t, store, def, WithSessionWorkspaces(ws, baseB))
	restored, err := restoring.RestoreSession(ctx, id)
	if err != nil {
		t.Fatalf("RestoreSession on a different workspace base: %v", err)
	}
	defer func() { _ = restored.Shutdown(context.Background()) }()

	status := workspaceStatusOf(t, restored)
	canonB, err := canonicalPath(baseB)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(status.Root, canonB+string(filepath.Separator)) {
		t.Fatalf("restored physical root %q is not under the new base %q", status.Root, canonB)
	}
	if status.LogicalRoot == "" || status.LogicalRoot != created.LogicalRoot {
		t.Fatalf("restored logical root %q, want the created one %q", status.LogicalRoot, created.LogicalRoot)
	}
	if got := readRelocated(t, filepath.Join(status.Root, "notes.txt")); got != "bytes-from-host-a" {
		t.Fatalf("restored notes.txt = %q, want the checkpointed bytes", got)
	}
	// A relocation is not a configuration change: nothing is adopted, so the journal
	// carries no ConfigurationAdopted that would mark the context stale.
	for _, ev := range replayRigEvents(t, store, id) {
		if adopted, ok := ev.(event.ConfigurationAdopted); ok {
			t.Fatalf("a base relocation adopted a configuration change: %+v", adopted.Drift)
		}
	}
}

// TestRestoreThroughASymlinkedBaseThatResolvesElsewhereIsCompatible covers the base that
// is the same configured string but canonicalizes (EvalSymlinks) to a different directory
// on the restoring node.
func TestRestoreThroughASymlinkedBaseThatResolvesElsewhereIsCompatible(t *testing.T) {
	store := sessionStoreT(t)
	ws := wsStoreT(t)
	probe := &bindingRecorder{}
	def := relocationLoop(t, probe)

	link := filepath.Join(t.TempDir(), "workspaces")
	targetA, targetB := t.TempDir(), t.TempDir()
	if err := os.Symlink(targetA, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	id, _ := createCheckpointedSession(t, relocationRig(t, store, def, WithSessionWorkspaces(ws, link)), "via-link")
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(targetB, link); err != nil {
		t.Fatalf("re-symlink: %v", err)
	}
	restored, err := relocationRig(t, store, def, WithSessionWorkspaces(ws, link)).RestoreSession(context.Background(), id)
	if err != nil {
		t.Fatalf("RestoreSession through a re-pointed symlink: %v", err)
	}
	_ = restored.Shutdown(context.Background())
}

// TestRestoreStillRefusesAPlacementModeChange keeps the part of the workspace fingerprint
// that IS semantic: a per-session workspace restored as a shared fixed root is a different
// workspace contract, and the default policy still refuses it.
func TestRestoreStillRefusesAPlacementModeChange(t *testing.T) {
	store := sessionStoreT(t)
	ws := wsStoreT(t)
	probe := &bindingRecorder{}
	def := relocationLoop(t, probe)

	id, _ := createCheckpointedSession(t, relocationRig(t, store, def, WithSessionWorkspaces(ws, t.TempDir())), "x")
	restored, err := relocationRig(t, store, def, WithSharedWorkspace(ws, t.TempDir())).RestoreSession(context.Background(), id)
	if restored != nil {
		_ = restored.Shutdown(context.Background())
	}
	var rejected *session.RestoreRejectedError
	if !errors.As(err, &rejected) {
		t.Fatalf("restore across a placement-mode change = %T %v, want RestoreRejectedError", err, err)
	}
	if !hasDriftCategory(rejected.Assessment, event.DriftWorkspace) {
		t.Fatalf("rejection did not name workspace drift: %+v", rejected.Assessment)
	}
}

// TestRestoreStillRefusesADifferentFixedRoot keeps the fixed-root identity: an exclusive or
// shared root names WHICH tree (and whose .skills/) the session ran against, so moving it
// is a different workspace, not a relocation.
func TestRestoreStillRefusesADifferentFixedRoot(t *testing.T) {
	store := sessionStoreT(t)
	ws := wsStoreT(t)
	probe := &bindingRecorder{}
	def := relocationLoop(t, probe)

	ctx := context.Background()
	s, err := relocationRig(t, store, def, WithSharedWorkspace(ws, t.TempDir())).NewSession(ctx)
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	id := s.SessionID()
	if err := s.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	restored, err := relocationRig(t, store, def, WithSharedWorkspace(ws, t.TempDir())).RestoreSession(ctx, id)
	if restored != nil {
		_ = restored.Shutdown(ctx)
	}
	var rejected *session.RestoreRejectedError
	if !errors.As(err, &rejected) || !hasDriftCategory(rejected.Assessment, event.DriftWorkspace) {
		t.Fatalf("restore on a different shared root = %T %v, want workspace RestoreRejectedError", err, err)
	}
}

func hasDriftCategory(a event.DriftAssessment, c event.DriftCategory) bool {
	for _, change := range a.Changes {
		if change.Category == c {
			return true
		}
	}
	return false
}

func readRelocated(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile %s: %v", path, err)
	}
	return string(b)
}
