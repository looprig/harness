package content

// Chunk is the sealed interface over streaming content deltas. Separate from
// Block because complete blocks have fields only valid at end-of-stream (e.g.
// ThinkingBlock.Signature). Chunks are never serialized, so there is no codec
// and no ChunkType wire tag.
type Chunk interface{ isChunk() }

func (*TextChunk) isChunk()     {}
func (*ThinkingChunk) isChunk() {}
func (*ToolUseChunk) isChunk()  {}

type TextChunk struct{ Text string }

type ThinkingChunk struct{ Thinking string }

// ToolUseChunk is a streaming delta of a tool call. Providers emit these as they
// parse function-call deltas; the runner accumulates by Index into a ToolUseBlock.
type ToolUseChunk struct {
	Index     int    // tool call's position in the response
	ID        string // tool_use id (may arrive only on the first delta for this Index)
	Name      string // tool name (likewise)
	InputJSON string // partial JSON delta of the arguments
}
