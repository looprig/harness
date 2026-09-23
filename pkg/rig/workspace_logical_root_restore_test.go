package rig

import (
	"context"
	"testing"
)

// TestARestoredLoopIsBoundToTheSessionsLogicalRoot pins that a loop planned at restore gets
// the same WorkspaceBinding.LogicalRoot a fresh loop got and WorkspaceStatus reports — the
// restore constructor must not bind through an identity-less probe.
func TestARestoredLoopIsBoundToTheSessionsLogicalRoot(t *testing.T) {
	store := sessionStoreT(t)
	ws := wsStoreT(t)
	probe := &bindingRecorder{}
	def := relocationLoop(t, probe)
	r := relocationRig(t, store, def, WithSessionWorkspaces(ws, t.TempDir()))

	id, created := createCheckpointedSession(t, r, "x")
	fresh := probe.snapshot()
	if len(fresh) == 0 || fresh[0].LogicalRoot == "" || fresh[0].LogicalRoot != created.LogicalRoot {
		t.Fatalf("fresh bindings %+v, want LogicalRoot %q", fresh, created.LogicalRoot)
	}
	probe.reset()

	restored, err := r.RestoreSession(context.Background(), id)
	if err != nil {
		t.Fatalf("RestoreSession: %v", err)
	}
	defer func() { _ = restored.Shutdown(context.Background()) }()
	status := workspaceStatusOf(t, restored)
	got := probe.snapshot()
	if len(got) == 0 {
		t.Fatal("the restored loop bound no workspace tool")
	}
	for _, b := range got {
		if b.LogicalRoot != created.LogicalRoot || b.LogicalRoot != status.LogicalRoot {
			t.Fatalf("restored binding LogicalRoot %q, want %q (created) == %q (status)", b.LogicalRoot, created.LogicalRoot, status.LogicalRoot)
		}
		if b.Root != status.Root {
			t.Fatalf("restored binding Root %q, want status Root %q", b.Root, status.Root)
		}
	}
}
