package loop

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"

	"github.com/inventivepotter/urvi/internal/agent/loop/event"
	"github.com/inventivepotter/urvi/internal/content"
	"github.com/inventivepotter/urvi/internal/llm"
	"github.com/inventivepotter/urvi/internal/tool"
)

// runTurn drives the agentic loop for one user turn: it re-streams after each
// tool batch until the model returns no tool calls (TurnDone), the runaway guard
// fires (TurnFailed{ToolLimitError}), the provider errors (TurnFailed), or the
// turn is cancelled (TurnInterrupted). It returns updated history and the
// terminal event.
//
// Whole-turn rollback: base := len(msgs) is snapshotted at turn start; any
// abnormal exit truncates msgs[:base], discarding the ENTIRE turn's exchange so
// history never holds a tool_use without its matching ToolResultMessage (which a strict
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

// streamOnce streams one LLM response, driving each chunk through a
// chunkProcessor (emit a TokenDelta THEN fold into the blockState's text/
// thinking/tool-use accumulators) and materializing the assembled AIMessage and
// the tool-call blocks to execute via blockState after EOF. ok=false means a
// terminal event was produced (provider error, empty response, or interrupt) and
// the assembled message must be discarded.
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

	// blockState folds streamed chunks into thinking/text/tool-use accumulators;
	// the chunkProcessor owns the per-chunk "emit TokenDelta THEN accumulate" order.
	state := blockState{}
	proc := newChunkProcessor(emit, chunkState{blocks: &state})
	for {
		chunk, err := sr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, nil, streamFailure(ctx, turnIndex, err), false
		}
		proc.process(chunk, turnIndex)
	}

	// Materialize the single assistant message (thinking?, text?, then tool_use
	// blocks in ascending Index order) and the raw executable tool-use view. The
	// AIMessage's tool-use blocks are a DISTINCT allocation from rawCalls, so
	// sanitizing the stored message below never mutates the raw executable Input.
	aiMsg := state.AIMessage()
	rawCalls := state.ToolUses()

	// A successful stream with no content at all (no text, no thinking, no tool
	// calls) is a failure — the same controlled TurnFailed as the single-stream
	// loop, rather than appending an empty assistant message. Emptiness is decided
	// on the materialized block text (an empty-string-only chunk leaves a zero-
	// length block that does not count as content), matching prior behavior.
	if isEmptyAssistantMessage(aiMsg, rawCalls) {
		return nil, nil, event.TurnFailed{TurnIndex: turnIndex, Err: &event.EmptyResponseError{}}, false
	}

	// Sanitize the stored assistant message: drop zero-length text/thinking blocks
	// (prior behavior only included them when non-empty) and rewrite any malformed
	// tool-use Input to a fresh, valid-JSON "{}" so the stored message re-encodes
	// cleanly. The raw (possibly invalid) Input is still returned for execution, so
	// RunBatch reports the failure as a model-visible tool-result error.
	aiMsg.Blocks = sanitizeAssistantBlocks(aiMsg.Blocks)
	return aiMsg, rawCalls, nil, true
}

// isEmptyAssistantMessage reports whether a materialized assistant message
// carries no usable content: no non-empty text, no non-empty thinking, and no
// tool calls. This is the EmptyResponseError trigger and matches the prior
// builder-length check (a zero-length block does not count as content).
func isEmptyAssistantMessage(aiMsg *content.AIMessage, rawCalls []content.ToolUseBlock) bool {
	if len(rawCalls) > 0 {
		return false
	}
	for _, b := range aiMsg.Blocks {
		switch v := b.(type) {
		case *content.TextBlock:
			if v.Text != "" {
				return false
			}
		case *content.ThinkingBlock:
			if v.Thinking != "" {
				return false
			}
		}
	}
	return true
}

// sanitizeAssistantBlocks returns the storable form of the materialized blocks:
// zero-length text/thinking blocks are dropped (prior behavior only stored them
// when non-empty), and a tool-use block with invalid Input is rewritten to a
// fresh, valid-JSON "{}" so the stored history re-encodes cleanly. A fresh block
// allocation keeps each history block's Input independently owned.
func sanitizeAssistantBlocks(blocks []content.Block) []content.Block {
	out := make([]content.Block, 0, len(blocks))
	for _, b := range blocks {
		switch v := b.(type) {
		case *content.TextBlock:
			if v.Text != "" {
				out = append(out, v)
			}
		case *content.ThinkingBlock:
			if v.Thinking != "" {
				out = append(out, v)
			}
		case *content.ToolUseBlock:
			stored := *v
			if !validToolCall(stored) {
				stored.Input = json.RawMessage("{}")
			}
			out = append(out, &stored)
		default:
			out = append(out, b)
		}
	}
	return out
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

// toolResultMessage wraps one tool result into a ToolResultMessage carrying the
// flattened result text (flattenToText is REUSED from runner.go: TextBlocks pass
// through; non-text → "[unsupported …]" placeholder; empty → "error: empty
// result") and the originating tool_use id, so the model pairs result↔call.
func toolResultMessage(r result) *content.ToolResultMessage {
	text := flattenToText(r.Content)
	return &content.ToolResultMessage{
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
