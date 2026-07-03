package openaiapi_test

import (
	"reflect"
	"testing"

	"github.com/looprig/harness/pkg/content"
	"github.com/looprig/harness/pkg/llm"
	"github.com/looprig/harness/pkg/llm/codec/openaiapi"
)

// Codec must satisfy the llm.Codec contract.
var _ llm.Codec = openaiapi.Codec{}

// TestCodec_EncodeRequest confirms the typed RequestMode maps to the stream bool
// the same way the free EncodeRequest does: Invoke omits "stream", Stream sets it.
func TestCodec_EncodeRequest(t *testing.T) {
	t.Parallel()

	req := llm.Request{
		Model: llm.Model{
			Provider:  llm.ProviderLMStudio,
			APIFormat: llm.APIFormatOpenAI,
			BaseURL:   "http://localhost:1234",
			Name:      "m",
		},
		Messages: content.AgenticMessages{
			&content.UserMessage{Message: content.Message{
				Role:   content.RoleUser,
				Blocks: []content.Block{&content.TextBlock{Text: "hi"}},
			}},
		},
	}

	cases := []struct {
		name       string
		mode       llm.RequestMode
		wantStream bool
	}{
		{name: "invoke mode omits stream", mode: llm.RequestModeInvoke, wantStream: false},
		{name: "stream mode sets stream", mode: llm.RequestModeStream, wantStream: true},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := openaiapi.Codec{}.EncodeRequest(req, tc.mode)
			if err != nil {
				t.Fatalf("EncodeRequest error: %v", err)
			}
			want, err := openaiapi.EncodeRequest(req, tc.wantStream)
			if err != nil {
				t.Fatalf("free EncodeRequest error: %v", err)
			}
			if string(got) != string(want) {
				t.Errorf("Codec.EncodeRequest mismatch:\n got %s\nwant %s", got, want)
			}
		})
	}
}

// TestCodec_DecodeResponse confirms the method delegates to the free function.
func TestCodec_DecodeResponse(t *testing.T) {
	t.Parallel()

	const body = `{"id":"c1","model":"m","choices":[{"message":{"role":"assistant","content":"hello"}}],"usage":{"prompt_tokens":1,"completion_tokens":2}}`

	cases := []struct {
		name    string
		body    string
		wantErr bool
	}{
		{name: "valid response decodes", body: body, wantErr: false},
		{name: "no choices is an error", body: `{"id":"c1","model":"m","choices":[]}`, wantErr: true},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, gotErr := openaiapi.Codec{}.DecodeResponse([]byte(tc.body))
			want, wantErr := openaiapi.DecodeResponse([]byte(tc.body))
			if (gotErr != nil) != tc.wantErr {
				t.Fatalf("Codec.DecodeResponse err = %v, wantErr %v", gotErr, tc.wantErr)
			}
			if (wantErr != nil) != tc.wantErr {
				t.Fatalf("free DecodeResponse err = %v, wantErr %v", wantErr, tc.wantErr)
			}
			if tc.wantErr {
				return
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("Codec.DecodeResponse = %+v, want %+v", got, want)
			}
		})
	}
}

// TestCodec_DecodeEvent exercises the per-event decoder directly. It mirrors the
// scenarios covered by NewStream (thinking, text, tool-call fan-out, skips) but at
// the single-event granularity the Codec exposes: malformed JSON, empty choices,
// and role-only/empty deltas return (nil, nil) — a skip, not an error — while a
// single delta line carrying several tool-call entries returns them all.
func TestCodec_DecodeEvent(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		payload string
		want    []content.Chunk
	}{
		{
			name:    "text delta yields one text chunk",
			payload: `{"choices":[{"delta":{"content":"hello"}}]}`,
			want:    []content.Chunk{&content.TextChunk{Text: "hello"}},
		},
		{
			name:    "reasoning delta yields one thinking chunk",
			payload: `{"choices":[{"delta":{"reasoning_content":"let me think"}}]}`,
			want:    []content.Chunk{&content.ThinkingChunk{Thinking: "let me think"}},
		},
		{
			name:    "reasoning takes precedence over content in a single delta",
			payload: `{"choices":[{"delta":{"reasoning_content":"plan","content":"ignored"}}]}`,
			want:    []content.Chunk{&content.ThinkingChunk{Thinking: "plan"}},
		},
		{
			name:    "single tool-call entry yields one tool-use chunk",
			payload: `{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"read","arguments":"{}"}}]}}]}`,
			want: []content.Chunk{
				&content.ToolUseChunk{Index: 0, ID: "call_1", Name: "read", InputJSON: "{}"},
			},
		},
		{
			name:    "multiple tool-call entries in one delta are all returned",
			payload: `{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"a","function":{"name":"f","arguments":"{}"}},{"index":1,"id":"b","function":{"name":"g","arguments":"{}"}}]}}]}`,
			want: []content.Chunk{
				&content.ToolUseChunk{Index: 0, ID: "a", Name: "f", InputJSON: "{}"},
				&content.ToolUseChunk{Index: 1, ID: "b", Name: "g", InputJSON: "{}"},
			},
		},
		{
			name:    "arg-fragment-only tool delta preserves index with empty id/name",
			payload: `{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"ath\":"}}]}}]}`,
			want: []content.Chunk{
				&content.ToolUseChunk{Index: 0, InputJSON: `ath":`},
			},
		},
		{
			name:    "wholly-empty tool-call entry is skipped, yielding no chunk",
			payload: `{"choices":[{"delta":{"tool_calls":[{"index":0}]}}]}`,
			want:    nil,
		},
		{
			name:    "malformed JSON is a skip, not an error",
			payload: `not-json`,
			want:    nil,
		},
		{
			name:    "empty choices is a skip",
			payload: `{"choices":[]}`,
			want:    nil,
		},
		{
			name:    "missing choices field is a skip",
			payload: `{"usage":{"prompt_tokens":1,"completion_tokens":2}}`,
			want:    nil,
		},
		{
			name:    "role-only delta is a skip",
			payload: `{"choices":[{"delta":{"role":"assistant"}}]}`,
			want:    nil,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := openaiapi.Codec{}.DecodeEvent([]byte(tc.payload))
			if err != nil {
				t.Fatalf("DecodeEvent returned unexpected error: %v", err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("DecodeEvent = %+v, want %+v", got, tc.want)
			}
		})
	}
}
