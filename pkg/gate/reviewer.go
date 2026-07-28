package gate

import (
	"encoding/json"
	"reflect"
	"strings"
	"unicode/utf8"

	"github.com/looprig/harness/pkg/hustle"
)

// PermissionAssessmentOutcome is one classifier's applicability and terminal
// review state. Only applicable, allowed outcomes carry an assessment that can
// contribute to eligibility.
type PermissionAssessmentOutcome struct {
	Subject    PermissionReviewSubject
	Applicable bool
	Status     ReviewStatus
	Assessment PermissionAssessment
}

// CombinePermissionAssessments applies ordered conjunctive review semantics.
// The first applicable failure wins; non-applicable outcomes are neutral only
// when their status is exactly not_applicable.
func CombinePermissionAssessments(
	policy PermissionReviewPolicy,
	outcomes []PermissionAssessmentOutcome,
) ReviewDecision {
	if !validPermissionReviewPolicy(policy) {
		return reviewDecision(ReviewDecisionInvalidPolicy)
	}
	var commonDigest [32]byte
	revisions := make(map[string]struct{}, len(outcomes))
	for index, outcome := range outcomes {
		if !validPermissionAssessmentOutcomeStatus(outcome) {
			return reviewDecision(ReviewDecisionClassifierStatus)
		}
		if outcome.Status != ReviewStatusAllowed &&
			!zeroPermissionAssessment(outcome.Assessment) {
			return reviewDecision(ReviewDecisionInvalidAssessment)
		}
		if !validStoredPermissionReviewSubject(outcome.Subject) {
			return reviewDecision(ReviewDecisionInvalidAssessment)
		}
		if policy.Revision != outcome.Subject.Basis.GatePolicyRevision {
			return reviewDecision(ReviewDecisionInvalidPolicy)
		}
		revision := outcome.Subject.Basis.ClassifierRevision
		if _, duplicate := revisions[revision]; duplicate {
			return reviewDecision(ReviewDecisionInvalidAssessment)
		}
		revisions[revision] = struct{}{}
		digest, err := permissionReviewCommonSubjectDigest(outcome.Subject)
		if err != nil {
			return reviewDecision(ReviewDecisionInvalidAssessment)
		}
		if index == 0 {
			commonDigest = digest
		} else if digest != commonDigest {
			return reviewDecision(ReviewDecisionInvalidAssessment)
		}
		if !validPermissionAssessmentOutcome(outcome) {
			return reviewDecision(ReviewDecisionInvalidAssessment)
		}
	}
	applicable := false
	for _, outcome := range outcomes {
		if !outcome.Applicable {
			continue
		}
		applicable = true
		if outcome.Status != ReviewStatusAllowed {
			return reviewDecision(ReviewDecisionClassifierStatus)
		}
		decision := EvaluatePermissionAssessment(policy, outcome.Subject, outcome.Assessment)
		if !decision.Eligible {
			return decision
		}
	}
	if !applicable {
		return reviewDecision(ReviewDecisionNoApplicableClassifier)
	}
	return ReviewDecision{Eligible: true, Reason: ReviewDecisionEligible}
}

func validPermissionAssessmentOutcome(outcome PermissionAssessmentOutcome) bool {
	if outcome.Status != ReviewStatusAllowed {
		return zeroPermissionAssessment(outcome.Assessment)
	}
	return outcome.Assessment.Basis == outcome.Subject.Basis &&
		validPermissionAssessment(outcome.Assessment)
}

func validPermissionAssessmentOutcomeStatus(
	outcome PermissionAssessmentOutcome,
) bool {
	if !outcome.Applicable {
		return outcome.Status == ReviewStatusNotApplicable
	}
	status, valid := ParseReviewStatus(string(outcome.Status))
	return valid && status != ReviewStatusNotApplicable
}

func zeroPermissionAssessment(assessment PermissionAssessment) bool {
	return assessment.Basis == (ReviewBasis{}) &&
		assessment.Risk == "" &&
		assessment.Authorization == "" &&
		len(assessment.Categories) == 0 &&
		assessment.Recommendation == "" &&
		assessment.Rationale == ""
}

// PermissionClassifier is the deliberately narrow contract implemented by
// trusted classifier packages. It conveys data and immutable Hustle policy,
// never gate response or durable-grant authority.
type PermissionClassifier interface {
	Name() hustle.Name
	Revision() string
	Definition() hustle.Definition
	Applies(PermissionReviewSubject) bool
	MarshalInput(PermissionReviewSubject) (json.RawMessage, error)
	ValidateResult(PermissionReviewSubject, hustle.Result) (PermissionAssessment, error)
}

// PermissionClassifierSet is an immutable, ordered classifier registry.
type PermissionClassifierSet struct {
	ordered []PermissionClassifier
}

// PermissionClassifierValidationReason is the bounded registry rejection
// domain. Rejected classifier metadata is never included in the error.
type PermissionClassifierValidationReason string

const (
	PermissionClassifierInvalid   PermissionClassifierValidationReason = "invalid"
	PermissionClassifierDuplicate PermissionClassifierValidationReason = "duplicate"
)

// PermissionClassifierValidationError reports only a bounded reason and
// registration position.
type PermissionClassifierValidationError struct {
	Index  int
	Reason PermissionClassifierValidationReason
}

func (*PermissionClassifierValidationError) Error() string {
	return "gate: invalid permission classifier registration"
}

// NewPermissionClassifierSet validates metadata without executing classifier
// applicability, serialization, or result parsing behavior.
func NewPermissionClassifierSet(
	classifiers ...PermissionClassifier,
) (PermissionClassifierSet, error) {
	if len(classifiers) == 0 {
		return PermissionClassifierSet{}, classifierSetError(0, PermissionClassifierInvalid)
	}
	ordered := append([]PermissionClassifier(nil), classifiers...)
	names := make(map[hustle.Name]struct{}, len(ordered))
	revisions := make(map[string]struct{}, len(ordered))
	for index, classifier := range ordered {
		if nilPermissionClassifier(classifier) {
			return PermissionClassifierSet{}, classifierSetError(index, PermissionClassifierInvalid)
		}
		name := classifier.Name()
		if err := name.Validate(); err != nil {
			return PermissionClassifierSet{}, classifierSetError(index, PermissionClassifierInvalid)
		}
		if _, duplicate := names[name]; duplicate {
			return PermissionClassifierSet{}, classifierSetError(index, PermissionClassifierDuplicate)
		}
		revision := classifier.Revision()
		if strings.TrimSpace(revision) == "" ||
			!utf8.ValidString(revision) ||
			len(revision) > MaxPermissionClassifierRevisionBytes {
			return PermissionClassifierSet{}, classifierSetError(index, PermissionClassifierInvalid)
		}
		if _, duplicate := revisions[revision]; duplicate {
			return PermissionClassifierSet{}, classifierSetError(index, PermissionClassifierDuplicate)
		}
		definition := classifier.Definition()
		descriptor := definition.Descriptor()
		if err := descriptor.Validate(); err != nil ||
			descriptor.Participation != hustle.ParticipationBlocking ||
			descriptor.ModelSource != hustle.ModelSourceNamed ||
			descriptor.OutputSchemaName == "" ||
			descriptor.OutputSchemaSHA256 == ([32]byte{}) ||
			descriptor.StructuredOutputRevision == "" ||
			descriptor.Name != name ||
			descriptor.PolicyRevision != revision {
			return PermissionClassifierSet{}, classifierSetError(index, PermissionClassifierInvalid)
		}
		names[name] = struct{}{}
		revisions[revision] = struct{}{}
	}
	return PermissionClassifierSet{ordered: ordered}, nil
}

// Classifiers returns an independent ordered registry view.
func (s PermissionClassifierSet) Classifiers() []PermissionClassifier {
	return append([]PermissionClassifier(nil), s.ordered...)
}

func nilPermissionClassifier(classifier PermissionClassifier) bool {
	if classifier == nil {
		return true
	}
	value := reflect.ValueOf(classifier)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map,
		reflect.Pointer, reflect.Slice:
		return value.IsNil()
	default:
		return false
	}
}

func classifierSetError(
	index int,
	reason PermissionClassifierValidationReason,
) error {
	return &PermissionClassifierValidationError{Index: index, Reason: reason}
}
