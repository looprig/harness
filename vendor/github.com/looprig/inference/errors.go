package inference

import "fmt"

// NetworkError wraps a transport-level failure (DNS, TCP, TLS).
// Err must not be nil; a nil Err will panic on Error() — callers are responsible for providing a cause.
type NetworkError struct {
	Err error
}

func (e *NetworkError) Error() string { return "inference: network error: " + e.Err.Error() }
func (e *NetworkError) Unwrap() error { return e.Err }

// APIError is a non-2xx response from the provider.
// Body holds the raw response payload for provider-specific error parsing and may be nil.
type APIError struct {
	Status  int
	Message string
	Body    []byte
}

func (e *APIError) Error() string {
	return fmt.Sprintf("inference: api error %d: %s", e.Status, e.Message)
}

// ValidationError is a self-contradictory request rejected before sending to the provider.
// Field identifies which request parameter is invalid; Reason explains the constraint violated.
type ValidationError struct {
	Field  string
	Reason string
}

func (e *ValidationError) Error() string {
	return fmt.Sprintf("inference: validation error: %s: %s", e.Field, e.Reason)
}

// AuthKind classifies the credential a generic auth mechanism requires. Provider-specific
// credential kinds (e.g. cloud request-signing) live in the llm module, not here.
type AuthKind string

const (
	AuthNone   AuthKind = "none"
	AuthAPIKey AuthKind = "api_key"
)

// MissingCredentialsError is returned by a generic auth mechanism when a required credential
// or header value is absent. It names the missing credential without depending on any provider
// policy table. Fail-closed: an authorize step returning this error must block the request.
// Carries no secret.
type MissingCredentialsError struct {
	Credential string
}

func (e *MissingCredentialsError) Error() string {
	return fmt.Sprintf("inference: missing credential: %s", e.Credential)
}

// ModelMismatchError is returned before any network I/O when a Request's model names a
// provider, endpoint, or API format that differs from the connection the client is bound
// to. Fail-closed. All three binding dimensions are carried so a caller doing errors.As
// can see exactly which one conflicted; an empty request-side field is a wildcard, not a
// claim, so it never triggers this error.
type ModelMismatchError struct {
	BoundProvider    ProviderName
	RequestProvider  ProviderName
	BoundEndpoint    string
	RequestEndpoint  string
	BoundAPIFormat   APIFormat
	RequestAPIFormat APIFormat
}

func (e *ModelMismatchError) Error() string {
	return fmt.Sprintf("inference: request model provider %q/endpoint %q/format %q does not match bound client %q/%q/%q",
		e.RequestProvider, e.RequestEndpoint, e.RequestAPIFormat,
		e.BoundProvider, e.BoundEndpoint, e.BoundAPIFormat)
}
