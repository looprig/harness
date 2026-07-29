package sessionruntime

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/looprig/core/content"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/gate"
	"github.com/looprig/harness/pkg/hustle"
	"github.com/looprig/harness/pkg/identity"
)

// applyProbeClassifier is a minimal gate.PermissionClassifier that only
// proves Applies was reached — it always reports itself inapplicable
// (Applies returns false) so the adapter never schedules a Hustle run and
// never invokes the (blocking) reviewer client. That keeps this test free of
// any dependency on the evidence-runtime wiring Task 24 owns: this addendum
// only proves the CONFIGURATION (classifiers + policy) reaches a real
// session and StartPermissionReview actually dispatches into it, not that a
// classifier run completes end to end.
type applyProbeClassifier struct {
	name       hustle.Name
	revision   string
	definition hustle.Definition
	applied    chan struct{}
}

func (c *applyProbeClassifier) Name() hustle.Name             { return c.name }
func (c *applyProbeClassifier) Revision() string              { return c.revision }
func (c *applyProbeClassifier) Definition() hustle.Definition { return c.definition }

func (c *applyProbeClassifier) Applies(gate.PermissionReviewSubject) bool {
	select {
	case c.applied <- struct{}{}:
	default:
	}
	return false
}

func (c *applyProbeClassifier) MarshalInput(gate.PermissionReviewSubject) (json.RawMessage, error) {
	return json.RawMessage(`{}`), nil
}

func (c *applyProbeClassifier) ValidateResult(subject gate.PermissionReviewSubject, _ hustle.Result) (gate.PermissionAssessment, error) {
	return gate.PermissionAssessment{Basis: subject.Basis}, nil
}

func newApplyProbeClassifier(t *testing.T, name hustle.Name, revision string) *applyProbeClassifier {
	t.Helper()
	return &applyProbeClassifier{
		name:       name,
		revision:   revision,
		definition: newValidReviewClassifierDefinition(t, &reviewClassifierClient{}, name, revision),
		applied:    make(chan struct{}, 1),
	}
}

// TestWithLifecyclePermissionReviewThreadsToNewSessionAndRestoreSession proves
// WithLifecyclePermissionReview's classifier set and policy reach a real
// session's permissionClassifiers/permissionReviewPolicy fields through BOTH
// NewSession and RestoreSession, and that a configured session's
// StartPermissionReview actually dispatches into the classifier (Applies is
// reached) rather than taking the "no classifiers configured" no-op branch.
func TestWithLifecyclePermissionReviewThreadsToNewSessionAndRestoreSession(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		restore bool
	}{
		{name: "new session"},
		{name: "restored session", restore: true},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			store := newRestoreStore(t)
			definition := restoreCfg(&stubLLM{chunks: []content.Chunk{textChunk("reply")}}, "model-x", "be helpful")

			var sessionID uuid.UUID
			if tt.restore {
				sessionID = runAndShutdown(t, store, definition)
			}

			classifier := newApplyProbeClassifier(t, "probe-classifier", "probe-rev-1")
			set, err := gate.NewPermissionClassifierSet(classifier)
			if err != nil {
				t.Fatalf("NewPermissionClassifierSet: %v", err)
			}
			policy := validReviewPolicy(t)

			// Registers a real, EVIDENCE-FREE hustle (not the classifier's own
			// Definition, which requires an EvidenceToolPolicy per
			// gate.NewPermissionClassifierSet's descriptor checks) purely to
			// bind a real *hustleruntime.Controller onto the session, exactly
			// like TestLifecycleBindsHustlesBeforeReturning does. Populating
			// hustleruntime.RuntimeConfig.Evidence so a classifier's OWN
			// evidence-bearing Definition can bind is Task 24 work (out of
			// scope here — see the classifier hustle-controller gap this test
			// intentionally routes around). Applies() on classifier below
			// always returns false, so adapter.review never needs to schedule
			// a Hustle run against a controller that does not know the
			// classifier's own Name — only that StartPermissionReview
			// actually dispatches into the classifier is under test.
			lifecycle, err := newTestLifecycle(
				definition, store,
				WithLifecycleHustles([]hustle.Definition{testHustleDefinition(t, "unrelated-background-hustle")}, testHustleLimits()),
				WithLifecyclePermissionReview(set, policy),
			)
			if err != nil {
				t.Fatalf("NewTopologyLifecycle: %v", err)
			}

			var s *Session
			if tt.restore {
				s, err = lifecycle.RestoreSession(context.Background(), sessionID)
			} else {
				s, err = lifecycle.NewSession(context.Background(), "")
			}
			if err != nil {
				t.Fatalf("session construction: %v", err)
			}
			t.Cleanup(func() { _ = s.Shutdown(context.Background()) })

			got := s.permissionClassifiers.Classifiers()
			if len(got) != 1 || got[0].Name() != classifier.name {
				t.Fatalf("permissionClassifiers = %#v, want exactly [%q]", got, classifier.name)
			}
			if s.permissionReviewPolicy.Revision != policy.Revision {
				t.Fatalf("permissionReviewPolicy.Revision = %q, want %q", s.permissionReviewPolicy.Revision, policy.Revision)
			}
			if s.hustleController == nil {
				t.Fatal("hustleController not bound; StartPermissionReview would no-op regardless of classifier wiring")
			}

			ctx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)
			req := validReviewRequest(t, mustUUID(), mustUUID())
			s.StartPermissionReview(ctx, req)

			select {
			case <-classifier.applied:
			case <-time.After(2 * time.Second):
				t.Fatal("StartPermissionReview never reached the configured classifier's Applies method")
			}
		})
	}
}

// TestWithLifecyclePermissionReviewBreakerThreadsFullLimitsToNewSessionAndRestoreSession
// proves WithLifecyclePermissionReviewBreaker's complete limits value
// (every turn-scoped field, InterruptOnTrip, and every session-scoped field)
// reaches a real session's review.limits through BOTH NewSession and
// RestoreSession, converted to the private reviewCircuitBreakerLimits shape.
func TestWithLifecyclePermissionReviewBreakerThreadsFullLimitsToNewSessionAndRestoreSession(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		restore bool
	}{
		{name: "new session"},
		{name: "restored session", restore: true},
	}
	limits := PermissionReviewBreakerLimits{
		MaxConsecutiveNeedsHuman: 11,
		MaxInvalidOrFailed:       12,
		MaxIdenticalSubjects:     13,
		MaxStaleResponses:        14,
		InterruptOnTrip:          true,
		Session: PermissionReviewSessionBreakerLimits{
			MaxConsecutiveNeedsHuman: 21,
			MaxInvalidOrFailed:       22,
			MaxIdenticalSubjects:     23,
			MaxStaleResponses:        24,
		},
	}
	want := reviewCircuitBreakerLimits{
		MaxConsecutiveNeedsHuman: 11,
		MaxInvalidOrFailed:       12,
		MaxIdenticalSubjects:     13,
		MaxStaleResponses:        14,
		InterruptOnTrip:          true,
		Session: reviewSessionCircuitBreakerLimits{
			MaxConsecutiveNeedsHuman: 21,
			MaxInvalidOrFailed:       22,
			MaxIdenticalSubjects:     23,
			MaxStaleResponses:        24,
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			store := newRestoreStore(t)
			definition := restoreCfg(&stubLLM{chunks: []content.Chunk{textChunk("reply")}}, "model-x", "be helpful")

			var sessionID uuid.UUID
			if tt.restore {
				sessionID = runAndShutdown(t, store, definition)
			}

			lifecycle, err := newTestLifecycle(
				definition, store,
				WithLifecyclePermissionReviewBreaker(limits),
			)
			if err != nil {
				t.Fatalf("NewTopologyLifecycle: %v", err)
			}

			var s *Session
			if tt.restore {
				s, err = lifecycle.RestoreSession(context.Background(), sessionID)
			} else {
				s, err = lifecycle.NewSession(context.Background(), "")
			}
			if err != nil {
				t.Fatalf("session construction: %v", err)
			}
			t.Cleanup(func() { _ = s.Shutdown(context.Background()) })

			if s.review.limits != want {
				t.Fatalf("review.limits = %+v, want %+v", s.review.limits, want)
			}
		})
	}
}

// TestWithLifecyclePermissionReviewBreakerTripsAtConfiguredThreshold proves
// the threaded limits are not just stored data but actually consulted: a
// session built from a Lifecycle configured with MaxConsecutiveNeedsHuman: 1
// trips its turn-scoped breaker on the first "needs human" outcome, while an
// otherwise-identical session from a Lifecycle with NEITHER permission-review
// option applied never trips (the pre-existing default-disabled-zero
// behavior), proving the option — not some other default — is what causes
// the trip.
func TestWithLifecyclePermissionReviewBreakerTripsAtConfiguredThreshold(t *testing.T) {
	t.Parallel()
	definition := restoreCfg(&stubLLM{chunks: []content.Chunk{textChunk("reply")}}, "model-x", "be helpful")
	coords := identity.Coordinates{SessionID: mustUUID(), LoopID: mustUUID(), TurnID: mustUUID(), StepID: mustUUID()}
	outcome := reviewBreakerOutcome{Reason: gate.ReviewDecisionRecommendation, Eligible: false}

	t.Run("configured breaker trips at threshold", func(t *testing.T) {
		t.Parallel()
		store := newRestoreStore(t)
		lifecycle, err := newTestLifecycle(
			definition, store,
			WithLifecyclePermissionReviewBreaker(PermissionReviewBreakerLimits{MaxConsecutiveNeedsHuman: 1}),
		)
		if err != nil {
			t.Fatalf("NewTopologyLifecycle: %v", err)
		}
		s, err := lifecycle.NewSession(context.Background(), "")
		if err != nil {
			t.Fatalf("NewSession: %v", err)
		}
		t.Cleanup(func() { _ = s.Shutdown(context.Background()) })

		if !s.reviewBreakerAllows(coords) {
			t.Fatal("breaker already tripped before any outcome was observed")
		}
		s.observePermissionReviewOutcome(coords, outcome)
		if s.reviewBreakerAllows(coords) {
			t.Fatal("breaker did not trip at the configured threshold")
		}
	})

	t.Run("unconfigured lifecycle never trips (regression)", func(t *testing.T) {
		t.Parallel()
		store := newRestoreStore(t)
		lifecycle, err := newTestLifecycle(definition, store)
		if err != nil {
			t.Fatalf("NewTopologyLifecycle: %v", err)
		}
		s, err := lifecycle.NewSession(context.Background(), "")
		if err != nil {
			t.Fatalf("NewSession: %v", err)
		}
		t.Cleanup(func() { _ = s.Shutdown(context.Background()) })

		s.observePermissionReviewOutcome(coords, outcome)
		if !s.reviewBreakerAllows(coords) {
			t.Fatal("breaker tripped even though no breaker limits were ever configured")
		}
	})
}

// TestLifecycleWithNeitherPermissionReviewOptionPreservesNoOpDefault is the
// regression test for the zero-Lifecycle case: when neither
// WithLifecyclePermissionReview nor WithLifecyclePermissionReviewBreaker is
// applied, a NewSession-built session keeps the exact pre-existing
// "no permission review configured" defaults — a zero classifier set, a zero
// review policy, and zero (fully-disabled) breaker limits.
func TestLifecycleWithNeitherPermissionReviewOptionPreservesNoOpDefault(t *testing.T) {
	t.Parallel()
	store := newRestoreStore(t)
	definition := restoreCfg(&stubLLM{chunks: []content.Chunk{textChunk("reply")}}, "model-x", "be helpful")
	lifecycle, err := newTestLifecycle(definition, store)
	if err != nil {
		t.Fatalf("NewTopologyLifecycle: %v", err)
	}
	s, err := lifecycle.NewSession(context.Background(), "")
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	t.Cleanup(func() { _ = s.Shutdown(context.Background()) })

	if len(s.permissionClassifiers.Classifiers()) != 0 {
		t.Fatalf("permissionClassifiers = %#v, want empty", s.permissionClassifiers.Classifiers())
	}
	if s.permissionReviewPolicy.Revision != "" {
		t.Fatalf("permissionReviewPolicy.Revision = %q, want empty", s.permissionReviewPolicy.Revision)
	}
	var zero reviewCircuitBreakerLimits
	if s.review.limits != zero {
		t.Fatalf("review.limits = %+v, want the zero value", s.review.limits)
	}

	req := validReviewRequest(t, mustUUID(), mustUUID())
	done := make(chan struct{})
	go func() {
		s.StartPermissionReview(context.Background(), req)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("StartPermissionReview did not return promptly for an unconfigured session")
	}
}
