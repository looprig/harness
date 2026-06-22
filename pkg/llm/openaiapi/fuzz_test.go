// internal/llm/openaiapi/fuzz_test.go
package openaiapi_test

import (
	"testing"

	"github.com/ciram-co/looprig/pkg/llm/openaiapi"
)

// FuzzDecodeResponse ensures DecodeResponse never panics on arbitrary input.
// Run with: go test -fuzz=FuzzDecodeResponse ./internal/llm/openaiapi/... -fuzztime=30s
func FuzzDecodeResponse(f *testing.F) {
	// Seed corpus: valid responses.
	f.Add([]byte(`{"id":"1","model":"gpt-4o","choices":[{"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":2}}`))
	f.Add([]byte(`{"id":"2","model":"o3","choices":[{"message":{"role":"assistant","content":"","reasoning_content":"thinking"},"finish_reason":"stop"}]}`))
	f.Add([]byte(`{"id":"3","model":"m","choices":[]}`))
	f.Add([]byte(`{}`))
	f.Add([]byte(`invalid json`))

	f.Fuzz(func(t *testing.T, data []byte) {
		// Must not panic regardless of input.
		_, _ = openaiapi.DecodeResponse(data)
	})
}
