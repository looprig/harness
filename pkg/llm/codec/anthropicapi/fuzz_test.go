package anthropicapi_test

import (
	"testing"

	"github.com/looprig/harness/pkg/llm/codec/anthropicapi"
)

// FuzzDecode ensures the two untrusted-input parsers — DecodeResponse (a full
// Messages response body) and Codec.DecodeEvent (one de-framed SSE event) — never
// panic on arbitrary bytes. Both are fed each input because either can receive
// hostile or truncated provider data. A single target keeps `-fuzz=Fuzz` matching
// exactly one test (Go refuses to fuzz when more than one matches).
//
// Run: GOWORK=off go test -run '^$' -fuzz=Fuzz -fuzztime=30s ./pkg/llm/codec/anthropicapi/
func FuzzDecode(f *testing.F) {
	// Response-shaped seeds.
	f.Add([]byte(`{"id":"1","type":"message","role":"assistant","model":"claude","content":[{"type":"text","text":"hi"}],"usage":{"input_tokens":3,"output_tokens":2}}`))
	f.Add([]byte(`{"type":"error","error":{"type":"overloaded_error","message":"overloaded"}}`))
	f.Add([]byte(`{"type":"message","content":[{"type":"tool_use","id":"t1","name":"run","input":{"x":1}},{"type":"thinking","thinking":"hm","signature":"s"}]}`))
	f.Add([]byte(`{"content":[]}`))
	// Event-shaped seeds.
	f.Add([]byte(`{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"t1","name":"run"}}`))
	f.Add([]byte(`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}`))
	f.Add([]byte(`{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"hm"}}`))
	f.Add([]byte(`{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"x\":"}}`))
	f.Add([]byte(`{"type":"message_stop"}`))
	// Degenerate seeds.
	f.Add([]byte(`{}`))
	f.Add([]byte(`invalid json`))
	f.Add([]byte(``))

	f.Fuzz(func(t *testing.T, data []byte) {
		// Must not panic regardless of input; error returns are expected and ignored.
		_, _ = anthropicapi.DecodeResponse(data)
		_, _ = anthropicapi.Codec{}.DecodeEvent(data)
	})
}
