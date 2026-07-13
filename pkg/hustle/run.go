package hustle

import (
	"encoding/json"

	"github.com/looprig/core/content"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/identity"
)

// RunID identifies one hustle invocation.
type RunID uuid.UUID

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
