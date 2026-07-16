package codec

import (
	"io"
	"net/http"

	"github.com/looprig/core/content"
	"github.com/looprig/inference"
	"github.com/looprig/inference/stream"
)

// EncodedRequest is a semantic API encoding of a Request: the wire headers the encoder wants
// applied and the single-shot request body. Body is consumed exactly once — the generic
// transport must not retry or replay it.
type EncodedRequest struct {
	Header http.Header
	Body   io.Reader
}

// RequestEncoder maps a provider-neutral Request into wire form. It handles normal request
// bodies, including streaming-mode bodies when the wire API represents streaming as a JSON flag.
type RequestEncoder interface {
	EncodeRequest(req inference.Request, mode RequestMode) (EncodedRequest, error)
}

// ResponseDecoder decodes a non-streaming response from an already-drained successful body.
// The transport owns HTTP status mapping, body closing, and read errors.
type ResponseDecoder interface {
	DecodeResponse(body []byte) (*inference.Response, error)
}

// StreamFrame is a raw stream event: an optional event Name, optional Metadata, and the Data
// payload. It is wire-level, not semantic — a StreamDecoder maps frames to content.Chunk.
// StreamFramer converts a streaming body into raw stream events. It owns closing that body
// through the returned StreamReader; if it returns an error before returning a reader, it must
// close the body before returning.
type StreamFramer interface {
	DecodeStreamFrames(body io.ReadCloser) (*stream.StreamReader[stream.StreamFrame], error)
}

// StreamDecoder owns the full streaming response path from a successful HTTP response to
// content.Chunk. Once DecodeStream is called it owns resp.Body; if it returns an error before
// returning a reader, it must close resp.Body before returning.
type StreamDecoder interface {
	DecodeStream(resp *http.Response) (*stream.StreamReader[content.Chunk], error)
}

// Codec is the non-streaming composition of request encoding and response decoding. It does
// NOT embed StreamDecoder: streaming is optional, so a non-streaming API satisfies Codec
// without stubbing DecodeStream.
type Codec interface {
	RequestEncoder
	ResponseDecoder
}

// StreamingCodec is a Codec that also decodes streaming responses.
type StreamingCodec interface {
	Codec
	StreamDecoder
}
