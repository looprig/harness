package gemini_test

import (
	"testing"

	"github.com/ciram-co/looprig/pkg/llm/codec/gemini"
)

// FuzzDecode ensures the two untrusted-input parsers — DecodeResponse (a full
// generateContent body) and Codec.DecodeEvent (one de-framed streamGenerateContent
// chunk) — never panic on arbitrary bytes. Both are fed each input because either
// can receive hostile or truncated provider data. A single target keeps
// `-fuzz=Fuzz` matching exactly one test (Go refuses to fuzz when more than one
// matches).
//
// Run: GOWORK=off go test -run '^$' -fuzz=Fuzz -fuzztime=30s ./pkg/llm/codec/gemini/
func FuzzDecode(f *testing.F) {
	// Response / chunk-shaped seeds (identical shape for generateContent + stream).
	f.Add([]byte(`{"candidates":[{"content":{"role":"model","parts":[{"text":"hi"}]},"finishReason":"STOP","index":0}],"usageMetadata":{"promptTokenCount":3,"candidatesTokenCount":2},"modelVersion":"gemini-2.5-flash"}`))
	f.Add([]byte(`{"candidates":[{"content":{"parts":[{"functionCall":{"id":"c1","name":"run","args":{"x":1}}}]}}]}`))
	f.Add([]byte(`{"candidates":[{"content":{"parts":[{"text":"thought","thought":true},{"text":"answer"}]}}]}`))
	f.Add([]byte(`{"candidates":[]}`))
	// Degenerate seeds.
	f.Add([]byte(`{}`))
	f.Add([]byte(`invalid json`))
	f.Add([]byte(``))

	f.Fuzz(func(t *testing.T, data []byte) {
		// Must not panic regardless of input; error returns are expected and ignored.
		_, _ = gemini.DecodeResponse(data)
		_, _ = gemini.Codec{}.DecodeEvent(data)
	})
}
