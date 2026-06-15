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
	// pending holds tool-call entries left over from a single SSE delta line that
	// carried more than one entry. The loop emits one chunk per Next(); these are
	// drained before the next SSE line is read so multi-entry lines are not dropped.
	var pending []sseToolCallDelta
	return llm.NewStreamReader(func() (content.Chunk, error) {
		for {
			// Drain any buffered tool-call entries from a prior multi-entry line first.
			if len(pending) > 0 {
				tc := pending[0]
				pending = pending[1:]
				return &content.ToolUseChunk{
					Index:     tc.Index,
					ID:        tc.ID,
					Name:      tc.Function.Name,
					InputJSON: tc.Function.Arguments,
				}, nil
			}

			line, err := sse.Next()
			if err != nil {
				return nil, err
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
				return &content.ThinkingChunk{Thinking: delta.ReasoningContent}, nil
			}
			if delta.Content != "" {
				return &content.TextChunk{Text: delta.Content}, nil
			}
			if len(delta.ToolCalls) > 0 {
				// Buffer this line's entries (dropping wholly-empty ones) and let the
				// drain branch at the top of the loop emit them one per Next().
				for _, tc := range delta.ToolCalls {
					if tc.ID == "" && tc.Function.Name == "" && tc.Function.Arguments == "" {
						continue
					}
					pending = append(pending, tc)
				}
				continue
			}
			// Empty delta (role-only or finish): keep reading.
		}
	}, body.Close)
}
