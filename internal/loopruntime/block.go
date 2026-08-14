package loopruntime

import (
	"sort"

	"github.com/looprig/core/content"
	"github.com/looprig/core/content/streamaccumulator"
)

// blockState is the assistant block state for one AIMessage: thinking, text, and
// tool-use blocks accumulated from chunks. The zero value is ready to use.
//
// Phase 10 (Open Items A) validated the thin `block{state blockState}` wrapper
// against real code and COLLAPSED it: it was a one-field struct with no methods
// and no runtime role beyond holding its state — every caller reached straight
// through to blockState. blockState carries the materialization methods
// (AIMessage/ToolUses) directly, so the wrapper added no value (YAGNI). The step
// wrapper was collapsed for the same reason; the turn runtime state is the real
// one and is owned by the actor.
type blockState struct {
	msgs blockMessages
}

// blockMessages holds the stream accumulators that fold streamed chunks into
// complete content blocks — one per chunk variant core's sealed union declares.
// Each is a pure converter from core/content/streamaccumulator; the loop owns
// event emission and policy, not these.
type blockMessages struct {
	thinking streamaccumulator.Thinking
	text     streamaccumulator.Text
	refusal  streamaccumulator.Refusal
	images   streamaccumulator.Images
	toolUses streamaccumulator.ToolUses
	order    []blockKey
	seen     map[blockKey]struct{}
}

type blockKind uint8

const (
	blockKindThinking blockKind = iota
	blockKindText
	blockKindRefusal
	blockKindImage
	blockKindToolUse
)

type blockKey struct {
	kind  blockKind
	index int
}

func (m *blockMessages) add(chunk content.Chunk) {
	var key blockKey
	switch c := chunk.(type) {
	case *content.TextChunk:
		key = blockKey{kind: blockKindText}
		m.text.Add(c)
	case *content.ThinkingChunk:
		key = blockKey{kind: blockKindThinking, index: c.Index}
		m.thinking.Add(c)
	case *content.ToolUseChunk:
		key = blockKey{kind: blockKindToolUse, index: c.Index}
		m.toolUses.Add(c)
	case *content.RefusalChunk:
		key = blockKey{kind: blockKindRefusal}
		m.refusal.Add(c)
	case *content.ImageChunk:
		key = blockKey{kind: blockKindImage, index: c.Index}
		m.images.Add(c)
	default:
		return
	}
	if m.seen == nil {
		m.seen = make(map[blockKey]struct{})
	}
	if _, ok := m.seen[key]; !ok {
		m.seen[key] = struct{}{}
		insert := len(m.order)
		for i, existing := range m.order {
			if existing.kind == key.kind && existing.index > key.index {
				insert = i
				break
			}
		}
		m.order = append(m.order, blockKey{})
		copy(m.order[insert+1:], m.order[insert:])
		m.order[insert] = key
	}
}

// AIMessage materializes the single assistant message from the accumulated
// blocks in provider emission order. Deltas for the same indexed block fold
// into that block without adding another position. An empty accumulator
// contributes no block; an all-empty blockState yields an AIMessage with no
// blocks. The tool-use blocks carry the RAW concatenated Input verbatim; any
// validation or sanitization of the stored message is the caller's policy.
//
// The refusal sits with the reply content it belongs to rather than at the end,
// because it IS the reply on a turn that has one: a refusal and ordinary text
// are mutually exclusive answers to the same request. It materializes on the
// accumulator having RECEIVED a delta, never on the accumulated text being
// non-empty — a provider may decline with no explanation, and dropping that
// block restores the bug the variant exists to prevent: a refusal decoding as a
// successful empty reply.
//
// Every reasoning block is emitted, not just the first. A response may
// legitimately contain several: Anthropic interleaved thinking opens a fresh
// thinking / redacted_thinking block around every tool call, and each block
// carries its OWN signature or opaque provider state that the next request must
// replay block-for-block. Keeping only the lowest-index block dropped blocks
// 2..N together with their continuation state.
func (b *blockState) AIMessage() *content.AIMessage {
	blocks := b.msgs.blocksInOrder()
	return &content.AIMessage{Message: content.Message{Role: content.RoleAssistant, Blocks: blocks}}
}

func (m *blockMessages) blocksInOrder() []content.Block {
	if len(m.order) == 0 {
		return nil
	}
	materialized := make(map[blockKey]content.Block, len(m.order))
	assignIndexed := func(kind blockKind, blocks []content.Block) {
		indexes := make([]int, 0, len(blocks))
		for _, key := range m.order {
			if key.kind == kind {
				indexes = append(indexes, key.index)
			}
		}
		sort.Ints(indexes)
		for i, index := range indexes {
			materialized[blockKey{kind: kind, index: index}] = blocks[i]
		}
	}

	thinking := m.thinking.Blocks()
	thinkingBlocks := make([]content.Block, len(thinking))
	for i := range thinking {
		thinkingBlocks[i] = &thinking[i]
	}
	assignIndexed(blockKindThinking, thinkingBlocks)

	images := m.images.Blocks()
	imageBlocks := make([]content.Block, len(images))
	for i := range images {
		imageBlocks[i] = &images[i]
	}
	assignIndexed(blockKindImage, imageBlocks)

	toolUses := m.toolUses.Blocks()
	toolUseBlocks := make([]content.Block, len(toolUses))
	for i := range toolUses {
		toolUseBlocks[i] = &toolUses[i]
	}
	assignIndexed(blockKindToolUse, toolUseBlocks)

	if text := m.text.Block(); text != nil {
		materialized[blockKey{kind: blockKindText}] = text
	}
	if refusal := m.refusal.Block(); refusal != nil {
		materialized[blockKey{kind: blockKindRefusal}] = refusal
	}

	blocks := make([]content.Block, 0, len(m.order))
	for _, key := range m.order {
		blocks = append(blocks, materialized[key])
	}
	return blocks
}

// ToolUses returns the executable view of the tool-use blocks contained in the
// assistant message, in ascending Index order, with their RAW concatenated
// Input. This is a distinct allocation from the AIMessage's child tool-use
// blocks, so the caller may sanitize the stored message's Input independently of
// the raw executable view. It returns nil when no tool-use chunk was folded.
func (b *blockState) ToolUses() []content.ToolUseBlock {
	return b.msgs.toolUses.Blocks()
}
