// pkg/llm/codec/openaiapi/encode_toolargs_test.go
package openaiapi_test

import (
	"encoding/json"
	"testing"

	"github.com/looprig/harness/pkg/content"
	"github.com/looprig/harness/pkg/llm"
	"github.com/looprig/harness/pkg/llm/codec/openaiapi"
)

// TestEncodeToolCallArgumentsAreJSONString locks in the OpenAI wire contract:
// tool_calls[].function.arguments must be a JSON-encoded STRING, not a raw
// object. Emitting a bare object made strict servers (vLLM/chutes) reject the
// turn that follows a tool call with an opaque 400.
func TestEncodeToolCallArgumentsAreJSONString(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		input    json.RawMessage
		wantArgs string // expected JSON value after string-decoding the wire field
	}{
		{name: "object input is stringified", input: json.RawMessage(`{"pattern":"*"}`), wantArgs: `{"pattern":"*"}`},
		{name: "empty input becomes empty object", input: nil, wantArgs: `{}`},
		{name: "nested input preserved", input: json.RawMessage(`{"a":{"b":[1,2]}}`), wantArgs: `{"a":{"b":[1,2]}}`},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			req := llm.Request{
				Model: llm.Model{Name: "test-model"},
				Messages: content.AgenticMessages{
					&content.AIMessage{Message: content.Message{
						Role: content.RoleAssistant,
						Blocks: []content.Block{
							&content.ToolUseBlock{ID: "call-1", Name: "Glob", Input: tt.input},
						},
					}},
				},
			}
			raw, err := openaiapi.EncodeRequest(req, false)
			if err != nil {
				t.Fatalf("EncodeRequest: %v", err)
			}

			var probe struct {
				Messages []struct {
					ToolCalls []struct {
						Function struct {
							Arguments json.RawMessage `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"messages"`
			}
			if err := json.Unmarshal(raw, &probe); err != nil {
				t.Fatalf("unmarshal probe: %v", err)
			}
			args := probe.Messages[0].ToolCalls[0].Function.Arguments

			// The field MUST decode as a JSON string (this is the contract the
			// strict server enforces); decoding into a string fails for a bare
			// object, which is exactly the 400 bug.
			var got string
			if err := json.Unmarshal(args, &got); err != nil {
				t.Fatalf("arguments must be a JSON string, got raw %s: %v", args, err)
			}
			if got != tt.wantArgs {
				t.Errorf("arguments = %q, want %q", got, tt.wantArgs)
			}
		})
	}
}
