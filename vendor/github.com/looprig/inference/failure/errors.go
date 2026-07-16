// Package failure owns provider-neutral inference failures shared by codecs,
// transports, and provider integrations.
package failure

import (
	"fmt"

	"github.com/looprig/inference/model"
)

type NetworkError struct {
	Err error
}

func (e *NetworkError) Error() string { return "inference: network error: " + e.Err.Error() }
func (e *NetworkError) Unwrap() error { return e.Err }

type APIError struct {
	Status  int
	Message string
	Body    []byte
}

func (e *APIError) Error() string {
	return fmt.Sprintf("inference: api error %d: %s", e.Status, e.Message)
}

type ModelMismatchError struct {
	BoundProvider    model.ProviderName
	RequestProvider  model.ProviderName
	BoundEndpoint    string
	RequestEndpoint  string
	BoundAPIFormat   model.APIFormat
	RequestAPIFormat model.APIFormat
}

func (e *ModelMismatchError) Error() string {
	return fmt.Sprintf("inference: request model provider %q/endpoint %q/format %q does not match bound client %q/%q/%q",
		e.RequestProvider, e.RequestEndpoint, e.RequestAPIFormat,
		e.BoundProvider, e.BoundEndpoint, e.BoundAPIFormat)
}
