package eval

import (
	"math"
	"strconv"
	"time"

	"github.com/looprig/core/content"
)

// This file declares Observation and its trace: the canonical record of one
// evaluated interaction. The conversation is the single source of truth for
// message text, tool calls, tool results, and usage; the trace holds only facts
// the conversation does not carry — correlation identifiers, timing, operation
// status, safe attributes, and typed evidence. Operations never duplicate
// message text; they reference evidence and messages by index instead.
//
// Observation.Validate is the boundary check for an assembled observation. It
// delegates to the leaf Validate methods, then enforces the cross-cutting
// invariants that require the whole value: time ordering, that every message
// index lies within the conversation, that evidence IDs are unique, and that
// every evidence reference resolves. It is read-only and never reorders the
// conversation, operations, or evidence.

// Byte bound for free-form correlation identifiers carried on the trace and
// operations (trace/session/turn/operation IDs). Subject and operation IDs
// reuse MaxIDBytes from evidence.go.
const (
	// MaxAttributeValueBytes bounds a single Attribute value in bytes.
	MaxAttributeValueBytes = 512
	// MaxOperationAttributes bounds how many attributes one Operation may carry.
	// Attributes are a small set of safe key/value facts, not a transcript.
	MaxOperationAttributes = 32
)

// Observation is the canonical record of one evaluated interaction. Conversation
// is the semantic record; Trace holds the operational facts that are not present
// in the conversation. A nil/empty Conversation is a valid empty thread, but any
// message index or range must then be empty too.
//
// Expectation is optional qualification data (see expectation.go). Qualification
// observations carry it; production observations normally omit it. When present
// it is validated by Observation.Validate; a nil Expectation is valid.
type Observation struct {
	Conversation content.AgenticMessages
	Scope        Scope
	Subject      Subject
	Trace        Trace
	Expectation  *Expectation
}

// SubjectKind names what is under evaluation. There is no valid zero value: a
// subject must declare what it is, so an unset kind is rejected and cannot be
// silently treated as, say, a model. The member set covers the Phase-1 targets
// the design names and nothing more.
type SubjectKind string

const (
	// SubjectModel is a bare inference model.
	SubjectModel SubjectKind = "model"
	// SubjectAgent is an agent entry point.
	SubjectAgent SubjectKind = "agent"
	// SubjectPrompt is a prompt or prompt template.
	SubjectPrompt SubjectKind = "prompt"
	// SubjectHTTPEndpoint is an HTTP service under test.
	SubjectHTTPEndpoint SubjectKind = "http_endpoint"
	// SubjectProcess is a local process under test.
	SubjectProcess SubjectKind = "process"
)

// Validate reports whether k is a known SubjectKind. The offending token is
// withheld because it may be untrusted.
func (k SubjectKind) Validate() error {
	switch k {
	case SubjectModel, SubjectAgent, SubjectPrompt, SubjectHTTPEndpoint, SubjectProcess:
		return nil
	default:
		return &InvalidEnumError{Enum: "SubjectKind"}
	}
}

// Subject identifies what produced the observation. ID is a required correlation
// identifier; Name and Revision reuse the domain identity types so the same
// validation rules apply everywhere.
type Subject struct {
	ID       string
	Kind     SubjectKind
	Name     Name
	Revision Revision
}

// Validate reports whether s is a well-formed Subject.
func (s Subject) Validate() error {
	if err := validateIdentifier("Subject.ID", s.ID, MaxIDBytes); err != nil {
		return err
	}
	if err := s.Kind.Validate(); err != nil {
		return err
	}
	if err := s.Name.Validate(); err != nil {
		return err
	}
	return s.Revision.Validate()
}

// MessageRange addresses a contiguous span [Start, Start+Len) of the
// conversation. A zero-length range is permitted at any in-bounds start.
type MessageRange struct {
	Start int
	Len   int
}

// validate reports whether r lies within a conversation of length convLen.
func (r MessageRange) validate(convLen int) error {
	if r.Start < 0 {
		return &ValidationError{Field: "MessageRange.Start", Reason: "must not be negative"}
	}
	if r.Len < 0 {
		return &ValidationError{Field: "MessageRange.Len", Reason: "must not be negative"}
	}
	// Guard against int overflow in Start+Len by comparing against the room left.
	if r.Start > convLen || r.Len > convLen-r.Start {
		// Report the range's exclusive end, but saturate on overflow: with a huge
		// Len, Start+Len would wrap to a negative, misleading index. Either way the
		// boundary is out of [0,convLen); a saturated MaxInt reads as out-of-range
		// without lying about the sign.
		end := r.Start + r.Len
		if end < r.Start {
			end = math.MaxInt
		}
		return &IndexRangeError{Field: "MessageRange", Index: end, Len: convLen}
	}
	return nil
}

// Trace holds facts not already present in the conversation: correlation
// identifiers, timing, the model and prompt revisions in effect, message ranges,
// operations, and typed evidence.
type Trace struct {
	TraceID       string
	SessionID     string
	TurnID        string
	StartedAt     time.Time
	EndedAt       time.Time
	Model         Revision
	Prompt        Revision
	MessageRanges []MessageRange
	Operations    []Operation
	Evidence      []Evidence
}

// validate checks the trace against a conversation of length convLen. Callers
// pass the length rather than the conversation because the trace only ever
// addresses messages by index.
func (t Trace) validate(convLen int) error {
	// Time range: reject End before Start only when both endpoints are set, so a
	// zero (unset) endpoint is never spuriously compared.
	if !t.StartedAt.IsZero() && !t.EndedAt.IsZero() && t.EndedAt.Before(t.StartedAt) {
		return &ValidationError{Field: "Trace", Reason: "EndedAt is before StartedAt"}
	}
	// Model and Prompt are optional context; validate only when supplied.
	if t.Model != "" {
		if err := t.Model.Validate(); err != nil {
			return err
		}
	}
	if t.Prompt != "" {
		if err := t.Prompt.Validate(); err != nil {
			return err
		}
	}
	for _, id := range []struct {
		field string
		value string
	}{{"Trace.TraceID", t.TraceID}, {"Trace.SessionID", t.SessionID}, {"Trace.TurnID", t.TurnID}} {
		if id.value != "" {
			if err := validateIdentifier(id.field, id.value, MaxIDBytes); err != nil {
				return err
			}
		}
	}
	for _, r := range t.MessageRanges {
		if err := r.validate(convLen); err != nil {
			return err
		}
	}
	ids, err := t.validateEvidence(convLen)
	if err != nil {
		return err
	}
	return t.validateOperations(convLen, ids)
}

// validateEvidence validates each evidence entry, rejects duplicate IDs, and
// enforces the upper index bound for message-addressing evidence. It returns the
// set of evidence IDs for operation-reference resolution. Iteration order is not
// modified.
func (t Trace) validateEvidence(convLen int) (map[EvidenceID]struct{}, error) {
	ids := make(map[EvidenceID]struct{}, len(t.Evidence))
	for _, ev := range t.Evidence {
		if err := ev.Validate(); err != nil {
			return nil, err
		}
		if _, dup := ids[ev.ID]; dup {
			return nil, &DuplicateEvidenceError{}
		}
		ids[ev.ID] = struct{}{}
		if idx, ok := ev.messageIndexWithin(); ok {
			if idx >= convLen {
				return nil, &IndexRangeError{Field: "Evidence.MessageIndex", Index: idx, Len: convLen}
			}
		}
	}
	return ids, nil
}

// validateOperations validates each operation and resolves its evidence
// references against the trace's evidence IDs and the conversation length.
func (t Trace) validateOperations(convLen int, ids map[EvidenceID]struct{}) error {
	for _, op := range t.Operations {
		if err := op.Validate(); err != nil {
			return err
		}
		for _, ref := range op.Evidence {
			if ref.Evidence != "" {
				if _, ok := ids[ref.Evidence]; !ok {
					return &UnknownEvidenceError{}
				}
			}
			if ref.MessageIndex != nil && *ref.MessageIndex >= convLen {
				return &IndexRangeError{Field: "EvidenceRef.MessageIndex", Index: *ref.MessageIndex, Len: convLen}
			}
		}
	}
	return nil
}

// OperationKind classifies a timed step in a trace. There is no valid zero
// value: every operation declares what it is.
type OperationKind string

const (
	// OperationInference is a model inference call.
	OperationInference OperationKind = "inference"
	// OperationTool is a tool execution.
	OperationTool OperationKind = "tool"
	// OperationNetwork is a network request.
	OperationNetwork OperationKind = "network"
	// OperationProcess is a process action.
	OperationProcess OperationKind = "process"
	// OperationSandbox is a sandbox decision.
	OperationSandbox OperationKind = "sandbox"
	// OperationStep is any other timed step.
	OperationStep OperationKind = "step"
)

// Validate reports whether k is a known OperationKind.
func (k OperationKind) Validate() error {
	switch k {
	case OperationInference, OperationTool, OperationNetwork,
		OperationProcess, OperationSandbox, OperationStep:
		return nil
	default:
		return &InvalidEnumError{Enum: "OperationKind"}
	}
}

// OperationStatus is the terminal disposition of an operation. There is no valid
// zero value.
type OperationStatus string

const (
	// OperationOK indicates the operation completed successfully.
	OperationOK OperationStatus = "ok"
	// OperationFailed indicates the operation failed.
	OperationFailed OperationStatus = "failed"
	// OperationCancelled indicates the operation was cancelled.
	OperationCancelled OperationStatus = "cancelled"
	// OperationTimedOut indicates the operation exceeded its deadline.
	OperationTimedOut OperationStatus = "timed_out"
)

// Validate reports whether s is a known OperationStatus.
func (s OperationStatus) Validate() error {
	switch s {
	case OperationOK, OperationFailed, OperationCancelled, OperationTimedOut:
		return nil
	default:
		return &InvalidEnumError{Enum: "OperationStatus"}
	}
}

// ErrorClass classifies an operation's failure. Unlike the other enums it has a
// valid zero value: the empty string means "no error classification", which is
// the correct state for a successful operation. A non-empty value must be a
// known member.
type ErrorClass string

const (
	// ErrorTimeout: the operation exceeded a deadline.
	ErrorTimeout ErrorClass = "timeout"
	// ErrorCancelled: the operation was cancelled.
	ErrorCancelled ErrorClass = "cancelled"
	// ErrorRateLimited: the operation was rate limited.
	ErrorRateLimited ErrorClass = "rate_limited"
	// ErrorInvalidInput: the operation received invalid input.
	ErrorInvalidInput ErrorClass = "invalid_input"
	// ErrorUnavailable: a dependency was unavailable.
	ErrorUnavailable ErrorClass = "unavailable"
	// ErrorInternal: an internal error occurred.
	ErrorInternal ErrorClass = "internal"
)

// Validate reports whether c is a known ErrorClass. The empty value is valid and
// means "no classification".
func (c ErrorClass) Validate() error {
	switch c {
	case "", ErrorTimeout, ErrorCancelled, ErrorRateLimited,
		ErrorInvalidInput, ErrorUnavailable, ErrorInternal:
		return nil
	default:
		return &InvalidEnumError{Enum: "ErrorClass"}
	}
}

// Attribute is a single safe key/value fact about an operation. Values are
// bounded and never echoed in diagnostics; callers must not place untrusted
// content, secrets, or PII here.
type Attribute struct {
	Key   Name
	Value string
}

// Validate reports whether a is a well-formed attribute.
func (a Attribute) Validate() error {
	if err := a.Key.Validate(); err != nil {
		return err
	}
	if len(a.Value) > MaxAttributeValueBytes {
		return &ValidationError{Field: "Attribute.Value", Reason: "exceeds " + strconv.Itoa(MaxAttributeValueBytes) + " bytes"}
	}
	return nil
}

// Operation describes one timed step: an inference call, tool execution, network
// request, process action, sandbox decision, or other step. It carries typed
// status, timestamps, parent/child correlation, a small set of safe attributes,
// an optional error classification, and evidence references. It never carries a
// transcript: message text lives only in the conversation.
type Operation struct {
	ID         string
	ParentID   string
	Kind       OperationKind
	Status     OperationStatus
	StartedAt  time.Time
	EndedAt    time.Time
	Attributes []Attribute
	ErrorClass ErrorClass
	Evidence   []EvidenceRef
}

// Validate reports whether o is well-formed in isolation. It does not resolve
// evidence references against a trace or conversation; Trace.validate does that.
func (o Operation) Validate() error {
	if err := validateIdentifier("Operation.ID", o.ID, MaxIDBytes); err != nil {
		return err
	}
	if o.ParentID != "" {
		if err := validateIdentifier("Operation.ParentID", o.ParentID, MaxIDBytes); err != nil {
			return err
		}
	}
	if err := o.Kind.Validate(); err != nil {
		return err
	}
	if err := o.Status.Validate(); err != nil {
		return err
	}
	if err := o.ErrorClass.Validate(); err != nil {
		return err
	}
	if !o.StartedAt.IsZero() && !o.EndedAt.IsZero() && o.EndedAt.Before(o.StartedAt) {
		return &ValidationError{Field: "Operation", Reason: "EndedAt is before StartedAt"}
	}
	if len(o.Attributes) > MaxOperationAttributes {
		return &ValidationError{Field: "Operation.Attributes", Reason: "exceeds " + strconv.Itoa(MaxOperationAttributes) + " attributes"}
	}
	for _, a := range o.Attributes {
		if err := a.Validate(); err != nil {
			return err
		}
	}
	for _, ref := range o.Evidence {
		if err := ref.validateShape(); err != nil {
			return err
		}
	}
	return nil
}

// Validate reports whether the observation is well-formed: a valid scope and
// subject, a trace whose time range, message ranges, evidence, and operation
// references are all consistent with the conversation, and a valid Expectation
// when one is present. It is read-only and preserves the order of the
// conversation, operations, and evidence.
func (o Observation) Validate() error {
	if err := o.Scope.Validate(); err != nil {
		return err
	}
	if err := o.Subject.Validate(); err != nil {
		return err
	}
	if err := o.Trace.validate(len(o.Conversation)); err != nil {
		return err
	}
	// Expectation is optional; validate it only when present. A nil pointer is a
	// valid "no expectation" and Expectation.Validate handles nil defensively.
	return o.Expectation.Validate()
}
