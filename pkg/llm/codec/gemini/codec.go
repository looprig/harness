// pkg/llm/codec/gemini/codec.go
package gemini

import (
	"encoding/json"

	"github.com/looprig/core/content"
	"github.com/looprig/harness/pkg/llm"
)

// Codec is the Google Gemini generateContent wire dialect expressed as an
// llm.Codec. It is stateless (an empty struct with value-receiver methods), so
// one value is safely shared across goroutines: the transport owns wire framing
// (SSE de-framing via the shared codec/sse helper), the Codec owns only the JSON
// body + per-event semantics. Methods delegate to package-level free functions so
// the two surfaces cannot diverge.
type Codec struct{}

// Compile-time proof that Codec honors the full llm.Codec contract.
var _ llm.Codec = Codec{}

// EncodeRequest encodes the request body. The RequestMode is intentionally
// ignored: Gemini's generateContent and streamGenerateContent bodies are
// identical — streaming is chosen by the transport via the endpoint + `?alt=sse`,
// not a body field — so Invoke and Stream produce the same bytes.
func (Codec) EncodeRequest(req llm.Request, _ llm.RequestMode) ([]byte, error) {
	return EncodeRequest(req)
}

// DecodeResponse parses a non-streaming Gemini generateContent body, delegating
// to the free DecodeResponse.
func (Codec) DecodeResponse(body []byte) (*llm.Response, error) {
	return DecodeResponse(body)
}

// DecodeEvent decodes one already-de-framed streamGenerateContent chunk (a
// partial GenerateContentResponse) into the chunk(s) it yields. It is tolerant by
// contract (matching the transport's StreamChunks): malformed JSON and a chunk
// with no candidates return (nil, nil) — a skip, not an error. DecodeEvent is
// stateless; cross-event assembly happens downstream in the stream accumulator.
func (Codec) DecodeEvent(event []byte) ([]content.Chunk, error) {
	return decodeEvent(event)
}

// decodeEvent is the single per-event decoder. It maps candidates[0].content
// parts to chunks in order: a functionCall part -> ToolUseChunk, a thought-tagged
// text part -> ThinkingChunk, any other non-empty text part -> TextChunk. Empty
// and unknown parts are skipped, so an event with nothing to emit returns
// (nil, nil).
//
// Tool-call Index: Gemini sends a complete functionCall (full name + args) per
// part and provides no index, so Index is the positional order of functionCall
// parts WITHIN this event. This is correct for the common case where a turn's
// (parallel) calls arrive together in one chunk; distinct calls split across
// separate chunks would collide on Index — a known stateless-decoder limitation.
func decodeEvent(payload []byte) ([]content.Chunk, error) {
	var ev GenerateContentResponse
	if err := json.Unmarshal(payload, &ev); err != nil {
		return nil, nil // skip malformed lines
	}
	if len(ev.Candidates) == 0 {
		return nil, nil
	}

	var out []content.Chunk
	fnIndex := 0
	for _, p := range ev.Candidates[0].Content.Parts {
		switch {
		case p.FunctionCall != nil:
			out = append(out, &content.ToolUseChunk{
				Index:     fnIndex,
				ID:        p.FunctionCall.ID,
				Name:      p.FunctionCall.Name,
				InputJSON: argsString(p.FunctionCall.Args),
			})
			fnIndex++
		case p.Thought && p.Text != "":
			out = append(out, &content.ThinkingChunk{Thinking: p.Text})
		case p.Text != "":
			out = append(out, &content.TextChunk{Text: p.Text})
		}
	}
	return out, nil
}

// argsString renders a streamed functionCall's arguments as the InputJSON string
// the accumulator concatenates. Gemini delivers the complete args object in one
// chunk, so this is the full JSON; an empty payload becomes "{}".
func argsString(raw json.RawMessage) string {
	if len(raw) == 0 {
		return "{}"
	}
	return string(raw)
}
