package gate

import (
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/tool"
)

const (
	ReviewValidationFieldBasis   ReviewValidationField = "basis"
	ReviewValidationFieldDigest  ReviewValidationField = "digest"
	ReviewValidationFieldRequest ReviewValidationField = "request"
	ReviewValidationFieldWire    ReviewValidationField = "wire"
)

const ReviewValidationMismatch ReviewValidationReason = "mismatch"

// ReviewBasis binds a classifier decision to one exact live permission
// request and the policy revisions under which it was reviewed.
type ReviewBasis struct {
	GateID             ID       `json:"gate_id"`
	ToolExecutionID    ID       `json:"tool_execution_id"`
	SubjectDigest      [32]byte `json:"subject_digest"`
	ContextRevision    string   `json:"context_revision"`
	GatePolicyRevision string   `json:"gate_policy_revision"`
	ClassifierRevision string   `json:"classifier_revision"`
	SecurityCeiling    string   `json:"security_ceiling"`
}

// PermissionReviewSubject is the immutable, authority-labeled input to a
// permission classifier.
type PermissionReviewSubject struct {
	Basis   ReviewBasis   `json:"basis"`
	Request tool.Request  `json:"request"`
	Context ReviewContext `json:"context"`
}

// NewPermissionReviewSubject validates, owns, and digest-stamps one subject.
func NewPermissionReviewSubject(
	basis ReviewBasis,
	request tool.Request,
	context ReviewContext,
) (PermissionReviewSubject, error) {
	if basis.SubjectDigest != ([32]byte{}) {
		return PermissionReviewSubject{}, reviewSubjectError(
			ReviewValidationFieldDigest,
			ReviewValidationReserved,
		)
	}
	subject := PermissionReviewSubject{
		Basis:   basis,
		Request: request.Clone(),
		Context: context.Clone(),
	}
	if err := validatePermissionReviewSubject(subject); err != nil {
		return PermissionReviewSubject{}, err
	}
	digest, err := SubjectDigest(subject)
	if err != nil {
		return PermissionReviewSubject{}, err
	}
	subject.Basis.SubjectDigest = digest
	return subject, nil
}

// Clone returns an owned copy of the subject and all nested slices.
func (s PermissionReviewSubject) Clone() PermissionReviewSubject {
	clone := s
	clone.Request = s.Request.Clone()
	clone.Context = s.Context.Clone()
	return clone
}

// SubjectDigest validates the non-digest subject invariants and recomputes its
// canonical digest while deliberately ignoring the stored digest.
func SubjectDigest(subject PermissionReviewSubject) ([32]byte, error) {
	if err := validatePermissionReviewSubject(subject); err != nil {
		return [32]byte{}, err
	}
	return permissionReviewSubjectDigest(subject)
}

func validatePermissionReviewSubject(subject PermissionReviewSubject) error {
	basis := subject.Basis
	if basis.GateID.IsZero() || basis.ToolExecutionID.IsZero() {
		return reviewSubjectError(ReviewValidationFieldBasis, ReviewValidationRequired)
	}
	for _, value := range []string{
		basis.ContextRevision,
		basis.GatePolicyRevision,
		basis.ClassifierRevision,
		basis.SecurityCeiling,
	} {
		if value == "" {
			return reviewSubjectError(ReviewValidationFieldBasis, ReviewValidationRequired)
		}
		if !utf8.ValidString(value) {
			return reviewSubjectError(ReviewValidationFieldBasis, ReviewValidationInvalid)
		}
	}
	if err := tool.ValidateRequest(subject.Request); err != nil {
		return reviewSubjectError(ReviewValidationFieldRequest, ReviewValidationInvalid)
	}
	if subject.Request.ExecutionID != "" {
		executionID, err := uuid.Parse(subject.Request.ExecutionID)
		if err != nil ||
			executionID.String() != subject.Request.ExecutionID ||
			executionID != basis.ToolExecutionID {
			return reviewSubjectError(ReviewValidationFieldRequest, ReviewValidationMismatch)
		}
	}
	if err := validateBuiltReviewContext(subject.Context); err != nil {
		return err
	}
	if basis.ContextRevision != subject.Context.ContextRevision ||
		basis.GatePolicyRevision != subject.Context.GatePolicyRevision ||
		basis.SecurityCeiling != subject.Context.SecurityCeiling {
		return reviewSubjectError(ReviewValidationFieldBasis, ReviewValidationMismatch)
	}
	return nil
}

func validateBuiltReviewContext(context ReviewContext) error {
	if context.Coordinates.SessionID.IsZero() ||
		context.Coordinates.LoopID.IsZero() ||
		context.Coordinates.TurnID.IsZero() ||
		context.Coordinates.StepID.IsZero() {
		return reviewContextError(ReviewValidationFieldContext, ReviewValidationRequired)
	}
	for _, value := range []string{
		context.ContextRevision,
		context.WorkspaceRoot,
		context.WorkingDirectory,
		context.RetryReason,
		context.SecurityCeiling,
		context.GatePolicyRevision,
	} {
		if !utf8.ValidString(value) {
			return reviewContextError(ReviewValidationFieldContext, ReviewValidationInvalid)
		}
	}
	if context.ContextRevision == "" ||
		context.WorkspaceRoot == "" ||
		context.WorkingDirectory == "" ||
		context.SecurityCeiling == "" ||
		context.GatePolicyRevision == "" {
		return reviewContextError(ReviewValidationFieldContext, ReviewValidationRequired)
	}
	if !cleanAbsolutePath(context.WorkspaceRoot) ||
		!cleanAbsolutePath(context.WorkingDirectory) {
		return reviewContextError(ReviewValidationFieldContext, ReviewValidationInvalid)
	}
	relative, err := filepath.Rel(context.WorkspaceRoot, context.WorkingDirectory)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return reviewContextError(ReviewValidationFieldContext, ReviewValidationInvalid)
	}
	if context.Truncation.Applied&^SupportedReviewTruncationMask != 0 ||
		context.Truncation.Material&^SupportedReviewTruncationMask != 0 ||
		context.Truncation.Material&^context.Truncation.Applied != 0 ||
		context.Truncation.OmittedEntries < 0 ||
		context.Truncation.OmittedEntries > MaxPermissionReviewSubjectWireBytes ||
		context.Truncation.OmittedBytes < 0 ||
		context.Truncation.OmittedBytes > MaxPermissionReviewSubjectWireBytes {
		return reviewContextError(ReviewValidationFieldContext, ReviewValidationOutOfBounds)
	}

	currentUser := -1
	activeAction := -1
	omissions := 0
	truncatedEntries := 0
	var explainedNonBudget ReviewTruncationMask
	for index, entry := range context.Entries {
		if !utf8.ValidString(string(entry.Origin)) ||
			!utf8.ValidString(string(entry.Kind)) ||
			!utf8.ValidString(entry.Content) ||
			!validReviewContextPair(entry.Origin, entry.Kind) {
			return reviewContextError(ReviewValidationFieldContextEntry, ReviewValidationInvalid)
		}
		switch entry.Kind {
		case ReviewContextKindUserMessage:
			currentUser = index
		case ReviewContextKindAssistantToolRequest:
			activeAction = index
		case ReviewContextKindOmission:
			omissions++
			if entry.Truncated ||
				context.Truncation.OmittedEntries <= 0 ||
				entry.Content != reviewContextOmissionMarker(
					context.Truncation.OmittedEntries,
					context.Truncation.OmittedBytes,
				) {
				return reviewContextError(ReviewValidationFieldContextEntry, ReviewValidationInvalid)
			}
		}
		if entry.Truncated {
			truncatedEntries++
			entryMask := reviewTruncationMaskForEntry(entry)
			exercised := context.Truncation.Applied & entryMask
			explainedNonBudget |= exercised
			markerIndex := strings.Index(entry.Content, reviewContextTruncationMarker)
			if exercised == 0 ||
				markerIndex <= 0 ||
				markerIndex+len(reviewContextTruncationMarker) >= len(entry.Content) ||
				strings.Count(entry.Content, reviewContextTruncationMarker) != 1 ||
				materialReviewContextKind(entry.Kind) &&
					context.Truncation.Material&exercised != exercised {
				return reviewContextError(ReviewValidationFieldContextEntry, ReviewValidationInvalid)
			}
		}
	}
	if currentUser < 0 || activeAction < 0 {
		return reviewContextError(ReviewValidationFieldContextEntry, ReviewValidationRequired)
	}
	if context.Entries[activeAction].Truncated {
		return reviewContextError(ReviewValidationFieldContextEntry, ReviewValidationInvalid)
	}
	if context.Truncation.Applied&reviewNonBudgetTruncationMask&^explainedNonBudget != 0 ||
		context.Truncation.Material&reviewNonBudgetTruncationMask&^explainedNonBudget != 0 {
		return reviewContextError(ReviewValidationFieldContext, ReviewValidationInvalid)
	}
	hasOmissions := context.Truncation.OmittedEntries > 0
	hasBudgetTruncation := context.Truncation.Applied&reviewBudgetTruncationMask != 0
	if (omissions == 1) != hasOmissions ||
		hasBudgetTruncation != hasOmissions ||
		context.Truncation.Material&reviewBudgetTruncationMask !=
			context.Truncation.Applied&reviewBudgetTruncationMask ||
		!hasOmissions && context.Truncation.OmittedBytes != 0 ||
		omissions > 1 ||
		truncatedEntries > 0 && context.Truncation.Applied == 0 ||
		context.Truncation.Applied == 0 &&
			(context.Truncation.Material != 0 || hasOmissions || truncatedEntries > 0) {
		return reviewContextError(ReviewValidationFieldContext, ReviewValidationInvalid)
	}
	return nil
}

const reviewBudgetTruncationMask = ReviewTruncationEntryCount |
	ReviewTruncationTotalBytes |
	ReviewTruncationEstimatedTokens

const reviewNonBudgetTruncationMask = ReviewTruncationUserEntry |
	ReviewTruncationAssistantEntry |
	ReviewTruncationToolEntry |
	ReviewTruncationBlock |
	ReviewTruncationActiveAction

func reviewTruncationMaskForEntry(entry ReviewContextEntry) ReviewTruncationMask {
	switch entry.Kind {
	case ReviewContextKindUserMessage:
		return ReviewTruncationUserEntry | ReviewTruncationBlock
	case ReviewContextKindAssistantMessage, ReviewContextKindAssistantToolRequest:
		return ReviewTruncationAssistantEntry | ReviewTruncationBlock
	case ReviewContextKindToolResult:
		return ReviewTruncationToolEntry | ReviewTruncationBlock
	default:
		return ReviewTruncationBlock
	}
}

func reviewSubjectError(field ReviewValidationField, reason ReviewValidationReason) error {
	return &ReviewValidationError{Field: field, Reason: reason}
}
