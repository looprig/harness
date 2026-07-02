// pkg/llm/codec/openaiapi/stream.go
package openaiapi

import (
	"io"

	"github.com/ciram-co/looprig/pkg/content"
	"github.com/ciram-co/looprig/pkg/llm"
	"github.com/ciram-co/looprig/pkg/llm/codec/sse"
)

// NewStream constructs a StreamReader[content.Chunk] from an HTTP response body
// containing OpenAI SSE events. The caller must Close the reader when done.
//
// It de-frames each SSE data line via the shared sse.Reader and decodes it with
// decodeEvent — the exact same per-event logic Codec.DecodeEvent exposes — then
// emits one chunk per Next(). A single delta line that yields more than one chunk
// (multiple tool-call entries) is buffered in pending and drained one per call so
// none are dropped. Retained for chutes and the existing OpenAI-dialect tests;
// the transport's StreamChunks is the codec-injected equivalent.
func NewStream(body io.ReadCloser) *llm.StreamReader[content.Chunk] {
	reader := sse.NewReader(body)
	// pending holds chunks left over from a single SSE line that decoded to more
	// than one chunk; they are drained before the next line is read.
	var pending []content.Chunk
	return llm.NewStreamReader(func() (content.Chunk, error) {
		for {
			if len(pending) > 0 {
				c := pending[0]
				pending = pending[1:]
				return c, nil
			}
			line, err := reader.Next()
			if err != nil {
				return nil, err
			}
			chunks, err := decodeEvent([]byte(line))
			if err != nil {
				return nil, err
			}
			if len(chunks) == 0 {
				continue // malformed / empty / role-only: keep reading
			}
			pending = chunks[1:]
			return chunks[0], nil
		}
	}, body.Close)
}
