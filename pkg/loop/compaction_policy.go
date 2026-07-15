package loop

import (
	"crypto/sha256"
	"encoding/json"
	"time"

	"github.com/looprig/core/content"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/hustle"
	"github.com/looprig/inference"
)

// CounterPolicy selects the count qualities automatic compaction may trust.
type CounterPolicy uint8

const (
	CounterPolicyUnknown CounterPolicy = iota
	CounterPolicyRequireExact
	CounterPolicyAllowConservative
)

// CompactionPolicy is the complete explicit policy installed by WithCompaction.
// Harness supplies no timeout or threshold defaults.
type CompactionPolicy struct {
	Automatic        bool
	CounterPolicy    CounterPolicy
	CompactAt        event.BasisPoints
	RearmBelow       event.BasisPoints
	ReservedOutput   content.TokenCount
	SafetyMargin     content.TokenCount
	MaxSummaryTokens content.TokenCount
	CountTimeout     time.Duration
	Hustle           hustle.Name
}

// CompactionPolicyField identifies one rejected policy dimension.
type CompactionPolicyField string

const (
	CompactionFieldCounterPolicy    CompactionPolicyField = "CounterPolicy"
	CompactionFieldCompactAt        CompactionPolicyField = "CompactAt"
	CompactionFieldRearmBelow       CompactionPolicyField = "RearmBelow"
	CompactionFieldReservedOutput   CompactionPolicyField = "ReservedOutput"
	CompactionFieldSafetyMargin     CompactionPolicyField = "SafetyMargin"
	CompactionFieldMaxSummaryTokens CompactionPolicyField = "MaxSummaryTokens"
	CompactionFieldCountTimeout     CompactionPolicyField = "CountTimeout"
	CompactionFieldHustle           CompactionPolicyField = "Hustle"
)

// CompactionPolicyError reports invalid explicit compaction configuration.
type CompactionPolicyError struct {
	Field CompactionPolicyField
	Cause error
}

func (e *CompactionPolicyError) Error() string {
	message := "loop: invalid compaction policy field " + string(e.Field)
	if e.Cause != nil {
		return message + ": " + e.Cause.Error()
	}
	return message
}

func (e *CompactionPolicyError) Unwrap() error { return e.Cause }

// Validate checks the policy against already-validated, I/O-free counter
// metadata. It never calls CountContext.
func (p CompactionPolicy) Validate(capability inference.CounterCapability) error {
	if p.ReservedOutput == 0 {
		return &CompactionPolicyError{Field: CompactionFieldReservedOutput}
	}
	if p.MaxSummaryTokens == 0 {
		return &CompactionPolicyError{Field: CompactionFieldMaxSummaryTokens}
	}
	if p.CountTimeout <= 0 {
		return &CompactionPolicyError{Field: CompactionFieldCountTimeout}
	}
	if err := p.Hustle.Validate(); err != nil {
		return &CompactionPolicyError{Field: CompactionFieldHustle, Cause: err}
	}
	if capability.Quality == inference.CountQualityHeuristicEstimate && p.SafetyMargin == 0 {
		return &CompactionPolicyError{Field: CompactionFieldSafetyMargin}
	}
	if !p.Automatic {
		return nil
	}
	if p.RearmBelow == 0 || p.RearmBelow >= p.CompactAt {
		return &CompactionPolicyError{Field: CompactionFieldRearmBelow}
	}
	if p.CompactAt >= event.FullScaleBasisPoints {
		return &CompactionPolicyError{Field: CompactionFieldCompactAt}
	}
	switch p.CounterPolicy {
	case CounterPolicyRequireExact:
		if capability.Quality != inference.CountQualityExactProvider && capability.Quality != inference.CountQualityExactLocal {
			return &CompactionPolicyError{Field: CompactionFieldCounterPolicy}
		}
	case CounterPolicyAllowConservative:
		if capability.Quality != inference.CountQualityExactProvider && capability.Quality != inference.CountQualityExactLocal && capability.Quality != inference.CountQualityHeuristicEstimate {
			return &CompactionPolicyError{Field: CompactionFieldCounterPolicy}
		}
	default:
		return &CompactionPolicyError{Field: CompactionFieldCounterPolicy}
	}
	return nil
}

// ContextTransportBindingError reports a live or predeclared model that would
// replace the fixed provider transport used by the bound client and counter.
type ContextTransportBindingError struct {
	Field     string
	Bound     string
	Candidate string
}

func (e *ContextTransportBindingError) Error() string {
	return "loop: context model changes fixed transport field " + e.Field
}

func validateContextTransportBinding(bound, candidate inference.Model) error {
	if bound.Provider != candidate.Provider {
		return &ContextTransportBindingError{Field: "Provider", Bound: string(bound.Provider), Candidate: string(candidate.Provider)}
	}
	if bound.APIFormat != candidate.APIFormat {
		return &ContextTransportBindingError{Field: "APIFormat", Bound: string(bound.APIFormat), Candidate: string(candidate.APIFormat)}
	}
	if bound.BaseURL != candidate.BaseURL {
		return &ContextTransportBindingError{Field: "BaseURL", Bound: bound.BaseURL, Candidate: candidate.BaseURL}
	}
	return nil
}

// RequestFingerprintInput is the complete secret-free request-shape projection
// used to identify one measurement. Revisions identify opaque prompt/tool/runtime
// context producers; model and capabilities are included in full.
type RequestFingerprintInput struct {
	SystemRevision         string
	ToolPolicyRevision     string
	Model                  inference.Model
	Basis                  event.ContextBasis
	RuntimeContextRevision string
	CounterCapability      inference.CounterCapability
	InferenceCapability    inference.InferenceCapability
}

// RequestFingerprintError reports an invalid projection or marshal defect.
type RequestFingerprintError struct {
	Field string
	Cause error
}

func (e *RequestFingerprintError) Error() string {
	message := "loop: invalid request fingerprint input " + e.Field
	if e.Cause != nil {
		return message + ": " + e.Cause.Error()
	}
	return message
}

func (e *RequestFingerprintError) Unwrap() error { return e.Cause }

// RequestFingerprint returns the deterministic SHA-256 identity of all
// secret-free request-shape inputs that affect counting.
func RequestFingerprint(input RequestFingerprintInput) ([32]byte, error) {
	if input.SystemRevision == "" {
		return [32]byte{}, &RequestFingerprintError{Field: "SystemRevision"}
	}
	if input.ToolPolicyRevision == "" {
		return [32]byte{}, &RequestFingerprintError{Field: "ToolPolicyRevision"}
	}
	if input.RuntimeContextRevision == "" {
		return [32]byte{}, &RequestFingerprintError{Field: "RuntimeContextRevision"}
	}
	if err := input.Model.Validate(); err != nil {
		return [32]byte{}, &RequestFingerprintError{Field: "Model", Cause: err}
	}
	if err := input.Model.Key().Validate(); err != nil {
		return [32]byte{}, &RequestFingerprintError{Field: "Model", Cause: err}
	}
	if input.Basis.Revision == 0 || input.Basis.ThroughEventID.IsZero() {
		return [32]byte{}, &RequestFingerprintError{Field: "Basis"}
	}
	if err := input.CounterCapability.Validate(); err != nil {
		return [32]byte{}, &RequestFingerprintError{Field: "CounterCapability", Cause: err}
	}
	if err := input.InferenceCapability.Validate(); err != nil {
		return [32]byte{}, &RequestFingerprintError{Field: "InferenceCapability", Cause: err}
	}
	encoded, err := json.Marshal(input)
	if err != nil {
		return [32]byte{}, &RequestFingerprintError{Field: "Encoding", Cause: err}
	}
	return sha256.Sum256(encoded), nil
}
