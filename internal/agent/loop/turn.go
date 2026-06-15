package loop

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"sort"
	"strings"

	"github.com/inventivepotter/urvi/internal/agent/loop/event"
	"github.com/inventivepotter/urvi/internal/content"
	"github.com/inventivepotter/urvi/internal/llm"
	"github.com/inventivepotter/urvi/internal/tool"
)

// emptyArgs is the canonical valid-JSON argument object a malformed tool-call's
// Input is sanitized to BEFORE it is stored in history, so the stored assistant
// message always re-encodes cleanly. The raw (invalid) Input is still handed to
// RunBatch, which reports it as a pre-execution failure.
var emptyArgs = json.RawMessage("{}")

// runTurn drives the agentic loop for one user turn: it re-streams after each
// tool batch until the model returns no tool calls (TurnDone), the runaway guard
// fires (TurnFailed{ToolLimitError}), the provider errors (TurnFailed), or the
// turn is cancelled (TurnInterrupted). It returns updated history and the
// terminal event.
//
// Whole-turn rollback: base := len(msgs) is snapshotted at turn start; any
// abnormal exit truncates msgs[:base], discarding the ENTIRE turn's exchange so
// history never holds a tool_use without its matching ToolMessage (which a strict
// provider would reject on the next request). Only a TurnDone path keeps the
// turn — and only well-formed, fully paired exchanges reach a TurnDone.
func runTurn(
	ctx context.Context,
	input []content.Block,
	turnIndex event.TurnIndex,
	msgs content.AgenticMessages,
	cfg Config,
	client llm.LLM,
	gateReg chan<- gateRegistration,
	emit func(event.Event),
) (content.AgenticMessages, event.Event) {
	base := len(msgs)
	rollback := func() content.AgenticMessages { return msgs[:base] }

	userMsg := &content.UserMessage{
		Message: content.Message{Role: content.RoleUser, Blocks: input},
	}
	msgs = append(msgs, userMsg)
	emit(event.TurnStarted{TurnIndex: turnIndex})

	defs := toolDefs(ctx, cfg.Tools.Registry)

	var iters, calls int
	for {
		req := llm.Request{Model: cfg.Model, Messages: msgs, Tools: defs}

		aiMsg, toolUses, term, ok := streamOnce(ctx, req, turnIndex, client, emit)
		if !ok {
			// streamOnce returned a terminal (provider error / empty response /
			// interrupt): whole-turn rollback.
			return rollback(), term
		}
		msgs = append(msgs, aiMsg)

		// Text-only completion ALWAYS wins, regardless of iteration count: the
		// runaway cap is only checked when the model wants ANOTHER tool batch.
		if len(toolUses) == 0 {
			return msgs, event.TurnDone{TurnIndex: turnIndex, Message: aiMsg}
		}

		iters++
		calls += len(toolUses)
		if iters > cfg.Tools.MaxToolIterations || calls > cfg.Tools.MaxToolCallsPerTurn {
			// The tool-call assistant message was just appended, but rollback
			// truncates to base BEFORE any further provider request is built — so
			// no unpaired tool_use ever survives into history.
			return rollback(), event.TurnFailed{
				TurnIndex: turnIndex,
				Err: &event.ToolLimitError{
					Iterations:    iters,
					MaxIterations: cfg.Tools.MaxToolIterations,
					Calls:         calls,
					MaxCalls:      cfg.Tools.MaxToolCallsPerTurn,
				},
			}
		}

		results := RunBatch(ctx, toolUses, cfg.Tools, gateReg, cfg.idGen, emit)
		if ctx.Err() != nil {
			// A cancelled batch's results are discarded; whole-turn rollback.
			return rollback(), event.TurnInterrupted{TurnIndex: turnIndex}
		}
		for _, r := range results {
			msgs = append(msgs, toolResultMessage(r))
		}
		// Loop: the next stream lets the model react to the tool results.
	}
}

// streamOnce streams one LLM response, emitting a TokenDelta per chunk and
// accumulating text/thinking into buffers and tool-call deltas (folded by Index)
// into ToolUseBlocks. It returns the assembled AIMessage and the tool-call blocks
// to execute. ok=false means a terminal event was produced (provider error,
// empty response, or interrupt) and the assembled message must be discarded.
//
// Each assembled tool-call block is validated (ID & Name non-empty, valid JSON
// Input). An invalid block is sanitized to emptyArgs IN THE STORED MESSAGE so
// history re-encodes cleanly; the block handed back for execution keeps its RAW
// folded Input so RunBatch detects the invalid JSON and reports it as a
// pre-execution failure (a model-visible, retry-able tool-result error).
func streamOnce(
	ctx context.Context,
	req llm.Request,
	turnIndex event.TurnIndex,
	client llm.LLM,
	emit func(event.Event),
) (*content.AIMessage, []content.ToolUseBlock, event.Event, bool) {
	sr, err := client.Stream(ctx, req)
	if err != nil {
		return nil, nil, streamFailure(ctx, turnIndex, err), false
	}
	defer func() {
		if cerr := sr.Close(); cerr != nil {
			slog.Warn("loop: stream close error", "error", cerr)
		}
	}()

	var textBuf, thinkBuf strings.Builder
	acc := newToolAccumulator()
	for {
		chunk, err := sr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, nil, streamFailure(ctx, turnIndex, err), false
		}
		emit(event.TokenDelta{TurnIndex: turnIndex, Chunk: chunk})
		switch c := chunk.(type) {
		case *content.TextChunk:
			textBuf.WriteString(c.Text)
		case *content.ThinkingChunk:
			thinkBuf.WriteString(c.Thinking)
		case *content.ToolUseChunk:
			acc.add(c)
		}
	}

	rawCalls := acc.blocks()

	// A successful stream with no content at all (no text, no thinking, no tool
	// calls) is a failure — the same controlled TurnFailed as the single-stream
	// loop, rather than appending an empty assistant message.
	if textBuf.Len() == 0 && thinkBuf.Len() == 0 && len(rawCalls) == 0 {
		return nil, nil, event.TurnFailed{TurnIndex: turnIndex, Err: &event.EmptyResponseError{}}, false
	}

	// Build the assistant message's blocks: thinking?, text?, then tool_use blocks
	// with invalid Input sanitized to {}. The raw calls are returned for execution.
	var blocks []content.Block
	if thinkBuf.Len() > 0 {
		blocks = append(blocks, &content.ThinkingBlock{Thinking: thinkBuf.String()})
	}
	if textBuf.Len() > 0 {
		blocks = append(blocks, &content.TextBlock{Text: textBuf.String()})
	}
	for _, c := range rawCalls {
		stored := c
		if !validToolCall(c) {
			stored.Input = emptyArgs
		}
		blocks = append(blocks, &stored)
	}

	aiMsg := &content.AIMessage{Message: content.Message{Role: content.RoleAssistant, Blocks: blocks}}
	return aiMsg, rawCalls, nil, true
}

// streamFailure maps a stream/provider error to the right terminal event: a
// cancelled ctx is an interrupt (whole-turn rollback, no error surfaced); any
// other error is a TurnFailed carrying the typed cause.
func streamFailure(ctx context.Context, turnIndex event.TurnIndex, err error) event.Event {
	if ctx.Err() != nil {
		return event.TurnInterrupted{TurnIndex: turnIndex}
	}
	return event.TurnFailed{TurnIndex: turnIndex, Err: err}
}

// validToolCall reports whether an assembled tool-call block is well-formed:
// non-empty ID and Name and valid-JSON Input. A malformed block is still handed
// to RunBatch (which reports the failure), but its STORED form is sanitized.
func validToolCall(b content.ToolUseBlock) bool {
	return b.ID != "" && b.Name != "" && json.Valid(b.Input)
}

// toolResultMessage wraps one tool result into a ToolMessage carrying the
// flattened result text (flattenToText is REUSED from runner.go: TextBlocks pass
// through; non-text → "[unsupported …]" placeholder; empty → "error: empty
// result") and the originating tool_use id, so the model pairs result↔call.
func toolResultMessage(r result) *content.ToolMessage {
	text := flattenToText(r.Content)
	return &content.ToolMessage{
		Message:   content.Message{Role: content.RoleTool, Blocks: []content.Block{&content.TextBlock{Text: text}}},
		ToolUseID: r.ToolUseID,
	}
}

// toolDefs maps each registered tool's Info(ctx) to an llm.Tool definition
// (ToolInfo.Schema is json.RawMessage, 1:1 with llm.Tool.Schema). A tool whose
// Info errors (or returns nil) is SKIPPED rather than aborting the turn or
// panicking: a misbehaving tool definition must not block all tool use. The skip
// is logged for observability.
func toolDefs(ctx context.Context, registry []tool.InvokableTool) []llm.Tool {
	if len(registry) == 0 {
		return nil
	}
	defs := make([]llm.Tool, 0, len(registry))
	for _, t := range registry {
		info, err := t.Info(ctx)
		if err != nil || info == nil {
			slog.Warn("loop: skipping tool with unavailable Info in tool definitions", "error", err)
			continue
		}
		defs = append(defs, llm.Tool{
			Name:        info.Name,
			Description: info.Desc,
			Schema:      info.Schema,
		})
	}
	return defs
}

// toolAccumulator folds streaming ToolUseChunk deltas into complete
// ToolUseBlocks. It is keyed by the provider-supplied Index (which is
// attacker/provider-influenced), so it uses a map rather than slice indexing: a
// negative or huge Index can NEVER panic or allocate an unbounded slice. The
// first delta for an Index carries ID/Name; later deltas carry InputJSON
// fragments to concatenate. blocks() emits the assembled blocks in ASCENDING
// Index order (the deterministic response order).
type toolAccumulator struct {
	parts map[int]*toolPart
	order []int // Index values in first-seen order; sorted ascending by blocks()
}

type toolPart struct {
	id    string
	name  string
	input strings.Builder
}

func newToolAccumulator() *toolAccumulator {
	return &toolAccumulator{parts: make(map[int]*toolPart)}
}

// add folds one delta into the accumulator, bounds-safe on any Index value.
func (a *toolAccumulator) add(c *content.ToolUseChunk) {
	p, ok := a.parts[c.Index]
	if !ok {
		p = &toolPart{}
		a.parts[c.Index] = p
		a.order = append(a.order, c.Index)
	}
	// ID/Name arrive on the first delta for an Index; never overwrite a set value
	// with a later empty fragment.
	if c.ID != "" {
		p.id = c.ID
	}
	if c.Name != "" {
		p.name = c.Name
	}
	p.input.WriteString(c.InputJSON)
}

// blocks returns the assembled ToolUseBlocks in ascending Index order. The raw
// concatenated Input is used verbatim (validation/sanitization happens in the
// caller, which needs both the raw and sanitized forms).
func (a *toolAccumulator) blocks() []content.ToolUseBlock {
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
