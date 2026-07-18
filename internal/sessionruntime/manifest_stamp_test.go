package sessionruntime

import (
	"context"
	"testing"

	"github.com/looprig/harness/pkg/event"
)

// TestSessionStartedCarriesManifestBaseline proves a newly created session stamps
// the rig-supplied ConfigManifest onto the first SessionStarted it publishes, right
// beside the legacy Config fingerprint. A late subscriber cannot observe the
// construction-time SessionStarted (the hub has no replay), so this drives the
// durable path: create via a journal-backed lifecycle, shut down, replay, and read
// the first SessionStarted back out of the ledger.
func TestSessionStartedCarriesManifestBaseline(t *testing.T) {
	t.Parallel()
	store := newRestoreStore(t)
	definition := restoreCfg(&stubLLM{}, "model-x", "be helpful")

	// Model rig's frozen path: a compatibility fingerprint and the manifest counterpart
	// assembled from the SAME inputs, so their shared topology revision is byte-equal.
	fingerprint := fingerprintFromDefinition(definition)
	fingerprint.TopologyRev = "topology-rev-baseline"
	manifest := event.ManifestFromLegacy(fingerprint)
	manifest.SchemaVersion = event.ManifestSchemaVersion

	lifecycle, err := newTestLifecycle(definition, store,
		WithLifecycleFingerprint(fingerprint),
		WithLifecycleManifest(manifest),
	)
	if err != nil {
		t.Fatalf("newTestLifecycle: %v", err)
	}
	session, err := lifecycle.NewSession(context.Background(), "")
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	sessionID := session.SessionID()
	if err := session.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}

	var ss event.SessionStarted
	found := false
	for _, ev := range replayAllSessionEvents(t, store, sessionID) {
		if started, ok := ev.(event.SessionStarted); ok {
			ss = started
			found = true
			break
		}
	}
	if !found {
		t.Fatal("no SessionStarted in replayed events")
	}

	// The newly created session carries a real manifest baseline.
	if ss.Manifest.SchemaVersion != event.ManifestSchemaVersion {
		t.Fatalf("SessionStarted.Manifest.SchemaVersion = %d, want %d", ss.Manifest.SchemaVersion, event.ManifestSchemaVersion)
	}
	// Manifest and legacy fingerprint agree on topology (parity under hustles).
	if ss.Manifest.TopologyRev != ss.Config.TopologyRev {
		t.Errorf("Manifest.TopologyRev %q != Config.TopologyRev %q", ss.Manifest.TopologyRev, ss.Config.TopologyRev)
	}
	// Manifest fingerprint is self-consistent (this is what ConfigurationAdopted later cross-checks).
	if ss.Manifest.Fingerprint() == "" {
		t.Error("Manifest.Fingerprint() empty")
	}
}
