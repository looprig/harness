package openaiapi

// UnsupportedBlockError is returned by the encoder when a content block has a
// concrete type the OpenAI chat completions dialect does not model in that
// position (e.g. audio or document blocks anywhere, or any non-text block in a
// text-only tool message). Block holds the Go type name for diagnosis.
// Fail-secure per CLAUDE.md and consistent with the sibling anthropicapi and
// geminiapi codecs: an unmodeled block is refused, never silently dropped, so
// the model never receives less than the caller sent. Callers may errors.As to
// detect it.
type UnsupportedBlockError struct {
	Block string
}

func (e *UnsupportedBlockError) Error() string {
	return "openai: unsupported content block type " + e.Block
}
