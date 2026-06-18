// Package streamaccumulator folds streaming content chunks into complete
// content blocks. It is a pure converter shared by the loop and the TUI/CLI live
// display path:
//
//	ThinkingChunk -> ThinkingBlock
//	TextChunk     -> TextBlock
//	ToolUseChunk  -> ToolUseBlock
//
// It does NOT send events, validate tool permissions, decide turn failure, or
// know about the loop. Policy stays in the loop; this package only converts.
// It deliberately imports nothing beyond the standard library and
// internal/content (in particular, never internal/agent/loop or its event
// package) so it carries no dependency cycle.
package streamaccumulator

import (
	"encoding/json"
	"sort"
	"strings"

	"github.com/inventivepotter/urvi/internal/content"
)

// ToolUses folds streaming ToolUseChunk deltas into complete ToolUseBlocks. It is
// keyed by the provider-supplied Index (which is provider/attacker-influenced),
// so it uses a map rather than slice indexing: a negative or huge Index can NEVER
// panic or allocate an unbounded slice. The first delta for an Index typically
// carries ID/Name; later deltas carry InputJSON fragments to concatenate.
// Blocks() emits the assembled blocks in ASCENDING Index order (the deterministic
// response order). The zero value is ready to use.
type ToolUses struct {
	parts map[int]*toolPart
	order []int // Index values in first-seen order; sorted ascending by Blocks()
}

type toolPart struct {
	id    string
	name  string
	input strings.Builder
}

// Add folds one delta into the accumulator, bounds-safe on any Index value.
func (a *ToolUses) Add(chunk *content.ToolUseChunk) {
	if a.parts == nil {
		a.parts = make(map[int]*toolPart)
	}
	p, ok := a.parts[chunk.Index]
	if !ok {
		p = &toolPart{}
		a.parts[chunk.Index] = p
		a.order = append(a.order, chunk.Index)
	}
	// ID/Name arrive on the first delta for an Index; never overwrite a set value
	// with a later empty fragment (last non-empty value wins).
	if chunk.ID != "" {
		p.id = chunk.ID
	}
	if chunk.Name != "" {
		p.name = chunk.Name
	}
	p.input.WriteString(chunk.InputJSON)
}

// Blocks returns the assembled ToolUseBlocks in ascending Index order, or nil if
// no chunk was received. The raw concatenated Input is used verbatim; any
// validation or sanitization happens in the caller.
func (a ToolUses) Blocks() []content.ToolUseBlock {
	if len(a.order) == 0 {
		return nil
	}
	idx := make([]int, len(a.order))
	copy(idx, a.order)
	sort.Ints(idx)
	out := make([]content.ToolUseBlock, 0, len(idx))
	for _, i := range idx {
		p := a.parts[i]
		out = append(out, content.ToolUseBlock{
			ID:    p.id,
			Name:  p.name,
			Input: json.RawMessage(p.input.String()),
		})
	}
	return out
}

// Empty reports whether no chunk has been added yet.
func (a ToolUses) Empty() bool { return len(a.order) == 0 }
