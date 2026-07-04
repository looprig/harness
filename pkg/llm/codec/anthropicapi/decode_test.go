package anthropicapi_test

import (
	"errors"
	"testing"

	"github.com/looprig/core/content"
	"github.com/looprig/harness/pkg/llm"
	"github.com/looprig/harness/pkg/llm/codec/anthropicapi"
)

// TestDecodeResponse_CompileTimeCheck pins the free function signature.
func TestDecodeResponse_CompileTimeCheck(t *testing.T) {
	t.Parallel()
	var _ func([]byte) (*llm.Response, error) = anthropicapi.DecodeResponse
}

// blockType maps a decoded block to its wire tag for order assertions.
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

	cases := []struct {
		name string
		body []byte

		wantErr    bool
		wantAPIErr bool

		wantModel        string
		wantTypes        []content.BlockType
		wantText         string
		wantThinking     string
		wantToolID       string
		wantToolName     string
		wantToolInput    string
		wantUsageNil     bool
		wantInputTokens  int
		wantOutputTokens int
	}{
		{
			name: "text response with usage",
			body: []byte(`{
				"id":"msg_1","type":"message","role":"assistant","model":"claude-opus-4-8",
				"content":[{"type":"text","text":"Hello, world!"}],
				"stop_reason":"end_turn",
				"usage":{"input_tokens":10,"output_tokens":5}
			}`),
			wantModel:        "claude-opus-4-8",
			wantTypes:        []content.BlockType{content.TypeText},
			wantText:         "Hello, world!",
			wantInputTokens:  10,
			wantOutputTokens: 5,
		},
		{
			name: "tool_use response",
			body: []byte(`{
				"id":"msg_2","type":"message","role":"assistant","model":"claude-opus-4-8",
				"content":[{"type":"tool_use","id":"toolu_1","name":"get_weather","input":{"city":"Paris"}}],
				"stop_reason":"tool_use",
				"usage":{"input_tokens":20,"output_tokens":8}
			}`),
			wantModel:        "claude-opus-4-8",
			wantTypes:        []content.BlockType{content.TypeToolUse},
			wantToolID:       "toolu_1",
			wantToolName:     "get_weather",
			wantToolInput:    `{"city":"Paris"}`,
			wantInputTokens:  20,
			wantOutputTokens: 8,
		},
		{
			name: "text + tool_use in order",
			body: []byte(`{
				"id":"msg_3","type":"message","role":"assistant","model":"claude-opus-4-8",
				"content":[
					{"type":"text","text":"Let me check."},
					{"type":"tool_use","id":"toolu_2","name":"search","input":{"q":"x"}}
				],
				"usage":{"input_tokens":3,"output_tokens":4}
			}`),
			wantModel:        "claude-opus-4-8",
			wantTypes:        []content.BlockType{content.TypeText, content.TypeToolUse},
			wantText:         "Let me check.",
			wantToolID:       "toolu_2",
			wantToolName:     "search",
			wantToolInput:    `{"q":"x"}`,
			wantInputTokens:  3,
			wantOutputTokens: 4,
		},
		{
			name: "thinking block decodes with signature",
			body: []byte(`{
				"id":"msg_4","type":"message","role":"assistant","model":"claude-opus-4-8",
				"content":[{"type":"thinking","thinking":"pondering","signature":"sig123"},{"type":"text","text":"done"}],
				"usage":{"input_tokens":1,"output_tokens":2}
			}`),
			wantModel:        "claude-opus-4-8",
			wantTypes:        []content.BlockType{content.TypeThinking, content.TypeText},
			wantThinking:     "pondering",
			wantText:         "done",
			wantInputTokens:  1,
			wantOutputTokens: 2,
		},
		{
			name: "unknown block types are skipped",
			body: []byte(`{
				"id":"msg_5","type":"message","role":"assistant","model":"claude-opus-4-8",
				"content":[{"type":"redacted_thinking","data":"..."},{"type":"text","text":"visible"}],
				"usage":{"input_tokens":1,"output_tokens":1}
			}`),
			wantModel:        "claude-opus-4-8",
			wantTypes:        []content.BlockType{content.TypeText},
			wantText:         "visible",
			wantInputTokens:  1,
			wantOutputTokens: 1,
		},
		{
			name: "empty content is valid (no error)",
			body: []byte(`{
				"id":"msg_6","type":"message","role":"assistant","model":"claude-opus-4-8",
				"content":[],
				"stop_reason":"end_turn",
				"usage":{"input_tokens":7,"output_tokens":0}
			}`),
			wantModel:        "claude-opus-4-8",
			wantTypes:        nil,
			wantInputTokens:  7,
			wantOutputTokens: 0,
		},
		{
			name: "missing usage yields nil Usage",
			body: []byte(`{
				"id":"msg_7","type":"message","role":"assistant","model":"claude-opus-4-8",
				"content":[{"type":"text","text":"hi"}]
			}`),
			wantModel:    "claude-opus-4-8",
			wantTypes:    []content.BlockType{content.TypeText},
			wantText:     "hi",
			wantUsageNil: true,
		},
		{
			name:       "error envelope is an APIError",
			body:       []byte(`{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`),
			wantErr:    true,
			wantAPIErr: true,
		},
		{
			name:    "malformed json is an error",
			body:    []byte(`{not json`),
			wantErr: true,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			resp, err := anthropicapi.DecodeResponse(tc.body)
			if (err != nil) != tc.wantErr {
				t.Fatalf("DecodeResponse err = %v, wantErr %v", err, tc.wantErr)
			}
			if tc.wantErr {
				if tc.wantAPIErr {
					var apiErr *llm.APIError
					if !errors.As(err, &apiErr) {
						t.Errorf("error = %v, want *llm.APIError", err)
					}
				}
				return
			}

			if resp.Model != tc.wantModel {
				t.Errorf("Model = %q, want %q", resp.Model, tc.wantModel)
			}
			if resp.Message == nil || resp.Message.Role != content.RoleAssistant {
				t.Fatalf("Message = %+v, want assistant role", resp.Message)
			}

			blocks := resp.Message.Blocks
			if len(blocks) != len(tc.wantTypes) {
				t.Fatalf("block count = %d, want %d", len(blocks), len(tc.wantTypes))
			}
			for i, wt := range tc.wantTypes {
				if got := blockType(blocks[i]); got != wt {
					t.Errorf("block[%d] type = %q, want %q", i, got, wt)
				}
			}

			for _, b := range blocks {
				switch v := b.(type) {
				case *content.TextBlock:
					if tc.wantText != "" && v.Text != tc.wantText {
						t.Errorf("text = %q, want %q", v.Text, tc.wantText)
					}
				case *content.ThinkingBlock:
					if tc.wantThinking != "" && v.Thinking != tc.wantThinking {
						t.Errorf("thinking = %q, want %q", v.Thinking, tc.wantThinking)
					}
				case *content.ToolUseBlock:
					if v.ID != tc.wantToolID {
						t.Errorf("tool id = %q, want %q", v.ID, tc.wantToolID)
					}
					if v.Name != tc.wantToolName {
						t.Errorf("tool name = %q, want %q", v.Name, tc.wantToolName)
					}
					if string(v.Input) != tc.wantToolInput {
						t.Errorf("tool input = %s, want %s", v.Input, tc.wantToolInput)
					}
				}
			}

			if tc.wantUsageNil {
				if resp.Usage != nil {
					t.Errorf("Usage = %+v, want nil", resp.Usage)
				}
				return
			}
			if resp.Usage == nil {
				t.Fatalf("Usage = nil, want non-nil")
			}
			if resp.Usage.InputTokens != tc.wantInputTokens || resp.Usage.OutputTokens != tc.wantOutputTokens {
				t.Errorf("Usage = %+v, want {In:%d Out:%d}", resp.Usage, tc.wantInputTokens, tc.wantOutputTokens)
			}
		})
	}
}
