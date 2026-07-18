package eval

import "strconv"

// This file declares Expectation: the optional qualification data attached to a
// scenario or an observation. It records what a correct interaction should and
// must not do — required facts, forbidden actions, expected tool calls, an
// expected structured-output shape, optional reference answers, and an optional
// policy reference. These are author-supplied test-fixture fields, not runtime
// conversation content; even so, every field is a named type, every collection
// is size-bounded, and Validate never echoes a field's value in a diagnostic.
//
// Expectation is optional data. A wholly-empty Expectation is valid: it simply
// asserts nothing. Individual fields are independently optional; a field is
// validated only when populated, and an empty member within a populated
// collection (an empty required fact, an empty forbidden action) is a failure,
// because a blank assertion can never be checked.

// Byte bounds for the free-form (author-supplied) expectation text fields, and
// count bounds for the collections. They reject absurd or hostile fixtures
// before they reach evaluators, reports, or sinks. All are byte or element
// counts, not rune counts.
const (
	// MaxFactBytes bounds a single required Fact in UTF-8 bytes.
	MaxFactBytes = 1024
	// MaxActionNameBytes bounds a single forbidden ActionName in UTF-8 bytes.
	MaxActionNameBytes = 256
	// MaxReferenceAnswerBytes bounds a single ReferenceAnswer in UTF-8 bytes.
	MaxReferenceAnswerBytes = 4096

	// MaxRequiredFacts bounds how many required facts one Expectation may carry.
	MaxRequiredFacts = 64
	// MaxForbiddenActions bounds how many forbidden actions one Expectation may
	// carry.
	MaxForbiddenActions = 64
	// MaxExpectedToolCalls bounds how many tool-call expectations one Expectation
	// may carry.
	MaxExpectedToolCalls = 64
	// MaxReferenceAnswers bounds how many reference answers one Expectation may
	// carry.
	MaxReferenceAnswers = 16
)

// Fact is a single statement a correct interaction must establish or support,
// for example "the invoice is non-refundable". A valid Fact is non-empty, valid
// UTF-8, and within MaxFactBytes. It carries author-supplied domain meaning, so
// it is a named type rather than a bare string.
type Fact string

// Validate reports whether f is a well-formed Fact. Its diagnostic references
// only the field name and bound, never the fact text.
func (f Fact) Validate() error {
	return validateIdentifier("Fact", string(f), MaxFactBytes)
}

// ActionName names an action a correct interaction must not take, for example
// "issue_refund". A valid ActionName is non-empty, valid UTF-8, and within
// MaxActionNameBytes. It is distinct from Name so a forbidden-action list can
// never be confused with an identifier list.
type ActionName string

// Validate reports whether a is a well-formed ActionName.
func (a ActionName) Validate() error {
	return validateIdentifier("ActionName", string(a), MaxActionNameBytes)
}

// ReferenceAnswer is an author-supplied golden answer used by evaluators that
// compare against a reference. A valid ReferenceAnswer is non-empty, valid
// UTF-8, and within MaxReferenceAnswerBytes.
type ReferenceAnswer string

// Validate reports whether r is a well-formed ReferenceAnswer.
func (r ReferenceAnswer) Validate() error {
	return validateIdentifier("ReferenceAnswer", string(r), MaxReferenceAnswerBytes)
}

// ToolCallExpectation asserts that a named tool is invoked a bounded number of
// times. Tool is required. MinCount is the inclusive lower bound and must not be
// negative. MaxCount is an optional inclusive upper bound; when set it must be
// non-negative and no less than MinCount. A nil MaxCount leaves the count
// unbounded above.
type ToolCallExpectation struct {
	Tool     Name
	MinCount int
	MaxCount *int
}

// Validate reports whether t is a well-formed tool-call expectation.
func (t ToolCallExpectation) Validate() error {
	if err := t.Tool.Validate(); err != nil {
		return err
	}
	if t.MinCount < 0 {
		return &ValidationError{Field: "ToolCallExpectation.MinCount", Reason: "must not be negative"}
	}
	if t.MaxCount != nil {
		if *t.MaxCount < 0 {
			return &ValidationError{Field: "ToolCallExpectation.MaxCount", Reason: "must not be negative"}
		}
		if *t.MaxCount < t.MinCount {
			return &ValidationError{Field: "ToolCallExpectation.MaxCount", Reason: "must not be less than MinCount"}
		}
	}
	return nil
}

// StructuredOutputExpectation asserts that the interaction produces a terminal
// structured output conforming to a named schema revision. Schema is required
// (an expectation with no schema asserts nothing and is malformed). Strict
// mirrors the inference OutputSchema.Strict flag: when true the output must
// match the schema exactly.
type StructuredOutputExpectation struct {
	Schema Revision
	Strict bool
}

// Validate reports whether s is a well-formed structured-output expectation.
func (s StructuredOutputExpectation) Validate() error {
	return s.Schema.Validate()
}

// Expectation is optional qualification data describing a correct interaction.
// Every field is independently optional; a wholly-empty Expectation is valid and
// simply asserts nothing.
type Expectation struct {
	// RequiredFacts are statements a correct answer must establish or support.
	RequiredFacts []Fact
	// ForbiddenActions name actions a correct interaction must not take.
	ForbiddenActions []ActionName
	// ExpectedToolCalls constrain which tools are invoked and how often.
	ExpectedToolCalls []ToolCallExpectation
	// StructuredOutput, when set, requires a conforming terminal structured
	// output.
	StructuredOutput *StructuredOutputExpectation
	// ReferenceAnswers are author-supplied golden answers for reference-based
	// evaluators.
	ReferenceAnswers []ReferenceAnswer
	// PolicyRef, when set, references an external policy revision this scenario
	// qualifies against.
	PolicyRef Revision
}

// Validate reports whether e is well-formed. Each populated collection is
// size-bounded and each member is validated; a nil or empty Expectation is
// valid. Diagnostics reference field names and bounds only, never field values.
func (e *Expectation) Validate() error {
	if e == nil {
		return nil
	}
	if err := e.validateFacts(); err != nil {
		return err
	}
	if err := e.validateActions(); err != nil {
		return err
	}
	if err := e.validateToolCalls(); err != nil {
		return err
	}
	if e.StructuredOutput != nil {
		if err := e.StructuredOutput.Validate(); err != nil {
			return err
		}
	}
	if err := e.validateReferenceAnswers(); err != nil {
		return err
	}
	if e.PolicyRef != "" {
		return e.PolicyRef.Validate()
	}
	return nil
}

func (e *Expectation) validateFacts() error {
	if len(e.RequiredFacts) > MaxRequiredFacts {
		return &ValidationError{Field: "Expectation.RequiredFacts", Reason: "exceeds " + strconv.Itoa(MaxRequiredFacts) + " facts"}
	}
	for _, f := range e.RequiredFacts {
		if err := f.Validate(); err != nil {
			return err
		}
	}
	return nil
}

func (e *Expectation) validateActions() error {
	if len(e.ForbiddenActions) > MaxForbiddenActions {
		return &ValidationError{Field: "Expectation.ForbiddenActions", Reason: "exceeds " + strconv.Itoa(MaxForbiddenActions) + " actions"}
	}
	for _, a := range e.ForbiddenActions {
		if err := a.Validate(); err != nil {
			return err
		}
	}
	return nil
}

func (e *Expectation) validateToolCalls() error {
	if len(e.ExpectedToolCalls) > MaxExpectedToolCalls {
		return &ValidationError{Field: "Expectation.ExpectedToolCalls", Reason: "exceeds " + strconv.Itoa(MaxExpectedToolCalls) + " tool calls"}
	}
	for _, c := range e.ExpectedToolCalls {
		if err := c.Validate(); err != nil {
			return err
		}
	}
	return nil
}

func (e *Expectation) validateReferenceAnswers() error {
	if len(e.ReferenceAnswers) > MaxReferenceAnswers {
		return &ValidationError{Field: "Expectation.ReferenceAnswers", Reason: "exceeds " + strconv.Itoa(MaxReferenceAnswers) + " answers"}
	}
	for _, r := range e.ReferenceAnswers {
		if err := r.Validate(); err != nil {
			return err
		}
	}
	return nil
}
