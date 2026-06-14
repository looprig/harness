package content

// Chunk is the sealed interface over streaming content deltas. Separate from
// Block because complete blocks have fields only valid at end-of-stream (e.g.
// ThinkingBlock.Signature). Chunks are never serialized, so there is no codec
// and no ChunkType wire tag.
type Chunk interface{ isChunk() }

func (*TextChunk) isChunk()     {}
func (*ThinkingChunk) isChunk() {}

type TextChunk struct{ Text string }

type ThinkingChunk struct{ Thinking string }
