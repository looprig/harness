package sessionruntime

import (
	"encoding/json"
	"testing"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/identity"
	"github.com/looprig/harness/pkg/sessionwire"
)

// TestPublicWorkspaceRootIsTheModelVisibleLogicalRoot pins the public projection's
// workspace_root to THIS package's logical-root derivation. pkg/sessionwire derives
// the same path independently (it may not import the runtime), so without this test
// the root a viewer is shown and the root the agent works under could drift apart.
func TestPublicWorkspaceRootIsTheModelVisibleLogicalRoot(t *testing.T) {
	t.Parallel()
	sid, err := uuid.New()
	if err != nil {
		t.Fatalf("uuid.New: %v", err)
	}
	eventID, err := uuid.New()
	if err != nil {
		t.Fatalf("uuid.New: %v", err)
	}
	physical := PlacementFingerprint(PlacementSession, "/var/lib/host-7/sessions")
	ev := event.SessionStarted{
		Header: event.Header{Coordinates: identity.Coordinates{SessionID: sid}, EventID: eventID},
		Config: event.ConfigFingerprint{WorkspaceRoot: physical},
	}
	projection, err := sessionwire.Project("tenant-a", "public-session", ev)
	if err != nil {
		t.Fatalf("Project: %v", err)
	}
	var body struct {
		Config struct {
			WorkspaceRoot string `json:"workspace_root"`
		} `json:"config"`
	}
	if err := json.Unmarshal(projection.Body, &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got, want := body.Config.WorkspaceRoot, logicalWorkspaceRoot(sid); got != want || want == "" {
		t.Errorf("public workspace_root = %q, want the runtime's logical root %q", got, want)
	}
}
