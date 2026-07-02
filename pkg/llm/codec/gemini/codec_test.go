package gemini_test

import (
	"reflect"
	"testing"

	"github.com/ciram-co/looprig/pkg/content"
	"github.com/ciram-co/looprig/pkg/llm"
	"github.com/ciram-co/looprig/pkg/llm/codec/gemini"
)

// Codec must satisfy the llm.Codec contract.
var _ llm.Codec = gemini.Codec{}

// TestCodec_EncodeRequest confirms the method delegates to the free EncodeRequest
// and, crucially, that the RequestMode does NOT change the body: Gemini's invoke
// and stream endpoints share an identical request body (streaming is a URL +
// ?alt=sse concern owned by the transport).
func TestCodec_EncodeRequest(t *testing.T) {
	t.Parallel()

	req := llm.Request{
		Model:    llm.Model{Name: "gemini-2.5-flash"},
		System:   "be brief",
		Messages: content.AgenticMessages{&content.UserMessage{Message: content.Message{Role: content.RoleUser, Blocks: []content.Block{&content.TextBlock{Text: "hi"}}}}},
	}

	invoke, err := gemini.Codec{}.EncodeRequest(req, llm.RequestModeInvoke)
	if err != nil {
		t.Fatalf("invoke encode error: %v", err)
	}
	stream, err := gemini.Codec{}.EncodeRequest(req, llm.RequestModeStream)
	if err != nil {
		t.Fatalf("stream encode error: %v", err)
	}
	if string(invoke) != string(stream) {
		t.Errorf("invoke and stream bodies differ:\n invoke %s\n stream %s", invoke, stream)
	}
	free, err := gemini.EncodeRequest(req)
	if err != nil {
		t.Fatalf("free encode error: %v", err)
	}
	if string(invoke) != string(free) {
		t.Errorf("Codec.EncodeRequest != free EncodeRequest:\n got %s\nwant %s", invoke, free)
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
		{name: "valid response decodes", body: `{"candidates":[{"content":{"parts":[{"text":"hello"}],"role":"model"}}],"usageMetadata":{"promptTokenCount":1,"candidatesTokenCount":2}}`, wantErr: false},
		{name: "no candidates is an error", body: `{"candidates":[]}`, wantErr: true},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, gotErr := gemini.Codec{}.DecodeResponse([]byte(tc.body))
			want, wantErr := gemini.DecodeResponse([]byte(tc.body))
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

// TestCodec_DecodeEvent exercises the stateless per-event decoder against
// realistic streamGenerateContent chunk payloads.
func TestCodec_DecodeEvent(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		payload string
		want    []content.Chunk
	}{
		{
			name:    "text chunk yields one text chunk",
			payload: `{"candidates":[{"content":{"parts":[{"text":"Hello"}],"role":"model"}}]}`,
			want:    []content.Chunk{&content.TextChunk{Text: "Hello"}},
		},
		{
			name:    "thought-tagged text yields a thinking chunk",
			payload: `{"candidates":[{"content":{"parts":[{"text":"let me think","thought":true}],"role":"model"}}]}`,
			want:    []content.Chunk{&content.ThinkingChunk{Thinking: "let me think"}},
		},
		{
			name:    "complete functionCall yields one tool-use chunk with full args",
			payload: `{"candidates":[{"content":{"parts":[{"functionCall":{"name":"get_weather","args":{"location":"Boston, MA"}}}],"role":"model"}}]}`,
			want: []content.Chunk{
				&content.ToolUseChunk{Index: 0, Name: "get_weather", InputJSON: `{"location":"Boston, MA"}`},
			},
		},
		{
			name:    "functionCall with id preserves id",
			payload: `{"candidates":[{"content":{"parts":[{"functionCall":{"id":"c1","name":"run","args":{}}}],"role":"model"}}]}`,
			want: []content.Chunk{
				&content.ToolUseChunk{Index: 0, ID: "c1", Name: "run", InputJSON: `{}`},
			},
		},
		{
			name:    "parallel functionCalls in one chunk get distinct positional indices",
			payload: `{"candidates":[{"content":{"parts":[{"functionCall":{"name":"a","args":{}}},{"functionCall":{"name":"b","args":{}}}],"role":"model"}}]}`,
			want: []content.Chunk{
				&content.ToolUseChunk{Index: 0, Name: "a", InputJSON: `{}`},
				&content.ToolUseChunk{Index: 1, Name: "b", InputJSON: `{}`},
			},
		},
		{
			name:    "interleaved text and functionCall preserve order and reset index only for calls",
			payload: `{"candidates":[{"content":{"parts":[{"text":"ok "},{"functionCall":{"name":"a","args":{}}}],"role":"model"}}]}`,
			want: []content.Chunk{
				&content.TextChunk{Text: "ok "},
				&content.ToolUseChunk{Index: 0, Name: "a", InputJSON: `{}`},
			},
		},
		{
			name:    "functionCall with no args normalizes to empty object",
			payload: `{"candidates":[{"content":{"parts":[{"functionCall":{"name":"noop"}}],"role":"model"}}]}`,
			want: []content.Chunk{
				&content.ToolUseChunk{Index: 0, Name: "noop", InputJSON: `{}`},
			},
		},
		{
			name:    "empty text part is a skip",
			payload: `{"candidates":[{"content":{"parts":[{"text":""}],"role":"model"}}]}`,
			want:    nil,
		},
		{
			name:    "no candidates is a skip",
			payload: `{"candidates":[]}`,
			want:    nil,
		},
		{
			name:    "missing candidates field is a skip",
			payload: `{"usageMetadata":{"promptTokenCount":1,"candidatesTokenCount":2}}`,
			want:    nil,
		},
		{
			name:    "malformed JSON is a skip, not an error",
			payload: `not-json`,
			want:    nil,
		},
		{
			name:    "empty parts is a skip",
			payload: `{"candidates":[{"content":{"role":"model"}}]}`,
			want:    nil,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := gemini.Codec{}.DecodeEvent([]byte(tc.payload))
			if err != nil {
				t.Fatalf("DecodeEvent returned unexpected error: %v", err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("DecodeEvent = %+v, want %+v", got, tc.want)
			}
		})
	}
}
