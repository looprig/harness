package gemini

import (
	"fmt"

	"github.com/looprig/harness/pkg/llm"
)

// UnsupportedAPIFormatError is a fail-closed rejection, before any I/O, of a
// request whose Model.APIFormat this client cannot honor. This client encodes only
// the Gemini generateContent dialect. Provider.supportsAPIFormat currently admits
// only APIFormatGemini for ProviderGoogle, so a Validate-passing Google Model can
// never reach this guard — it is defense-in-depth (Open/Closed): should a second
// Google dialect ever be admitted upstream, this keeps the client from silently
// Gemini-encoding a request it does not understand. Carries the offending format so
// callers can branch via errors.As.
type UnsupportedAPIFormatError struct {
	APIFormat llm.APIFormat
}

func (e *UnsupportedAPIFormatError) Error() string {
	return fmt.Sprintf("gemini: API format %q is not implemented; this client encodes only the Gemini dialect (%q)", e.APIFormat, llm.APIFormatGemini)
}

// RequestBuildError is a failure to CONSTRUCT the outbound HTTP request (a
// malformed endpoint/URL), kept distinct from *llm.NetworkError (reserved for
// transport failures out of hc.Do) so errors.As never misclassifies a config bug
// as a transport fault. Unwrap exposes the net/http cause.
type RequestBuildError struct {
	Err error
}

func (e *RequestBuildError) Error() string { return "gemini: build request: " + e.Err.Error() }
func (e *RequestBuildError) Unwrap() error { return e.Err }
