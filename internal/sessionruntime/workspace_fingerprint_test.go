package sessionruntime

import (
	"testing"

	"github.com/looprig/harness/pkg/event"
)

func TestRelocatedWorkspaceRoot(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		persisted string
		live      string
		want      string
	}{
		{name: "per-session base relocated is compatible", persisted: "session:/pods/a/ws", live: "session:/pods/b/ws", want: "session:/pods/b/ws"},
		{name: "per-session base unchanged", persisted: "session:/ws", live: "session:/ws", want: "session:/ws"},
		{name: "mode change compares verbatim", persisted: "session:/ws", live: "shared:/ws", want: "session:/ws"},
		{name: "mode change the other way compares verbatim", persisted: "exclusive:/ws", live: "session:/ws", want: "exclusive:/ws"},
		{name: "fixed exclusive root change compares verbatim", persisted: "exclusive:/repo-a", live: "exclusive:/repo-b", want: "exclusive:/repo-a"},
		{name: "fixed shared root change compares verbatim", persisted: "shared:/repo-a", live: "shared:/repo-b", want: "shared:/repo-a"},
		{name: "caller-supplied root compares verbatim", persisted: "/repo-a", live: "/repo-b", want: "/repo-a"},
		{name: "placement added compares verbatim", persisted: "", live: "session:/ws", want: ""},
		{name: "placement removed compares verbatim", persisted: "session:/ws", live: "", want: "session:/ws"},
		{name: "a bare mode word is not a placement fingerprint", persisted: "session", live: "session:/ws", want: "session"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := relocatedWorkspaceRoot(tt.persisted, tt.live); got != tt.want {
				t.Fatalf("relocatedWorkspaceRoot(%q, %q) = %q, want %q", tt.persisted, tt.live, got, tt.want)
			}
		})
	}
}

// TestPlacementFingerprintFormatIsUnchanged pins the v0.37.0 encoding: journals written by
// either version must carry the same WorkspaceRoot, so a mixed fleet and a rollback see
// the format they always wrote.
func TestPlacementFingerprintFormatIsUnchanged(t *testing.T) {
	t.Parallel()
	for mode, want := range map[WorkspacePlacementMode]string{
		PlacementExclusive: "exclusive:/r",
		PlacementSession:   "session:/r",
		PlacementShared:    "shared:/r",
		PlacementNone:      "none:/r",
	} {
		if got := PlacementFingerprint(mode, "/r"); got != want {
			t.Fatalf("PlacementFingerprint(%d) = %q, want %q", mode, got, want)
		}
	}
}

// TestLegacyRestoreAcceptsARelocatedSessionBase covers the schema-0 fingerprint path: a
// relocated per-session base is neither a mismatch nor a stale context, while a real
// fingerprint change still is.
func TestLegacyRestoreAcceptsARelocatedSessionBase(t *testing.T) {
	t.Parallel()
	persisted := event.ConfigFingerprint{ModelID: "m", WorkspaceRoot: "session:/pods/a"}
	live := event.ConfigFingerprint{ModelID: "m", WorkspaceRoot: "session:/pods/b"}
	stale, err := restoredContextDisposition(persisted, live, false)
	if err != nil || stale {
		t.Fatalf("relocated base: stale=%v err=%v, want compatible and not stale", stale, err)
	}
	live.ModelID = "other"
	if _, err := restoredContextDisposition(persisted, live, false); err == nil {
		t.Fatal("a model change under a relocated base was accepted")
	}
	moved := event.ConfigFingerprint{ModelID: "m", WorkspaceRoot: "shared:/pods/b"}
	if _, err := restoredContextDisposition(persisted, moved, false); err == nil {
		t.Fatal("a placement-mode change was accepted")
	}
}
