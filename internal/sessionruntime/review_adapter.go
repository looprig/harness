package sessionruntime

import (
	"context"
	"log/slog"
	"reflect"
	"strings"

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

// permissionReviewResponder is the narrow seam through which an eligible
// combined review decision reaches gate state. It is satisfied by
// *Session.respondFromClassifier — the ONLY method in the codebase permitted
// to stamp gate.ResponseFromClassifier provenance — and is declared here,
// package-private, so nothing outside sessionruntime can supply a
// substitute: a consumer cannot construct an adapter whose "responder" skips
// the private claim/drift-check path (design §25.2).
type permissionReviewResponder interface {
	respondFromClassifier(ctx context.Context, basis gate.ReviewBasis, reason string) error
}

// permissionReviewAdapter is the session-wide (not per-loop, unlike
// compaction) facility that begins classifier review once a permission gate
// has activated and the loop actor has handed off a
// loopruntime.PermissionReviewRequest via Session.StartPermissionReview.
//
// It implements the whole of design §14.3 ("Start review"): evaluate
// classifier applicability, build one subject+basis per applicable
// classifier, schedule that classifier's Hustle run (steps 1-2+4);
// validate every result and combine decisions via
// gate.CombinePermissionAssessments (steps 5-6); and, only when the combined
// decision is eligible, attempt a classifier-originated response through
// permissionReviewResponder, which itself re-reads live gate state and
// recomputes the entire basis immediately before claiming (step 7-8). Every
// other outcome — not applicable, any classifier status other than allowed,
// an invalid/mismatched assessment, or an ineligible combined decision —
// reaches no responder call at all, so design §25.4 ("every ambiguous,
// invalid, missing, stale, cancelled, or unknown condition preserves the
// human gate") holds by construction, not by error handling.
//
// Publishing secret-free start/completed events (design §14.3 step 3) is
// explicitly deferred to Task 17; this adapter does not publish any events.
type permissionReviewAdapter struct {
	runner      permissionReviewHustleRunner
	classifiers gate.PermissionClassifierSet
	policy      gate.PermissionReviewPolicy
	responder   permissionReviewResponder
}

// permissionReviewAdapterField names the collaborator newPermissionReviewAdapter
// rejected, mirroring compactionAdapterField's shape.
type permissionReviewAdapterField string

const (
	permissionReviewAdapterFieldRunner      permissionReviewAdapterField = "runner"
	permissionReviewAdapterFieldClassifiers permissionReviewAdapterField = "classifiers"
	permissionReviewAdapterFieldPolicy      permissionReviewAdapterField = "policy"
	permissionReviewAdapterFieldResponder   permissionReviewAdapterField = "responder"
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
	responder permissionReviewResponder,
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
	if nilPermissionReviewResponder(responder) {
		return nil, &permissionReviewAdapterError{Field: permissionReviewAdapterFieldResponder}
	}
	return &permissionReviewAdapter{runner: runner, classifiers: classifiers, policy: policy, responder: responder}, nil
}

func nilPermissionReviewRunner(value permissionReviewHustleRunner) bool {
	if value == nil {
		return true
	}
	reflected := reflect.ValueOf(value)
	return reflected.Kind() == reflect.Pointer && reflected.IsNil()
}

func nilPermissionReviewResponder(value permissionReviewResponder) bool {
	if value == nil {
		return true
	}
	reflected := reflect.ValueOf(value)
	return reflected.Kind() == reflect.Pointer && reflected.IsNil()
}

// review evaluates every registered classifier's applicability against req,
// schedules a Hustle run for each applicable one (in registration order),
// combines every classifier's terminal outcome via
// gate.CombinePermissionAssessments, and — only when that combined decision
// is eligible — attempts a classifier-originated gate response through
// a.responder.
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
	classifiers := a.classifiers.Classifiers()
	outcomes := make([]gate.PermissionAssessmentOutcome, 0, len(classifiers))
	for _, classifier := range classifiers {
		outcomes = append(outcomes, a.reviewOne(ctx, req, classifier))
	}
	decision := gate.CombinePermissionAssessments(a.policy, a.classifiers, outcomes)
	if !decision.Eligible {
		return
	}
	basis, reason, ok := classifierApprovalBasis(classifiers, outcomes)
	if !ok {
		// Structurally unreachable given decision.Eligible == true (eligibility
		// requires at least one applicable+allowed outcome), but fails closed
		// rather than attempting a response with no evidence to justify it.
		return
	}
	if err := a.responder.respondFromClassifier(ctx, basis, reason); err != nil {
		slog.Warn("sessionruntime: classifier-originated gate response failed",
			"gate_id", req.GateID, "error", err)
	}
}

// reviewOne builds ONE classifier's subject+basis (design §14.3 step 2),
// checks applicability (step 1), and — only when applicable — marshals the
// input, schedules the classifier's Hustle, and validates its result (steps
// 4-5). It ALWAYS returns exactly one gate.PermissionAssessmentOutcome for
// this classifier — never skips — because
// gate.CombinePermissionAssessments requires exactly one outcome per
// registered classifier, in registration order, to even structurally
// validate.
//
// Every failure mode (invalid subject, marshal error, a non-allowed
// terminal Hustle outcome, a validator that rejects the result) is logged
// and reported as a non-allowed outcome rather than propagated: there is no
// caller waiting on this fire-and-forget goroutine, and a non-allowed
// outcome can never itself contribute to eligibility (see the type doc) —
// CombinePermissionAssessments fails the WHOLE combined decision closed the
// moment any applicable classifier's status is not allowed.
func (a *permissionReviewAdapter) reviewOne(ctx context.Context, req loopruntime.PermissionReviewRequest, classifier gate.PermissionClassifier) gate.PermissionAssessmentOutcome {
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
		slog.Warn("sessionruntime: permission review subject invalid; failing classifier closed",
			"classifier", classifier.Name(), "gate_id", req.GateID, "error", err)
		// No valid subject can be constructed for this classifier's slot at
		// all: leave the outcome's ClassifierRevision empty so it can never
		// match this classifier's registered revision, which makes
		// CombinePermissionAssessments' very first structural check fail the
		// whole decision closed.
		return gate.PermissionAssessmentOutcome{Applicable: true, Status: gate.ReviewStatusFailed}
	}
	if !classifier.Applies(subject) {
		return gate.PermissionAssessmentOutcome{Subject: subject, Status: gate.ReviewStatusNotApplicable}
	}
	input, err := classifier.MarshalInput(subject)
	if err != nil {
		slog.Warn("sessionruntime: permission review input marshal failed; failing classifier closed",
			"classifier", classifier.Name(), "gate_id", req.GateID, "error", err)
		return gate.PermissionAssessmentOutcome{Subject: subject, Applicable: true, Status: gate.ReviewStatusFailed}
	}

	var assessment gate.PermissionAssessment
	allowed := false
	validate := func(_ context.Context, result hustle.Result) error {
		parsed, validationErr := classifier.ValidateResult(subject, result)
		if validationErr != nil {
			return validationErr
		}
		assessment = parsed
		return nil
	}
	finish := func(_ context.Context, outcome hustle.Outcome) error {
		allowed = outcome.Err == nil && outcome.Result != nil
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
	if !allowed {
		return gate.PermissionAssessmentOutcome{Subject: subject, Applicable: true, Status: gate.ReviewStatusFailed}
	}
	return gate.PermissionAssessmentOutcome{
		Subject: subject, Applicable: true, Status: gate.ReviewStatusAllowed, Assessment: assessment,
	}
}

// classifierApprovalBasis derives the common ReviewBasis and a stable,
// human-readable classifier-identity reason string from an eligible combined
// decision's outcomes. It requires at least one applicable+allowed outcome
// (guaranteed whenever gate.CombinePermissionAssessments reports Eligible)
// and returns ok=false otherwise, so a caller cannot accidentally attempt a
// classifier response backed by no real evidence.
//
// classifiers and outcomes MUST be the same length and in the same order —
// review always builds them that way, one outcome per registered classifier.
// Every applicable classifier's subject shares the same basis fields other
// than ClassifierRevision/SubjectDigest (CombinePermissionAssessments enforces
// this via its common-subject-digest check), so taking the basis from the
// first applicable outcome and zeroing its classifier-specific fields yields
// the one shared ReviewBasis every contributing classifier agreed on.
func classifierApprovalBasis(
	classifiers []gate.PermissionClassifier,
	outcomes []gate.PermissionAssessmentOutcome,
) (gate.ReviewBasis, string, bool) {
	var basis gate.ReviewBasis
	haveBasis := false
	var reasons []string
	for index, outcome := range outcomes {
		if !outcome.Applicable {
			continue
		}
		if !haveBasis {
			basis = outcome.Subject.Basis
			basis.ClassifierRevision = ""
			basis.SubjectDigest = [32]byte{}
			haveBasis = true
		}
		if index < len(classifiers) {
			reasons = append(reasons, string(classifiers[index].Name())+"@"+classifiers[index].Revision())
		}
	}
	if !haveBasis || len(reasons) == 0 {
		return gate.ReviewBasis{}, "", false
	}
	return basis, strings.Join(reasons, ";"), true
}
