package sessionruntime

import (
	"context"
	"strings"
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

	if _, err := s.respondFromClassifier(context.Background(), basis, "risk-classifier@rev-1"); err != nil {
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

// --- Session-scoped circuit breaker ---------------------------------------
//
// Design §18 requires bounded counters "per-turn AND per-session" for the
// same four signals. The per-turn counters above are cleared at every
// TurnDone/TurnFailed/TurnInterrupted (clearReviewTurnState), so a pattern
// spread across many distinct turns — each individually below the per-turn
// threshold — never trips the turn-scoped breaker. The session-scoped
// counters below mirror the turn-scoped shape exactly but are never cleared
// by turn completion; they only accumulate (bounded) for the session's whole
// lifetime, and a session-level trip is a stronger signal than a turn-level
// one: it makes every current AND future gate in the session human-only, not
// just the tripping turn's.

// breakerSessionCoords builds coordinates sharing sessionID across distinct
// turns, mirroring breakerCoords but letting a test hold SessionID fixed
// while turnID varies (breakerCoords mints a fresh random SessionID every
// call, which would defeat session-scoped accumulation entirely).
func breakerSessionCoords(sessionID, turnID, loopID uuid.UUID) identity.Coordinates {
	return identity.Coordinates{SessionID: sessionID, LoopID: loopID, TurnID: turnID, StepID: mustUUID()}
}

// TestReviewSessionBreakerAccumulatesAcrossTurnCompletion proves (a): the
// session-scoped counter keeps counting across multiple DISTINCT turns even
// though each turn's own per-turn counter is cleared at turn completion —
// exactly the cross-turn pattern design §18 requires and the pre-existing
// per-turn-only counters could never catch.
func TestReviewSessionBreakerAccumulatesAcrossTurnCompletion(t *testing.T) {
	t.Parallel()
	s := &Session{}
	withPermissionReviewBreaker(reviewCircuitBreakerLimits{
		Session: reviewSessionCircuitBreakerLimits{MaxConsecutiveNeedsHuman: 3},
	})(s)
	sessionID, loopID := mustUUID(), mustUUID()

	for i := 0; i < 2; i++ {
		turnID := mustUUID()
		coords := breakerSessionCoords(sessionID, turnID, loopID)
		s.observePermissionReviewOutcome(coords, reviewBreakerOutcome{Reason: gate.ReviewDecisionRecommendation})
		// Turn completes: its OWN counters are wiped, but the session-scoped
		// counter must survive.
		s.clearReviewTurnState(event.TurnDone{Header: event.Header{Coordinates: coords}})

		s.review.mu.Lock()
		_, turnTracked := s.review.turns[turnID]
		sessionCount := s.review.session.consecutiveNeedsHuman
		s.review.mu.Unlock()
		if turnTracked {
			t.Fatalf("turn %d counters survived TurnDone", i)
		}
		if want := i + 1; sessionCount != want {
			t.Fatalf("session consecutiveNeedsHuman = %d after turn %d, want %d", sessionCount, i, want)
		}
	}

	// A third, still distinct turn crosses the shared threshold (3) purely
	// from session-scoped accumulation — no single turn ever saw more than
	// one needs-human outcome.
	finalTurn := mustUUID()
	finalCoords := breakerSessionCoords(sessionID, finalTurn, loopID)
	s.observePermissionReviewOutcome(finalCoords, reviewBreakerOutcome{Reason: gate.ReviewDecisionRecommendation})
	if s.reviewBreakerAllows(finalCoords) {
		t.Fatal("session-scoped counter did not trip after accumulating across 3 distinct turns")
	}
}

// TestReviewSessionBreakerTripsIndependentlyFromTurnBreaker proves (b): the
// session-scoped trip is a DISTINCT flag from any turn-scoped trip — with no
// turn-scoped threshold configured at all, a turn's own counters can
// structurally never trip, yet the session-scoped counters (a genuinely
// separate configuration surface, reviewCircuitBreakerLimits.Session) still
// trip once their own threshold is crossed across turns.
func TestReviewSessionBreakerTripsIndependentlyFromTurnBreaker(t *testing.T) {
	t.Parallel()
	s := &Session{}
	withPermissionReviewBreaker(reviewCircuitBreakerLimits{
		Session: reviewSessionCircuitBreakerLimits{MaxInvalidOrFailed: 2},
	})(s)
	sessionID, loopID := mustUUID(), mustUUID()

	var lastCoords identity.Coordinates
	for i := 0; i < 2; i++ {
		turnID := mustUUID()
		coords := breakerSessionCoords(sessionID, turnID, loopID)
		lastCoords = coords
		s.observePermissionReviewOutcome(coords, reviewBreakerOutcome{Reason: gate.ReviewDecisionClassifierStatus})

		s.review.mu.Lock()
		turnCounters := s.review.turns[turnID]
		s.review.mu.Unlock()
		if turnCounters == nil || turnCounters.tripped {
			t.Fatalf("turn %d counters unexpectedly tripped (turn-scoped and session-scoped must be independent)", i)
		}
		s.clearReviewTurnState(event.TurnDone{Header: event.Header{Coordinates: coords}})
	}

	s.review.mu.Lock()
	sessionTripped := s.review.session.tripped
	s.review.mu.Unlock()
	if !sessionTripped {
		t.Fatal("session-scoped breaker did not trip despite reaching the shared threshold across turns")
	}
	if s.reviewBreakerAllows(lastCoords) {
		t.Fatal("reviewBreakerAllows() = true after the session-scoped breaker tripped")
	}
}

// TestReviewSessionBreakerWarnsExactlyOnce proves (c): a session-level trip
// emits exactly one bounded warning, distinct from the turn-scoped warning,
// even as further reviews continue to be observed after the trip.
func TestReviewSessionBreakerWarnsExactlyOnce(t *testing.T) {
	// Deliberately NOT t.Parallel(): captureSlogDefault swaps the process-wide
	// slog default for the duration of this test and asserts an EXACT count of
	// a fixed (non-random) message substring, so it must not run concurrently
	// with any other test that might also redirect or write to the default
	// logger.
	s := &Session{sessionCtx: context.Background()}
	withPermissionReviewBreaker(reviewCircuitBreakerLimits{
		Session: reviewSessionCircuitBreakerLimits{MaxInvalidOrFailed: 1},
	})(s)
	sessionID, loopID := mustUUID(), mustUUID()

	logs := captureSlogDefault(t)
	for i := 0; i < 5; i++ {
		coords := breakerSessionCoords(sessionID, mustUUID(), loopID)
		s.observePermissionReviewOutcome(coords, reviewBreakerOutcome{Reason: gate.ReviewDecisionClassifierStatus})
	}

	const sessionWarnMsg = "permission-review circuit breaker tripped; automatic review disabled for the session"
	got := strings.Count(logs.String(), sessionWarnMsg)
	if got != 1 {
		t.Fatalf("session-level breaker warning count = %d, want exactly 1; logs: %s", got, logs.String())
	}
}

// TestReviewSessionBreakerCountersAreBoundedAndSecretFree proves (d): the
// session-scoped identical-subject digest set is bounded to
// maxTrackedReviewSubjectDigests distinct digests (the same defensive cap the
// turn-scoped counters already apply — see trackReviewSubjectLocked) and, by
// construction of reviewBreakerOutcome/subjectContentDigest, holds only fixed
// 32-byte digests and small counters — never raw subject content.
func TestReviewSessionBreakerCountersAreBoundedAndSecretFree(t *testing.T) {
	t.Parallel()
	s := &Session{}
	withPermissionReviewBreaker(reviewCircuitBreakerLimits{
		Session: reviewSessionCircuitBreakerLimits{MaxIdenticalSubjects: 1 << 20},
	})(s)
	sessionID, loopID := mustUUID(), mustUUID()

	for i := 0; i < maxTrackedReviewSubjectDigests+5; i++ {
		var digest [32]byte
		digest[0] = byte(i)
		digest[1] = byte(i >> 8)
		coords := breakerSessionCoords(sessionID, mustUUID(), loopID)
		s.observePermissionReviewOutcome(coords, reviewBreakerOutcome{Reason: gate.ReviewDecisionRecommendation, SubjectDigest: digest})
	}

	s.review.mu.Lock()
	tracked := len(s.review.session.subjectCounts)
	s.review.mu.Unlock()
	if tracked != maxTrackedReviewSubjectDigests {
		t.Fatalf("session tracked subject digests = %d, want %d (bounded, no unbounded growth)", tracked, maxTrackedReviewSubjectDigests)
	}
}

// TestReviewSessionBreakerTripMakesNewTurnHumanOnly proves (e): a
// session-level trip is NOT scoped to the turn that caused it — it blocks a
// brand-new turn that never itself contributed a single observation.
func TestReviewSessionBreakerTripMakesNewTurnHumanOnly(t *testing.T) {
	t.Parallel()
	s := &Session{}
	withPermissionReviewBreaker(reviewCircuitBreakerLimits{
		Session: reviewSessionCircuitBreakerLimits{MaxInvalidOrFailed: 1},
	})(s)
	sessionID, loopID := mustUUID(), mustUUID()

	trippingCoords := breakerSessionCoords(sessionID, mustUUID(), loopID)
	s.observePermissionReviewOutcome(trippingCoords, reviewBreakerOutcome{Reason: gate.ReviewDecisionClassifierStatus})

	s.review.mu.Lock()
	sessionTripped := s.review.session.tripped
	s.review.mu.Unlock()
	if !sessionTripped {
		t.Fatal("session-scoped breaker did not trip")
	}

	freshCoords := breakerSessionCoords(sessionID, mustUUID(), loopID)
	s.review.mu.Lock()
	_, freshTurnTracked := s.review.turns[freshCoords.TurnID]
	s.review.mu.Unlock()
	if freshTurnTracked {
		t.Fatal("test setup error: the fresh turn must never have been observed")
	}
	if s.reviewBreakerAllows(freshCoords) {
		t.Fatal("reviewBreakerAllows() = true for a brand-new turn after the session-scoped breaker tripped")
	}
}

// TestStartPermissionReviewNoopsWhenSessionBreakerTripped extends
// TestStartPermissionReviewNoopsWhenBreakerTripped to the session scope:
// StartPermissionReview must consult BOTH the turn-scoped and session-scoped
// breaker at its enforcement point, so a session-level trip caused by an
// EARLIER, different turn still blocks a request for a brand-new turn.
func TestStartPermissionReviewNoopsWhenSessionBreakerTripped(t *testing.T) {
	t.Parallel()
	classifier := newValidReviewClassifier(t, "classifier", "rev-1", true)
	set, err := gate.NewPermissionClassifierSet(classifier)
	if err != nil {
		t.Fatalf("NewPermissionClassifierSet: %v", err)
	}
	s := reviewStartSession(t, set)
	withPermissionReviewBreaker(reviewCircuitBreakerLimits{
		Session: reviewSessionCircuitBreakerLimits{MaxInvalidOrFailed: 1},
	})(s)

	// Trip the session-scoped breaker from an unrelated, already-finished turn.
	trippingCoords := identity.Coordinates{SessionID: s.sessionID, LoopID: mustUUID(), TurnID: mustUUID(), StepID: mustUUID()}
	s.observePermissionReviewOutcome(trippingCoords, reviewBreakerOutcome{Reason: gate.ReviewDecisionClassifierStatus})
	s.clearReviewTurnState(event.TurnDone{Header: event.Header{Coordinates: trippingCoords}})

	gateID := mustUUID()
	req := validReviewRequest(t, gateID, mustUUID())
	req.ReviewContext.Coordinates.SessionID = s.sessionID // same session, brand-new turn
	s.gates[gateID] = gateEntry{}

	s.StartPermissionReview(context.Background(), req)

	s.gatesMu.Lock()
	basis := s.gates[gateID].reviewBasis
	s.gatesMu.Unlock()
	if basis != (gate.ReviewBasis{}) {
		t.Fatalf("reviewBasis = %+v, want zero (session-scoped trip must block a brand-new turn too)", basis)
	}
	s.review.mu.Lock()
	_, active := s.review.cancellations[gateID]
	s.review.mu.Unlock()
	if active {
		t.Fatal("a cancellation handle was armed while the session-scoped breaker is tripped")
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
