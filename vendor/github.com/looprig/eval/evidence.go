package eval

import (
	"strconv"
	"time"
	"unicode/utf8"

	"github.com/looprig/core/content"
)

// This file declares Evidence: the tagged, typed union of operational and
// semantic facts an evaluator can attach to an observation or assessment. Every
// distinct fact is a concrete payload type; Evidence carries exactly one such
// payload matching its Kind. Sensitive material — conversation excerpts, tool
// arguments, judge or error explanations — is only ever representable through a
// bounded redacted excerpt, a content hash, a classification enum, or a byte
// count. Raw untrusted content never occupies an unbounded field that could
// reach a report label.
//
// Phase-1 kinds only: conversation excerpt, message-index reference, timing,
// model usage, tool operation, structured-output error, and evaluator
// diagnostic. HTTP, DNS, process, file, and sandbox variants are added later,
// alongside the adapters that first produce and consume them.

// Byte bounds for the sensitive-content fields. They cap redacted excerpts and
// content hashes so a misused field cannot balloon a report or sink. They are
// byte counts, not rune counts.
const (
	// MaxExcerptBytes bounds a RedactedExcerpt in UTF-8 bytes.
	MaxExcerptBytes = 512
	// MaxHashBytes bounds a ContentHash in bytes.
	MaxHashBytes = 128
	// MaxIDBytes bounds a free-form correlation identifier in bytes.
	MaxIDBytes = 256
)

// RedactedExcerpt is a bounded, caller-redacted snippet of otherwise untrusted
// content. It is the only textual representation of conversation or judge text
// permitted on evidence; callers are responsible for redacting before
// construction, and Validate enforces the length bound and UTF-8 validity so a
// hostile or oversized value cannot pass through. The empty value is valid: a
// hash or classification may carry the correlation instead.
type RedactedExcerpt string

// Validate reports whether x is within bounds and valid UTF-8.
func (x RedactedExcerpt) Validate() error {
	if len(x) > MaxExcerptBytes {
		return &ValidationError{Field: "RedactedExcerpt", Reason: "exceeds " + strconv.Itoa(MaxExcerptBytes) + " bytes"}
	}
	if !utf8.ValidString(string(x)) {
		return &ValidationError{Field: "RedactedExcerpt", Reason: "must be valid UTF-8"}
	}
	return nil
}

// ContentHash is a safe, correlatable digest (for example "sha256:...") of
// sensitive content. It is safe to render in reports and sinks. The empty value
// is valid: hashing is optional.
type ContentHash string

// Validate reports whether h is within bounds and valid UTF-8.
func (h ContentHash) Validate() error {
	if len(h) > MaxHashBytes {
		return &ValidationError{Field: "ContentHash", Reason: "exceeds " + strconv.Itoa(MaxHashBytes) + " bytes"}
	}
	if !utf8.ValidString(string(h)) {
		return &ValidationError{Field: "ContentHash", Reason: "must be valid UTF-8"}
	}
	return nil
}

// EvidenceID identifies one evidence entry within a trace. IDs must be unique
// within a trace so references resolve unambiguously. A valid EvidenceID is
// non-empty, valid UTF-8, and no longer than MaxIDBytes bytes.
type EvidenceID string

// Validate reports whether id is a well-formed EvidenceID.
func (id EvidenceID) Validate() error {
	return validateIdentifier("EvidenceID", string(id), MaxIDBytes)
}

// EvidenceKind is the discriminator of the Evidence union. There is no valid
// zero value: an unset kind does not validate, so evidence with no declared
// shape can never be silently accepted.
type EvidenceKind string

const (
	// EvidenceConversationExcerpt references a message by index and carries a
	// redacted excerpt and/or hash of its content.
	EvidenceConversationExcerpt EvidenceKind = "conversation_excerpt"
	// EvidenceMessageIndex is a bare reference to a message by index.
	EvidenceMessageIndex EvidenceKind = "message_index"
	// EvidenceTiming records how long a step took.
	EvidenceTiming EvidenceKind = "timing"
	// EvidenceUsage records model token usage.
	EvidenceUsage EvidenceKind = "usage"
	// EvidenceToolOperation records a tool invocation as safe metadata: name,
	// hashed/counted arguments, and a result classification.
	EvidenceToolOperation EvidenceKind = "tool_operation"
	// EvidenceStructuredError records a structured-output validation failure as a
	// classification, never as inferred free text.
	EvidenceStructuredError EvidenceKind = "structured_output_error"
	// EvidenceDiagnostic records an evaluator's own diagnostic.
	EvidenceDiagnostic EvidenceKind = "evaluator_diagnostic"
)

// Validate reports whether k is a known EvidenceKind. The offending token is
// withheld from the diagnostic because it may be untrusted.
func (k EvidenceKind) Validate() error {
	switch k {
	case EvidenceConversationExcerpt, EvidenceMessageIndex, EvidenceTiming,
		EvidenceUsage, EvidenceToolOperation, EvidenceStructuredError, EvidenceDiagnostic:
		return nil
	default:
		return &InvalidEnumError{Enum: "EvidenceKind"}
	}
}

// Evidence is a tagged union: Kind selects exactly one non-nil payload pointer.
// Validate enforces that invariant. Representing the union as a struct with one
// optional pointer per variant keeps every payload strictly typed and the whole
// value trivially comparable to nil, JSON-encodable, and table-testable.
type Evidence struct {
	ID   EvidenceID
	Kind EvidenceKind

	// Exactly one of the following is non-nil, matching Kind.
	ConversationExcerpt *ConversationExcerpt
	MessageIndex        *MessageIndexRef
	Timing              *TimingEvidence
	Usage               *UsageEvidence
	ToolOperation       *ToolOperationEvidence
	StructuredError     *StructuredOutputError
	Diagnostic          *DiagnosticEvidence
}

// ConversationExcerpt references a message by index and carries a redacted,
// bounded view of its content plus an optional correlatable hash.
type ConversationExcerpt struct {
	MessageIndex int
	Role         content.Role
	Hash         ContentHash
	Redacted     RedactedExcerpt
}

func (c *ConversationExcerpt) validate() error {
	if c.MessageIndex < 0 {
		return &ValidationError{Field: "ConversationExcerpt.MessageIndex", Reason: "must not be negative"}
	}
	if err := c.Hash.Validate(); err != nil {
		return err
	}
	return c.Redacted.Validate()
}

// MessageIndexRef is a bare reference to a message by index.
type MessageIndexRef struct {
	Index int
}

func (m *MessageIndexRef) validate() error {
	if m.Index < 0 {
		return &ValidationError{Field: "MessageIndexRef.Index", Reason: "must not be negative"}
	}
	return nil
}

// TimingEvidence records the duration of a step. Duration is a safe scalar.
type TimingEvidence struct {
	Label    Name
	Duration time.Duration
}

func (t *TimingEvidence) validate() error {
	if t.Duration < 0 {
		return &ValidationError{Field: "TimingEvidence.Duration", Reason: "must not be negative"}
	}
	// Label is optional context; validate only when supplied.
	if t.Label != "" {
		return t.Label.Validate()
	}
	return nil
}

// UsageEvidence records model token usage. All fields are safe counts.
type UsageEvidence struct {
	Model Revision
	Usage content.Usage
}

func (u *UsageEvidence) validate() error {
	if u.Model != "" {
		if err := u.Model.Validate(); err != nil {
			return err
		}
	}
	return u.Usage.Validate()
}

// ToolOperationEvidence records a tool invocation as safe metadata only. Tool
// arguments and results are sensitive, so they are represented by a hash and
// byte counts — never stored raw on this value.
type ToolOperationEvidence struct {
	ToolName    Name
	ToolUseID   string
	ArgsHash    ContentHash
	ArgsBytes   int
	ResultBytes int
	IsError     bool
}

func (o *ToolOperationEvidence) validate() error {
	if err := o.ToolName.Validate(); err != nil {
		return err
	}
	if o.ArgsBytes < 0 {
		return &ValidationError{Field: "ToolOperationEvidence.ArgsBytes", Reason: "must not be negative"}
	}
	if o.ResultBytes < 0 {
		return &ValidationError{Field: "ToolOperationEvidence.ResultBytes", Reason: "must not be negative"}
	}
	return o.ArgsHash.Validate()
}

// StructuredErrorReason classifies why a structured-output response failed
// validation. There is no valid zero value: an unclassified failure is rejected
// so a bare error can never masquerade as a known, benign one.
type StructuredErrorReason string

const (
	// StructuredErrorInvalidJSON: the response was not valid JSON.
	StructuredErrorInvalidJSON StructuredErrorReason = "invalid_json"
	// StructuredErrorSchemaMismatch: the response did not match the schema shape.
	StructuredErrorSchemaMismatch StructuredErrorReason = "schema_mismatch"
	// StructuredErrorMissingField: a required field was absent.
	StructuredErrorMissingField StructuredErrorReason = "missing_field"
	// StructuredErrorOutOfRange: a value fell outside its permitted range.
	StructuredErrorOutOfRange StructuredErrorReason = "out_of_range"
	// StructuredErrorEmptyOutput: the model produced no terminal output.
	StructuredErrorEmptyOutput StructuredErrorReason = "empty_output"
)

// Validate reports whether r is a known StructuredErrorReason.
func (r StructuredErrorReason) Validate() error {
	switch r {
	case StructuredErrorInvalidJSON, StructuredErrorSchemaMismatch,
		StructuredErrorMissingField, StructuredErrorOutOfRange, StructuredErrorEmptyOutput:
		return nil
	default:
		return &InvalidEnumError{Enum: "StructuredErrorReason"}
	}
}

// StructuredOutputError records a structured-output validation failure as a
// classification plus an optional correlatable hash of the offending detail.
// The raw model text is never stored.
type StructuredOutputError struct {
	Schema     Revision
	Reason     StructuredErrorReason
	DetailHash ContentHash
}

func (s *StructuredOutputError) validate() error {
	if err := s.Reason.Validate(); err != nil {
		return err
	}
	if s.Schema != "" {
		if err := s.Schema.Validate(); err != nil {
			return err
		}
	}
	return s.DetailHash.Validate()
}

// DiagnosticEvidence records an evaluator's own diagnostic: a safe code, a
// severity, and a bounded redacted message. Because a diagnostic may quote a
// judge or an error, its Message is a RedactedExcerpt — bounded and never raw.
type DiagnosticEvidence struct {
	Code     Name
	Severity Severity
	Message  RedactedExcerpt
}

func (d *DiagnosticEvidence) validate() error {
	if err := d.Code.Validate(); err != nil {
		return err
	}
	if err := d.Severity.Validate(); err != nil {
		return err
	}
	return d.Message.Validate()
}

// Validate reports whether e is a well-formed evidence value: a valid ID, a
// known Kind, exactly one payload matching that Kind, and a valid payload. It
// checks that message indexes are non-negative but not their upper bound, which
// depends on the conversation; Observation.Validate enforces the upper bound.
func (e Evidence) Validate() error {
	if err := e.ID.Validate(); err != nil {
		return err
	}
	if err := e.Kind.Validate(); err != nil {
		return err
	}
	switch e.payloadCount() {
	case 1:
		// ok
	case 0:
		return &EvidencePayloadError{Reason: payloadReasonNone}
	default:
		return &EvidencePayloadError{Reason: payloadReasonMultiple}
	}
	return e.validatePayload()
}

// payloadCount reports how many payload pointers are non-nil.
func (e Evidence) payloadCount() int {
	n := 0
	if e.ConversationExcerpt != nil {
		n++
	}
	if e.MessageIndex != nil {
		n++
	}
	if e.Timing != nil {
		n++
	}
	if e.Usage != nil {
		n++
	}
	if e.ToolOperation != nil {
		n++
	}
	if e.StructuredError != nil {
		n++
	}
	if e.Diagnostic != nil {
		n++
	}
	return n
}

// validatePayload dispatches to the payload named by Kind. It assumes exactly
// one payload is set (checked by Validate) and rejects a set payload that does
// not match Kind.
func (e Evidence) validatePayload() error {
	switch e.Kind {
	case EvidenceConversationExcerpt:
		if e.ConversationExcerpt == nil {
			return &EvidencePayloadError{Reason: payloadReasonMismatch}
		}
		return e.ConversationExcerpt.validate()
	case EvidenceMessageIndex:
		if e.MessageIndex == nil {
			return &EvidencePayloadError{Reason: payloadReasonMismatch}
		}
		return e.MessageIndex.validate()
	case EvidenceTiming:
		if e.Timing == nil {
			return &EvidencePayloadError{Reason: payloadReasonMismatch}
		}
		return e.Timing.validate()
	case EvidenceUsage:
		if e.Usage == nil {
			return &EvidencePayloadError{Reason: payloadReasonMismatch}
		}
		return e.Usage.validate()
	case EvidenceToolOperation:
		if e.ToolOperation == nil {
			return &EvidencePayloadError{Reason: payloadReasonMismatch}
		}
		return e.ToolOperation.validate()
	case EvidenceStructuredError:
		if e.StructuredError == nil {
			return &EvidencePayloadError{Reason: payloadReasonMismatch}
		}
		return e.StructuredError.validate()
	case EvidenceDiagnostic:
		if e.Diagnostic == nil {
			return &EvidencePayloadError{Reason: payloadReasonMismatch}
		}
		return e.Diagnostic.validate()
	default:
		// Unreachable: Kind was validated above. Fail secure.
		return &InvalidEnumError{Enum: "EvidenceKind"}
	}
}

// messageIndexWithin reports the conversation-relative message index this
// evidence addresses, or (0, false) when it addresses no message. It is used by
// Observation.Validate to enforce the upper index bound.
func (e Evidence) messageIndexWithin() (int, bool) {
	switch e.Kind {
	case EvidenceConversationExcerpt:
		if e.ConversationExcerpt != nil {
			return e.ConversationExcerpt.MessageIndex, true
		}
	case EvidenceMessageIndex:
		if e.MessageIndex != nil {
			return e.MessageIndex.Index, true
		}
	}
	return 0, false
}

// EvidenceRef references evidence by EvidenceID and/or a message index. It is
// used by Operations (and later Findings) to point at supporting evidence
// without duplicating it. At least one of the two must be set.
type EvidenceRef struct {
	Evidence     EvidenceID
	MessageIndex *int
}

// validateShape checks the reference is well-formed in isolation: at least one
// target is set, a set EvidenceID is well-formed, and a set message index is
// non-negative. Existence of the target (the EvidenceID resolves, the index is
// within the conversation) is checked by Observation.Validate.
func (r EvidenceRef) validateShape() error {
	hasEvidence := r.Evidence != ""
	hasMessage := r.MessageIndex != nil
	if !hasEvidence && !hasMessage {
		return &ValidationError{Field: "EvidenceRef", Reason: "must reference evidence or a message"}
	}
	if hasEvidence {
		if err := r.Evidence.Validate(); err != nil {
			return err
		}
	}
	if hasMessage && *r.MessageIndex < 0 {
		return &ValidationError{Field: "EvidenceRef.MessageIndex", Reason: "must not be negative"}
	}
	return nil
}
