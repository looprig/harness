package llm

import "github.com/looprig/harness/pkg/content"

// Codec owns one wire dialect's JSON + stream-event semantics. It does NOT own wire framing:
// the transport de-frames the response (SSE / AWS eventstream, via the shared codec/sse helper)
// and hands the codec one already-de-framed event payload at a time.
type Codec interface {
	EncodeRequest(req Request, mode RequestMode) ([]byte, error) // typed mode, not a bool
	DecodeResponse(body []byte) (*Response, error)               // non-streaming body → Response
	DecodeEvent(event []byte) ([]content.Chunk, error)           // one de-framed stream event → chunks
}
