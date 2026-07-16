package anthropicapi

import (
	"encoding/json"

	"github.com/looprig/inference/internal/usagenorm"
)

// Wire-value constants for the Anthropic Messages API. Centralized so the encode,
// decode, and stream-event paths cannot drift on a string literal.
const (
	roleUser      = "user"
	roleAssistant = "assistant"

	blockTypeText       = "text"
	blockTypeImage      = "image"
	blockTypeThinking   = "thinking"
	blockTypeToolUse    = "tool_use"
	blockTypeToolResult = "tool_result"

	imageSourceBase64 = "base64"
	imageSourceURL    = "url"

	thinkingTypeAdaptive = "adaptive"

	responseTypeError = "error"

	// SSE event `type` values.
	eventContentBlockStart = "content_block_start"
	eventContentBlockDelta = "content_block_delta"
	eventMessageStart      = "message_start"
	eventMessageDelta      = "message_delta"
	eventMessageStop       = "message_stop"

	// content_block_delta `delta.type` values.
	deltaText      = "text_delta"
	deltaThinking  = "thinking_delta"
	deltaInputJSON = "input_json_delta"

	// emptyObject is the fallback for a tool_use `input`: Anthropic requires
	// input to be a JSON object, so an empty ToolUseBlock.Input becomes "{}".
	emptyObject = "{}"

	// defaultSchema is the fallback for a tool with no schema; Anthropic requires
	// input_schema to be a JSON object.
	defaultSchema = `{"type":"object"}`
)

// defaultMaxTokens is the max_tokens value sent when the effective Sampling
// leaves MaxTokens unset. Anthropic REQUIRES max_tokens on every request, so a
// codec-level default is mandatory. 4096 is the conservative Anthropic-example
// default: large enough for typical replies, small enough to avoid SDK/HTTP
// timeouts on non-streaming calls. Callers wanting long outputs set
// Sampling.MaxTokens explicitly (and should stream — see the transport).
const defaultMaxTokens = 4096

// messagesRequest is the Anthropic `POST /v1/messages` request body. Field order
// is irrelevant on the wire (JSON is an unordered object); it is laid out to
// read model → conversation → tools → sampling → thinking → stream.
type messagesRequest struct {
	Model         string             `json:"model"`
	System        string             `json:"system,omitempty"`
	Messages      []anthropicMessage `json:"messages"`
	Tools         []anthropicTool    `json:"tools,omitempty"`
	MaxTokens     int                `json:"max_tokens"`
	Temperature   *float64           `json:"temperature,omitempty"`
	TopP          *float64           `json:"top_p,omitempty"`
	StopSequences []string           `json:"stop_sequences,omitempty"`
	Thinking      *thinkingConfig    `json:"thinking,omitempty"`
	OutputConfig  *outputConfig      `json:"output_config,omitempty"`
	Stream        bool               `json:"stream,omitempty"`
}

// thinkingConfig is the `thinking` request field. Only adaptive thinking is
// emitted (Type == "adaptive"): manual budget_tokens is rejected by current
// Anthropic models, and adaptive is the sole on-mode the codec targets.
type thinkingConfig struct {
	Type string `json:"type"`
}

// outputConfig carries the `effort` knob that governs thinking/overall token
// spend. Emitted alongside adaptive thinking when Effort is set.
type outputConfig struct {
	Effort string `json:"effort,omitempty"`
}

// anthropicTool is one entry of the `tools` array.
type anthropicTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema"`
}

// anthropicMessage is one entry of the `messages` array: a role plus an ordered
// array of content blocks.
type anthropicMessage struct {
	Role    string           `json:"role"`
	Content []anthropicBlock `json:"content"`
}

// anthropicBlock is the wire form of one Anthropic content block. Anthropic
// content blocks are a tagged union discriminated by `type`; this single struct
// (with omitempty on every optional field) is the serialization DTO for that
// union — a common Go pattern for tagged-union wire formats that keeps the codec
// strictly typed (no interface{}). Only the fields relevant to a given `type`
// are populated by the encoders; the rest stay zero and drop out via omitempty.
// It is reused for DECODE of response content blocks (text / thinking / tool_use),
// where the extra fields simply stay zero.
type anthropicBlock struct {
	Type string `json:"type"`

	// text
	Text string `json:"text,omitempty"`

	// image
	Source *imageSource `json:"source,omitempty"`

	// thinking
	Thinking  string `json:"thinking,omitempty"`
	Signature string `json:"signature,omitempty"`

	// tool_use
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`

	// tool_result
	ToolUseID string           `json:"tool_use_id,omitempty"`
	Content   []anthropicBlock `json:"content,omitempty"`
	IsError   bool             `json:"is_error,omitempty"`
}

// imageSource is the `source` object of an image block: either a base64 inline
// payload (Type "base64" + MediaType + Data) or a remote reference (Type "url" +
// URL).
type imageSource struct {
	Type      string `json:"type"`
	MediaType string `json:"media_type,omitempty"`
	Data      string `json:"data,omitempty"`
	URL       string `json:"url,omitempty"`
}

// messageResponse is the non-streaming `POST /v1/messages` response body and the
// shape carried inside a `message_start` stream event. Usage is a pointer so its
// absence is distinguishable from a zeroed count. Error is populated only when
// Type == "error".
type messageResponse struct {
	ID      string           `json:"id"`
	Type    string           `json:"type"`
	Role    string           `json:"role"`
	Model   string           `json:"model"`
	Content []anthropicBlock `json:"content"`
	Usage   *messageUsage    `json:"usage"`
	Error   *anthropicError  `json:"error"`
}

// messageUsage is the `usage` object of a message response.
type messageUsage struct {
	InputTokens         usagenorm.Count `json:"input_tokens"`
	OutputTokens        usagenorm.Count `json:"output_tokens"`
	CacheReadTokens     usagenorm.Count `json:"cache_read_input_tokens"`
	CacheCreationTokens usagenorm.Count `json:"cache_creation_input_tokens"`
}

// anthropicError is the `error` object of an error-type response.
type anthropicError struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

// streamEvent is the union view of one de-framed SSE event the codec cares about.
// Content fields feed DecodeEvent; message, usage, and error fields feed the
// stream result collector without entering the content chunk vocabulary.
type streamEvent struct {
	Type         string           `json:"type"`
	Index        int              `json:"index"`
	ContentBlock *streamBlock     `json:"content_block"`
	Delta        *streamDelta     `json:"delta"`
	Message      *messageResponse `json:"message"`
	Usage        *messageUsage    `json:"usage"`
	Error        *anthropicError  `json:"error"`
}

// streamBlock is the `content_block` object on a content_block_start event. The
// codec reads Type (to detect tool_use) plus the tool_use ID and Name.
type streamBlock struct {
	Type string `json:"type"`
	ID   string `json:"id"`
	Name string `json:"name"`
}

// streamDelta is the `delta` object on content_block_delta and message_delta
// events. Content events populate one content field; message_delta can populate
// StopReason for terminal metadata.
type streamDelta struct {
	Type        string `json:"type"`
	Text        string `json:"text"`
	PartialJSON string `json:"partial_json"`
	Thinking    string `json:"thinking"`
	StopReason  string `json:"stop_reason"`
}
