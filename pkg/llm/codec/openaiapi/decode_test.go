// pkg/llm/codec/openaiapi/decode_test.go
package openaiapi_test

import (
	"encoding/json"
	"testing"

	"github.com/ciram-co/looprig/pkg/content"
	"github.com/ciram-co/looprig/pkg/llm"
	"github.com/ciram-co/looprig/pkg/llm/codec/openaiapi"
)

// TestDecodeResponse_CompileTimeCheck asserts the exact signature of DecodeResponse.
func TestDecodeResponse_CompileTimeCheck(t *testing.T) {
	t.Parallel()
	var _ func([]byte) (*llm.Response, error) = openaiapi.DecodeResponse
}

// blockType maps a concrete sealed block to its wire-tag BlockType, used to
// assert decoded block ordering without a Type field on the value.
func blockType(b content.Block) content.BlockType {
	switch b.(type) {
	case *content.TextBlock:
		return content.TypeText
	case *content.ThinkingBlock:
		return content.TypeThinking
	case *content.ToolUseBlock:
		return content.TypeToolUse
	case *content.ImageBlock:
		return content.TypeImage
	default:
		return ""
	}
}

func TestDecodeResponse(t *testing.T) {
	t.Parallel()

	rawArgs := json.RawMessage(`{"key":"value"}`)

	cases := []struct {
		name string
		body []byte

		// expected success fields
		wantModel        string
		wantBlockCount   int
		wantBlockTypes   []content.BlockType
		wantThinking     string
		wantText         string
		wantToolUseID    string
		wantToolUseName  string
		wantToolUseInput json.RawMessage
		wantUsageNil     bool
		wantInputTokens  int
		wantOutputTokens int

		// expected error fields
		wantErr       bool
		wantAPIErr    bool
		wantNoChoices bool
	}{
		{
			name: "text response",
			body: []byte(`{
				"id": "chatcmpl-1",
				"model": "gpt-4o",
				"choices": [{"message": {"role": "assistant", "content": "Hello, world!"}, "finish_reason": "stop"}],
				"usage": {"prompt_tokens": 10, "completion_tokens": 5}
			}`),
			wantModel:        "gpt-4o",
			wantBlockCount:   1,
			wantBlockTypes:   []content.BlockType{content.TypeText},
			wantText:         "Hello, world!",
			wantInputTokens:  10,
			wantOutputTokens: 5,
		},
		{
			name: "tool call response with null content",
			body: []byte(`{
				"id": "chatcmpl-2",
				"model": "gpt-4o",
				"choices": [{"message": {"role": "assistant", "content": null, "tool_calls": [{"id": "call_abc", "type": "function", "function": {"name": "get_weather", "arguments": {"key":"value"}}}]}, "finish_reason": "tool_calls"}],
				"usage": {"prompt_tokens": 20, "completion_tokens": 8}
			}`),
			wantModel:        "gpt-4o",
			wantBlockCount:   1,
			wantBlockTypes:   []content.BlockType{content.TypeToolUse},
			wantToolUseID:    "call_abc",
			wantToolUseName:  "get_weather",
			wantToolUseInput: rawArgs,
			wantInputTokens:  20,
			wantOutputTokens: 8,
		},
		{
			name: "reasoning content only",
			body: []byte(`{
				"id": "chatcmpl-3",
				"model": "o1-preview",
				"choices": [{"message": {"role": "assistant", "content": null, "reasoning_content": "I need to think about this..."}, "finish_reason": "stop"}],
				"usage": {"prompt_tokens": 30, "completion_tokens": 15}
			}`),
			wantModel:        "o1-preview",
			wantBlockCount:   1,
			wantBlockTypes:   []content.BlockType{content.TypeThinking},
			wantThinking:     "I need to think about this...",
			wantInputTokens:  30,
			wantOutputTokens: 15,
		},
		{
			name: "both reasoning and content",
			body: []byte(`{
				"id": "chatcmpl-4",
				"model": "o1-preview",
				"choices": [{"message": {"role": "assistant", "content": "My answer.", "reasoning_content": "First I thought..."}, "finish_reason": "stop"}],
				"usage": {"prompt_tokens": 50, "completion_tokens": 25}
			}`),
			wantModel:        "o1-preview",
			wantBlockCount:   2,
			wantBlockTypes:   []content.BlockType{content.TypeThinking, content.TypeText},
			wantThinking:     "First I thought...",
			wantText:         "My answer.",
			wantInputTokens:  50,
			wantOutputTokens: 25,
		},
		{
			name: "null usage",
			body: []byte(`{
				"id": "chatcmpl-5",
				"model": "gpt-4o",
				"choices": [{"message": {"role": "assistant", "content": "hello"}, "finish_reason": "stop"}],
				"usage": null
			}`),
			wantModel:      "gpt-4o",
			wantBlockCount: 1,
			wantBlockTypes: []content.BlockType{content.TypeText},
			wantText:       "hello",
			wantUsageNil:   true,
		},
		{
			name: "usage present",
			body: []byte(`{
				"id": "chatcmpl-6",
				"model": "gpt-4o",
				"choices": [{"message": {"role": "assistant", "content": "hi"}, "finish_reason": "stop"}],
				"usage": {"prompt_tokens": 7, "completion_tokens": 3}
			}`),
			wantModel:        "gpt-4o",
			wantBlockCount:   1,
			wantText:         "hi",
			wantInputTokens:  7,
			wantOutputTokens: 3,
		},
		{
			name:          "empty choices",
			body:          []byte(`{"id":"x","model":"gpt-4o","choices":[],"usage":null}`),
			wantErr:       true,
			wantAPIErr:    true,
			wantNoChoices: true,
		},
		{
			name:    "invalid JSON",
			body:    []byte(`not json at all`),
			wantErr: true,
		},
		{
			name: "model field preserved",
			body: []byte(`{
				"id": "chatcmpl-7",
				"model": "deepseek-chat",
				"choices": [{"message": {"role": "assistant", "content": "ok"}, "finish_reason": "stop"}],
				"usage": null
			}`),
			wantModel:      "deepseek-chat",
			wantBlockCount: 1,
			wantText:       "ok",
			wantUsageNil:   true,
		},
		{
			name: "multiple tool calls",
			body: []byte(`{
				"id": "chatcmpl-8",
				"model": "gpt-4o",
				"choices": [{"message": {"role": "assistant", "content": null, "tool_calls": [
					{"id": "call_1", "type": "function", "function": {"name": "fn_a", "arguments": {"key":"value"}}},
					{"id": "call_2", "type": "function", "function": {"name": "fn_b", "arguments": {"key":"value"}}}
				]}, "finish_reason": "tool_calls"}],
				"usage": {"prompt_tokens": 40, "completion_tokens": 12}
			}`),
			wantModel:        "gpt-4o",
			wantBlockCount:   2,
			wantBlockTypes:   []content.BlockType{content.TypeToolUse, content.TypeToolUse},
			wantInputTokens:  40,
			wantOutputTokens: 12,
		},
		{
			name: "empty string content produces no text block",
			body: []byte(`{
				"id": "chatcmpl-9",
				"model": "gpt-4o",
				"choices": [{"message": {"role": "assistant", "content": ""}, "finish_reason": "stop"}],
				"usage": null
			}`),
			wantModel:      "gpt-4o",
			wantBlockCount: 0,
			wantUsageNil:   true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			resp, err := openaiapi.DecodeResponse(tc.body)

			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got nil")
				}
				if tc.wantAPIErr {
					apiErr, ok := err.(*llm.APIError)
					if !ok {
						t.Fatalf("expected *llm.APIError, got %T: %v", err, err)
					}
					if tc.wantNoChoices && apiErr.Status != 0 {
						t.Errorf("no-choices APIError: want Status=0, got %d", apiErr.Status)
					}
				}
				return
			}

			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if resp == nil {
				t.Fatal("expected non-nil response")
			}

			if resp.Model != tc.wantModel {
				t.Errorf("Model: want %q, got %q", tc.wantModel, resp.Model)
			}

			if resp.Message == nil {
				t.Fatal("expected non-nil Message")
			}

			if got := len(resp.Message.Blocks); got != tc.wantBlockCount {
				t.Fatalf("block count: want %d, got %d", tc.wantBlockCount, got)
			}

			for i, wantType := range tc.wantBlockTypes {
				if i >= len(resp.Message.Blocks) {
					break
				}
				if got := blockType(resp.Message.Blocks[i]); got != wantType {
					t.Errorf("block[%d].Type: want %q, got %q", i, wantType, got)
				}
			}

			if tc.wantThinking != "" {
				var found bool
				for _, b := range resp.Message.Blocks {
					if tb, ok := b.(*content.ThinkingBlock); ok && tb.Thinking == tc.wantThinking {
						found = true
						break
					}
				}
				if !found {
					t.Errorf("expected ThinkingBlock with %q", tc.wantThinking)
				}
			}

			if tc.wantText != "" {
				var found bool
				for _, b := range resp.Message.Blocks {
					if tb, ok := b.(*content.TextBlock); ok && tb.Text == tc.wantText {
						found = true
						break
					}
				}
				if !found {
					t.Errorf("expected TextBlock with %q", tc.wantText)
				}
			}

			if tc.wantToolUseID != "" || tc.wantToolUseName != "" {
				var found bool
				for _, b := range resp.Message.Blocks {
					tu, ok := b.(*content.ToolUseBlock)
					if ok && tu.ID == tc.wantToolUseID && tu.Name == tc.wantToolUseName {
						found = true
						if string(tc.wantToolUseInput) != "" {
							if string(tu.Input) != string(tc.wantToolUseInput) {
								t.Errorf("ToolUseBlock.Input: want %s, got %s", tc.wantToolUseInput, tu.Input)
							}
						}
						break
					}
				}
				if !found {
					t.Errorf("expected ToolUseBlock id=%q name=%q", tc.wantToolUseID, tc.wantToolUseName)
				}
			}

			if tc.wantUsageNil {
				if resp.Usage != nil {
					t.Errorf("expected nil Usage, got %+v", resp.Usage)
				}
			} else {
				if resp.Usage == nil {
					t.Fatal("expected non-nil Usage")
				}
				if resp.Usage.InputTokens != tc.wantInputTokens {
					t.Errorf("InputTokens: want %d, got %d", tc.wantInputTokens, resp.Usage.InputTokens)
				}
				if resp.Usage.OutputTokens != tc.wantOutputTokens {
					t.Errorf("OutputTokens: want %d, got %d", tc.wantOutputTokens, resp.Usage.OutputTokens)
				}
			}
		})
	}
}
