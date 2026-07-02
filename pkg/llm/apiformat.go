package llm

// APIFormat names the wire dialect a model endpoint speaks. It is data carried on a
// model/catalog row, not structure: one codec implementation per value. See the layout design.
type APIFormat string

const (
	APIFormatOpenAI          APIFormat = "openai"
	APIFormatAnthropic       APIFormat = "anthropic"
	APIFormatBedrockConverse APIFormat = "bedrock-converse"
	APIFormatGemini          APIFormat = "gemini"
)

// Valid reports whether f is a known wire dialect.
func (f APIFormat) Valid() bool {
	switch f {
	case APIFormatOpenAI, APIFormatAnthropic, APIFormatBedrockConverse, APIFormatGemini:
		return true
	default:
		return false
	}
}
