// internal/llm/openaiapi/types.go
package openaiapi

import "encoding/json"

// ChatRequest is the OpenAI chat completions wire request. Exported so
// provider packages can embed it in a typed extension struct (e.g. chutes adds
// e2e_response_pk) without round-tripping through map[string]json.RawMessage.
type ChatRequest struct {
	Model       string        `json:"model"`
	Messages    []chatMessage `json:"messages"`
	Tools       []chatTool    `json:"tools,omitempty"`
	Temperature *float64      `json:"temperature,omitempty"`
	TopP        *float64      `json:"top_p,omitempty"`
	MaxTokens   *int          `json:"max_tokens,omitempty"`
	Stop        []string      `json:"stop,omitempty"`
	Stream      bool          `json:"stream,omitempty"`

	// o-series reasoning
	ReasoningEffort string `json:"reasoning_effort,omitempty"`
}

type chatMessage struct {
	Role             string      `json:"role"`
	Content          interface{} `json:"content"`                     // string or []chatContentPart; interface{} required at JSON serialization boundary
	ReasoningContent string      `json:"reasoning_content,omitempty"` // DeepSeek / o-series
	ToolCalls        []toolCall  `json:"tool_calls,omitempty"`
	ToolCallID       string      `json:"tool_call_id,omitempty"`
}

type chatContentPart struct {
	Type     string        `json:"type"`
	Text     string        `json:"text,omitempty"`
	ImageURL *imageURLPart `json:"image_url,omitempty"`
}

type imageURLPart struct {
	URL string `json:"url"`
}

type toolCall struct {
	ID       string           `json:"id"`
	Type     string           `json:"type"` // always "function"
	Function toolCallFunction `json:"function"`
}

type toolCallFunction struct {
	Name string `json:"name"`
	// Arguments is json.RawMessage to tolerate both wire shapes on DECODE
	// (some servers send a JSON string, others a bare object). On ENCODE it
	// MUST be a JSON-encoded string — see encodeAIMessage, which quotes the
	// raw object before assigning it here. Do not assign a bare object.
	Arguments json.RawMessage `json:"arguments"`
}

type chatTool struct {
	Type     string       `json:"type"` // always "function"
	Function chatFunction `json:"function"`
}

type chatFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

// chatResponse is the OpenAI chat completions wire response.
type chatResponse struct {
	ID      string       `json:"id"`
	Model   string       `json:"model"`
	Choices []chatChoice `json:"choices"`
	Usage   *chatUsage   `json:"usage"`
}

type chatChoice struct {
	Message      chatMessage `json:"message"`
	FinishReason string      `json:"finish_reason"`
}

type chatUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
}

// sseChunk is one streaming delta event.
type sseChunk struct {
	Choices []sseChoice `json:"choices"`
	Usage   *chatUsage  `json:"usage"`
}

type sseChoice struct {
	Delta sseMessageDelta `json:"delta"`
}

type sseMessageDelta struct {
	Role             string             `json:"role"`
	Content          string             `json:"content"`
	ReasoningContent string             `json:"reasoning_content"` // DeepSeek / o-series
	ToolCalls        []sseToolCallDelta `json:"tool_calls"`
}

// sseToolCallDelta is one streaming tool-call delta entry. Unlike the
// non-streaming toolCall, it carries a per-call Index and delivers
// Function.Arguments as string fragments that the runner concatenates by Index.
type sseToolCallDelta struct {
	Index    int    `json:"index"`
	ID       string `json:"id"` // first delta only
	Function struct {
		Name      string `json:"name"`      // first delta only
		Arguments string `json:"arguments"` // FRAGMENT — concatenate across deltas
	} `json:"function"`
}
