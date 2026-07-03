// pkg/llm/codec/openaiapi/codec.go
package openaiapi

import (
	"encoding/json"

	"github.com/looprig/harness/pkg/content"
	"github.com/looprig/harness/pkg/llm"
)

// Codec is the OpenAI Chat Completions wire dialect expressed as an llm.Codec.
// It is stateless (an empty struct with value-receiver methods), so one value is
// safely shared across goroutines: the transport owns wire framing (SSE), the
// Codec owns only JSON body + per-event semantics. The methods delegate to the
// package-level free functions kept for chutes and the existing tests, so the two
// surfaces cannot diverge.
type Codec struct{}

// Compile-time proof that Codec honors the full llm.Codec contract.
var _ llm.Codec = Codec{}

// EncodeRequest translates the typed RequestMode into the free EncodeRequest's
// stream bool: RequestModeStream sets "stream":true, every other mode omits it.
func (Codec) EncodeRequest(req llm.Request, mode llm.RequestMode) ([]byte, error) {
	return EncodeRequest(req, mode == llm.RequestModeStream)
}

// DecodeResponse parses a non-streaming OpenAI chat completions body, delegating
// to the free DecodeResponse.
func (Codec) DecodeResponse(body []byte) (*llm.Response, error) {
	return DecodeResponse(body)
}

// DecodeEvent decodes one already-de-framed SSE data payload into the chunk(s) it
// yields. It is tolerant by contract (matching NewStream): malformed JSON, an
// event with no choices, and role-only/empty deltas return (nil, nil) — a skip,
// not an error — while a single delta line carrying multiple tool-call entries
// returns all of them. DecodeEvent is stateless: cross-event tool-argument
// assembly happens downstream in the stream accumulator, not here.
func (Codec) DecodeEvent(event []byte) ([]content.Chunk, error) {
	return decodeEvent(event)
}

// decodeEvent is the single per-event decoder shared by Codec.DecodeEvent and
// NewStream. NewStream drives it one SSE line at a time (buffering the
// multi-tool-call case and draining one chunk per Next()); Codec.DecodeEvent
// hands it a de-framed payload directly. Precedence — reasoning, then text, then
// tool calls — is preserved from the original NewStream loop, so at most one kind
// of chunk is produced per event.
func decodeEvent(payload []byte) ([]content.Chunk, error) {
	var ev sseChunk
	if err := json.Unmarshal(payload, &ev); err != nil {
		return nil, nil // skip malformed lines
	}
	if len(ev.Choices) == 0 {
		return nil, nil
	}
	delta := ev.Choices[0].Delta

	if delta.ReasoningContent != "" {
		return []content.Chunk{&content.ThinkingChunk{Thinking: delta.ReasoningContent}}, nil
	}
	if delta.Content != "" {
		return []content.Chunk{&content.TextChunk{Text: delta.Content}}, nil
	}
	if len(delta.ToolCalls) > 0 {
		var out []content.Chunk
		for _, tc := range delta.ToolCalls {
			// Drop wholly-empty entries (no id, name, or argument fragment).
			if tc.ID == "" && tc.Function.Name == "" && tc.Function.Arguments == "" {
				continue
			}
			out = append(out, &content.ToolUseChunk{
				Index:     tc.Index,
				ID:        tc.ID,
				Name:      tc.Function.Name,
				InputJSON: tc.Function.Arguments,
			})
		}
		return out, nil
	}
	// Empty delta (role-only or finish): no chunk.
	return nil, nil
}
