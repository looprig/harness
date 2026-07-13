package hustle

import (
	"encoding/json"

	"github.com/looprig/core/content"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/identity"
)

// RunID identifies one hustle invocation.
type RunID uuid.UUID

// Stage identifies the bounded execution phase in which a hustle failed.
type Stage uint8

const (
	StageUnknown Stage = iota
	StageQueue
	StageModelResolution
	StageInference
	StageOutput
	StageTerminal
	StageFinalization
)

// Valid reports whether the stage is a durable, recognized failure phase.
func (s Stage) Valid() bool { return s >= StageQueue && s <= StageFinalization }

// ReasonCode is the bounded, security-safe classification of a hustle failure.
type ReasonCode uint8

const (
	ReasonUnknown ReasonCode = iota
	ReasonRejected
	ReasonCanceled
	ReasonTimeout
	ReasonModelResolution
	ReasonInference
	ReasonInvalidOutput
	ReasonTerminal
	ReasonFinalization
	ReasonInternal
)

// Valid reports whether the reason is recognized for durable audit.
func (r ReasonCode) Valid() bool { return r >= ReasonRejected && r <= ReasonInternal }

// Request is the shared runtime's data-only serialization envelope.
type Request struct {
	Name  Name
	Cause identity.Cause
	Input json.RawMessage
}

// Result is the validated serialized output and normalized usage.
type Result struct {
	Output json.RawMessage
	Usage  *content.Usage
}

// Outcome carries exactly one terminal result or error.
type Outcome struct {
	Result *Result
	Err    error
}
