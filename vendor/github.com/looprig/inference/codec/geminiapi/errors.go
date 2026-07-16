package geminiapi

// EncodeError is a failure while translating an inference.Request into the Gemini wire
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

// UnsupportedBlockError is returned by the encoder when a user or model content
// block has a concrete type the Gemini generateContent dialect does not model
// (e.g. audio or document blocks). Block holds the Go type name for diagnosis.
// Fail-secure per CLAUDE.md and consistent with the sibling anthropicapi codec:
// an unmodeled block is refused, never silently dropped, so the model never
// receives less than the caller sent. Callers may errors.As to detect it.
type UnsupportedBlockError struct {
	Block string
}

func (e *UnsupportedBlockError) Error() string {
	return "gemini: unsupported content block type " + e.Block
}

// DecodeError is a failure while parsing a Gemini response body into a
// provider-neutral Response (a JSON unmarshal failure). The distinct
// "no candidates" case returns *failure.APIError instead, matching the sibling
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
