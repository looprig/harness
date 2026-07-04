package inference

import "github.com/looprig/core/content"

// FramesToChunks adapts a raw StreamFrame reader plus a per-frame semantic mapper into
// a content.Chunk stream. A StreamDecoder built on a wire framer (sse, ndjson, …) uses
// it to keep the frame-draining, multi-chunk buffering, and body-close plumbing in one
// place while supplying only its codec's per-frame decode logic.
//
// mapFrame maps one raw frame to zero or more chunks:
//   - returning (chunks, nil) yields those chunks (buffered so a frame that decodes to
//     several chunks drops none);
//   - returning (nil, nil) is a tolerant skip (malformed or uninteresting frame);
//   - returning a non-nil error ends the stream with that error. Returning io.EOF is
//     how a codec signals a terminal sentinel such as OpenAI's [DONE].
//
// The returned reader's Close delegates to frames.Close, so the wire framer keeps
// ownership of the underlying body.
func FramesToChunks(frames *StreamReader[StreamFrame], mapFrame func(StreamFrame) ([]content.Chunk, error)) *StreamReader[content.Chunk] {
	var pending []content.Chunk
	next := func() (content.Chunk, error) {
		for {
			if len(pending) > 0 {
				c := pending[0]
				pending = pending[1:]
				return c, nil
			}
			frame, err := frames.Next()
			if err != nil {
				return nil, err
			}
			chunks, err := mapFrame(frame)
			if err != nil {
				return nil, err
			}
			if len(chunks) == 0 {
				continue // tolerant skip: read the next frame
			}
			pending = chunks[1:]
			return chunks[0], nil
		}
	}
	return NewStreamReader(next, frames.Close)
}
