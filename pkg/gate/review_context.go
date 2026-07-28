package gate

import (
	"encoding/json"
	"math"
	"path/filepath"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/looprig/harness/pkg/identity"
)

// ReviewContextOrigin is the authority origin of one permission-review entry.
type ReviewContextOrigin string

const (
	ReviewContextOriginUser      ReviewContextOrigin = "user"
	ReviewContextOriginAssistant ReviewContextOrigin = "assistant"
	ReviewContextOriginTool      ReviewContextOrigin = "tool"
	ReviewContextOriginRuntime   ReviewContextOrigin = "runtime"
	ReviewContextOriginExternal  ReviewContextOrigin = "external"
	ReviewContextOriginOmission  ReviewContextOrigin = "omission"
)

// ParseReviewContextOrigin parses one exact closed authority origin.
func ParseReviewContextOrigin(value string) (ReviewContextOrigin, bool) {
	switch ReviewContextOrigin(value) {
	case ReviewContextOriginUser,
		ReviewContextOriginAssistant,
		ReviewContextOriginTool,
		ReviewContextOriginRuntime,
		ReviewContextOriginExternal,
		ReviewContextOriginOmission:
		return ReviewContextOrigin(value), true
	default:
		return "", false
	}
}

// ReviewContextKind is the semantic kind of one permission-review entry.
type ReviewContextKind string

const (
	ReviewContextKindUserMessage          ReviewContextKind = "user_message"
	ReviewContextKindAssistantMessage     ReviewContextKind = "assistant_message"
	ReviewContextKindAssistantToolRequest ReviewContextKind = "assistant_tool_request"
	ReviewContextKindToolResult           ReviewContextKind = "tool_result"
	ReviewContextKindRuntimeContext       ReviewContextKind = "runtime_context"
	ReviewContextKindExternalContent      ReviewContextKind = "external_content"
	ReviewContextKindOmission             ReviewContextKind = "omission"
)

// ParseReviewContextKind parses one exact closed entry kind.
func ParseReviewContextKind(value string) (ReviewContextKind, bool) {
	switch ReviewContextKind(value) {
	case ReviewContextKindUserMessage,
		ReviewContextKindAssistantMessage,
		ReviewContextKindAssistantToolRequest,
		ReviewContextKindToolResult,
		ReviewContextKindRuntimeContext,
		ReviewContextKindExternalContent,
		ReviewContextKindOmission:
		return ReviewContextKind(value), true
	default:
		return "", false
	}
}

// ReviewContextEntry is an authority-labeled block in a review snapshot.
type ReviewContextEntry struct {
	Origin    ReviewContextOrigin
	Kind      ReviewContextKind
	Content   string
	Truncated bool
}

// ReviewContext is a bounded live-only snapshot used by permission review.
type ReviewContext struct {
	Coordinates        identity.Coordinates
	ContextRevision    string
	WorkspaceRoot      string
	WorkingDirectory   string
	RetryReason        string
	SecurityCeiling    string
	GatePolicyRevision string
	Entries            []ReviewContextEntry
	Truncation         ReviewTruncation
}

// ReviewContextPolicy specifies deterministic bounds for a review snapshot.
type ReviewContextPolicy struct {
	Revision             string
	MaxBytes             int
	MaxEstimatedTokens   int
	MaxEntries           int
	MaxUserEntryBytes    int
	MaxAgentEntryBytes   int
	MaxToolEntryBytes    int
	MaxBlockBytes        int
	MaxActiveActionBytes int
}

// ReviewTruncationMask identifies which context limits were exercised.
type ReviewTruncationMask uint16

const (
	ReviewTruncationUserEntry ReviewTruncationMask = 1 << iota
	ReviewTruncationAssistantEntry
	ReviewTruncationToolEntry
	ReviewTruncationBlock
	ReviewTruncationEntryCount
	ReviewTruncationTotalBytes
	ReviewTruncationEstimatedTokens
	ReviewTruncationActiveAction
)

// SupportedReviewTruncationMask contains every truncation bit understood by
// this revision. Callers can identify unknown bits with
// mask&^SupportedReviewTruncationMask.
const SupportedReviewTruncationMask = ReviewTruncationUserEntry |
	ReviewTruncationAssistantEntry |
	ReviewTruncationToolEntry |
	ReviewTruncationBlock |
	ReviewTruncationEntryCount |
	ReviewTruncationTotalBytes |
	ReviewTruncationEstimatedTokens |
	ReviewTruncationActiveAction

// ReviewTruncation summarizes bounded context loss.
type ReviewTruncation struct {
	Applied        ReviewTruncationMask
	Material       ReviewTruncationMask
	OmittedEntries int
	OmittedBytes   int
}

const (
	ReviewValidationFieldContext       ReviewValidationField = "context"
	ReviewValidationFieldContextEntry  ReviewValidationField = "context_entry"
	ReviewValidationFieldContextPolicy ReviewValidationField = "context_policy"
)

const reviewContextTruncationMarker = "\n…[review context truncated]…\n"

const (
	ReviewValidationRequired    ReviewValidationReason = "required"
	ReviewValidationInvalid     ReviewValidationReason = "invalid"
	ReviewValidationOutOfBounds ReviewValidationReason = "out_of_bounds"
	ReviewValidationReserved    ReviewValidationReason = "reserved"
)

// Clone returns a context that does not alias the receiver's entry slice.
func (c ReviewContext) Clone() ReviewContext {
	clone := c
	clone.Entries = append([]ReviewContextEntry(nil), c.Entries...)
	return clone
}

// BuildReviewContext builds an owned, validated review context snapshot.
func BuildReviewContext(input ReviewContext, policy ReviewContextPolicy) (ReviewContext, error) {
	if err := validateReviewContextRoot(input, policy); err != nil {
		return ReviewContext{}, err
	}

	currentUser := -1
	activeAction := -1
	for i := range input.Entries {
		entry := input.Entries[i]
		if !utf8.ValidString(string(entry.Origin)) ||
			!utf8.ValidString(string(entry.Kind)) ||
			!utf8.ValidString(entry.Content) {
			return ReviewContext{}, reviewContextError(ReviewValidationFieldContextEntry, ReviewValidationInvalid)
		}
		if entry.Truncated {
			return ReviewContext{}, reviewContextError(ReviewValidationFieldContextEntry, ReviewValidationInvalid)
		}
		if entry.Kind == ReviewContextKindOmission || entry.Origin == ReviewContextOriginOmission {
			return ReviewContext{}, reviewContextError(ReviewValidationFieldContextEntry, ReviewValidationReserved)
		}
		if !validReviewContextPair(entry.Origin, entry.Kind) {
			return ReviewContext{}, reviewContextError(ReviewValidationFieldContextEntry, ReviewValidationUnsupported)
		}
		switch entry.Kind {
		case ReviewContextKindUserMessage:
			currentUser = i
		case ReviewContextKindAssistantToolRequest:
			activeAction = i
		}
	}
	if currentUser < 0 || activeAction < 0 {
		return ReviewContext{}, reviewContextError(ReviewValidationFieldContextEntry, ReviewValidationRequired)
	}
	activeLen := len(input.Entries[activeAction].Content)
	if activeLen > policy.MaxActiveActionBytes ||
		activeLen > policy.MaxAgentEntryBytes ||
		activeLen > policy.MaxBlockBytes {
		return ReviewContext{}, reviewContextError(ReviewValidationFieldContextEntry, ReviewValidationOutOfBounds)
	}

	output := input.Clone()
	for i := range output.Entries {
		if i == activeAction {
			continue
		}
		entry := &output.Entries[i]
		limit, mask := entryLimit(*entry, policy)
		if len(entry.Content) > limit {
			truncated, err := truncateReviewContextContent(entry.Content, limit)
			if err != nil {
				return ReviewContext{}, err
			}
			entry.Content = truncated
			entry.Truncated = true
			output.Truncation.Applied |= mask
			if materialReviewContextKind(entry.Kind) {
				output.Truncation.Material |= mask
			}
		}
		if len(entry.Content) > policy.MaxBlockBytes {
			truncated, err := truncateReviewContextContent(entry.Content, policy.MaxBlockBytes)
			if err != nil {
				return ReviewContext{}, err
			}
			entry.Content = truncated
			entry.Truncated = true
			output.Truncation.Applied |= ReviewTruncationBlock
			if materialReviewContextKind(entry.Kind) {
				output.Truncation.Material |= ReviewTruncationBlock
			}
		}
	}
	boundedOutput, err := applyReviewContextBudgets(
		output,
		currentUser,
		activeAction,
		policy,
	)
	if err != nil {
		return ReviewContext{}, err
	}
	return boundedOutput, nil
}

func validateReviewContextRoot(input ReviewContext, policy ReviewContextPolicy) error {
	if input.Coordinates.SessionID.IsZero() ||
		input.Coordinates.LoopID.IsZero() ||
		input.Coordinates.TurnID.IsZero() ||
		input.Coordinates.StepID.IsZero() {
		return reviewContextError(ReviewValidationFieldContext, ReviewValidationRequired)
	}
	text := []string{
		input.ContextRevision,
		input.WorkspaceRoot,
		input.WorkingDirectory,
		input.RetryReason,
		input.SecurityCeiling,
		input.GatePolicyRevision,
		policy.Revision,
	}
	for _, value := range text {
		if !utf8.ValidString(value) {
			return reviewContextError(ReviewValidationFieldContext, ReviewValidationInvalid)
		}
	}
	if input.ContextRevision == "" ||
		input.WorkspaceRoot == "" ||
		input.WorkingDirectory == "" ||
		input.SecurityCeiling == "" ||
		input.GatePolicyRevision == "" ||
		policy.Revision == "" {
		return reviewContextError(ReviewValidationFieldContext, ReviewValidationRequired)
	}
	if !cleanAbsolutePath(input.WorkspaceRoot) || !cleanAbsolutePath(input.WorkingDirectory) {
		return reviewContextError(ReviewValidationFieldContext, ReviewValidationInvalid)
	}
	relative, err := filepath.Rel(input.WorkspaceRoot, input.WorkingDirectory)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return reviewContextError(ReviewValidationFieldContext, ReviewValidationInvalid)
	}
	if policy.MaxBytes <= 0 ||
		policy.MaxEstimatedTokens <= 0 ||
		policy.MaxEntries <= 0 ||
		policy.MaxUserEntryBytes <= 0 ||
		policy.MaxAgentEntryBytes <= 0 ||
		policy.MaxToolEntryBytes <= 0 ||
		policy.MaxBlockBytes <= 0 ||
		policy.MaxActiveActionBytes <= 0 {
		return reviewContextError(ReviewValidationFieldContextPolicy, ReviewValidationOutOfBounds)
	}
	if input.Truncation != (ReviewTruncation{}) {
		return reviewContextError(ReviewValidationFieldContext, ReviewValidationInvalid)
	}
	return nil
}

func validReviewContextPair(origin ReviewContextOrigin, kind ReviewContextKind) bool {
	switch kind {
	case ReviewContextKindUserMessage:
		return origin == ReviewContextOriginUser
	case ReviewContextKindAssistantMessage, ReviewContextKindAssistantToolRequest:
		return origin == ReviewContextOriginAssistant
	case ReviewContextKindToolResult:
		return origin == ReviewContextOriginTool
	case ReviewContextKindRuntimeContext:
		return origin == ReviewContextOriginRuntime
	case ReviewContextKindExternalContent:
		return origin == ReviewContextOriginExternal
	case ReviewContextKindOmission:
		return origin == ReviewContextOriginOmission
	default:
		return false
	}
}

func cleanAbsolutePath(path string) bool {
	return filepath.IsAbs(path) && filepath.Clean(path) == path
}

func reviewContextError(field ReviewValidationField, reason ReviewValidationReason) error {
	return &ReviewValidationError{Field: field, Reason: reason}
}

func entryLimit(entry ReviewContextEntry, policy ReviewContextPolicy) (int, ReviewTruncationMask) {
	switch entry.Kind {
	case ReviewContextKindUserMessage:
		return policy.MaxUserEntryBytes, ReviewTruncationUserEntry
	case ReviewContextKindAssistantMessage, ReviewContextKindAssistantToolRequest:
		return policy.MaxAgentEntryBytes, ReviewTruncationAssistantEntry
	case ReviewContextKindToolResult:
		return policy.MaxToolEntryBytes, ReviewTruncationToolEntry
	default:
		return policy.MaxBlockBytes, ReviewTruncationBlock
	}
}

func materialReviewContextKind(kind ReviewContextKind) bool {
	switch kind {
	case ReviewContextKindUserMessage,
		ReviewContextKindToolResult,
		ReviewContextKindRuntimeContext,
		ReviewContextKindExternalContent,
		ReviewContextKindAssistantToolRequest:
		return true
	default:
		return false
	}
}

func truncateReviewContextContent(content string, limit int) (string, error) {
	if len(content) <= limit {
		return content, nil
	}
	available := limit - len(reviewContextTruncationMarker)
	first, firstSize := utf8.DecodeRuneInString(content)
	last, lastSize := utf8.DecodeLastRuneInString(content)
	if first == utf8.RuneError && firstSize == 0 || last == utf8.RuneError && lastSize == 0 ||
		available < firstSize+lastSize || firstSize+lastSize >= len(content) {
		return "", reviewContextError(ReviewValidationFieldContextPolicy, ReviewValidationOutOfBounds)
	}

	prefixBudget := available / 2
	if prefixBudget < firstSize {
		prefixBudget = firstSize
	}
	if maximumPrefix := available - lastSize; prefixBudget > maximumPrefix {
		prefixBudget = maximumPrefix
	}
	prefixEnd := 0
	for prefixEnd < len(content)-lastSize {
		_, size := utf8.DecodeRuneInString(content[prefixEnd:])
		if prefixEnd+size > prefixBudget {
			break
		}
		prefixEnd += size
	}
	if prefixEnd == 0 {
		prefixEnd = firstSize
	}

	suffixBudget := available - prefixEnd
	suffixStart := len(content)
	for suffixStart > prefixEnd {
		_, size := utf8.DecodeLastRuneInString(content[:suffixStart])
		if len(content)-suffixStart+size > suffixBudget {
			break
		}
		suffixStart -= size
	}
	if suffixStart == len(content) {
		suffixStart = len(content) - lastSize
	}
	if prefixEnd > suffixStart ||
		prefixEnd+len(content)-suffixStart > available {
		return "", reviewContextError(ReviewValidationFieldContextPolicy, ReviewValidationOutOfBounds)
	}
	return content[:prefixEnd] + reviewContextTruncationMarker + content[suffixStart:], nil
}

func applyReviewContextBudgets(
	context ReviewContext,
	currentUser int,
	activeAction int,
	policy ReviewContextPolicy,
) (ReviewContext, error) {
	initialFailures, err := reviewContextBudgetFailures(context, policy)
	if err != nil {
		return ReviewContext{}, err
	}
	if initialFailures == 0 {
		return context, nil
	}

	keep := make([]bool, len(context.Entries))
	keep[currentUser] = true
	keep[activeAction] = true
	requiredBytes, ok := checkedReviewContextAdd(
		len(context.Entries[currentUser].Content),
		len(context.Entries[activeAction].Content),
	)
	if !ok {
		return ReviewContext{}, reviewContextError(
			ReviewValidationFieldContextPolicy,
			ReviewValidationOutOfBounds,
		)
	}
	totalContentBytes, err := reviewContextContentBytes(context.Entries)
	if err != nil {
		return ReviewContext{}, err
	}
	omittedEntries := len(context.Entries) - 2
	omittedBytes := totalContentBytes - requiredBytes
	if omittedEntries == 0 {
		return ReviewContext{}, reviewContextError(
			ReviewValidationFieldContextPolicy,
			ReviewValidationOutOfBounds,
		)
	}

	marker := reviewContextOmissionMarker(omittedEntries, omittedBytes)
	if len(marker) > policy.MaxBlockBytes {
		return ReviewContext{}, reviewContextError(
			ReviewValidationFieldContextPolicy,
			ReviewValidationOutOfBounds,
		)
	}
	applied := initialFailures
	base := reviewContextBudgetCandidate(
		context,
		keep,
		marker,
		applied,
		omittedEntries,
		omittedBytes,
	)
	baseFailures, err := reviewContextBudgetFailures(base, policy)
	if err != nil || baseFailures != 0 {
		return ReviewContext{}, reviewContextError(
			ReviewValidationFieldContextPolicy,
			ReviewValidationOutOfBounds,
		)
	}

	for i := len(context.Entries) - 1; i >= 0; i-- {
		if keep[i] {
			continue
		}
		candidateOmittedEntries := omittedEntries - 1
		if candidateOmittedEntries <= 0 {
			break
		}
		candidateOmittedBytes := omittedBytes - len(context.Entries[i].Content)
		candidateMarker := reviewContextOmissionMarker(candidateOmittedEntries, candidateOmittedBytes)
		if len(candidateMarker) > policy.MaxBlockBytes {
			applied |= ReviewTruncationBlock
			break
		}
		candidateKeep := append([]bool(nil), keep...)
		candidateKeep[i] = true
		candidate := reviewContextBudgetCandidate(
			context,
			candidateKeep,
			candidateMarker,
			applied,
			candidateOmittedEntries,
			candidateOmittedBytes,
		)
		failures, candidateErr := reviewContextBudgetFailures(candidate, policy)
		if candidateErr != nil {
			return ReviewContext{}, candidateErr
		}
		if failures != 0 {
			applied |= failures
			break
		}
		keep = candidateKeep
		omittedEntries = candidateOmittedEntries
		omittedBytes = candidateOmittedBytes
		marker = candidateMarker
	}

	if omittedEntries <= 0 {
		return ReviewContext{}, reviewContextError(
			ReviewValidationFieldContextPolicy,
			ReviewValidationInvalid,
		)
	}
	output := reviewContextBudgetCandidate(
		context,
		keep,
		marker,
		applied,
		omittedEntries,
		omittedBytes,
	)
	finalFailures, err := reviewContextBudgetFailures(output, policy)
	if err != nil || finalFailures != 0 {
		return ReviewContext{}, reviewContextError(
			ReviewValidationFieldContextPolicy,
			ReviewValidationOutOfBounds,
		)
	}
	return output, nil
}

func reviewContextBudgetFailures(
	context ReviewContext,
	policy ReviewContextPolicy,
) (ReviewTruncationMask, error) {
	var failures ReviewTruncationMask
	if len(context.Entries) > policy.MaxEntries {
		failures |= ReviewTruncationEntryCount
	}
	encodedSize, err := permissionReviewContextEncodedSize(context)
	if err != nil {
		return 0, err
	}
	if encodedSize > policy.MaxBytes {
		failures |= ReviewTruncationTotalBytes
	}
	contentBytes, err := reviewContextContentBytes(context.Entries)
	if err != nil {
		return 0, err
	}
	// Estimated tokens are deliberately deterministic: ceil(all entry content
	// bytes / 4). This is a stable bound, not a model-specific tokenizer.
	estimatedTokens := contentBytes / 4
	if contentBytes%4 != 0 {
		if estimatedTokens == math.MaxInt {
			return 0, reviewContextError(
				ReviewValidationFieldContextPolicy,
				ReviewValidationOutOfBounds,
			)
		}
		estimatedTokens++
	}
	if estimatedTokens > policy.MaxEstimatedTokens {
		failures |= ReviewTruncationEstimatedTokens
	}
	return failures, nil
}

func reviewContextContentBytes(entries []ReviewContextEntry) (int, error) {
	total := 0
	for i := range entries {
		var ok bool
		total, ok = checkedReviewContextAdd(total, len(entries[i].Content))
		if !ok {
			return 0, reviewContextError(
				ReviewValidationFieldContextPolicy,
				ReviewValidationOutOfBounds,
			)
		}
	}
	return total, nil
}

func reviewContextOmissionMarker(entries int, bytes int) string {
	return "omitted_entries=" + strconv.Itoa(entries) + " omitted_bytes=" + strconv.Itoa(bytes)
}

func reviewContextBudgetCandidate(
	context ReviewContext,
	keep []bool,
	marker string,
	applied ReviewTruncationMask,
	omittedEntries int,
	omittedBytes int,
) ReviewContext {
	output := context.Clone()
	output.Entries = make([]ReviewContextEntry, 0, len(context.Entries)-omittedEntries+1)
	markerInserted := false
	for i := range context.Entries {
		if keep[i] {
			output.Entries = append(output.Entries, context.Entries[i])
			continue
		}
		if !markerInserted {
			output.Entries = append(output.Entries, ReviewContextEntry{
				Origin:  ReviewContextOriginOmission,
				Kind:    ReviewContextKindOmission,
				Content: marker,
			})
			markerInserted = true
		}
	}
	output.Truncation.Applied |= applied
	// V1 omission carries no kind inventory. Every exercised omission budget is
	// therefore material so cleared metadata can never manufacture eligibility.
	output.Truncation.Material |= applied
	output.Truncation.OmittedEntries = omittedEntries
	output.Truncation.OmittedBytes = omittedBytes
	return output
}

func permissionReviewContextEncodedSize(context ReviewContext) (int, error) {
	data, err := json.Marshal(permissionReviewContextToWire(context))
	if err != nil || len(data) > MaxPermissionReviewSubjectWireBytes {
		return 0, reviewContextError(
			ReviewValidationFieldContextPolicy,
			ReviewValidationOutOfBounds,
		)
	}
	return len(data), nil
}

func checkedReviewContextAdd(left int, right int) (int, bool) {
	if left < 0 || right < 0 || left > math.MaxInt-right {
		return 0, false
	}
	return left + right, true
}
