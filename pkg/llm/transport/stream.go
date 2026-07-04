package transport

import (
	"io"

	"github.com/looprig/core/content"
	"github.com/looprig/harness/pkg/llm"
	"github.com/looprig/harness/pkg/llm/codec/sse"
)

// StreamChunks adapts an SSE response body plus a Codec into a
// StreamReader[content.Chunk]. It de-frames each event with the shared sse.Reader,
// hands the payload to codec.DecodeEvent, and emits the resulting chunk(s) one per
// Next() — a single event that decodes to several chunks (e.g. multiple tool-call
// entries) is buffered in pending and drained across successive calls so none are
// dropped. Next does NOT close on io.EOF; the caller must Close the returned
// reader when done (same contract as openaiapi.NewStream), and that Close is what
// runs the closer that closes body.
func StreamChunks(body io.ReadCloser, codec llm.Codec) *llm.StreamReader[content.Chunk] {
	reader := sse.NewReader(body)
	var pending []content.Chunk
	return llm.NewStreamReader(func() (content.Chunk, error) {
		for {
			if len(pending) > 0 {
				c := pending[0]
				pending = pending[1:]
				return c, nil
			}
			payload, err := reader.Next()
			if err != nil {
				return nil, err
			}
			chunks, err := codec.DecodeEvent([]byte(payload))
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
