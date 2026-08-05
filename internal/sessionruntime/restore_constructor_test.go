package sessionruntime

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/looprig/core/content"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/internal/loopruntime"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/hub"
	"github.com/looprig/harness/pkg/identity"
	"github.com/looprig/harness/pkg/journal"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/sessionstore"
	"github.com/looprig/inference"
	contextcount "github.com/looprig/inference/contextcount"
	model "github.com/looprig/inference/model"
)

// failingNewID is an idGenerator seam that mints monotonically-distinct UUIDs but
// returns a hard error on the failOnCall-th call (1-based), succeeding on every other
// call. It drives a single restore-lifecycle id-mint failure at a chosen point while
// letting the surrounding mints (notably the RestoreErrored recorded by recordErrored)
// succeed, so the fail-secure exit can be observed end to end.
type failingNewID struct {
	n          int
	failOnCall int
}

func TestPreSessionRestoreFailureRetainsOwnershipUntilErroredAppendDrains(t *testing.T) {
	t.Parallel()

	appendEntered := make(chan struct{})
	appendRelease := make(chan struct{})
	released := make(chan struct{})
	start := time.Now()
	runRestoreFailureCleanup(25*time.Millisecond, func(context.Context) {
		close(appendEntered)
		<-appendRelease
	}, func() { close(released) })
	if elapsed := time.Since(start); elapsed < 20*time.Millisecond || elapsed > 100*time.Millisecond {
		t.Fatalf("pre-session restore cleanup elapsed = %v, want bounded return", elapsed)
	}
	select {
	case <-appendEntered:
	default:
		t.Fatal("RestoreErrored append did not start")
	}
	select {
	case <-released:
		t.Fatal("ownership released while RestoreErrored append remained in flight")
	default:
	}
	close(appendRelease)
	select {
	case <-released:
	case <-time.After(time.Second):
		t.Fatal("cleanup owner did not release after RestoreErrored append drained")
	}
}

// errMintFailed is the leaf cause an injected id-mint failure surfaces. A sentinel is
// permitted: it is a context-free leaf used only by this test seam.
var errMintFailed = errors.New("restore_constructor_test: injected id-mint failure")

func (f *failingNewID) next() (uuid.UUID, error) {
	f.n++
	if f.n == f.failOnCall {
		return uuid.UUID{}, errMintFailed
	}
	// A distinct, non-zero id per call so the journal's Nats-Msg-Id never collides.
	return uuid.UUID{0xD0, byte(f.n)}, nil
}

// fixedClock is a deterministic event.Clock for the restore-lifecycle stamps.
func fixedClock() time.Time { return time.Date(2026, 6, 21, 12, 0, 0, 0, time.UTC) }

// TestRestoreSessionFailSecureExits drives restoreTopologySession's post-journal failure exits
// directly through the injectable id-gen seam and asserts the fail-secure contract for
// each: a RestoreErrored is durably recorded AND the single-writer lease is released
// (a successor can re-acquire) AND (nil, *RestoreError{RestoreAppendFailed}) is
// returned — no Session ever comes up. The failure is forced at a restore-lifecycle
// id-mint (RestoreStarted, then RestoreDone) so it lands AFTER the journal exists,
// which is exactly where recordErrored is the single fail-secure exit.
func TestRestoreSessionFailSecureExits(t *testing.T) {
	tests := []struct {
		name string
		// failOnCall is the 1-based restore-lifecycle id-mint that fails. On a clean
		// (no open turn) stream the mints in order are: 1=RestoreStarted, 2=RestoreDone,
		// 3=RestoreErrored (in recordErrored). Failing on 1 routes to recordErrored whose
		// own mint is call 2 (succeeds); failing on 2 routes to recordErrored whose mint
		// is call 3 (succeeds) — so RestoreErrored is recorded in BOTH cases.
		failOnCall int
	}{
		{name: "RestoreStarted mint fails", failOnCall: 1},
		{name: "RestoreDone mint fails", failOnCall: 2},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			store := newRestoreStore(t)
			fp := fingerprintFromDefinition(restoreCfg(&stubLLM{}, "model-x", "be helpful"))

			// A clean original run (ends on TurnDone, no open turn): the restore mints
			// exactly RestoreStarted, RestoreDone, then RestoreErrored on the failure path.
			orig := buildOriginalRun(t, store, fp, restoreCfg(&stubLLM{chunks: []content.Chunk{textChunk("reply")}}, "model-x", "be helpful"), 1)
			handOver(t, orig.lease)

			seam := &failingNewID{failOnCall: tt.failOnCall}
			s, err := restoreTopologySession(
				context.Background(),
				singleDefinitionTopology(restoreCfg(&stubLLM{}, "model-x", "be helpful")),
				orig.sessionID, store,
				seam.next, fixedClock,
				WithFingerprintProvider(testFingerprintProvider),
			)

			// (a) No Session comes up.
			if s != nil {
				t.Fatalf("restoreTopologySession returned a non-nil Session on a forced failure")
			}
			// (b) A typed *RestoreError classifying the append/mint failure is returned.
			var re *RestoreError
			if !errors.As(err, &re) {
				t.Fatalf("restoreTopologySession err = %v, want *RestoreError", err)
			}
			if re.Kind != RestoreAppendFailed {
				t.Errorf("RestoreError.Kind = %q, want %q", re.Kind, RestoreAppendFailed)
			}
			// The injected mint failure chains through as the cause.
			if !errors.Is(err, errMintFailed) {
				t.Errorf("err does not chain the injected mint failure: %v", err)
			}

			// (c) A RestoreErrored is durably recorded (the failure is in the log, and no
			// RestoreDone followed it — the restore did not silently half-succeed).
			tail := restoreEventTail(t, store, orig.sessionID, orig.rootLoopID)
			if !lastIs(tail, event.RestoreErrored{}) {
				t.Errorf("restore-event tail does not end with RestoreErrored: %v", tailTypes(tail))
			}
			for _, ev := range tail {
				if _, ok := ev.(event.RestoreDone); ok {
					t.Errorf("a RestoreDone is present on a failed restore: %v", tailTypes(tail))
				}
			}

			// (d) The lease was released: a successor can re-acquire it through the store (the
			// failed restore must not leave the session's single-writer lease held).
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			successorLease, acqErr := store.AcquireLease(ctx, orig.sessionID)
			if acqErr != nil {
				t.Fatalf("successor Acquire after failed restore = %v, want success (lease should have been released)", acqErr)
			}
			t.Cleanup(func() {
				rctx, rcancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer rcancel()
				_ = successorLease.Release(rctx)
			})
		})
	}
}

// TestRestoreCrashSeamAppendFailSecure drives the restore-lifecycle append path to FAIL at
// the crash-seam TurnInterrupted append and (separately) at the final RestoreDone append,
// through the SAME injectable id-gen seam the other fail-secure exits use. On a stream
// ending mid-turn the restore-lifecycle mints are, in order: 1=RestoreStarted,
// 2=TurnInterrupted (crash seam), 3=RestoreDone. Failing the mint that stamps the
// TurnInterrupted (the crash seam) or the RestoreDone routes appendRestoreEvent's failure
// through the single fail-secure recordErrored exit. The documented contract must hold for
// BOTH: no controller comes up, no RestoreDone is persisted, a best-effort RestoreErrored
// is recorded, the derived session context is torn down (cleanup), and the single-writer
// lease is released so a successor can re-acquire. This closes the append-failure gap the
// clean-stream TestRestoreSessionFailSecureExits does not exercise: the crash seam, and a
// RestoreDone failure AFTER the live session was already built.
func TestRestoreCrashSeamAppendFailSecure(t *testing.T) {
	tests := []struct {
		name string
		// failOnCall is the 1-based restore-lifecycle mint that fails. On a CRASHED stream
		// (exactly one open turn) the mints are 1=RestoreStarted, 2=TurnInterrupted (crash
		// seam), 3=RestoreDone; recordErrored's own RestoreErrored mint is the next call and
		// succeeds, so the failure is always durably recorded.
		failOnCall int
	}{
		{name: "TurnInterrupted append fails at the crash seam", failOnCall: 2},
		{name: "RestoreDone append fails after the session is built", failOnCall: 3},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			store := newRestoreStore(t)
			fp := fingerprintFromDefinition(restoreCfg(&stubLLM{}, "model-x", "be helpful"))

			// A crashed original run: ends on an OPEN turn, so restore must close it with a
			// crash-seam TurnInterrupted before it can append RestoreDone.
			orig := buildCrashedRun(t, store, fp)
			handOver(t, orig.lease)

			seam := &failingNewID{failOnCall: tt.failOnCall}
			s, err := restoreTopologySession(
				context.Background(),
				singleDefinitionTopology(restoreCfg(&stubLLM{chunks: []content.Chunk{textChunk("recovered")}}, "model-x", "be helpful")),
				orig.sessionID, store,
				seam.next, fixedClock,
				WithFingerprintProvider(testFingerprintProvider),
			)

			// (a) No controller comes up.
			if s != nil {
				t.Fatalf("restoreTopologySession returned a non-nil Session on a forced append failure")
			}
			// (b) A typed *RestoreError classifying the append failure, chaining the mint cause.
			var re *RestoreError
			if !errors.As(err, &re) {
				t.Fatalf("restoreTopologySession err = %v, want *RestoreError", err)
			}
			if re.Kind != RestoreAppendFailed {
				t.Errorf("RestoreError.Kind = %q, want %q", re.Kind, RestoreAppendFailed)
			}
			if !errors.Is(err, errMintFailed) {
				t.Errorf("err does not chain the injected append failure: %v", err)
			}

			// (c) A best-effort RestoreErrored is recorded, and NO RestoreDone was persisted.
			tail := restoreEventTail(t, store, orig.sessionID, orig.rootLoopID)
			if !lastIs(tail, event.RestoreErrored{}) {
				t.Errorf("restore-event tail does not end with RestoreErrored: %v", tailTypes(tail))
			}
			for _, ev := range tail {
				if _, ok := ev.(event.RestoreDone); ok {
					t.Errorf("a RestoreDone was persisted on a failed restore: %v", tailTypes(tail))
				}
			}

			// (d) The lease was released (cleanup + fail-secure): a successor can re-acquire
			// it through the store without waiting out the TTL.
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			successorLease, acqErr := store.AcquireLease(ctx, orig.sessionID)
			if acqErr != nil {
				t.Fatalf("successor Acquire after failed restore = %v, want success (lease should have been released)", acqErr)
			}
			t.Cleanup(func() {
				rctx, rcancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer rcancel()
				_ = successorLease.Release(rctx)
			})
		})
	}
}

// --- Task 3.3: restore-time hard validation for an undeclared model transport ------

// transportMismatchBaseModel is transportMismatchDefinition's declared base model — the
// ONLY member of its ContextTransport set, so a durably-folded LoopInferenceChanged
// carrying any OTHER transport is provably undeclared.
func transportMismatchBaseModel() model.Model {
	return validModel("transport-mismatch-base")
}

// transportMismatchDefinition declares exactly ONE ContextTransport (its own base
// model's) with a real ContextCounter configured — the precondition
// (bound.ContextCounter() != nil) NewRestoredWithRuntime's hard transport check requires
// before it runs at all. It is used UNCHANGED for both the original run and restore (no
// config drift of its own), isolating these tests to the transport check alone.
func transportMismatchDefinition(client inference.Client) loop.Definition {
	base := transportMismatchBaseModel()
	capability := contextcount.InferenceCapability{Transport: contextcount.InferenceTransportLocal, Retention: contextcount.RetentionNone}
	counter := &liveCompactionCounter{
		capability: contextcount.CounterCapability{
			Transport: contextcount.CounterTransportLocal, Retention: contextcount.RetentionNone,
			TokenizerRev: "transport-mismatch-v1", Quality: contextcount.CountQualityExactLocal,
		},
		counts: []content.TokenCount{20},
	}
	return mustDefine(
		loop.WithName("agent"),
		loop.WithInference(client, base),
		loop.WithSystem("system"),
		loop.WithDrainTimeout(200*time.Millisecond),
		loop.WithContextCounter(counter),
		loop.WithInferenceCapability(capability),
		loop.WithContextTransports(loop.ContextTransport{Provider: base.Provider, APIFormat: base.APIFormat, BaseURL: base.BaseURL, Capability: capability}),
		loop.WithContextObservation(loop.ContextObservationPolicy{ReservedOutput: 10, CountTimeout: time.Second}),
	)
}

// undeclaredTransportRuntime is a durable ModelRuntime whose transport (Provider/
// APIFormat/BaseURL) is deliberately NEVER a member of transportMismatchDefinition's
// declared ContextTransport set.
func undeclaredTransportRuntime() event.ModelRuntime {
	return event.ModelRuntime{
		Key:       model.ModelKey{Provider: "undeclared-provider", Model: "undeclared-model"},
		APIFormat: model.APIFormatAnthropic,
		BaseURL:   "https://undeclared.example.test",
	}
}

// buildTransportMismatchStream persists a minimal original run whose root loop starts on
// transportMismatchDefinition's own declared (base) transport, then durably records a
// LoopInferenceChanged selecting undeclaredTransportRuntime()'s undeclared transport —
// exactly what a live cross-provider inference change would have journaled had it been
// permitted (or what a since-narrowed ContextTransport set leaves behind in the log).
// manifest is written on SessionStarted so a caller can opt a variant into the
// drift-assessed restore path; the zero value keeps a restore on the legacy fingerprint
// path.
func buildTransportMismatchStream(t *testing.T, store *sessionstore.Store, fp event.ConfigFingerprint, manifest event.ConfigManifest, base model.Model) persistedStream {
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
			AgentName:   "agent",
		},
		Runtime: event.ModelRuntime{Key: base.Key(), APIFormat: base.APIFormat, BaseURL: base.BaseURL},
	})
	// LoopInferenceChanged is stamped inline rather than through es.stamp/setHeader
	// (restore_roundtrip_test.go): setHeader only covers the original-run event set that
	// package's fixtures publish, which does not include this event.
	es.n++
	changed := event.LoopInferenceChanged{
		Header: event.Header{
			Coordinates: identity.Coordinates{SessionID: sessionID, LoopID: rootLoopID},
			EventID:     uuid.UUID{0xE0, es.n},
			CreatedAt:   time.Date(2026, 6, 21, 12, 0, 0, 0, time.UTC),
		},
		Runtime: undeclaredTransportRuntime(),
	}
	if err := h.PublishEvent(ctx, changed); err != nil {
		t.Fatalf("PublishEvent(LoopInferenceChanged): %v", err)
	}

	return persistedStream{sessionID: sessionID, rootLoopID: rootLoopID, lease: lease}
}

// TestRestoreTransportMismatchEndToEnd proves a whole-session restore fails the ENTIRE
// RestoreTopology call when its one loop's folded ModelRuntime carries a transport that is
// no longer a member of the current bound definition's declared ContextTransport set —
// composing through the SAME RestoreLoopFailed wrap every other NewRestoredWithRuntime
// failure uses (attachRestoredLoop, restore_constructor.go), and chaining all the way down
// to the typed *loopruntime.RestoreTransportMismatchError cause via errors.As.
//
// It also proves the failure is NOT suppressible: WithAllowConfigMismatch (which installs
// AcceptAllDecider AND relaxes the legacy fingerprint check) fails IDENTICALLY to the
// default fail-secure restore, because RestoreTransportMismatchError deliberately never
// routes through RestoreDecider/allowConfigMismatch — see the design doc's "New
// restore-time hard validation": a resolved model whose trust tier can no longer be
// determined has no coherent "resume anyway" answer.
func TestRestoreTransportMismatchEndToEnd(t *testing.T) {
	tests := []struct {
		name    string
		options []Option
	}{
		{name: "default fail-secure restore, no decider/shim configured"},
		{name: "WithAllowConfigMismatch installed (installs AcceptAllDecider too)", options: []Option{WithAllowConfigMismatch()}},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			store := newRestoreStore(t)
			definition := transportMismatchDefinition(&stubLLM{})
			base := transportMismatchBaseModel()
			fp := fingerprintFromDefinition(definition)

			orig := buildTransportMismatchStream(t, store, fp, event.ConfigManifest{}, base)
			handOver(t, orig.lease)

			s, err := restoreTestSession(context.Background(), definition, orig.sessionID, store, tt.options...)
			if s != nil {
				t.Fatalf("RestoreTopology returned a non-nil Session on an undeclared restored transport")
			}
			var restoreErr *RestoreError
			if !errors.As(err, &restoreErr) || restoreErr.Kind != RestoreLoopFailed {
				t.Fatalf("Restore err = %v, want *RestoreError{Kind: RestoreLoopFailed}", err)
			}
			var mismatch *loopruntime.RestoreTransportMismatchError
			if !errors.As(err, &mismatch) {
				t.Fatalf("Restore err = %v, want it to chain *loopruntime.RestoreTransportMismatchError", err)
			}
			want := undeclaredTransportRuntime()
			if mismatch.Provider != want.Key.Provider || mismatch.APIFormat != want.APIFormat || mismatch.BaseURL != want.BaseURL {
				t.Fatalf("mismatch = %+v, want Provider/APIFormat/BaseURL %q/%q/%q",
					mismatch, want.Key.Provider, want.APIFormat, want.BaseURL)
			}

			// Fail-secure: no RestoreDone, a RestoreErrored closes the tail — the same
			// contract every other restore failure honors.
			tail := restoreEventTail(t, store, orig.sessionID, orig.rootLoopID)
			if !lastIs(tail, event.RestoreErrored{}) {
				t.Errorf("restore-event tail does not end with RestoreErrored: %v", tailTypes(tail))
			}
			for _, ev := range tail {
				if _, ok := ev.(event.RestoreDone); ok {
					t.Errorf("a RestoreDone was persisted despite the undeclared transport: %v", tailTypes(tail))
				}
			}

			// The lease was released (fail-secure cleanup): a successor can re-acquire it.
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			successorLease, acqErr := store.AcquireLease(ctx, orig.sessionID)
			if acqErr != nil {
				t.Fatalf("successor Acquire after failed restore = %v, want success (lease should have been released)", acqErr)
			}
			t.Cleanup(func() {
				rctx, rcancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer rcancel()
				_ = successorLease.Release(rctx)
			})
		})
	}
}

// TestRestoreTransportMismatchNotSuppressibleByRestoreDecider is the new-path
// (drift-assessed restore, WithManifest) companion to
// TestRestoreTransportMismatchEndToEnd's WithAllowConfigMismatch case: even a caller-
// installed, unconditionally-accepting RestoreDecider (AcceptAllDecider) does not suppress
// the hard transport-mismatch failure, because the check runs inside
// NewRestoredWithRuntime — entirely outside the drift-assessment/decider seam the NEW
// restore path consults.
func TestRestoreTransportMismatchNotSuppressibleByRestoreDecider(t *testing.T) {
	t.Parallel()
	store := newRestoreStore(t)
	definition := transportMismatchDefinition(&stubLLM{})
	base := transportMismatchBaseModel()
	fp := fingerprintFromDefinition(definition)
	manifest := baselineManifest()

	orig := buildTransportMismatchStream(t, store, fp, manifest, base)
	handOver(t, orig.lease)

	s, err := restoreTestSession(context.Background(), definition, orig.sessionID, store,
		WithManifest(manifest), WithRestoreDecider(AcceptAllDecider{}))
	if s != nil {
		t.Fatalf("RestoreTopology returned a non-nil Session on an undeclared restored transport (decider AcceptAllDecider)")
	}
	var restoreErr *RestoreError
	if !errors.As(err, &restoreErr) || restoreErr.Kind != RestoreLoopFailed {
		t.Fatalf("Restore err = %v, want *RestoreError{Kind: RestoreLoopFailed}", err)
	}
	var mismatch *loopruntime.RestoreTransportMismatchError
	if !errors.As(err, &mismatch) {
		t.Fatalf("Restore err = %v, want it to chain *loopruntime.RestoreTransportMismatchError", err)
	}
}
