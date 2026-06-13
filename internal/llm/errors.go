package llm

import "fmt"

// NetworkError wraps a transport-level failure (DNS, TCP, TLS).
// Err must not be nil; a nil Err will panic on Error() — callers are responsible for providing a cause.
type NetworkError struct {
	Err error
}

func (e *NetworkError) Error() string { return "llm: network error: " + e.Err.Error() }
func (e *NetworkError) Unwrap() error { return e.Err }

// APIError is a non-2xx response from the provider.
// Body holds the raw response payload for provider-specific error parsing and may be nil.
type APIError struct {
	Status  int
	Message string
	Body    []byte
}

func (e *APIError) Error() string {
	return fmt.Sprintf("llm: api error %d: %s", e.Status, e.Message)
}

// ValidationError is a self-contradictory request rejected before sending to the provider.
// Field identifies which request parameter is invalid; Reason explains the constraint violated.
type ValidationError struct {
	Field  string
	Reason string
}

func (e *ValidationError) Error() string {
	return fmt.Sprintf("llm: validation error: %s: %s", e.Field, e.Reason)
}

// AttestationError is a TEE attestation failure.
// Fail-closed: a request must never be sent to the provider when this error is returned.
// Err may be nil when the failure has no underlying cause to chain.
type AttestationError struct {
	Reason string
	Err    error
}

func (e *AttestationError) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("llm: attestation error: %s: %v", e.Reason, e.Err)
	}
	return "llm: attestation error: " + e.Reason
}

func (e *AttestationError) Unwrap() error { return e.Err }
