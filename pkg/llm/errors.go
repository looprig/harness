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

// AuthKind classifies the credential a provider requires. Multi-auth-ready successor to the
// boolean Provider.RequiresKey.
type AuthKind string

const (
	AuthNone   AuthKind = "none"
	AuthAPIKey AuthKind = "api_key"
	AuthSigV4  AuthKind = "sigv4"
)

// AuthRequiredError is returned by the runtime factory when a provider that requires credentials
// is given none. Fail-closed. Carries no secret.
type AuthRequiredError struct {
	Provider Provider
	Kind     AuthKind
}

func (e *AuthRequiredError) Error() string {
	return fmt.Sprintf("provider %q requires %s credentials", e.Provider, e.Kind)
}

// ModelMismatchError is returned before any network I/O when a Request's model names a
// provider/endpoint that differs from the connection the client is bound to. Fail-closed.
type ModelMismatchError struct {
	BoundProvider   Provider
	RequestProvider Provider
	BoundEndpoint   string
	RequestEndpoint string
}

func (e *ModelMismatchError) Error() string {
	return fmt.Sprintf("request model provider %q/endpoint %q does not match bound client %q/%q",
		e.RequestProvider, e.RequestEndpoint, e.BoundProvider, e.BoundEndpoint)
}
