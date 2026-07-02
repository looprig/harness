// pkg/llm/codec/gemini/errors.go
package gemini

// EncodeError is a failure while translating an llm.Request into the Gemini wire
// body — an unknown conversation type or a JSON marshal failure. Typed per
// CLAUDE.md so callers can errors.As it to distinguish an encode fault from a
// transport or API error.
type EncodeError struct {
	Reason string
	Err    error
}

func (e *EncodeError) Error() string {
	if e.Err != nil {
		return "gemini: encode: " + e.Reason + ": " + e.Err.Error()
	}
	return "gemini: encode: " + e.Reason
}

func (e *EncodeError) Unwrap() error { return e.Err }

// DecodeError is a failure while parsing a Gemini response body into a
// provider-neutral Response (a JSON unmarshal failure). The distinct
// "no candidates" case returns *llm.APIError instead, matching the sibling
// OpenAI codec so the transport and callers treat every dialect uniformly.
type DecodeError struct {
	Reason string
	Err    error
}

func (e *DecodeError) Error() string {
	if e.Err != nil {
		return "gemini: decode: " + e.Reason + ": " + e.Err.Error()
	}
	return "gemini: decode: " + e.Reason
}

func (e *DecodeError) Unwrap() error { return e.Err }
