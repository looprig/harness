package anthropicapi_test

import (
	"reflect"
	"testing"

	"github.com/looprig/core/content"
	"github.com/looprig/harness/pkg/llm"
	"github.com/looprig/harness/pkg/llm/codec/anthropicapi"
)

// Codec must satisfy the llm.Codec contract.
var _ llm.Codec = anthropicapi.Codec{}

// TestCodec_EncodeRequest confirms the typed RequestMode maps to the stream bool
// the same way the free EncodeRequest does: Invoke omits "stream", Stream sets it.
func TestCodec_EncodeRequest(t *testing.T) {
	t.Parallel()

	req := llm.Request{
		Model:    baseModel(),
		Messages: content.AgenticMessages{userMsg(textBlock("hi"))},
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
			got, err := anthropicapi.Codec{}.EncodeRequest(req, tc.mode)
			if err != nil {
				t.Fatalf("Codec.EncodeRequest: %v", err)
			}
			want, err := anthropicapi.EncodeRequest(req, tc.wantStream)
			if err != nil {
				t.Fatalf("free EncodeRequest: %v", err)
			}
			if string(got) != string(want) {
				t.Errorf("mismatch:\n got %s\nwant %s", got, want)
			}
		})
	}
}

// TestCodec_DecodeResponse confirms the method delegates to the free function.
func TestCodec_DecodeResponse(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		body    string
		wantErr bool
	}{
		{
			name:    "valid response decodes",
			body:    `{"id":"m1","type":"message","role":"assistant","model":"claude-opus-4-8","content":[{"type":"text","text":"hi"}],"usage":{"input_tokens":1,"output_tokens":2}}`,
			wantErr: false,
		},
		{
			name:    "error envelope is an error",
			body:    `{"type":"error","error":{"type":"invalid_request_error","message":"bad"}}`,
			wantErr: true,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, gotErr := anthropicapi.Codec{}.DecodeResponse([]byte(tc.body))
			want, wantErr := anthropicapi.DecodeResponse([]byte(tc.body))
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

// TestCodec_DecodeEvent exercises the per-event decoder across every Anthropic
// SSE event type: block starts, the three mapped delta types, and the tolerant
// skips (unknown events, ping, message envelope events, malformed JSON).
func TestCodec_DecodeEvent(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		payload string
		want    []content.Chunk
	}{
		{
			name:    "message_start is a skip",
			payload: `{"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"claude-opus-4-8","content":[],"usage":{"input_tokens":25,"output_tokens":1}}}`,
			want:    nil,
		},
		{
			name:    "content_block_start text yields no chunk",
			payload: `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
			want:    nil,
		},
		{
			name:    "content_block_start thinking yields no chunk",
			payload: `{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`,
			want:    nil,
		},
		{
			name:    "content_block_start tool_use yields a seed ToolUseChunk",
			payload: `{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_1","name":"get_weather","input":{}}}`,
			want:    []content.Chunk{&content.ToolUseChunk{Index: 1, ID: "toolu_1", Name: "get_weather"}},
		},
		{
			name:    "text_delta yields a TextChunk",
			payload: `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hello"}}`,
			want:    []content.Chunk{&content.TextChunk{Text: "Hello"}},
		},
		{
			name:    "thinking_delta yields a ThinkingChunk",
			payload: `{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"let me think"}}`,
			want:    []content.Chunk{&content.ThinkingChunk{Thinking: "let me think"}},
		},
		{
			name:    "input_json_delta yields a verbatim ToolUseChunk fragment",
			payload: `{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"city\":"}}`,
			want:    []content.Chunk{&content.ToolUseChunk{Index: 1, InputJSON: `{"city":`}},
		},
		{
			name:    "signature_delta is a skip",
			payload: `{"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"abc"}}`,
			want:    nil,
		},
		{
			name:    "empty text_delta is a skip",
			payload: `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":""}}`,
			want:    nil,
		},
		{
			name:    "content_block_stop is a skip",
			payload: `{"type":"content_block_stop","index":0}`,
			want:    nil,
		},
		{
			name:    "message_delta is a skip",
			payload: `{"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":15}}`,
			want:    nil,
		},
		{
			name:    "message_stop is a skip",
			payload: `{"type":"message_stop"}`,
			want:    nil,
		},
		{
			name:    "ping is a skip",
			payload: `{"type":"ping"}`,
			want:    nil,
		},
		{
			name:    "unknown event type is a skip",
			payload: `{"type":"some_future_event","index":0}`,
			want:    nil,
		},
		{
			name:    "malformed json is a skip, not an error",
			payload: `not-json`,
			want:    nil,
		},
		{
			name:    "content_block_delta with no delta is a skip",
			payload: `{"type":"content_block_delta","index":0}`,
			want:    nil,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := anthropicapi.Codec{}.DecodeEvent([]byte(tc.payload))
			if err != nil {
				t.Fatalf("DecodeEvent unexpected error: %v", err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("DecodeEvent = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// TestDecodeEvent_Sequence feeds a realistic captured event stream (one text
// block then one tool_use block, multi-fragment args) through DecodeEvent in
// order and asserts the flattened chunk stream — the shape the stream
// accumulator downstream folds back into blocks.
func TestDecodeEvent_Sequence(t *testing.T) {
	t.Parallel()

	events := []string{
		`{"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"claude-opus-4-8","content":[],"usage":{"input_tokens":42,"output_tokens":1}}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Let me "}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"look."}}`,
		`{"type":"content_block_stop","index":0}`,
		`{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_9","name":"get_weather","input":{}}}`,
		`{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"city\""}}`,
		`{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":":\"Paris\"}"}}`,
		`{"type":"content_block_stop","index":1}`,
		`{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":20}}`,
		`{"type":"message_stop"}`,
	}

	var got []content.Chunk
	for _, ev := range events {
		chunks, err := anthropicapi.Codec{}.DecodeEvent([]byte(ev))
		if err != nil {
			t.Fatalf("DecodeEvent(%s): %v", ev, err)
		}
		got = append(got, chunks...)
	}

	want := []content.Chunk{
		&content.TextChunk{Text: "Let me "},
		&content.TextChunk{Text: "look."},
		&content.ToolUseChunk{Index: 1, ID: "toolu_9", Name: "get_weather"},
		&content.ToolUseChunk{Index: 1, InputJSON: `{"city"`},
		&content.ToolUseChunk{Index: 1, InputJSON: `:"Paris"}`},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("sequence chunks =\n %+v\nwant\n %+v", got, want)
	}
}
