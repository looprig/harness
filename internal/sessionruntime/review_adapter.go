package sessionruntime

import (
	"context"
	"log/slog"
	"reflect"

	"github.com/looprig/harness/internal/hustleruntime"
	"github.com/looprig/harness/internal/loopruntime"
	"github.com/looprig/harness/pkg/gate"
	"github.com/looprig/harness/pkg/hustle"
	"github.com/looprig/harness/pkg/identity"
)

// permissionReviewHustleRunner is the narrow seam the review adapter uses to
// run one classifier's bound Hustle. It has the exact same method shape as
// compactionHustleRunner (internal/sessionruntime/compaction_adapter.go) —
// both are satisfied by *hustleruntime.Controller — so review and compaction
// share the one session-wide Hustle runtime without either adapter depending
// on a generic session runner. It is declared again here, rather than reused,
// so the two call sites stay free to diverge (Interface Segregation): a
// future requirement on one must not force a method onto the other.
type permissionReviewHustleRunner interface {
	RunAndFinalize(context.Context, hustle.Request, hustleruntime.ValidateResult, hustleruntime.Finalizer) error
}

// permissionReviewAdapter is the session-wide (not per-loop, unlike
// compaction) facility that begins classifier review once a permission gate
// has activated and the loop actor has handed off a
// loopruntime.PermissionReviewRequest via Session.StartPermissionReview.
//
// It implements design §14.3 ("Start review") steps 1-2 in full — evaluate
// classifier applicability, and build one subject+basis per applicable
// classifier — and, because scheduling a Hustle run falls out naturally once
// a subject is built, also performs step 4 for each applicable classifier.
// It deliberately stops there: publishing secret-free start events (step 3),
// validating every result and combining decisions (steps 5-6), re-reading
// live gate/security state (step 7), and attempting a classifier-originated
// gate response (step 8) are Task 15's job, once gate.ResponseFromClassifier
// provenance stamping exists. Until Task 15 lands, a scheduled classifier
// run's outcome is intentionally discarded: nothing in this adapter can reach
// RespondGate, so every review failure mode already satisfies design §25.4
// ("every ambiguous, invalid, missing, stale, cancelled, or unknown condition
// preserves the human gate") by construction — there is no path from here to
// a gate response at all yet.
type permissionReviewAdapter struct {
	runner      permissionReviewHustleRunner
	classifiers gate.PermissionClassifierSet
	policy      gate.PermissionReviewPolicy
}

// permissionReviewAdapterField names the collaborator newPermissionReviewAdapter
// rejected, mirroring compactionAdapterField's shape.
type permissionReviewAdapterField string

const (
	permissionReviewAdapterFieldRunner      permissionReviewAdapterField = "runner"
	permissionReviewAdapterFieldClassifiers permissionReviewAdapterField = "classifiers"
	permissionReviewAdapterFieldPolicy      permissionReviewAdapterField = "policy"
)

type permissionReviewAdapterError struct{ Field permissionReviewAdapterField }

func (e *permissionReviewAdapterError) Error() string {
	return "sessionruntime: invalid permission review adapter field " + string(e.Field)
}

// newPermissionReviewAdapter validates its collaborators up front so a
// misconfigured adapter fails before the first gate rather than deep inside a
// fire-and-forget goroutine where nothing observes the error.
func newPermissionReviewAdapter(
	runner permissionReviewHustleRunner,
	classifiers gate.PermissionClassifierSet,
	policy gate.PermissionReviewPolicy,
) (*permissionReviewAdapter, error) {
	if nilPermissionReviewRunner(runner) {
		return nil, &permissionReviewAdapterError{Field: permissionReviewAdapterFieldRunner}
	}
	if len(classifiers.Classifiers()) == 0 {
		return nil, &permissionReviewAdapterError{Field: permissionReviewAdapterFieldClassifiers}
	}
	if policy.Revision == "" {
		return nil, &permissionReviewAdapterError{Field: permissionReviewAdapterFieldPolicy}
	}
	return &permissionReviewAdapter{runner: runner, classifiers: classifiers, policy: policy}, nil
}

func nilPermissionReviewRunner(value permissionReviewHustleRunner) bool {
	if value == nil {
		return true
	}
	reflected := reflect.ValueOf(value)
	return reflected.Kind() == reflect.Pointer && reflected.IsNil()
}

// review evaluates every registered classifier's applicability against req
// and schedules a Hustle run for each applicable one, in registration order.
//
// The caller (Session.StartPermissionReview) MUST invoke this on its own
// goroutine: review blocks for as long as each scheduled RunAndFinalize call
// takes, and doing that inline would block whatever called it. That
// obligation is exactly the one the loop actor places on
// StartPermissionReview itself (gate.go's permissionReviewStarter) — review
// simply keeps it.
//
// A live-only ReviewContext is required. req.ReviewContext.ContextRevision ==
// "" means no review context was captured for this turn (turnConfig.
// reviewContext was nil — internal/loopruntime/review_context.go), which
// fails closed as "nothing to review" rather than guessing at defaults.
func (a *permissionReviewAdapter) review(ctx context.Context, req loopruntime.PermissionReviewRequest) {
	if req.ReviewContext.ContextRevision == "" {
		return
	}
	for _, classifier := range a.classifiers.Classifiers() {
		a.reviewOne(ctx, req, classifier)
	}
}

// reviewOne builds ONE classifier's subject+basis (design §14.3 step 2),
// checks applicability (step 1), and — only when applicable — marshals the
// input and schedules the classifier's Hustle. Every failure mode (invalid
// subject, marshal error, a non-nil RunAndFinalize error) is logged and
// skipped rather than propagated: there is no caller waiting on this
// fire-and-forget goroutine, and skipping a classifier can never itself
// produce an approval (see the type doc).
func (a *permissionReviewAdapter) reviewOne(ctx context.Context, req loopruntime.PermissionReviewRequest, classifier gate.PermissionClassifier) {
	basis := gate.ReviewBasis{
		GateID:             req.GateID,
		ToolExecutionID:    req.ToolExecutionID,
		ContextRevision:    req.ReviewContext.ContextRevision,
		GatePolicyRevision: req.ReviewContext.GatePolicyRevision,
		ClassifierRevision: classifier.Revision(),
		SecurityCeiling:    req.ReviewContext.SecurityCeiling,
	}
	subject, err := gate.NewPermissionReviewSubject(basis, req.Request, req.ReviewContext)
	if err != nil {
		slog.Warn("sessionruntime: permission review subject invalid; skipping classifier",
			"classifier", classifier.Name(), "gate_id", req.GateID, "error", err)
		return
	}
	if !classifier.Applies(subject) {
		return
	}
	input, err := classifier.MarshalInput(subject)
	if err != nil {
		slog.Warn("sessionruntime: permission review input marshal failed; skipping classifier",
			"classifier", classifier.Name(), "gate_id", req.GateID, "error", err)
		return
	}
	validate := func(_ context.Context, result hustle.Result) error {
		_, validationErr := classifier.ValidateResult(subject, result)
		return validationErr
	}
	finish := func(context.Context, hustle.Outcome) error {
		// Task 15 replaces this with real validate+combine+classifier-originated
		// response handling (design §14.3 steps 5-8). Until then the outcome is
		// intentionally discarded — see the type doc.
		return nil
	}
	runRequest := hustle.Request{
		Name:  classifier.Name(),
		Cause: identity.Cause{Coordinates: req.ReviewContext.Coordinates},
		Input: input,
	}
	if err := a.runner.RunAndFinalize(ctx, runRequest, validate, finish); err != nil {
		slog.Warn("sessionruntime: permission review classifier run failed",
			"classifier", classifier.Name(), "gate_id", req.GateID, "error", err)
	}
}
