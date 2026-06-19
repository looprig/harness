package loop

import (
	"context"
	"encoding/json"
	"log/slog"

	"github.com/inventivepotter/urvi/internal/agent/loop/event"
	"github.com/inventivepotter/urvi/internal/content"
	"github.com/inventivepotter/urvi/internal/llm"
	"github.com/inventivepotter/urvi/internal/tool"
	"github.com/inventivepotter/urvi/internal/uuid"
)

// turnIdentity is the (session, loop, turn) identity a turn stamps onto the steps
// it runs and the StepDone events it emits. runLoop threads it in from
// loopState/turnState; runTurn copies it into each step's stepState.
type turnIdentity struct {
	sessionID uuid.UUID
	loopID    uuid.UUID
	turnID    uuid.UUID
}

// runTurn drives the agentic loop for one user turn: it runs one step (one LLM
// request/response cycle → exactly one AIMessage) per iteration, executes that
// step's tool batch (appending the ToolResultMessages to the same step), emits an
// Enduring StepDone per completed step, and re-streams after each tool batch until
// the model returns no tool calls (TurnDone), the runaway guard fires
// (TurnFailed{ToolLimitError}), the provider errors (TurnFailed), or the turn is
// cancelled (TurnInterrupted). It returns updated history and the terminal event.
//
// Whole-turn rollback: base := len(msgs) is snapshotted at turn start; any
// abnormal exit truncates msgs[:base], discarding the ENTIRE turn's exchange so
// history never holds a tool_use without its matching ToolResultMessage (which a
// strict provider would reject on the next request). Only a TurnDone path keeps the
// turn — and only well-formed, fully paired exchanges reach a TurnDone.
//
// StepDone is emitted directly via emit (the per-turn + sink path); it is Enduring
// and NON-terminal. A turn that completes N steps emits N StepDone events, each
// carrying that step's finalized group (AIMessage + its ToolResultMessages),
// interleaved after the step's TokenDeltas, then the single turn terminal.
func runTurn(
	ctx context.Context,
	input []content.Block,
	turnIndex event.TurnIndex,
	identity turnIdentity,
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
	for stepIdx := StepIndex(0); ; stepIdx++ {
		req := llm.Request{Model: cfg.Model, Messages: msgs, Tools: defs}

		// Mint this step's id BEFORE streaming so StepDone can be stamped from the
		// step's identity. Best-effort, mirroring the EventID mint in publish: a
		// crypto/rand failure here is a system-level fault that must not abort an
		// already-accepted turn, so log it and stamp a zero StepID rather than
		// dropping the step. (The turn's identity-critical TurnID was already minted
		// at the gate; only that aborts.)
		stepID, err := cfg.idGen()
		if err != nil {
			slog.Error("step id generation failed; stamping StepDone with zero StepID", "error", err)
		}
		st := newStepState(identity.sessionID, identity.loopID, identity.turnID, stepID, stepIdx)

		// runStep owns the LLM cycle: stream → exactly one AIMessage into st.msgs[0].
		// turnIndex is passed for the legacy TurnIndex on the emitted TokenDelta /
		// terminal events; the step's own index lives in st.index.
		res := runStep(ctx, stepConfig{req: req, client: client, emit: emit}, turnIndex, newStep(st))
		if res.terminal != nil {
			// The LLM cycle produced a terminal (provider error / empty response /
			// interrupt): whole-turn rollback.
			return rollback(), res.terminal
		}
		st = res.state
		aiMsg := st.msgs[0].(*content.AIMessage)
		msgs = append(msgs, aiMsg)

		// Raw executable tool-use view (unsanitized Input) for this step.
		toolUses := st.blocks.ToolUses()

		// Text-only completion ALWAYS wins, regardless of iteration count: the runaway
		// cap is only checked when the model wants ANOTHER tool batch.
		if len(toolUses) == 0 {
			return msgs, event.TurnDone{TurnIndex: turnIndex, Message: aiMsg}
		}

		iters++
		calls += len(toolUses)
		if iters > cfg.Tools.MaxToolIterations || calls > cfg.Tools.MaxToolCallsPerTurn {
			// The tool-call assistant message was just appended, but rollback truncates
			// to base BEFORE any further provider request is built — so no unpaired
			// tool_use ever survives into history, and no StepDone is emitted for an
			// uncompleted step.
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
			// A cancelled batch's results are discarded; whole-turn rollback. The step
			// never completes, so no StepDone is emitted for it.
			return rollback(), event.TurnInterrupted{TurnIndex: turnIndex}
		}
		for _, r := range results {
			msgs = append(msgs, toolResultMessage(r))
		}
		// Loop: the next stream lets the model react to the tool results.
	}
}

// closeStream closes a stream reader, logging (but not surfacing) a close error:
// a close failure must not change the turn's outcome, which is already decided by
// the stream's content or a prior terminal.
func closeStream(sr *llm.StreamReader[content.Chunk]) {
	if cerr := sr.Close(); cerr != nil {
		slog.Warn("loop: stream close error", "error", cerr)
	}
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
