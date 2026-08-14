package loopruntime

import (
	"github.com/looprig/core/content"
	"github.com/looprig/harness/pkg/event"
)

// chunkProcessor owns the per-chunk ordering of the loop's streaming path: for
// each streamed chunk it first emits the live TokenDelta event, then folds the
// chunk into the blockState. It does NOT finalize the message; blockState.
// AIMessage() materializes the assistant message after the stream reaches EOF.
//
// The chunk layer lives in loop because event emission is loop behavior; the
// folding is delegated to internal/content/streamaccumulator (via blockState),
// which stays pure and imports no loop events.
type chunkProcessor struct {
	emit  func(event.Event)
	state chunkState
}

// chunkState is the mutable state a chunkProcessor folds into: the blockState
// accumulating the assistant message's blocks.
type chunkState struct {
	blocks *blockState
}

// newChunkProcessor constructs a chunkProcessor over an emit sink and the
// block-accumulation state.
func newChunkProcessor(emit func(event.Event), state chunkState) chunkProcessor {
	return chunkProcessor{emit: emit, state: state}
}

// process handles one streamed chunk: it emits the live TokenDelta for the chunk
// FIRST, then folds the chunk into the blockState, dispatching by the chunk's
// concrete type — one arm per variant of core's sealed Chunk union. Emission and
// accumulation are independent: a chunk is folded even when the emit sink drops
// it.
//
// Every variant MUST have an arm. A chunk that is emitted as a live TokenDelta
// but never accumulated is the worst shape of loss available here, because the
// live display shows the model producing content that the materialized message,
// the committed history, and the next request all lack. That is how streamed
// refusals and streamed images were dropped: the consumer watched the model
// decline, and the turn recorded nothing.
func (p chunkProcessor) process(chunk content.Chunk, turnIndex event.TurnIndex) {
	p.emit(event.TokenDelta{TurnIndex: turnIndex, Chunk: chunk})
	p.state.blocks.msgs.add(chunk)
}
