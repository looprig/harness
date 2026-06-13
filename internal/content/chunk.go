package content

// ChunkType discriminates which payload field of a Chunk is populated.
type ChunkType string

const (
	ChunkTypeText     ChunkType = "text_delta"
	ChunkTypeThinking ChunkType = "thinking_delta"
)

// Chunk is the tagged union for streaming content deltas.
// Separate from Block because complete blocks have fields only valid at end-of-stream
// (e.g. ThinkingBlock.Signature).
type Chunk struct {
	Type     ChunkType
	Text     *TextChunk
	Thinking *ThinkingChunk
}

type TextChunk struct{ Text string }

type ThinkingChunk struct{ Thinking string }
