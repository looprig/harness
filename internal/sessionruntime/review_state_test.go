package sessionruntime

import (
	"context"
	"testing"
	"time"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/gate"
	"github.com/looprig/harness/pkg/hub"
	"github.com/looprig/harness/pkg/hustle"
	"github.com/looprig/harness/pkg/identity"
)

// review_state_test.go covers Task 16's lifecycle contract (design §15, §18):
// the six cancellation triggers reaching a per-gate review context, the
// per-turn circuit breaker's four bounded counters, its warn-once and
// per-turn-reset behavior, and the optional stricter interrupt action. It
// deliberately tests the session-level primitives directly (mirroring
// review_race_test.go's approach to respondFromClassifier) rather than
// standing up a real classifier/Hustle pipeline, which review_adapter_test.go
// already covers at its own layer.

// --- Cancellation group ------------------------------------------------

// TestCancelPermissionReviewOnGateResolveCancelsContext proves design §15's
// first trigger: an ordinary human RespondGate — which resolves the gate
// through the exact same respondGateCore path a classifier-originated or
// timeout-policy response also uses — cancels that gate's active review
// context.
func TestCancelPermissionReviewOnGateResolveCancelsContext(t *testing.T) {
	t.Parallel()
	s, _, loopID, _ := gateSession(t)
	gateID := activateOn(t, s, loopID, mustUUID(), permissionGate(), bashPayload())
	reviewCtx, done := s.beginPermissionReviewCancellation(context.Background(), gateID)
	defer done()

	if err := s.RespondGate(context.Background(), userDeny(gateID)); err != nil {
		t.Fatalf("RespondGate() error = %v", err)
	}

	select {
	case <-reviewCtx.Done():
	default:
		t.Fatal("gate resolve did not cancel the active review context")
	}
}

// TestCancelPermissionReviewOnGateCloseCancelsContext proves design §15's
// second trigger: the owner closing a gate (CloseGate) cancels its active
// review context.
func TestCancelPermissionReviewOnGateCloseCancelsContext(t *testing.T) {
	t.Parallel()
	s, _, loopID, _ := gateSession(t)
	gateID := activateOn(t, s, loopID, mustUUID(), permissionGate(), bashPayload())
	reviewCtx, done := s.beginPermissionReviewCancellation(context.Background(), gateID)
	defer done()

	if err := s.CloseGate(context.Background(), gateID, gate.CloseAbandoned); err != nil {
		t.Fatalf("CloseGate() error = %v", err)
	}

	select {
	case <-reviewCtx.Done():
	default:
		t.Fatal("CloseGate did not cancel the active review context")
	}
}

// TestCancelPermissionReviewOnPolicyTimeoutCancelsContext proves design
// §15's "the review deadline expires" trigger as it is actually reachable in
// this codebase: a gate's own ResponsePolicy timeout fires RespondGate
// (startGatePolicyTimerLocked), which resolves the gate through the same
// respondGateCore path as any other response and so cancels the review
// exactly like TestCancelPermissionReviewOnGateResolveCancelsContext.
func TestCancelPermissionReviewOnPolicyTimeoutCancelsContext(t *testing.T) {
	t.Parallel()
	s, _, loopID, cmds := gateSession(t)
	g := permissionGate()
	g.ResponsePolicy = gate.ResponsePolicy{
		Timeout:   10 * time.Millisecond,
		OnTimeout: gate.PolicyRespond,
		Response:  gate.ResponseTemplate{Action: string(gate.ApprovalDeny)},
	}
	gateID, err := s.PrepareGateOpen(context.Background(), loopID, g, bashPayload())
	if err != nil {
		t.Fatalf("PrepareGateOpen() error = %v", err)
	}
	if err := s.ActivateGate(context.Background(), gateID, gate.Route{GateID: gateID, LoopID: loopID}); err != nil {
		t.Fatalf("ActivateGate() error = %v", err)
	}
	reviewCtx, done := s.beginPermissionReviewCancellation(context.Background(), gateID)
	defer done()

	select {
	case <-reviewCtx.Done():
		// The policy timer fired RespondGate, which resolved the gate through
		// respondGateCore and cancelled the review context.
	case <-time.After(2 * time.Second):
		t.Fatal("policy timeout never fired: review context was never cancelled")
	}
	// Drain the dispatched timeout command so the buffered channel and its
	// backing loop handle do not leak past the test.
	select {
	case <-cmds:
	case <-time.After(2 * time.Second):
		t.Fatal("policy timeout response was never dispatched to the loop")
	}
}

// TestCancelPermissionReviewOnLoopInterruptCancelsContext proves design
// §15's third trigger: interrupting a loop (or its subtree) cancels every
// active review routed to that loop, before the interrupt fan-out is even
// acknowledged.
func TestCancelPermissionReviewOnLoopInterruptCancelsContext(t *testing.T) {
	t.Parallel()
	s, ids, cmds := fakeTreeSession(t, controllableRelease{release: make(chan struct{})}, treeLoop{name: "A"})
	loopID := ids["A"]
	gateID := gate.ID(mustUUID())
	s.gates = map[gate.ID]gateEntry{gateID: {route: gate.Route{LoopID: loopID}, state: gateOpen}}
	reviewCtx, done := s.beginPermissionReviewCancellation(context.Background(), gateID)
	defer done()

	interruptDone := make(chan error, 1)
	go func() {
		_, err := s.Interrupt(context.Background())
		interruptDone <- err
	}()
	ackInterrupt(t, cmds["A"], true)

	select {
	case <-reviewCtx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("loop interrupt did not cancel the active review context")
	}
	if err := <-interruptDone; err != nil {
		t.Fatalf("Interrupt() error = %v", err)
	}
}

// TestCancelPermissionReviewNoopForUnknownGate proves the primitive is a
// safe no-op for a gate id with no active review — the overwhelmingly common
// case, since most gates never have automatic review configured.
func TestCancelPermissionReviewNoopForUnknownGate(t *testing.T) {
	t.Parallel()
	s := &Session{}
	s.cancelPermissionReview(gate.ID(mustUUID()))
}

// TestBeginPermissionReviewCancellationCleanupRemovesEntry proves the owning
// goroutine's cleanup — and only that cleanup — removes the cancellation-map
// entry, and that cleanup also cancels the context (so a review that ends
// without any external trigger still releases its derived context).
func TestBeginPermissionReviewCancellationCleanupRemovesEntry(t *testing.T) {
	t.Parallel()
	s := &Session{}
	gateID := gate.ID(mustUUID())
	reviewCtx, done := s.beginPermissionReviewCancellation(context.Background(), gateID)

	s.review.mu.Lock()
	_, tracked := s.review.cancellations[gateID]
	s.review.mu.Unlock()
	if !tracked {
		t.Fatal("cancellation handle was not installed")
	}

	done()

	s.review.mu.Lock()
	_, stillTracked := s.review.cancellations[gateID]
	s.review.mu.Unlock()
	if stillTracked {
		t.Fatal("cleanup did not remove the cancellation-map entry")
	}
	select {
	case <-reviewCtx.Done():
	default:
		t.Fatal("cleanup did not cancel the review context")
	}
}

// --- Circuit breaker -----------------------------------------------------

func breakerCoords(turnID, loopID uuid.UUID) identity.Coordinates {
	return identity.Coordinates{SessionID: mustUUID(), LoopID: loopID, TurnID: turnID, StepID: mustUUID()}
}

// TestReviewBreakerAllowsWithoutConfiguredLimits proves the zero-limits
// default (no withPermissionReviewBreaker applied) never trips, matching
// every other permission-review Option's "zero configuration preserves
// current behavior" rule.
func TestReviewBreakerAllowsWithoutConfiguredLimits(t *testing.T) {
	t.Parallel()
	s := &Session{}
	coords := breakerCoords(mustUUID(), mustUUID())
	for i := 0; i < 100; i++ {
		s.observePermissionReviewOutcome(coords, reviewBreakerOutcome{Reason: gate.ReviewDecisionRecommendation})
	}
	if !s.reviewBreakerAllows(coords) {
		t.Fatal("reviewBreakerAllows() = false with no configured limits, want true")
	}
}

// TestReviewBreakerTripsOnConsecutiveNeedsHuman proves design §18's first
// counter: MaxConsecutiveNeedsHuman consecutive ReviewDecisionRecommendation
// (or other "ran cleanly but needs a human") outcomes trip the breaker for
// the turn.
func TestReviewBreakerTripsOnConsecutiveNeedsHuman(t *testing.T) {
	t.Parallel()
	s := &Session{}
	withPermissionReviewBreaker(reviewCircuitBreakerLimits{MaxConsecutiveNeedsHuman: 3})(s)
	coords := breakerCoords(mustUUID(), mustUUID())

	for i := 0; i < 2; i++ {
		s.observePermissionReviewOutcome(coords, reviewBreakerOutcome{Reason: gate.ReviewDecisionRiskCeiling})
		if !s.reviewBreakerAllows(coords) {
			t.Fatalf("breaker tripped after only %d needs-human outcomes, want 3", i+1)
		}
	}
	s.observePermissionReviewOutcome(coords, reviewBreakerOutcome{Reason: gate.ReviewDecisionAuthorization})
	if s.reviewBreakerAllows(coords) {
		t.Fatal("breaker did not trip after reaching MaxConsecutiveNeedsHuman")
	}
}

// TestReviewBreakerTripsOnInvalidOrFailed proves design §18's second
// counter: recent invalid/failed reviews (mechanical review failures, not
// policy-driven "ask a human") trip the breaker independently.
func TestReviewBreakerTripsOnInvalidOrFailed(t *testing.T) {
	t.Parallel()
	s := &Session{}
	withPermissionReviewBreaker(reviewCircuitBreakerLimits{MaxInvalidOrFailed: 2})(s)
	coords := breakerCoords(mustUUID(), mustUUID())

	s.observePermissionReviewOutcome(coords, reviewBreakerOutcome{Reason: gate.ReviewDecisionClassifierStatus})
	if !s.reviewBreakerAllows(coords) {
		t.Fatal("breaker tripped after only 1 invalid/failed outcome, want 2")
	}
	s.observePermissionReviewOutcome(coords, reviewBreakerOutcome{Reason: gate.ReviewDecisionInvalidAssessment})
	if s.reviewBreakerAllows(coords) {
		t.Fatal("breaker did not trip after reaching MaxInvalidOrFailed")
	}
}

// TestReviewBreakerTripsOnIdenticalSubjects proves design §18's third
// counter: repeated review of a materially identical prepared request (same
// content digest) trips the breaker even when interleaved with other
// distinct subjects.
func TestReviewBreakerTripsOnIdenticalSubjects(t *testing.T) {
	t.Parallel()
	s := &Session{}
	withPermissionReviewBreaker(reviewCircuitBreakerLimits{MaxIdenticalSubjects: 3})(s)
	coords := breakerCoords(mustUUID(), mustUUID())
	repeated := [32]byte{0xAA}
	distinct := [32]byte{0xBB}

	s.observePermissionReviewOutcome(coords, reviewBreakerOutcome{Reason: gate.ReviewDecisionRecommendation, SubjectDigest: repeated})
	s.observePermissionReviewOutcome(coords, reviewBreakerOutcome{Reason: gate.ReviewDecisionRecommendation, SubjectDigest: distinct})
	s.observePermissionReviewOutcome(coords, reviewBreakerOutcome{Reason: gate.ReviewDecisionRecommendation, SubjectDigest: repeated})
	if !s.reviewBreakerAllows(coords) {
		t.Fatal("breaker tripped after only 2 occurrences of the repeated subject, want 3")
	}
	s.observePermissionReviewOutcome(coords, reviewBreakerOutcome{Reason: gate.ReviewDecisionRecommendation, SubjectDigest: repeated})
	if s.reviewBreakerAllows(coords) {
		t.Fatal("breaker did not trip after the repeated subject reached MaxIdenticalSubjects")
	}
}

// TestReviewBreakerTripsOnStaleResponses proves design §18's fourth
// counter: recordPermissionReviewStale (respondFromClassifier's drift path)
// trips the breaker on its own.
func TestReviewBreakerTripsOnStaleResponses(t *testing.T) {
	t.Parallel()
	s := &Session{}
	withPermissionReviewBreaker(reviewCircuitBreakerLimits{MaxStaleResponses: 2})(s)
	coords := breakerCoords(mustUUID(), mustUUID())

	s.recordPermissionReviewStale(coords)
	if !s.reviewBreakerAllows(coords) {
		t.Fatal("breaker tripped after only 1 stale response, want 2")
	}
	s.recordPermissionReviewStale(coords)
	if s.reviewBreakerAllows(coords) {
		t.Fatal("breaker did not trip after reaching MaxStaleResponses")
	}
}

// TestRespondFromClassifierDriftFeedsCircuitBreaker proves the seam actually
// used in production: a stale classifier response detected inside
// respondFromClassifier's drift closure increments the SAME turn's stale
// counter recorded against the gate's trusted coordinates.
func TestRespondFromClassifierDriftFeedsCircuitBreaker(t *testing.T) {
	t.Parallel()
	s, _, cmds, gateID, callID := raceGateSession(t)
	withPermissionReviewBreaker(reviewCircuitBreakerLimits{MaxStaleResponses: 1})(s)
	basis := racePermissionReviewBasis(s, gateID, callID)
	basis.ToolExecutionID = mustUUID() // force drift/staleness

	if err := s.respondFromClassifier(context.Background(), basis, "risk-classifier@rev-1"); err != nil {
		t.Fatalf("respondFromClassifier() error = %v, want nil (stale, not a fault)", err)
	}
	if c, ok := drainCommand(cmds); ok {
		t.Fatalf("drifted classifier response dispatched a command %T, want none", c)
	}

	s.gatesMu.Lock()
	coords := s.gates[gateID].coordinates
	s.gatesMu.Unlock()
	if s.reviewBreakerAllows(coords) {
		t.Fatal("breaker did not trip from a stale classifier response reaching production drift detection")
	}
}

// TestReviewBreakerEligibleOutcomeResetsCounters proves a successful,
// eligible review resets the run-based counters rather than letting them
// accumulate indefinitely across intervening successes.
func TestReviewBreakerEligibleOutcomeResetsCounters(t *testing.T) {
	t.Parallel()
	s := &Session{}
	withPermissionReviewBreaker(reviewCircuitBreakerLimits{MaxConsecutiveNeedsHuman: 2})(s)
	coords := breakerCoords(mustUUID(), mustUUID())

	s.observePermissionReviewOutcome(coords, reviewBreakerOutcome{Reason: gate.ReviewDecisionRecommendation})
	s.observePermissionReviewOutcome(coords, reviewBreakerOutcome{Eligible: true, Reason: gate.ReviewDecisionEligible})
	s.observePermissionReviewOutcome(coords, reviewBreakerOutcome{Reason: gate.ReviewDecisionRecommendation})
	if !s.reviewBreakerAllows(coords) {
		t.Fatal("an eligible outcome between two needs-human outcomes should have reset the streak")
	}
}

// TestReviewBreakerIgnoresNoApplicableClassifier proves a review with no
// applicable classifier at all never counts toward any breaker threshold —
// it was never actually reviewed.
func TestReviewBreakerIgnoresNoApplicableClassifier(t *testing.T) {
	t.Parallel()
	s := &Session{}
	withPermissionReviewBreaker(reviewCircuitBreakerLimits{MaxInvalidOrFailed: 1})(s)
	coords := breakerCoords(mustUUID(), mustUUID())

	s.observePermissionReviewOutcome(coords, reviewBreakerOutcome{Reason: gate.ReviewDecisionNoApplicableClassifier})
	if !s.reviewBreakerAllows(coords) {
		t.Fatal("a no-applicable-classifier outcome must never contribute to the breaker")
	}
}

// TestReviewBreakerWarnsExactlyOnce proves design §18's "emits one bounded
// warning": repeatedly crossing (and re-crossing) the threshold for the same
// turn only warns once.
func TestReviewBreakerWarnsExactlyOnce(t *testing.T) {
	t.Parallel()
	s := &Session{sessionCtx: context.Background()}
	withPermissionReviewBreaker(reviewCircuitBreakerLimits{MaxInvalidOrFailed: 1})(s)
	coords := breakerCoords(mustUUID(), mustUUID())

	for i := 0; i < 5; i++ {
		s.observePermissionReviewOutcome(coords, reviewBreakerOutcome{Reason: gate.ReviewDecisionClassifierStatus})
	}

	s.review.mu.Lock()
	counters := s.review.turns[coords.TurnID]
	s.review.mu.Unlock()
	if counters == nil || !counters.tripped {
		t.Fatal("breaker never tripped")
	}
	if !counters.warned {
		t.Fatal("breaker tripped without ever warning")
	}
	// warned is set exactly once by construction (maybeTripReviewBreakerLocked
	// returns immediately once counters.tripped is already true), so the
	// bounded-warning contract is a structural property of five repeated
	// crossings each landing on the same already-tripped counters value.
}

// TestReviewBreakerClearsOnTurnCompletion proves design §18's "cleared when
// the turn completes": once the turn's TurnDone/TurnFailed/TurnInterrupted is
// observed, its breaker state — including a tripped/warned state — is gone,
// so the SAME turn id starting fresh (a defensive-programming edge case; in
// practice turn ids are not reused) is untracked, and a DIFFERENT gate's
// StartPermissionReview for that turn id is no longer blocked.
func TestReviewBreakerClearsOnTurnCompletion(t *testing.T) {
	t.Parallel()
	coords := breakerCoords(mustUUID(), mustUUID())
	tests := []struct {
		name string
		ev   event.Event
	}{
		{name: "TurnDone", ev: event.TurnDone{Header: event.Header{Coordinates: coords}}},
		{name: "TurnFailed", ev: event.TurnFailed{Header: event.Header{Coordinates: coords}}},
		{name: "TurnInterrupted", ev: event.TurnInterrupted{Header: event.Header{Coordinates: coords}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &Session{}
			withPermissionReviewBreaker(reviewCircuitBreakerLimits{MaxInvalidOrFailed: 1})(s)
			s.observePermissionReviewOutcome(coords, reviewBreakerOutcome{Reason: gate.ReviewDecisionClassifierStatus})
			if s.reviewBreakerAllows(coords) {
				t.Fatal("breaker did not trip")
			}

			s.clearReviewTurnState(tt.ev)

			if !s.reviewBreakerAllows(coords) {
				t.Fatal("breaker state survived turn completion")
			}
			s.review.mu.Lock()
			_, tracked := s.review.turns[coords.TurnID]
			s.review.mu.Unlock()
			if tracked {
				t.Fatal("turn counters were not removed on turn completion")
			}
		})
	}
}

// TestReviewBreakerClearOnlyRespondsToTerminalTurnEvents proves
// clearReviewTurnState ignores every event type other than
// TurnDone/TurnFailed/TurnInterrupted (mirroring recordLoopMechanicalState's
// own narrow switch) — an ordinary TurnStarted must not wipe a turn's
// in-progress breaker state.
func TestReviewBreakerClearOnlyRespondsToTerminalTurnEvents(t *testing.T) {
	t.Parallel()
	s := &Session{}
	withPermissionReviewBreaker(reviewCircuitBreakerLimits{MaxInvalidOrFailed: 1})(s)
	coords := breakerCoords(mustUUID(), mustUUID())
	s.observePermissionReviewOutcome(coords, reviewBreakerOutcome{Reason: gate.ReviewDecisionClassifierStatus})

	s.clearReviewTurnState(event.TurnStarted{Header: event.Header{Coordinates: coords}})

	if s.reviewBreakerAllows(coords) {
		t.Fatal("a non-terminal event cleared the breaker state")
	}
}

// --- StartPermissionReview integration -----------------------------------

// reviewStartSession builds a real Session with a bound, non-nil
// *hustleruntime.Controller — so StartPermissionReview's nil-controller
// no-op guard does not itself short-circuit the test — and classifier
// registered as the session's permission-review classifier set.
//
// The bound controller is deliberately built from a tool-less "probe"
// definition rather than classifier's own Hustle definition:
// gate.NewPermissionClassifierSet requires every registered classifier's
// definition to declare evidence tools, and wiring a real evidence-tool
// runtime (hustleruntime.RuntimeConfig.Evidence) into the session-wide
// controller is composition-root work outside this task's scope (see
// review_adapter_test.go's identical note on
// TestSessionStartPermissionReviewDoesNotWaitForScheduledClassifierRun).
// Every assertion in the tests using this helper only depends on
// StartPermissionReview's own SYNCHRONOUS behavior (the breaker check,
// recordPermissionReviewBasis, and beginPermissionReviewCancellation all run
// before the review goroutine is even dispatched), so a controller that
// cannot actually resolve classifier's Hustle name is sufficient: the
// dispatched goroutine's eventual RunAndFinalize failure is drained by an
// explicit cancellation, never awaited.
func reviewStartSession(t *testing.T, set gate.PermissionClassifierSet) *Session {
	t.Helper()
	sessionID := mustUUID()
	sessionCtx, sessionCancel := context.WithCancel(context.Background())
	t.Cleanup(sessionCancel)
	factory := event.NewFactory(uuid.New, time.Now)
	probe, err := hustle.Define(
		hustle.WithName("review-start-probe"),
		hustle.WithParticipation(hustle.ParticipationBlocking),
		hustle.WithTimeout(time.Second),
		hustle.WithLimits(hustle.Limits{InputBytes: 1024, OutputBytes: 1024}),
		hustle.WithNamedInference(&shutdownHustleClient{invoked: make(chan struct{}, 1)}, validModel("review-start-probe")),
		hustle.WithSystemPrompt("unused probe prompt", "prompt-v1"),
		hustle.WithPolicyRevision("policy-v1"),
	)
	if err != nil {
		t.Fatalf("hustle.Define: %v", err)
	}
	s := &Session{
		sessionID: sessionID, sessionCtx: sessionCtx, sessionCancel: sessionCancel,
		loops: map[uuid.UUID]*loopHandle{}, newID: uuid.New, now: time.Now, factory: factory,
		hustleDefinitions: []hustle.Definition{probe}, hustleLimits: testHustleLimits(),
		gates: map[gate.ID]gateEntry{},
	}
	s.hub = hub.New(sessionID, hub.WithFactory(factory))
	withPermissionReview(set, validReviewPolicy(t))(s)
	if err := s.bindSessionHustles(); err != nil {
		t.Fatalf("bindSessionHustles: %v", err)
	}
	return s
}

// TestStartPermissionReviewNoopsWhenBreakerTripped proves the circuit
// breaker's enforcement point: a tripped turn starts no review at all — no
// reviewBasis is ever stamped onto the gate — leaving current and future
// gates in that turn human-only (design §18 points 1 and 3).
func TestStartPermissionReviewNoopsWhenBreakerTripped(t *testing.T) {
	t.Parallel()
	classifier := newValidReviewClassifier(t, "classifier", "rev-1", true)
	set, err := gate.NewPermissionClassifierSet(classifier)
	if err != nil {
		t.Fatalf("NewPermissionClassifierSet: %v", err)
	}
	s := reviewStartSession(t, set)
	withPermissionReviewBreaker(reviewCircuitBreakerLimits{MaxInvalidOrFailed: 1})(s)

	gateID := mustUUID()
	req := validReviewRequest(t, gateID, mustUUID())
	s.gates[gateID] = gateEntry{}
	s.observePermissionReviewOutcome(req.ReviewContext.Coordinates, reviewBreakerOutcome{Reason: gate.ReviewDecisionClassifierStatus})

	s.StartPermissionReview(context.Background(), req)

	s.gatesMu.Lock()
	basis := s.gates[gateID].reviewBasis
	s.gatesMu.Unlock()
	if basis != (gate.ReviewBasis{}) {
		t.Fatalf("reviewBasis = %+v, want zero (no review started while the breaker is tripped)", basis)
	}
	s.review.mu.Lock()
	_, active := s.review.cancellations[gateID]
	s.review.mu.Unlock()
	if active {
		t.Fatal("a cancellation handle was armed while the breaker is tripped")
	}
}

// TestStartPermissionReviewProceedsWhenBreakerNotTripped is the converse
// regression guard: an untripped (or unconfigured) breaker must not itself
// block review from starting.
func TestStartPermissionReviewProceedsWhenBreakerNotTripped(t *testing.T) {
	t.Parallel()
	classifier := newValidReviewClassifier(t, "classifier", "rev-1", true)
	set, err := gate.NewPermissionClassifierSet(classifier)
	if err != nil {
		t.Fatalf("NewPermissionClassifierSet: %v", err)
	}
	s := reviewStartSession(t, set)

	gateID := mustUUID()
	req := validReviewRequest(t, gateID, mustUUID())
	s.gates[gateID] = gateEntry{}

	s.StartPermissionReview(context.Background(), req)

	s.gatesMu.Lock()
	basis := s.gates[gateID].reviewBasis
	s.gatesMu.Unlock()
	if basis == (gate.ReviewBasis{}) {
		t.Fatal("reviewBasis is zero, want StartPermissionReview to have recorded it")
	}
	s.review.mu.Lock()
	_, active := s.review.cancellations[gateID]
	s.review.mu.Unlock()
	if !active {
		t.Fatal("StartPermissionReview did not arm a cancellation handle")
	}
	// Drain the dispatched review goroutine so it does not leak past the test
	// (the classifier's client blocks on ctx.Done(); cancel via CloseGate's
	// equivalent path directly through the cancellation primitive).
	s.cancelPermissionReview(gateID)
}

// --- Optional stricter interrupt policy -----------------------------------

// TestReviewBreakerTripInterruptsLoopWhenConfigured proves design §18's
// optional stricter action: with InterruptOnTrip configured, a breaker trip
// additionally interrupts the tripping turn's loop.
func TestReviewBreakerTripInterruptsLoopWhenConfigured(t *testing.T) {
	t.Parallel()
	s, ids, cmds := fakeTreeSession(t, controllableRelease{release: make(chan struct{})}, treeLoop{name: "A"})
	loopID := ids["A"]
	withPermissionReviewBreaker(reviewCircuitBreakerLimits{MaxInvalidOrFailed: 1, InterruptOnTrip: true})(s)
	coords := identity.Coordinates{SessionID: s.sessionID, LoopID: loopID, TurnID: mustUUID(), StepID: mustUUID()}

	s.observePermissionReviewOutcome(coords, reviewBreakerOutcome{Reason: gate.ReviewDecisionClassifierStatus})

	ackInterrupt(t, cmds["A"], true)
}

// TestReviewBreakerTripDoesNotInterruptByDefault proves the default
// (InterruptOnTrip unset) never dispatches an interrupt — an open human gate
// already prevents rapid autonomous retry (design §18).
func TestReviewBreakerTripDoesNotInterruptByDefault(t *testing.T) {
	t.Parallel()
	s, ids, cmds := fakeTreeSession(t, controllableRelease{release: make(chan struct{})}, treeLoop{name: "A"})
	loopID := ids["A"]
	withPermissionReviewBreaker(reviewCircuitBreakerLimits{MaxInvalidOrFailed: 1})(s)
	coords := identity.Coordinates{SessionID: s.sessionID, LoopID: loopID, TurnID: mustUUID(), StepID: mustUUID()}

	s.observePermissionReviewOutcome(coords, reviewBreakerOutcome{Reason: gate.ReviewDecisionClassifierStatus})

	select {
	case cmd := <-cmds["A"]:
		t.Fatalf("unexpected command dispatched to loop A: %T", cmd)
	case <-time.After(100 * time.Millisecond):
	}
}
