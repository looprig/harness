package sessionruntime

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/hub"
	"github.com/looprig/harness/pkg/identity"
	"github.com/looprig/harness/pkg/journal"
	"github.com/looprig/harness/pkg/sessionstore"
	"github.com/looprig/storage"
	"github.com/looprig/storage/memstore"
)

// --- new-path (drift-assessed restore) fixtures -------------------------------------
//
// The NEW restore path is taken when a LIVE ConfigManifest (SchemaVersion >= 1) is
// configured via WithManifest. These fixtures persist an original stream whose
// SessionStarted carries a schema-1 manifest baseline (or a legacy Config only, for the
// baseline-upgrade case), then re-restore with an overridden live manifest + decider.

// baselineManifest is a self-consistent schema-1 manifest used as the persisted baseline.
func baselineManifest() event.ConfigManifest {
	return event.ConfigManifest{
		SchemaVersion:   event.ManifestSchemaVersion,
		ModelID:         "model-x",
		SystemPromptRev: "sys-rev-1",
		TopologyRev:     "topo-rev-1",
		WorkspaceRoot:   "/repo",
	}
}

// newManifestHub wires a journal-backed hub for an original run whose SessionStarted
// carries a schema-1 (or, when manifest is the zero value, legacy) ConfigManifest, plus a
// root LoopStarted stamped with agentName. It mirrors newOriginalHubNamed but stamps the
// additive Manifest field so latestAdoptedBaseline projects a real baseline.
func newManifestHub(t *testing.T, store *sessionstore.Store, fp event.ConfigFingerprint, manifest event.ConfigManifest, agentName identity.AgentName) (*hub.Hub, uuid.UUID, uuid.UUID, journal.Lease, *eventStamper) {
	t.Helper()
	sessionID := mustSessionID(t)
	rootLoopID := mustSessionID(t)
	lease := mustAcquireLease(t, store, sessionID)

	openCtx, openCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer openCancel()
	j, err := store.OpenJournal(openCtx, sessionID, lease)
	if err != nil {
		t.Fatalf("OpenJournal: %v", err)
	}
	h := hub.New(sessionID, hub.WithAppender(journal.NewJournalEventAppender(j)), hub.WithFactory(testFactory()))

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	es := &eventStamper{}
	es.stamp(t, ctx, h, event.SessionStarted{
		Header:   event.Header{Coordinates: identity.Coordinates{SessionID: sessionID}},
		Config:   fp,
		Manifest: manifest,
	})
	es.stamp(t, ctx, h, event.LoopStarted{
		Header: event.Header{
			Coordinates: identity.Coordinates{SessionID: sessionID, LoopID: rootLoopID},
			AgentName:   agentName,
		},
		Runtime: runtimeFromFingerprint(fp),
	})
	return h, sessionID, rootLoopID, lease, es
}

// buildManifestStream persists a minimal original run (SessionStarted+manifest, root
// LoopStarted) and returns the identifiers + still-held lease for the handover.
func buildManifestStream(t *testing.T, store *sessionstore.Store, fp event.ConfigFingerprint, manifest event.ConfigManifest, agentName identity.AgentName) persistedStream {
	t.Helper()
	_, sessionID, rootLoopID, lease, _ := newManifestHub(t, store, fp, manifest, agentName)
	return persistedStream{sessionID: sessionID, rootLoopID: rootLoopID, lease: lease}
}

// publishAdopted stamps a ConfigurationAdopted with a fresh EventID and publishes it
// through the journal-backed hub (setHeader does not cover ConfigurationAdopted, so the
// epoch-history fixtures stamp it here). tag distinguishes successive EventIDs.
func publishAdopted(t *testing.T, h *hub.Hub, tag byte, adopted event.ConfigurationAdopted) {
	t.Helper()
	hdr := adopted.Header
	hdr.EventID = uuid.UUID{0xC0, tag}
	hdr.CreatedAt = time.Date(2026, 6, 21, 12, 0, 0, 0, time.UTC)
	adopted.Header = hdr
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := h.PublishEvent(ctx, adopted); err != nil {
		t.Fatalf("PublishEvent(ConfigurationAdopted): %v", err)
	}
}

// adoptedEvent builds a valid session-scoped ConfigurationAdopted (self-consistent
// fingerprint, policy source) for seeding an epoch history.
func adoptedEvent(sessionID uuid.UUID, epoch event.ConfigEpoch, manifest event.ConfigManifest, prev string) event.ConfigurationAdopted {
	return event.ConfigurationAdopted{
		Header:              event.Header{Coordinates: identity.Coordinates{SessionID: sessionID}},
		Epoch:               epoch,
		PreviousFingerprint: prev,
		AdoptedFingerprint:  manifest.Fingerprint(),
		Manifest:            manifest,
		Source:              event.DecisionSourcePolicy,
	}
}

// findAdopted returns the single ConfigurationAdopted in a replayed stream (fails if there
// is not exactly one).
func findAdopted(t *testing.T, events []event.Event) event.ConfigurationAdopted {
	t.Helper()
	var found []event.ConfigurationAdopted
	for _, ev := range events {
		if adopted, ok := ev.(event.ConfigurationAdopted); ok {
			found = append(found, adopted)
		}
	}
	if len(found) != 1 {
		t.Fatalf("want exactly one ConfigurationAdopted, got %d", len(found))
	}
	return found[0]
}

func countAdopted(events []event.Event) int {
	n := 0
	for _, ev := range events {
		if _, ok := ev.(event.ConfigurationAdopted); ok {
			n++
		}
	}
	return n
}

// firstIndexOf returns the index of the first event whose concrete type matches want, or -1.
func firstIndexOf(events []event.Event, want event.Event) int {
	for i, ev := range events {
		switch ev.(type) {
		case event.RestoreStarted:
			if _, ok := want.(event.RestoreStarted); ok {
				return i
			}
		case event.RestoreDone:
			if _, ok := want.(event.RestoreDone); ok {
				return i
			}
		case event.ConfigurationAdopted:
			if _, ok := want.(event.ConfigurationAdopted); ok {
				return i
			}
		}
	}
	return -1
}

// errDecider is a RestoreDecider that always fails, proving a decider error rejects.
type errDecider struct{}

func (errDecider) DecideRestore(context.Context, event.DriftAssessment) (RestoreDecision, error) {
	return RestoreDecision{}, errors.New("decider boom")
}

// TestRestoreAcceptsInfoDriftAndAdopts: an Info-level manifest change (different ModelID)
// under the default decider restores successfully and durably records a ConfigurationAdopted
// at epoch 2, sourced by policy, carrying exactly the Info change, appended AFTER
// RestoreStarted and BEFORE RestoreDone.
func TestRestoreAcceptsInfoDriftAndAdopts(t *testing.T) {
	store := newRestoreStore(t)
	definition := restoreCfg(&stubLLM{}, "model-x", "be helpful")
	fp := fingerprintFromDefinition(definition)

	baseline := baselineManifest()
	orig := buildManifestStream(t, store, fp, baseline, "agent")
	handOver(t, orig.lease)

	candidate := baselineManifest()
	candidate.ModelID = "model-y" // Info drift
	s, err := restoreTestSession(context.Background(), definition, orig.sessionID, store,
		WithManifest(candidate))
	if err != nil {
		t.Fatalf("Restore (info drift): %v", err)
	}
	t.Cleanup(func() { _ = s.Shutdown(context.Background()) })

	events := replayAllSessionEvents(t, store, orig.sessionID)
	adopted := findAdopted(t, events)
	if adopted.Epoch != 2 {
		t.Errorf("adopted.Epoch = %d, want 2", adopted.Epoch)
	}
	if adopted.Source != event.DecisionSourcePolicy {
		t.Errorf("adopted.Source = %q, want %q", adopted.Source, event.DecisionSourcePolicy)
	}
	if adopted.AdoptedFingerprint != candidate.Fingerprint() {
		t.Errorf("adopted.AdoptedFingerprint = %q, want %q", adopted.AdoptedFingerprint, candidate.Fingerprint())
	}
	if adopted.PreviousFingerprint != baseline.Fingerprint() {
		t.Errorf("adopted.PreviousFingerprint = %q, want %q", adopted.PreviousFingerprint, baseline.Fingerprint())
	}
	if len(adopted.Drift) != 1 || adopted.Drift[0].Category != event.DriftModel || adopted.Drift[0].Severity != event.DriftInfo {
		t.Errorf("adopted.Drift = %+v, want exactly one DriftModel/Info change", adopted.Drift)
	}
	started := firstIndexOf(events, event.RestoreStarted{})
	adoptedIdx := firstIndexOf(events, event.ConfigurationAdopted{})
	done := firstIndexOf(events, event.RestoreDone{})
	if !(started >= 0 && adoptedIdx > started && done > adoptedIdx) {
		t.Errorf("ordering wrong: RestoreStarted@%d < ConfigurationAdopted@%d < RestoreDone@%d", started, adoptedIdx, done)
	}
}

// TestRestoreRejectsWarnDriftHeadless: a Warn change (different WorkspaceRoot) under the
// default fail-secure decider rejects with *RestoreRejectedError, records a RestoreErrored,
// appends NO ConfigurationAdopted, and releases the lease (a second restore re-acquires).
func TestRestoreRejectsWarnDriftHeadless(t *testing.T) {
	store := newRestoreStore(t)
	definition := restoreCfg(&stubLLM{}, "model-x", "be helpful")
	fp := fingerprintFromDefinition(definition)

	orig := buildManifestStream(t, store, fp, baselineManifest(), "agent")
	handOver(t, orig.lease)

	candidate := baselineManifest()
	candidate.WorkspaceRoot = "/somewhere-else" // Warn drift
	s, err := restoreTestSession(context.Background(), definition, orig.sessionID, store,
		WithManifest(candidate))
	if s != nil {
		t.Fatalf("Restore returned a non-nil Session on a rejected Warn drift")
	}
	var rejected *RestoreRejectedError
	if !errors.As(err, &rejected) {
		t.Fatalf("Restore err = %v, want *RestoreRejectedError", err)
	}

	events := replayAllSessionEvents(t, store, orig.sessionID)
	if countAdopted(events) != 0 {
		t.Errorf("want no ConfigurationAdopted on a rejected restore, got %d", countAdopted(events))
	}
	tail := restoreEventTail(t, store, orig.sessionID, orig.rootLoopID)
	if !lastIs(tail, event.RestoreErrored{}) {
		t.Errorf("restore-event tail does not end with RestoreErrored: %v", tailTypes(tail))
	}

	// The rejected attempt released its lease: a second restore (accepting the drift via the
	// shim) re-acquires without waiting out the TTL.
	s2, err := restoreTestSession(context.Background(), definition, orig.sessionID, store,
		WithManifest(candidate), WithAllowConfigMismatch())
	if err != nil {
		t.Fatalf("second Restore (lease not released?): %v", err)
	}
	t.Cleanup(func() { _ = s2.Shutdown(context.Background()) })
}

// TestRestoreNoDriftAppendsNoEpoch: an identical live manifest restores successfully and
// appends NO ConfigurationAdopted (no config difference, no schema upgrade).
func TestRestoreNoDriftAppendsNoEpoch(t *testing.T) {
	store := newRestoreStore(t)
	definition := restoreCfg(&stubLLM{}, "model-x", "be helpful")
	fp := fingerprintFromDefinition(definition)

	orig := buildManifestStream(t, store, fp, baselineManifest(), "agent")
	handOver(t, orig.lease)

	s, err := restoreTestSession(context.Background(), definition, orig.sessionID, store,
		WithManifest(baselineManifest()))
	if err != nil {
		t.Fatalf("Restore (no drift): %v", err)
	}
	t.Cleanup(func() { _ = s.Shutdown(context.Background()) })

	events := replayAllSessionEvents(t, store, orig.sessionID)
	if countAdopted(events) != 0 {
		t.Errorf("want no ConfigurationAdopted on a no-drift restore, got %d", countAdopted(events))
	}
	assertTail(t, restoreEventTail(t, store, orig.sessionID, orig.rootLoopID),
		[]event.Event{event.RestoreStarted{}, event.RestoreDone{}})
}

// TestRestoreLegacyBaselineUpgrades: a session whose SessionStarted carries a legacy Config
// only (SchemaVersion-0 baseline), restored with a behaviorally-identical schema-1
// candidate, appends a one-time baseline-upgrade ConfigurationAdopted (epoch 2, policy,
// empty PreviousFingerprint). A SECOND restore with the same manifest appends nothing (the
// baseline is now schema-1 and equal).
func TestRestoreLegacyBaselineUpgrades(t *testing.T) {
	store := newRestoreStore(t)
	definition := restoreCfg(&stubLLM{}, "model-x", "be helpful")

	candidate := event.ConfigManifest{
		SchemaVersion:   event.ManifestSchemaVersion,
		ModelID:         "model-x",
		SystemPromptRev: "sys-rev",
		TopologyRev:     "topo-rev",
	}
	// A legacy fingerprint whose projection is behaviorally identical to the candidate, so
	// the only assessed difference is the schema upgrade itself.
	legacyFP := event.ConfigFingerprint{
		ModelID:         "model-x",
		SystemPromptRev: "sys-rev",
		TopologyRev:     "topo-rev",
		ToolPolicyRev:   candidate.ToolNamesRev(),
	}
	// The persisted SessionStarted carries the legacy Config and a ZERO manifest.
	orig := buildManifestStream(t, store, legacyFP, event.ConfigManifest{}, "agent")
	handOver(t, orig.lease)

	s, err := restoreTestSession(context.Background(), definition, orig.sessionID, store,
		WithManifest(candidate))
	if err != nil {
		t.Fatalf("Restore (legacy upgrade): %v", err)
	}
	if err := s.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown #1: %v", err)
	}

	adopted := findAdopted(t, replayAllSessionEvents(t, store, orig.sessionID))
	if adopted.Epoch != 2 {
		t.Errorf("adopted.Epoch = %d, want 2", adopted.Epoch)
	}
	if adopted.Source != event.DecisionSourcePolicy {
		t.Errorf("adopted.Source = %q, want %q", adopted.Source, event.DecisionSourcePolicy)
	}
	if adopted.PreviousFingerprint != "" {
		t.Errorf("adopted.PreviousFingerprint = %q, want empty (legacy baseline has no fingerprint)", adopted.PreviousFingerprint)
	}
	if len(adopted.Drift) != 0 {
		t.Errorf("adopted.Drift = %+v, want empty (pure baseline upgrade)", adopted.Drift)
	}

	// Second restore: the baseline is now the schema-1 adoption and equal → nothing appended.
	s2, err := restoreTestSession(context.Background(), definition, orig.sessionID, store,
		WithManifest(candidate))
	if err != nil {
		t.Fatalf("Restore #2 (legacy upgrade): %v", err)
	}
	t.Cleanup(func() { _ = s2.Shutdown(context.Background()) })
	if got := countAdopted(replayAllSessionEvents(t, store, orig.sessionID)); got != 1 {
		t.Errorf("after second restore want still exactly 1 ConfigurationAdopted, got %d", got)
	}
}

// TestRestoreShimAcceptsWarn: a manifest session + WithAllowConfigMismatch accepts a Warn
// change through the NEW path (the shim installs AcceptAllDecider), recording a
// ConfigurationAdopted sourced by policy.
func TestRestoreShimAcceptsWarn(t *testing.T) {
	store := newRestoreStore(t)
	definition := restoreCfg(&stubLLM{}, "model-x", "be helpful")
	fp := fingerprintFromDefinition(definition)

	orig := buildManifestStream(t, store, fp, baselineManifest(), "agent")
	handOver(t, orig.lease)

	candidate := baselineManifest()
	candidate.WorkspaceRoot = "/elsewhere" // Warn drift
	s, err := restoreTestSession(context.Background(), definition, orig.sessionID, store,
		WithManifest(candidate), WithAllowConfigMismatch())
	if err != nil {
		t.Fatalf("Restore (shim accepts warn): %v", err)
	}
	t.Cleanup(func() { _ = s.Shutdown(context.Background()) })

	adopted := findAdopted(t, replayAllSessionEvents(t, store, orig.sessionID))
	if adopted.Source != event.DecisionSourcePolicy {
		t.Errorf("adopted.Source = %q, want %q", adopted.Source, event.DecisionSourcePolicy)
	}
	if adopted.Epoch != 2 {
		t.Errorf("adopted.Epoch = %d, want 2", adopted.Epoch)
	}
}

// TestRestoreDeciderErrorRejects: a decider returning a non-nil error fails the restore,
// records a RestoreErrored, and appends no adoption.
func TestRestoreDeciderErrorRejects(t *testing.T) {
	store := newRestoreStore(t)
	definition := restoreCfg(&stubLLM{}, "model-x", "be helpful")
	fp := fingerprintFromDefinition(definition)

	orig := buildManifestStream(t, store, fp, baselineManifest(), "agent")
	handOver(t, orig.lease)

	candidate := baselineManifest()
	candidate.ModelID = "model-y" // Info drift so the decider is consulted
	s, err := restoreTestSession(context.Background(), definition, orig.sessionID, store,
		WithManifest(candidate), WithRestoreDecider(errDecider{}))
	if s != nil {
		t.Fatalf("Restore returned a non-nil Session on a decider error")
	}
	if err == nil {
		t.Fatal("Restore err = nil, want a decider error")
	}

	events := replayAllSessionEvents(t, store, orig.sessionID)
	if countAdopted(events) != 0 {
		t.Errorf("want no ConfigurationAdopted after a decider error, got %d", countAdopted(events))
	}
	tail := restoreEventTail(t, store, orig.sessionID, orig.rootLoopID)
	if !lastIs(tail, event.RestoreErrored{}) {
		t.Errorf("restore-event tail does not end with RestoreErrored: %v", tailTypes(tail))
	}
}

// TestRestoreAgentNameMismatchIsWarnDrift: a manifest session whose persisted root AgentName
// differs from the configured one folds an agent-name Warn change into the assessment. The
// default decider REJECTS; an AcceptAllDecider SUCCEEDS and adopts.
func TestRestoreAgentNameMismatchIsWarnDrift(t *testing.T) {
	store := newRestoreStore(t)
	// The configured definition is named "agent"; the persisted root is "operator".
	definition := restoreCfgNamed(&stubLLM{}, "model-x", "be helpful", "agent")
	fp := fingerprintFromDefinition(definition)

	// No manifest drift — the ONLY change is the agent-name fold.
	orig := buildManifestStream(t, store, fp, baselineManifest(), "operator")
	handOver(t, orig.lease)

	// Default decider rejects the agent-name Warn.
	s, err := restoreTestSession(context.Background(), definition, orig.sessionID, store,
		WithManifest(baselineManifest()))
	if s != nil {
		t.Fatalf("Restore returned a non-nil Session on an agent-name Warn under default policy")
	}
	var rejected *RestoreRejectedError
	if !errors.As(err, &rejected) {
		t.Fatalf("Restore err = %v, want *RestoreRejectedError", err)
	}
	foundAgentChange := false
	for _, change := range rejected.Assessment.Changes {
		if change.Category == event.DriftAgentKind && change.Field == "agent_name" && change.Severity == event.DriftWarn {
			foundAgentChange = true
		}
	}
	if !foundAgentChange {
		t.Errorf("assessment.Changes = %+v, want an agent_name Warn change", rejected.Assessment.Changes)
	}

	// AcceptAllDecider accepts and adopts (the rejected attempt released its lease).
	s2, err := restoreTestSession(context.Background(), definition, orig.sessionID, store,
		WithManifest(baselineManifest()), WithRestoreDecider(AcceptAllDecider{}))
	if err != nil {
		t.Fatalf("Restore (accept-all, agent-name) err = %v, want success", err)
	}
	t.Cleanup(func() { _ = s2.Shutdown(context.Background()) })
	adopted := findAdopted(t, replayAllSessionEvents(t, store, orig.sessionID))
	sawAgentDrift := false
	for _, change := range adopted.Drift {
		if change.Category == event.DriftAgentKind && change.Field == "agent_name" {
			sawAgentDrift = true
		}
	}
	if !sawAgentDrift {
		t.Errorf("adopted.Drift = %+v, want an agent_name change", adopted.Drift)
	}
}

// TestRestoreEpochMonotonic: a journal already at epoch 3 (SessionStarted + two
// ConfigurationAdopted) restored with a fresh Info change adopts at epoch 4.
func TestRestoreEpochMonotonic(t *testing.T) {
	store := newRestoreStore(t)
	definition := restoreCfg(&stubLLM{}, "model-x", "be helpful")
	fp := fingerprintFromDefinition(definition)

	base := baselineManifest()
	manifest2 := baselineManifest()
	manifest2.ModelID = "model-2"
	manifest3 := baselineManifest()
	manifest3.ModelID = "model-3"

	h, sessionID, _, lease, _ := newManifestHub(t, store, fp, base, "agent")
	publishAdopted(t, h, 0x02, adoptedEvent(sessionID, 2, manifest2, base.Fingerprint()))
	publishAdopted(t, h, 0x03, adoptedEvent(sessionID, 3, manifest3, manifest2.Fingerprint()))
	handOver(t, lease)

	candidate := baselineManifest()
	candidate.ModelID = "model-4" // Info drift vs the epoch-3 baseline
	s, err := restoreTestSession(context.Background(), definition, sessionID, store,
		WithManifest(candidate))
	if err != nil {
		t.Fatalf("Restore (epoch monotonic): %v", err)
	}
	t.Cleanup(func() { _ = s.Shutdown(context.Background()) })

	events := replayAllSessionEvents(t, store, sessionID)
	var newest event.ConfigurationAdopted
	for _, ev := range events {
		if adopted, ok := ev.(event.ConfigurationAdopted); ok {
			newest = adopted
		}
	}
	if newest.Epoch != 4 {
		t.Errorf("newest adoption Epoch = %d, want 4 (baseline was epoch 3)", newest.Epoch)
	}
	if newest.PreviousFingerprint != manifest3.Fingerprint() {
		t.Errorf("newest.PreviousFingerprint = %q, want epoch-3 fingerprint", newest.PreviousFingerprint)
	}
}

// TestRestoreMultiEpochBaselineSelection pins the core behavioral change of the feature:
// the drift baseline is the LATEST adopted manifest, never the first SessionStarted. The
// committed history is SessionStarted(epoch-1 baseline) + ConfigurationAdopted(epoch 2) +
// ConfigurationAdopted(epoch 3); epoch 3's manifest EQUALS the live candidate while the
// epoch-1 SessionStarted manifest does NOT. Restore therefore assesses ZERO drift against the
// epoch-3 baseline, succeeds, and appends NO new ConfigurationAdopted. If restore wrongly
// baselined off the SessionStarted manifest it would see an Info (ModelID) change and adopt a
// fourth epoch — so a still-exactly-2 count is the proof.
func TestRestoreMultiEpochBaselineSelection(t *testing.T) {
	store := newRestoreStore(t)
	definition := restoreCfg(&stubLLM{}, "model-x", "be helpful")
	fp := fingerprintFromDefinition(definition)

	base := baselineManifest() // epoch-1 SessionStarted baseline (ModelID "model-x")
	manifest2 := baselineManifest()
	manifest2.ModelID = "model-2"
	manifest3 := baselineManifest()
	manifest3.ModelID = "model-3" // the LATEST adopted manifest

	h, sessionID, _, lease, _ := newManifestHub(t, store, fp, base, "agent")
	publishAdopted(t, h, 0x02, adoptedEvent(sessionID, 2, manifest2, base.Fingerprint()))
	publishAdopted(t, h, 0x03, adoptedEvent(sessionID, 3, manifest3, manifest2.Fingerprint()))
	handOver(t, lease)

	// The live candidate MATCHES the epoch-3 baseline exactly (model-3) but differs from the
	// epoch-1 SessionStarted manifest (model-x).
	candidate := baselineManifest()
	candidate.ModelID = "model-3"
	s, err := restoreTestSession(context.Background(), definition, sessionID, store,
		WithManifest(candidate))
	if err != nil {
		t.Fatalf("Restore (multi-epoch, candidate == latest baseline): %v", err)
	}
	t.Cleanup(func() { _ = s.Shutdown(context.Background()) })

	events := replayAllSessionEvents(t, store, sessionID)
	if got := countAdopted(events); got != 2 {
		t.Errorf("want exactly 2 ConfigurationAdopted (baseline is epoch-3 latest, zero drift), got %d", got)
	}
	assertTail(t, restoreEventTail(t, store, sessionID, mustRootLoopID(t, events)),
		[]event.Event{event.RestoreStarted{}, event.RestoreDone{}})
}

// mustRootLoopID returns the LoopID of the single root (zero-Cause) LoopStarted in a replayed
// stream — the loop coordinate restoreEventTail narrows on.
func mustRootLoopID(t *testing.T, events []event.Event) uuid.UUID {
	t.Helper()
	for _, ev := range events {
		if started, ok := ev.(event.LoopStarted); ok && started.Cause.Coordinates == (identity.Coordinates{}) {
			return started.LoopID
		}
	}
	t.Fatal("no root LoopStarted in replayed stream")
	return uuid.UUID{}
}

// --- lease-loss simulation for the restore adoption append -------------------------------
//
// The restore path acquires its OWN single-writer lease internally (store.AcquireLease), so a
// test cannot hand it a pre-lost lease. The reusable primitive is the journal's fast-path
// lease guard: sessionJournal.Append refuses with *journal.JournalLeaseLostError once the
// lease's Lost channel is closed (see sessionstore TestAppendAfterLeaseLost). We drive that
// same guard mid-restore by wrapping the backend the restore Store is opened over:
// leaseCapturingLeaser records every lease it grants, and leaseLosingLedger closes the most
// recently granted lease's Lost channel after a configured number of committed ledger
// appends. Restore's ledger appends, in order, are the opening LeaseFence (#1) and
// RestoreStarted (#2); the very next journal Append is the ConfigurationAdopted adoption.
// Losing the lease right after RestoreStarted commits (loseAfter == 2) makes the adoption
// append the FIRST append refused — precisely the append Task 10's appendConfigurationAdopted
// routes through the lease-checked journal.

// controllableLease wraps a storage.Lease, substituting a Lost channel the test closes to
// simulate an ownership takeover independent of the real backend.
type controllableLease struct {
	inner storage.Lease
	lost  chan struct{}
	once  sync.Once
}

func (l *controllableLease) Epoch() uint64         { return l.inner.Epoch() }
func (l *controllableLease) Lost() <-chan struct{} { return l.lost }

func (l *controllableLease) Release(ctx context.Context) error {
	l.lose()
	return l.inner.Release(ctx)
}

func (l *controllableLease) lose() { l.once.Do(func() { close(l.lost) }) }

// leaseCapturingLeaser wraps a storage.Leaser, wrapping each granted lease in a
// controllableLease and recording the most recent one so the ledger can lose it.
type leaseCapturingLeaser struct {
	inner storage.Leaser
	mu    sync.Mutex
	last  *controllableLease
}

func (le *leaseCapturingLeaser) Acquire(ctx context.Context, name string) (storage.Lease, error) {
	inner, err := le.inner.Acquire(ctx, name)
	if err != nil {
		return nil, err
	}
	l := &controllableLease{inner: inner, lost: make(chan struct{})}
	le.mu.Lock()
	le.last = l
	le.mu.Unlock()
	return l, nil
}

func (le *leaseCapturingLeaser) loseCurrent() {
	le.mu.Lock()
	l := le.last
	le.mu.Unlock()
	if l != nil {
		l.lose()
	}
}

// leaseLosingLedger wraps a storage.Ledger and, once loseAfter appends have committed, closes
// the leaser's current lease — simulating a mid-restore ownership takeover so the NEXT journal
// Append (the adoption) is refused with a lease-lost error.
type leaseLosingLedger struct {
	inner     storage.Ledger
	leaser    *leaseCapturingLeaser
	loseAfter int
	mu        sync.Mutex
	appends   int
}

func (lg *leaseLosingLedger) Append(ctx context.Context, name string, expected uint64, payload []byte) error {
	if err := lg.inner.Append(ctx, name, expected, payload); err != nil {
		return err
	}
	lg.mu.Lock()
	lg.appends++
	trigger := lg.appends == lg.loseAfter
	lg.mu.Unlock()
	if trigger {
		lg.leaser.loseCurrent()
	}
	return nil
}

func (lg *leaseLosingLedger) Read(ctx context.Context, name string, from uint64) (storage.Cursor, error) {
	return lg.inner.Read(ctx, name, from)
}

func (lg *leaseLosingLedger) Tip(ctx context.Context, name string) (uint64, error) {
	return lg.inner.Tip(ctx, name)
}

func (lg *leaseLosingLedger) Delete(ctx context.Context, name string) error {
	return lg.inner.Delete(ctx, name)
}

// TestRestoreAdoptionLeaseLost: a manifest-carrying session restored with an Info-level drift
// (so an adoption WOULD be appended) but with the session writer lease forced to move right
// after RestoreStarted commits. The adoption append — the next journal Append — must be
// refused under the lost lease, aborting the restore: no live session, the failure observable
// as a wrapped *journal.JournalLeaseLostError, and neither a ConfigurationAdopted nor a
// RestoreDone committed (the session never comes up under the unrecorded configuration).
func TestRestoreAdoptionLeaseLost(t *testing.T) {
	backend := memstore.New()
	origStore, err := sessionstore.Open(backend)
	if err != nil {
		t.Fatalf("sessionstore.Open(orig): %v", err)
	}
	definition := restoreCfg(&stubLLM{}, "model-x", "be helpful")
	fp := fingerprintFromDefinition(definition)

	orig := buildManifestStream(t, origStore, fp, baselineManifest(), "agent")
	handOver(t, orig.lease)

	// Wrap the SAME underlying backend for the restore Store so it reads the persisted stream
	// yet loses its lease after the opening fence + RestoreStarted, before the adoption append.
	leaser := &leaseCapturingLeaser{inner: backend.Leaser}
	ledger := &leaseLosingLedger{inner: backend.Ledger, leaser: leaser, loseAfter: 2}
	wrapped, err := storage.NewComposite(ledger, leaser, backend.KV, backend.Blobs)
	if err != nil {
		t.Fatalf("storage.NewComposite: %v", err)
	}
	restoreStore, err := sessionstore.Open(wrapped)
	if err != nil {
		t.Fatalf("sessionstore.Open(restore): %v", err)
	}

	candidate := baselineManifest()
	candidate.ModelID = "model-y" // Info drift → an adoption WOULD be appended
	s, err := restoreTestSession(context.Background(), definition, orig.sessionID, restoreStore,
		WithManifest(candidate))
	if s != nil {
		t.Fatalf("Restore returned a non-nil Session after the adoption append lost the lease")
	}
	var lost *journal.JournalLeaseLostError
	if !errors.As(err, &lost) {
		t.Fatalf("Restore err = %v, want a wrapped *journal.JournalLeaseLostError", err)
	}

	// The session never came up under the unrecorded configuration: no ConfigurationAdopted
	// and no RestoreDone committed to the durable stream. RestoreStarted DID commit (the lease
	// was lost only afterwards), proving the adoption is the append the lost lease refused.
	events := replayAllSessionEvents(t, restoreStore, orig.sessionID)
	if countAdopted(events) != 0 {
		t.Errorf("want no committed ConfigurationAdopted after a lost-lease adoption append, got %d", countAdopted(events))
	}
	sawStarted := false
	for _, ev := range events {
		switch ev.(type) {
		case event.RestoreStarted:
			sawStarted = true
		case event.RestoreDone:
			t.Error("RestoreDone committed despite the adoption append failing under a lost lease")
		}
	}
	if !sawStarted {
		t.Error("RestoreStarted was not committed; the lease was lost too early to exercise the adoption append")
	}
}
