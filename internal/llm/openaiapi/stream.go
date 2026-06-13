// internal/llm/openaiapi/stream.go
package openaiapi

import (
	"encoding/json"
	"io"

	"github.com/inventivepotter/urvi/internal/content"
	"github.com/inventivepotter/urvi/internal/llm"
)

// NewStream constructs a StreamReader[content.Chunk] from an HTTP response body
// containing OpenAI SSE events. The caller must Close the reader when done.
func NewStream(body io.ReadCloser) *llm.StreamReader[content.Chunk] {
	sse := NewSSEReader(body)
	return llm.NewStreamReader(func() (content.Chunk, error) {
		for {
			line, err := sse.Next()
			if err != nil {
				return content.Chunk{}, err
			}
			var ev sseChunk
			if err := json.Unmarshal([]byte(line), &ev); err != nil {
				continue // skip malformed lines
			}
			if len(ev.Choices) == 0 {
				continue
			}
			delta := ev.Choices[0].Delta

			if delta.ReasoningContent != "" {
				return content.Chunk{
					Type:     content.ChunkTypeThinking,
					Thinking: &content.ThinkingChunk{Thinking: delta.ReasoningContent},
				}, nil
			}
			if delta.Content != "" {
				return content.Chunk{
					Type: content.ChunkTypeText,
					Text: &content.TextChunk{Text: delta.Content},
				}, nil
			}
			// Empty delta (role-only or finish): keep reading.
		}
	}, body.Close)
}
