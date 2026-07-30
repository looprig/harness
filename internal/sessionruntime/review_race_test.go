package sessionruntime

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/looprig/core/content"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/command"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/gate"
	"github.com/looprig/harness/pkg/loop"
)

// This file exercises Task 15's central security property (design §14.4,
// §14.5, §25.4-5): human response and classifier-originated approval share
// ONE exactly-once gate claim, a classifier-originated response may only
// ever carry gate.ApprovalApprove, and every condition other than a live,
// still-matching, still-open eligible approval leaves the human gate exactly
// as it was.
//
// raceGateSession and racePermissionReviewBasis give every test below a real
// open permission gate and the trusted review-basis snapshot
// StartPermissionReview would have recorded for it, without needing a real
// Hustle/classifier pipeline: these tests are about the CLAIM and DRIFT
// mechanics respondFromClassifier owns, not about classifier inference
// itself (that is review_adapter_test.go's job).

const (
	raceContextRevision = "context-rev-1"
	raceSecurityCeiling = "ceiling-1"
)

// raceGateSession builds a gateSession (a real Session wired to a
// fakeGateAppender and a fake loop backend) with permission review policy
// configured, opens one real permission gate on it, and seeds that gate's
// trusted reviewBasis snapshot exactly as Session.StartPermissionReview
// would. It returns the session, the appender, the command channel the fake
// loop receives on, the open gate's id, and its ToolExecutionID.
func raceGateSession(t *testing.T) (*Session, *fakeGateAppender, chan command.Command, gate.ID, uuid.UUID) {
	t.Helper()
	s, app, loopID, cmds := gateSession(t)
	s.permissionReviewPolicy = validReviewPolicy(t)
	callID := mustUUID()
	gateID := activateOn(t, s, loopID, callID, permissionGate(), bashPayload())
	basis := racePermissionReviewBasis(s, gateID, callID)
	seedReviewBasis(s, gateID, basis)
	return s, app, cmds, gateID, callID
}

// racePermissionReviewBasis builds the common ReviewBasis a permission
// review's winning, eligible classifier assessment would carry for gateID:
// the session's currently-configured review policy revision plus fixed
// context/security-ceiling values a test can deliberately diverge from to
// simulate drift.
func racePermissionReviewBasis(s *Session, gateID gate.ID, toolExecutionID uuid.UUID) gate.ReviewBasis {
	return gate.ReviewBasis{
		GateID:             gateID,
		ToolExecutionID:    toolExecutionID,
		ContextRevision:    raceContextRevision,
		GatePolicyRevision: s.permissionReviewPolicy.Revision,
		SecurityCeiling:    raceSecurityCeiling,
	}
}

// seedReviewBasis installs the trusted reviewBasis snapshot on gateID's
// directory entry, mirroring what Session.recordPermissionReviewBasis does
// when a real StartPermissionReview call begins.
func seedReviewBasis(s *Session, gateID gate.ID, basis gate.ReviewBasis) {
	s.gatesMu.Lock()
	defer s.gatesMu.Unlock()
	entry := s.gates[gateID]
	entry.reviewBasis = basis
	s.gates[gateID] = entry
}

// drainCommand reports the single command already queued on cmds, or that
// none is queued, without blocking — the race tests below need to assert
// "no second command was dispatched" rather than wait for one that will
// never arrive.
func drainCommand(cmds chan command.Command) (command.Command, bool) {
	select {
	case c := <-cmds:
		return c, true
	default:
		return nil, false
	}
}

// TestRespondFromClassifierApprovesAndClosesGate proves the base case: an
// eligible classifier-originated response claims the gate exactly like a
// human approval, dispatches ApproveToolCall (and only ApproveToolCall —
// respondFromClassifier has no action parameter, so nothing else is even
// constructible on this path), stamps ResponseFromClassifier provenance on
// the durable GateResolved, and closes the gate.
func TestRespondFromClassifierApprovesAndClosesGate(t *testing.T) {
	t.Parallel()
	s, app, cmds, gateID, callID := raceGateSession(t)
	basis := racePermissionReviewBasis(s, gateID, callID)

	if _, err := s.respondFromClassifier(context.Background(), basis, nil, "risk-classifier@rev-1"); err != nil {
		t.Fatalf("respondFromClassifier() error = %v", err)
	}

	cmd := recvCommand(t, cmds)
	approve, ok := cmd.(command.ApproveToolCall)
	if !ok {
		t.Fatalf("dispatched command = %T, want ApproveToolCall", cmd)
	}
	if approve.Action != gate.ApprovalApprove {
		t.Fatalf("approve action = %q, want %q (classifier path may only ever emit Approve)", approve.Action, gate.ApprovalApprove)
	}
	if got := s.ListGates(context.Background()); len(got) != 0 {
		t.Fatalf("ListGates() = %+v, want 0 (gate resolved)", got)
	}
	resolved := app.snapshotResolved()
	if len(resolved) != 1 {
		t.Fatalf("resolved events = %d, want exactly 1", len(resolved))
	}
	if resolved[0].Source.Kind != gate.ResponseFromClassifier || resolved[0].Source.Reason != "risk-classifier@rev-1" {
		t.Fatalf("resolved source = %+v, want classifier provenance with the reason", resolved[0].Source)
	}
}

// TestRespondFromClassifierAfterHumanApprovalIsStale: human response first
// -> GateResolved commits -> a subsequent classifier response is stale.
func TestRespondFromClassifierAfterHumanApprovalIsStale(t *testing.T) {
	t.Parallel()
	s, app, cmds, gateID, callID := raceGateSession(t)
	basis := racePermissionReviewBasis(s, gateID, callID)

	if err := s.RespondGate(context.Background(), userApprove(gateID, gate.ApprovalApprove)); err != nil {
		t.Fatalf("RespondGate() error = %v", err)
	}
	recvCommand(t, cmds)

	if _, err := s.respondFromClassifier(context.Background(), basis, nil, "risk-classifier@rev-1"); err != nil {
		t.Fatalf("respondFromClassifier() after human approval error = %v, want nil (stale, not a fault)", err)
	}
	if c, ok := drainCommand(cmds); ok {
		t.Errorf("stale classifier response dispatched a second command %T, want none", c)
	}
	if got := app.snapshotResolved(); len(got) != 1 {
		t.Fatalf("resolved events = %d, want exactly 1 (human won)", len(got))
	}
}

// TestRespondGateAfterClassifierApprovalIsStale: classifier approval first
// -> GateResolved commits with classifier source -> ApproveToolCall routes
// -> a subsequent human response is stale.
func TestRespondGateAfterClassifierApprovalIsStale(t *testing.T) {
	t.Parallel()
	s, app, cmds, gateID, callID := raceGateSession(t)
	basis := racePermissionReviewBasis(s, gateID, callID)

	if _, err := s.respondFromClassifier(context.Background(), basis, nil, "risk-classifier@rev-1"); err != nil {
		t.Fatalf("respondFromClassifier() error = %v", err)
	}
	recvCommand(t, cmds)

	err := s.RespondGate(context.Background(), userDeny(gateID))
	if err == nil {
		t.Fatal("RespondGate() error = nil, want stale (gate already resolved by the classifier)")
	}
	var ge *GateError
	if !errors.As(err, &ge) || ge.Kind != GateNotFound {
		t.Fatalf("RespondGate() error = %v, want *GateError{GateNotFound}", err)
	}
	if c, ok := drainCommand(cmds); ok {
		t.Errorf("stale human response dispatched a second command %T, want none", c)
	}
	resolved := app.snapshotResolved()
	if len(resolved) != 1 || resolved[0].Source.Kind != gate.ResponseFromClassifier {
		t.Fatalf("resolved events = %+v, want exactly 1, classifier-sourced", resolved)
	}
}

// TestRespondFromClassifierAndHumanSimultaneousRaceExactlyOneWins fires a
// real human RespondGate(Deny) and a real classifier respondFromClassifier
// (Approve) concurrently, from separate goroutines with no ordering imposed
// by the test, and requires -race to prove there is no unsynchronized access
// under the exactly-once claim. Exactly one of them may win: exactly one
// GateResolved commits, and at most one command is ever dispatched to the
// loop, and the routed command's source must agree with whichever of the two
// GateResolved actions actually happened.
func TestRespondFromClassifierAndHumanSimultaneousRaceExactlyOneWins(t *testing.T) {
	t.Parallel()
	for i := 0; i < 20; i++ {
		t.Run(fmt.Sprintf("iteration-%d", i), func(t *testing.T) {
			t.Parallel()
			s, app, cmds, gateID, callID := raceGateSession(t)
			basis := racePermissionReviewBasis(s, gateID, callID)

			var wg sync.WaitGroup
			wg.Add(2)
			go func() {
				defer wg.Done()
				_ = s.RespondGate(context.Background(), userDeny(gateID))
			}()
			go func() {
				defer wg.Done()
				_, _ = s.respondFromClassifier(context.Background(), basis, nil, "risk-classifier@rev-1")
			}()
			wg.Wait()

			resolved := app.snapshotResolved()
			if len(resolved) != 1 {
				t.Fatalf("resolved events = %d, want exactly 1", len(resolved))
			}

			var commands []command.Command
			for {
				c, ok := drainCommand(cmds)
				if !ok {
					break
				}
				commands = append(commands, c)
			}
			if len(commands) != 1 {
				t.Fatalf("dispatched commands = %d, want exactly 1 (at most one routed approve/deny)", len(commands))
			}
			switch cmd := commands[0].(type) {
			case command.ApproveToolCall:
				if cmd.Action != gate.ApprovalApprove {
					t.Fatalf("approve action = %q, want %q", cmd.Action, gate.ApprovalApprove)
				}
				if resolved[0].Source.Kind != gate.ResponseFromClassifier {
					t.Fatalf("resolved source = %q, want classifier when ApproveToolCall routed", resolved[0].Source.Kind)
				}
			case command.DenyToolCall:
				if resolved[0].Source.Kind != gate.ResponseFromUser {
					t.Fatalf("resolved source = %q, want user when DenyToolCall routed", resolved[0].Source.Kind)
				}
			default:
				t.Fatalf("dispatched command = %T, want ApproveToolCall or DenyToolCall", cmd)
			}
		})
	}
}

// TestRespondFromClassifierDuplicateIsStale proves a second classifier
// response for an already-resolved gate is dropped exactly like a second
// human response would be — no second claim, no second command.
func TestRespondFromClassifierDuplicateIsStale(t *testing.T) {
	t.Parallel()
	s, app, cmds, gateID, callID := raceGateSession(t)
	basis := racePermissionReviewBasis(s, gateID, callID)

	if _, err := s.respondFromClassifier(context.Background(), basis, nil, "risk-classifier@rev-1"); err != nil {
		t.Fatalf("respondFromClassifier() #1 error = %v", err)
	}
	recvCommand(t, cmds)

	if _, err := s.respondFromClassifier(context.Background(), basis, nil, "risk-classifier@rev-1"); err != nil {
		t.Fatalf("respondFromClassifier() #2 error = %v, want nil (stale duplicate, not a fault)", err)
	}
	if c, ok := drainCommand(cmds); ok {
		t.Errorf("duplicate classifier response dispatched a second command %T, want none", c)
	}
	if got := app.snapshotResolved(); len(got) != 1 {
		t.Fatalf("resolved events = %d, want exactly 1", len(got))
	}
}

// TestRespondFromClassifierAfterGateCloseIsStale proves an owner-side
// CloseGate (abandon/withdraw) beats a subsequent classifier response: the
// gate is gone, so the response is dropped as stale rather than reopening or
// erroring.
func TestRespondFromClassifierAfterGateCloseIsStale(t *testing.T) {
	t.Parallel()
	s, app, cmds, gateID, callID := raceGateSession(t)
	basis := racePermissionReviewBasis(s, gateID, callID)

	if err := s.CloseGate(context.Background(), gateID, gate.CloseAbandoned); err != nil {
		t.Fatalf("CloseGate() error = %v", err)
	}

	if _, err := s.respondFromClassifier(context.Background(), basis, nil, "risk-classifier@rev-1"); err != nil {
		t.Fatalf("respondFromClassifier() after CloseGate error = %v, want nil (stale, not a fault)", err)
	}
	if c, ok := drainCommand(cmds); ok {
		t.Errorf("classifier response after owner close dispatched a command %T, want none", c)
	}
	if got := app.snapshotResolved(); len(got) != 1 {
		t.Fatalf("resolved events = %d, want exactly 1 (from CloseGate)", len(got))
	}
}

// TestRespondFromClassifierAfterPolicyTimeoutIsStale proves a stale classifier
// result racing a gate that already auto-resolved via response-policy timeout
// is dropped exactly like any other late arrival — the timeout's own
// resolution stands.
func TestRespondFromClassifierAfterPolicyTimeoutIsStale(t *testing.T) {
	t.Parallel()
	s, app, cmds, gateID, callID := raceGateSession(t)
	basis := racePermissionReviewBasis(s, gateID, callID)

	timeoutResponse := gate.GateResponse{
		GateID: gateID,
		Action: string(gate.ApprovalDeny),
		Source: gate.ResponseSource{Kind: gate.ResponseFromPolicy, Reason: "timeout"},
	}
	if err := s.RespondGate(context.Background(), timeoutResponse); err != nil {
		t.Fatalf("RespondGate(timeout) error = %v", err)
	}
	recvCommand(t, cmds)

	if _, err := s.respondFromClassifier(context.Background(), basis, nil, "risk-classifier@rev-1"); err != nil {
		t.Fatalf("respondFromClassifier() after policy timeout error = %v, want nil (stale, not a fault)", err)
	}
	if c, ok := drainCommand(cmds); ok {
		t.Errorf("classifier response after policy timeout dispatched a second command %T, want none", c)
	}
	resolved := app.snapshotResolved()
	if len(resolved) != 1 || resolved[0].Source.Kind != gate.ResponseFromPolicy {
		t.Fatalf("resolved events = %+v, want exactly 1, from the policy timeout", resolved)
	}
}

// TestRespondFromClassifierDriftDropsSilently proves design §14.3 step 7 and
// §25.4: respondFromClassifier recomputes the entire basis immediately
// before claiming, and ANY divergence between what a classifier's eligible
// assessment was computed against and the session's current trusted
// snapshot — a different tool execution, a context revision, gate-policy
// revision, or security ceiling that no longer matches, or several at once
// ("observation drift") — makes the response stale. Every case must leave
// the gate open and untouched, not return an error.
func TestRespondFromClassifierDriftDropsSilently(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		drift func(gate.ReviewBasis) gate.ReviewBasis
	}{
		{
			name: "basis drift (wrong tool execution)",
			drift: func(b gate.ReviewBasis) gate.ReviewBasis {
				b.ToolExecutionID = mustUUID()
				return b
			},
		},
		{
			name: "context drift",
			drift: func(b gate.ReviewBasis) gate.ReviewBasis {
				b.ContextRevision = "context-rev-2"
				return b
			},
		},
		{
			name: "policy drift",
			drift: func(b gate.ReviewBasis) gate.ReviewBasis {
				b.GatePolicyRevision = "review-policy-v2"
				return b
			},
		},
		{
			name: "security drift",
			drift: func(b gate.ReviewBasis) gate.ReviewBasis {
				b.SecurityCeiling = "ceiling-2"
				return b
			},
		},
		{
			// Renamed from "observation drift" (Addendum 4): this case only
			// exercises two of the four PRE-EXISTING basis-field comparisons
			// moving together — it predates and is unrelated to design
			// §13.4's real observation-token TOCTOU recheck, which
			// review_race_test.go's TestRespondFromClassifierObservation*
			// tests exercise separately.
			name: "context and security ceiling drift together",
			drift: func(b gate.ReviewBasis) gate.ReviewBasis {
				b.ContextRevision = "context-rev-2"
				b.SecurityCeiling = "ceiling-2"
				return b
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			s, app, cmds, gateID, callID := raceGateSession(t)
			basis := tt.drift(racePermissionReviewBasis(s, gateID, callID))

			if _, err := s.respondFromClassifier(context.Background(), basis, nil, "risk-classifier@rev-1"); err != nil {
				t.Fatalf("respondFromClassifier() error = %v, want nil (drift silently dropped)", err)
			}
			if c, ok := drainCommand(cmds); ok {
				t.Errorf("drifted classifier response dispatched a command %T, want none", c)
			}
			if got := app.snapshotResolved(); len(got) != 0 {
				t.Fatalf("resolved events = %d, want 0 (drift must never claim the gate)", len(got))
			}
			got := s.ListGates(context.Background())
			if len(got) != 1 || got[0].ID != gateID {
				t.Fatalf("ListGates() = %+v, want the gate still open (every non-allow path preserves the human gate)", got)
			}
		})
	}
}

// observationVerifierStub is a gate.EvidenceObservationVerifier fake: it
// records every (policy, requirements) call it receives and returns a
// configured error (nil for a match, non-nil for a mismatch/unverifiable
// target).
type observationVerifierStub struct {
	mu           sync.Mutex
	err          error
	calls        int
	lastPolicy   gate.EvidenceContainmentPolicy
	lastRequired []gate.ObservationRequirement
}

func (s *observationVerifierStub) VerifyEvidenceObservations(
	_ context.Context,
	policy gate.EvidenceContainmentPolicy,
	requirements []gate.ObservationRequirement,
) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	s.lastPolicy = policy
	s.lastRequired = append([]gate.ObservationRequirement(nil), requirements...)
	return s.err
}

func (s *observationVerifierStub) snapshot() (int, gate.EvidenceContainmentPolicy, []gate.ObservationRequirement) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls, s.lastPolicy, append([]gate.ObservationRequirement(nil), s.lastRequired...)
}

var raceObservation = gate.ObservationRequirement{Target: "/workspace/file", Token: "token-1"}

// TestRespondFromClassifierObservationMatchApprovesAndClosesGate proves
// design §13.4's TOCTOU recheck lets a genuinely matching observation
// through exactly like the base no-observations case: the verifier is
// consulted (with the aggregated requirements and a policy built from the
// session's own wsRoot/current security ceiling) and, on a nil error, the
// gate claims and resolves precisely as TestRespondFromClassifierApprovesAndClosesGate
// does.
func TestRespondFromClassifierObservationMatchApprovesAndClosesGate(t *testing.T) {
	t.Parallel()
	s, app, cmds, gateID, callID := raceGateSession(t)
	s.wsRoot = "/workspace"
	s.permissionReviewSecurityCeiling = "ceiling-current"
	verifier := &observationVerifierStub{}
	withPermissionReviewObservationVerifier(verifier)(s)
	basis := racePermissionReviewBasis(s, gateID, callID)

	applied, err := s.respondFromClassifier(context.Background(), basis, []gate.ObservationRequirement{raceObservation}, "risk-classifier@rev-1")
	if err != nil {
		t.Fatalf("respondFromClassifier() error = %v", err)
	}
	if !applied {
		t.Fatal("respondFromClassifier() applied = false, want true (matching observation must not block approval)")
	}

	cmd := recvCommand(t, cmds)
	approve, ok := cmd.(command.ApproveToolCall)
	if !ok || approve.Action != gate.ApprovalApprove {
		t.Fatalf("dispatched command = %#v, want ApproveToolCall{Approve}", cmd)
	}
	resolved := app.snapshotResolved()
	if len(resolved) != 1 || resolved[0].Source.Kind != gate.ResponseFromClassifier {
		t.Fatalf("resolved events = %+v, want exactly 1, classifier-sourced", resolved)
	}

	calls, policy, required := verifier.snapshot()
	if calls != 1 {
		t.Fatalf("verifier calls = %d, want exactly 1", calls)
	}
	if policy.ReadRoot != "/workspace" || policy.SecurityCeiling != "ceiling-current" {
		t.Fatalf("verifier policy = %+v, want session's own wsRoot/current ceiling", policy)
	}
	if len(required) != 1 || required[0] != raceObservation {
		t.Fatalf("verifier requirements = %+v, want [%+v]", required, raceObservation)
	}
}

// TestRespondFromClassifierObservationMismatchDropsSilently proves design
// §13.4's central security property: a verifier-reported mismatch (or
// unverifiable target) on ANY recorded observation makes the whole
// classifier-originated response stale — dropped silently before
// respondGateCore is ever called, gate left exactly as it was, no session
// fault — mirroring every other drift outcome in
// TestRespondFromClassifierDriftDropsSilently.
func TestRespondFromClassifierObservationMismatchDropsSilently(t *testing.T) {
	t.Parallel()
	s, app, cmds, gateID, callID := raceGateSession(t)
	verifier := &observationVerifierStub{err: errors.New("target changed since observation")}
	withPermissionReviewObservationVerifier(verifier)(s)
	basis := racePermissionReviewBasis(s, gateID, callID)

	applied, err := s.respondFromClassifier(context.Background(), basis, []gate.ObservationRequirement{raceObservation}, "risk-classifier@rev-1")
	if err != nil {
		t.Fatalf("respondFromClassifier() error = %v, want nil (observation mismatch silently dropped)", err)
	}
	if applied {
		t.Fatal("respondFromClassifier() applied = true, want false (mismatch must never claim the gate)")
	}
	if c, ok := drainCommand(cmds); ok {
		t.Errorf("observation mismatch dispatched a command %T, want none", c)
	}
	if got := app.snapshotResolved(); len(got) != 0 {
		t.Fatalf("resolved events = %d, want 0 (observation mismatch must never claim the gate)", len(got))
	}
	got := s.ListGates(context.Background())
	if len(got) != 1 || got[0].ID != gateID {
		t.Fatalf("ListGates() = %+v, want the gate still open", got)
	}
	if calls, _, _ := verifier.snapshot(); calls != 1 {
		t.Fatalf("verifier calls = %d, want exactly 1", calls)
	}
}

// TestRespondFromClassifierObservationsWithoutVerifierIsStale proves the
// fail-secure asymmetry documented on verifyPermissionReviewObservations: a
// session with recorded observations but NO EvidenceObservationVerifier
// configured (the "consumer wired a target-sensitive evidence tool but
// forgot rig.WithPermissionReviewObservations" misconfiguration) must never
// silently skip the recheck and approve anyway — it is treated exactly like
// a genuine mismatch.
func TestRespondFromClassifierObservationsWithoutVerifierIsStale(t *testing.T) {
	t.Parallel()
	s, app, cmds, gateID, callID := raceGateSession(t)
	basis := racePermissionReviewBasis(s, gateID, callID)

	applied, err := s.respondFromClassifier(context.Background(), basis, []gate.ObservationRequirement{raceObservation}, "risk-classifier@rev-1")
	if err != nil {
		t.Fatalf("respondFromClassifier() error = %v, want nil (unverifiable observation silently dropped)", err)
	}
	if applied {
		t.Fatal("respondFromClassifier() applied = true, want false (no verifier configured must never silently approve)")
	}
	if c, ok := drainCommand(cmds); ok {
		t.Errorf("unverifiable classifier response dispatched a command %T, want none", c)
	}
	if got := app.snapshotResolved(); len(got) != 0 {
		t.Fatalf("resolved events = %d, want 0", len(got))
	}
	got := s.ListGates(context.Background())
	if len(got) != 1 || got[0].ID != gateID {
		t.Fatalf("ListGates() = %+v, want the gate still open", got)
	}
}

// TestRespondFromClassifierNoObservationsSkipsVerifierEvenWhenConfigured
// proves the OTHER half of the same asymmetry: zero recorded observations
// never calls a configured verifier at all — a session with a
// target-sensitive-evidence-capable verifier wired, reviewing a gate whose
// classifier(s) happened to record nothing this time, behaves exactly like
// TestRespondFromClassifierApprovesAndClosesGate.
func TestRespondFromClassifierNoObservationsSkipsVerifierEvenWhenConfigured(t *testing.T) {
	t.Parallel()
	s, app, cmds, gateID, callID := raceGateSession(t)
	verifier := &observationVerifierStub{err: errors.New("must never be consulted")}
	withPermissionReviewObservationVerifier(verifier)(s)
	basis := racePermissionReviewBasis(s, gateID, callID)

	applied, err := s.respondFromClassifier(context.Background(), basis, nil, "risk-classifier@rev-1")
	if err != nil {
		t.Fatalf("respondFromClassifier() error = %v", err)
	}
	if !applied {
		t.Fatal("respondFromClassifier() applied = false, want true (no observations means nothing to recheck)")
	}
	recvCommand(t, cmds)
	if got := app.snapshotResolved(); len(got) != 1 {
		t.Fatalf("resolved events = %d, want exactly 1", len(got))
	}
	if calls, _, _ := verifier.snapshot(); calls != 0 {
		t.Fatalf("verifier calls = %d, want 0 (must never be consulted with zero observations)", calls)
	}
}

// TestRespondFromClassifierObservationMismatchFeedsCircuitBreaker proves an
// observation mismatch is counted against design §18's stale-response
// circuit breaker exactly like basis drift is
// (TestRespondFromClassifierDriftFeedsCircuitBreaker in review_state_test.go)
// — a persistent misconfiguration or attack on this new recheck path is
// eventually visible, not silently absorbed forever.
func TestRespondFromClassifierObservationMismatchFeedsCircuitBreaker(t *testing.T) {
	t.Parallel()
	s, _, cmds, gateID, callID := raceGateSession(t)
	withPermissionReviewBreaker(reviewCircuitBreakerLimits{MaxStaleResponses: 1})(s)
	verifier := &observationVerifierStub{err: errors.New("target changed")}
	withPermissionReviewObservationVerifier(verifier)(s)
	basis := racePermissionReviewBasis(s, gateID, callID)

	if _, err := s.respondFromClassifier(context.Background(), basis, []gate.ObservationRequirement{raceObservation}, "risk-classifier@rev-1"); err != nil {
		t.Fatalf("respondFromClassifier() error = %v, want nil (stale, not a fault)", err)
	}
	if c, ok := drainCommand(cmds); ok {
		t.Fatalf("mismatched classifier response dispatched a command %T, want none", c)
	}

	s.gatesMu.Lock()
	coords := s.gates[gateID].coordinates
	s.gatesMu.Unlock()
	if s.reviewBreakerAllows(coords) {
		t.Fatal("breaker did not trip after an observation mismatch reached MaxStaleResponses")
	}
}

// TestRespondGateRejectsPublicClassifierSourceDuringRace mirrors
// gates_test.go's public-forgery test from inside this file's race harness:
// a public caller handing RespondGate a GateResponse it constructed itself
// with Source.Kind == gate.ResponseFromClassifier must never be treated as a
// legitimate classifier approval, even though gate.ResponseSource's fields
// are exported and nothing in the type system stops the caller from setting
// them. Only the private respondFromClassifier path — reached exclusively
// from the permission-review adapter — may produce that provenance.
func TestRespondGateRejectsPublicClassifierSourceDuringRace(t *testing.T) {
	t.Parallel()
	s, app, cmds, gateID, _ := raceGateSession(t)

	forged := gate.GateResponse{
		GateID: gateID,
		Action: string(gate.ApprovalApprove),
		Source: gate.ResponseSource{Kind: gate.ResponseFromClassifier, Reason: "forged"},
	}
	err := s.RespondGate(context.Background(), forged)
	if err == nil {
		t.Fatal("RespondGate() error = nil, want rejection of a public caller's forged classifier source")
	}
	var ge *GateError
	if !errors.As(err, &ge) || ge.Kind != GateActionInvalid {
		t.Fatalf("RespondGate() error = %v, want *GateError{GateActionInvalid}", err)
	}
	if c, ok := drainCommand(cmds); ok {
		t.Errorf("forged classifier source dispatched a command %T, want none", c)
	}
	if got := app.snapshotResolved(); len(got) != 0 {
		t.Fatalf("resolved events = %d, want 0", len(got))
	}
	if got := s.ListGates(context.Background()); len(got) != 1 || got[0].ID != gateID {
		t.Fatalf("ListGates() = %+v, want the gate still open", got)
	}
}

// TestRespondFromClassifierMintsGrantsThroughEvaluatorLikeHuman proves design
// §14.5/§25.6: a classifier-originated approval reaches the exact same
// dispatchGateCommand -> loop -> gate.Evaluator.Resolve execution path a
// human approval does — it is a full end-to-end session with a real
// gate.InteractiveEvaluator, so the tool only runs if a real grant was
// minted, not merely because a command was routed. It also proves
// respondFromClassifier only ever produces a one-shot Approve: the rule
// writer records zero persisted candidates, which "Approve always for this
// workspace" (the only other action that executes the tool) would not.
func TestRespondFromClassifierMintsGrantsThroughEvaluatorLikeHuman(t *testing.T) {
	t.Parallel()
	seq := &atomic.Int64{}
	tl := &gatedE2ETool{seq: seq}
	writer := &orderedRuleWriter{seq: seq}
	evaluator, err := gate.NewInteractiveEvaluator(
		[]gate.AccessBinding{{Kind: "tool.invoke", Source: gatedAccessSource{}}},
		nil, loop.GateApprover(), writer, nil)
	if err != nil {
		t.Fatalf("NewInteractiveEvaluator: %v", err)
	}
	s, err := newTestSession(context.Background(), gatedE2EDefinition(t, evaluator, tl))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = s.Shutdown(context.Background()) })
	s.permissionReviewPolicy = validReviewPolicy(t)

	sub, err := s.SubscribeEvents(event.EventFilter{Enduring: event.LoopScope{All: true}})
	if err != nil {
		t.Fatalf("SubscribeEvents: %v", err)
	}
	defer func() { _ = sub.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := s.Submit(ctx, []content.Block{&content.TextBlock{Text: "go"}}); err != nil {
		t.Fatalf("Submit: %v", err)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case d, ok := <-sub.Events():
				if !ok {
					return
				}
				switch d.Event.(type) {
				case event.TurnDone, event.TurnFailed, event.TurnInterrupted:
					return
				}
			case <-ctx.Done():
				return
			}
		}
	}()

	var gateID gate.ID
	var callID uuid.UUID
	deadline := time.After(20 * time.Second)
waitOpen:
	for {
		open := s.ListGates(context.Background())
		if len(open) == 1 {
			gateID = open[0].ID
			callID = open[0].Subject.ToolExecutionID
			break waitOpen
		}
		select {
		case <-deadline:
			t.Fatal("gate did not open within deadline")
		case <-time.After(5 * time.Millisecond):
		}
	}

	basis := racePermissionReviewBasis(s, gateID, callID)
	seedReviewBasis(s, gateID, basis)
	if _, err := s.respondFromClassifier(context.Background(), basis, nil, "risk-classifier@rev-1"); err != nil {
		t.Fatalf("respondFromClassifier() error = %v", err)
	}

	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("turn did not reach a terminal within deadline")
	}

	runs, _ := tl.snapshot()
	if runs != 1 {
		t.Fatalf("tool runs = %d, want 1 (classifier approval must reach the same Evaluator.Resolve execution path a human approval does)", runs)
	}
	if batches, _ := writer.snapshot(); len(batches) != 0 {
		t.Errorf("writer batches = %d, want 0 (a classifier response is Approve, never Approve-always, so nothing persists)", len(batches))
	}
}

// blockingObservationVerifier is a gate.EvidenceObservationVerifier whose
// VerifyEvidenceObservations never returns on its own: it blocks until its
// ctx is cancelled (by an outer deadline or an outer cancellation) and then
// reports that cancellation as its error, exactly like a real verifier whose
// re-stat/git-ref-resolve I/O hangs (a stuck NFS mount, an unresponsive git
// remote) would look from the caller's side.
type blockingObservationVerifier struct{}

func (blockingObservationVerifier) VerifyEvidenceObservations(
	ctx context.Context,
	_ gate.EvidenceContainmentPolicy,
	_ []gate.ObservationRequirement,
) error {
	<-ctx.Done()
	return ctx.Err()
}

// TestVerifyPermissionReviewObservationsBoundedByAuditTimeout proves the
// TOCTOU observation recheck (design §13.4) cannot block
// respondFromClassifier indefinitely. verifyPermissionReviewObservations
// calls a consumer-supplied EvidenceObservationVerifier that performs real
// I/O (re-stat, resolve a git ref); this codebase's CLAUDE.md requires every
// I/O call to run under a context with a timeout or deadline. Before the
// fix, the ctx handed to VerifyEvidenceObservations traces back through
// respondFromClassifier -> StartPermissionReview's
// beginPermissionReviewCancellation, which is a bare context.WithCancel(ctx)
// with no deadline anywhere in the chain — so a hanging verifier hangs this
// call forever. With the fix, the call is bounded by s.hustleLimits.AuditTimeout
// (the same positive-guaranteed budget the adapter's own audit-publish call
// already reuses, per review_adapter.go's publish), so a hanging verifier is
// treated as an ordinary stale/unverifiable observation: the gate stays
// open, no command is dispatched, and respondFromClassifier returns
// (false, nil) promptly instead of hanging.
func TestVerifyPermissionReviewObservationsBoundedByAuditTimeout(t *testing.T) {
	t.Parallel()
	s, _, cmds, gateID, callID := raceGateSession(t)
	s.hustleLimits.AuditTimeout = 20 * time.Millisecond
	withPermissionReviewObservationVerifier(blockingObservationVerifier{})(s)
	basis := racePermissionReviewBasis(s, gateID, callID)

	type result struct {
		applied bool
		err     error
	}
	done := make(chan result, 1)
	start := time.Now()
	go func() {
		applied, err := s.respondFromClassifier(context.Background(), basis, []gate.ObservationRequirement{raceObservation}, "risk-classifier@rev-1")
		done <- result{applied: applied, err: err}
	}()

	var res result
	select {
	case res = <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("respondFromClassifier() did not return within 2s; the observation recheck is unbounded")
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("respondFromClassifier() took %v, want bounded near s.hustleLimits.AuditTimeout (20ms)", elapsed)
	}
	if res.err != nil {
		t.Fatalf("respondFromClassifier() error = %v, want nil (a bounded-out recheck is stale, not a session fault)", res.err)
	}
	if res.applied {
		t.Fatal("respondFromClassifier() applied = true, want false (a timed-out recheck must never approve)")
	}
	if c, ok := drainCommand(cmds); ok {
		t.Errorf("timed-out observation recheck dispatched a command %T, want none", c)
	}
	got := s.ListGates(context.Background())
	if len(got) != 1 || got[0].ID != gateID {
		t.Fatalf("ListGates() = %+v, want the gate still open", got)
	}
}
