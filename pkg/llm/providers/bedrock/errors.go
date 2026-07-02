package bedrock

import (
	"fmt"

	"github.com/ciram-co/looprig/pkg/llm"
)

// UnsupportedAPIFormatError is a fail-closed rejection, before any I/O, of a
// request whose Model.APIFormat this client cannot honor. Provider.supportsAPIFormat
// admits both APIFormatAnthropic and APIFormatBedrockConverse for Bedrock (so a
// Converse Model passes Model.Validate), but this client implements only the
// Anthropic-native dialect for now; a Converse codec is a documented follow-up.
// Returning this — rather than silently Anthropic-encoding a Converse request —
// keeps the client from "silently doing less" than its declared contract. Carries
// the offending format so callers can branch via errors.As.
type UnsupportedAPIFormatError struct {
	APIFormat llm.APIFormat
}

func (e *UnsupportedAPIFormatError) Error() string {
	return fmt.Sprintf("bedrock: API format %q is not implemented; this client encodes only the Anthropic dialect (%q)", e.APIFormat, llm.APIFormatAnthropic)
}

// RequestBuildError is a failure to CONSTRUCT the outbound HTTP request (a
// malformed endpoint/URL), kept distinct from *llm.NetworkError (reserved for
// transport failures out of hc.Do) so errors.As never misclassifies a config bug
// as a transport fault. Unwrap exposes the net/http cause.
type RequestBuildError struct {
	Err error
}

func (e *RequestBuildError) Error() string { return "bedrock: build request: " + e.Err.Error() }
func (e *RequestBuildError) Unwrap() error { return e.Err }

// ConfigError is a fail-closed rejection of an invalid bedrock.New configuration:
// an empty AWS region or empty SigV4 credentials. No Client is returned and no
// network object is created. Field names the offending input; Reason explains the
// constraint. Carries no secret (never the credential values themselves).
type ConfigError struct {
	Field  string
	Reason string
}

func (e *ConfigError) Error() string {
	return fmt.Sprintf("bedrock: invalid config: %s: %s", e.Field, e.Reason)
}

// BodyTransformError wraps a failure to turn the Anthropic Messages body into the
// Bedrock InvokeModel body (unmarshal, field rewrite, or re-marshal). It is kept
// distinct from transport/API errors so a caller can tell a local encode fault
// from a wire fault. Err is the underlying cause.
type BodyTransformError struct {
	Err error
}

func (e *BodyTransformError) Error() string {
	return "bedrock: transform request body: " + e.Err.Error()
}

func (e *BodyTransformError) Unwrap() error { return e.Err }

// StreamingNotSupportedError is returned by Client.Stream: Bedrock streaming uses
// the AWS eventstream (application/vnd.amazon.eventstream) framing, which is a
// documented follow-up and is not yet implemented. Fail-closed: no stream is
// opened. A typed error so a caller can branch (errors.As) and fall back to Invoke.
type StreamingNotSupportedError struct{}

func (*StreamingNotSupportedError) Error() string {
	return "bedrock: streaming (AWS eventstream) is not yet implemented; use Invoke"
}
