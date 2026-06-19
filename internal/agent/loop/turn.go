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

// turnState is the staged turn conversation owned by the turn goroutine. msgs
// starts with the single initial UserMessage and accumulates completed
// stepState.msgs groups; the LLM request for each step is built from
// turnConfig.base + turnState.msgs (never live loopState.msgs). The turn goroutine
// is the SOLE writer of turnState; the actor never touches it.
type turnState struct {
	// sessionID is copied from loopState so turn/step records correlate without
	// reaching back into loopState.
	sessionID uuid.UUID
	// loopID is copied from loopState (the parent loop).
	loopID uuid.UUID
	// id is this turn's id, minted by the actor before the turn runs.
	id uuid.UUID
	// index is the loop-local turn index.
	index event.TurnIndex
	// causationID is the submit command id (command.UserInput/SubagentResult
	// Header.ID) that initiated this turn.
	causationID uuid.UUID

	// msgs is the staged turn conversation: the initial UserMessage followed by
	// completed step message groups. It is the running view used to build the next
	// LLM request; committed history (loopState.msgs) grows group-by-group via
	// turnConfig.commit. It is NOT the LLM request base — that is turnConfig.base.
	msgs content.AgenticMessages

	toolIterations int
	toolCalls      int
}

// newTurnState builds a fresh turnState with its identity (copied from the loop)
// and seeds msgs with EXACTLY the one initial UserMessage. The actor commits that
// same UserMessage into loopState.msgs and emits TurnStarted before runTurn runs.
func newTurnState(
	sessionID, loopID, turnID uuid.UUID,
	index event.TurnIndex,
	causationID uuid.UUID,
	user *content.UserMessage,
) turnState {
	return turnState{
		sessionID:   sessionID,
		loopID:      loopID,
		id:          turnID,
		index:       index,
		causationID: causationID,
		msgs:        content.AgenticMessages{user},
	}
}

// turnConfig carries the dependencies of one turn: the request base, model/tools/
// client, the gate registry, the id generator, and the two actor handshakes
// (commit + emit). Dependencies stay at this boundary; turnState owns one turn's
// staged messages and counters.
type turnConfig struct {
	// base is a defensive CLONE of the pre-turn loopState.msgs with its OWN backing
	// array. The LLM request for each step is base + turnState.msgs. base MUST NOT
	// alias loopState.msgs: the actor keeps appending committed step groups to
	// loopState.msgs while runTurn reads base concurrently.
	base content.AgenticMessages

	model   llm.ModelSpec
	tools   ToolSet
	client  llm.LLM
	gateReg chan<- gateRegistration
	idGen   idGenerator

	// commit is the durability/event handshake back to the actor. runTurn prepares a
	// complete step group, but the actor is the only goroutine that mutates
	// loopState.msgs; it appends commit.Messages and emits commit.Event (the
	// StepDone) at the SAME actor-owned point, then acks. commit MUST be
	// ctx-cancellable so Interrupt/Shutdown frees a parked runTurn instead of
	// wedging it.
	commit func(context.Context, turnCommit) error

	// drainPending is the tool-continuation handshake back to the actor. After a
	// COMPLETED tool-using step commits (tool results appended), and BEFORE the
	// mandatory next LLM request, runTurn calls it to pull every accepted inbox
	// entry. The actor (the inbox's sole owner) pops + clears the inbox in order,
	// moves the popped entries into an actor-owned draining buffer (so an abnormal
	// terminal still returns them — they are no longer in the inbox), and replies the
	// foldedMsgs. runTurn appends each message to turnState.msgs and commits a
	// TurnFoldedInto for it via cfg.commit; the actor removes the entry from the
	// draining buffer at that commit point. It MUST be ctx-cancellable (select on the
	// reply AND turnCtx.Done) so an Interrupt/Shutdown during the handshake frees
	// runTurn. runTurn never calls it after a no-tool final answer (a final answer is
	// not a tool-continuation boundary).
	drainPending func(context.Context) ([]foldedMsg, error)

	// emit publishes this loop's events (TurnStarted is actor-emitted; runTurn emits
	// TokenDeltas, tool lifecycle events, and the turn terminal). StepDone is NOT
	// emitted here — it is emitted by the actor at the commit point.
	emit func(event.Event)

	// afterDrain is a test-only seam (nil in production) invoked by foldPending after
	// drainPending returns the batch but before the first TurnFoldedInto commit. See
	// Config.afterDrain for the rationale.
	afterDrain func()
}

// turnCommit is one commit request: the finalized step group to append to
// loopState.msgs and the Enduring event (StepDone) to emit at the same actor point.
type turnCommit struct {
	Messages content.AgenticMessages
	Event    event.Event
}

// foldedMsg is one drained inbox entry handed back to runTurn at a tool-continuation
// boundary. It carries the message to fold AND the provenance runTurn needs to stamp
// the TurnFoldedInto event: inputID (-> Header.CausationID + InputID) and triggeredBy
// (-> Header.TriggeredByLoopID, set for a SubagentResult hand-back, zero for a plain
// UserInput). triggeredBy is what releases the parent's {wake} quiescence token on the
// publish path, so it MUST survive the drain handshake.
type foldedMsg struct {
	inputID     uuid.UUID
	triggeredBy uuid.UUID
	msg         *content.UserMessage
}

// cloneMessages returns a copy of msgs with its OWN backing array (a fresh slice
// of the same element pointers). Appends to the source never reach the clone. A
// nil/empty source yields an empty (non-shared) slice.
func cloneMessages(msgs content.AgenticMessages) content.AgenticMessages {
	out := make(content.AgenticMessages, len(msgs))
	copy(out, msgs)
	return out
}

// turnIdentity is the (session, loop, turn) identity a turn stamps onto the steps
// it runs and the StepDone events the actor emits. runLoop threads it in from
// loopState/turnState; runTurn copies it into each step's stepState.
type turnIdentity struct {
	sessionID uuid.UUID
	loopID    uuid.UUID
	turnID    uuid.UUID
}

// runTurn drives the agentic loop for one user turn. It runs one step (one LLM
// request/response cycle → exactly one AIMessage) per iteration, executes that
// step's tool batch (appending the ToolResultMessages to the same step), and
// re-streams after each tool batch until the model returns no tool calls
// (TurnDone), the runaway guard fires (TurnFailed{ToolLimitError}), the provider
// errors (TurnFailed), or the turn is cancelled (TurnInterrupted). It returns the
// terminal event for the actor to deliver.
//
// Incremental, loop-owned commit (Phase 8): the actor commits the initial
// UserMessage and emits TurnStarted BEFORE runTurn starts. As each step completes,
// runTurn appends it to turnState.msgs and calls cfg.commit; the ACTOR appends that
// group to loopState.msgs and emits the Enduring StepDone at the same point (so
// StepDone is never a lie). cfg.commit is ctx-cancellable: an Interrupt/Shutdown
// during the handshake frees runTurn.
//
// Step-granularity rollback: a TurnFailed/TurnInterrupted discards ONLY the
// in-flight incomplete step (which never committed and never emitted StepDone);
// steps already committed stay in loopState.msgs (the actor never un-commits). A
// terminal means "the turn stopped here," not "the turn never happened."
//
// The LLM request for each step is built from cfg.base + ts.msgs — never live
// loopState.msgs — so the already-committed parts are not duplicated.
func runTurn(ctx context.Context, cfg turnConfig, ts turnState) event.Event {
	identity := turnIdentity{sessionID: ts.sessionID, loopID: ts.loopID, turnID: ts.id}
	defs := toolDefs(ctx, cfg.tools.Registry)

	for stepIdx := StepIndex(0); ; stepIdx++ {
		// Request base is the committed history clone + this turn's staged messages.
		req := llm.Request{
			Model:    cfg.model,
			Messages: requestMessages(cfg.base, ts.msgs),
			Tools:    defs,
		}

		// Mint this step's id BEFORE streaming so StepDone can be stamped from the
		// step's identity. Best-effort, mirroring the EventID mint in publish: a
		// crypto/rand failure here is a system-level fault that must not abort an
		// already-accepted turn, so log it and stamp a zero StepID rather than
		// dropping the step.
		stepID, err := cfg.idGen()
		if err != nil {
			slog.Error("step id generation failed; stamping StepDone with zero StepID", "error", err)
		}
		st := newStepState(identity.sessionID, identity.loopID, identity.turnID, stepID, stepIdx)

		// runStep owns the LLM cycle: stream → exactly one AIMessage into st.msgs[0].
		res := runStep(ctx, stepConfig{req: req, client: cfg.client, emit: cfg.emit}, ts.index, st)
		if res.terminal != nil {
			// The in-flight step never completed: discard it (it was never added to
			// ts.msgs and never committed) and return the terminal. Committed steps
			// stay in loopState.msgs.
			return res.terminal
		}
		st = res.state
		aiMsg := st.msgs[0].(*content.AIMessage)

		// Raw executable tool-use view (unsanitized Input) for this step.
		toolUses := st.blocks.ToolUses()

		// Text-only completion ALWAYS wins, regardless of iteration count: the runaway
		// cap is only checked when the model wants ANOTHER tool batch. The step's group
		// is just the AIMessage. Commit it (actor appends + emits StepDone), then end.
		if len(toolUses) == 0 {
			ts.msgs = append(ts.msgs, aiMsg)
			if cerr := commitStep(ctx, cfg, st); cerr != nil {
				// The commit handshake was cancelled (Interrupt/Shutdown) before the
				// actor committed/emitted this final step: treat as interrupt.
				return event.TurnInterrupted{TurnIndex: ts.index}
			}
			return event.TurnDone{TurnIndex: ts.index, Message: aiMsg}
		}

		ts.toolIterations++
		ts.toolCalls += len(toolUses)
		if ts.toolIterations > cfg.tools.MaxToolIterations || ts.toolCalls > cfg.tools.MaxToolCallsPerTurn {
			// The runaway cap fires on this UNCOMPLETED tool step: it is never appended
			// to ts.msgs and never committed, so no unpaired tool_use survives into
			// loopState.msgs and no StepDone is emitted for it.
			return event.TurnFailed{
				TurnIndex: ts.index,
				Err: &event.ToolLimitError{
					Iterations:    ts.toolIterations,
					MaxIterations: cfg.tools.MaxToolIterations,
					Calls:         ts.toolCalls,
					MaxCalls:      cfg.tools.MaxToolCallsPerTurn,
				},
			}
		}

		results := RunBatch(ctx, toolUses, cfg.tools, cfg.gateReg, cfg.idGen, cfg.emit)
		if ctx.Err() != nil {
			// A cancelled batch's results are discarded; the step never completes, so
			// it is not appended/committed and emits no StepDone.
			return event.TurnInterrupted{TurnIndex: ts.index}
		}
		for _, r := range results {
			trm := toolResultMessage(r)
			st.msgs = append(st.msgs, trm)
		}
		// The step is now COMPLETE (AIMessage finalized AND its tool results appended).
		// Append the whole group to the staged turn and commit it (actor appends to
		// loopState.msgs + emits StepDone at the same point).
		ts.msgs = append(ts.msgs, st.msgs...)
		if cerr := commitStep(ctx, cfg, st); cerr != nil {
			// The commit handshake was cancelled (Interrupt/Shutdown) before the actor
			// committed/emitted this completed step: treat as interrupt. Prior steps
			// already committed stay in loopState.msgs.
			return event.TurnInterrupted{TurnIndex: ts.index}
		}

		// Tool-continuation boundary: another LLM request is already required to send
		// the tool results, so this is the ONLY point where queued input may fold. Pull
		// every accepted inbox entry (ctx-cancellable), append each to the staged turn
		// AFTER the tool results, and commit a TurnFoldedInto for it. A no-tool final
		// answer (handled above) never reaches here, so folding cannot extend a turn
		// past the model's final answer.
		if ferr := foldPending(ctx, cfg, &ts); ferr != nil {
			// The drain or a fold commit was cancelled (Interrupt/Shutdown) before it
			// completed: treat as interrupt. Committed steps + any already-committed
			// folds stay in loopState.msgs; the actor returns the rest of the inbox and
			// the draining buffer via InputCancelled.
			return event.TurnInterrupted{TurnIndex: ts.index}
		}
		// Loop: the next stream lets the model react to the tool results (and any
		// folded user messages).
	}
}

// foldPending drains the actor's inbox at a tool-continuation boundary and folds the
// returned messages into the staged turn. For each drained entry it appends the
// message to ts.msgs (after the just-committed tool results) and commits a
// TurnFoldedInto for it through the ctx-cancellable cfg.commit handshake (the actor
// appends it to loopState.msgs, emits TurnFoldedInto, and clears it from the draining
// buffer at the same point). A cancellation (drain or commit) returns an error so
// runTurn stops; nothing is folded twice and the actor still owns returning the
// not-yet-committed entries.
func foldPending(ctx context.Context, cfg turnConfig, ts *turnState) error {
	batch, err := cfg.drainPending(ctx)
	if err != nil {
		return err
	}
	// Test-only seam (nil in production): the inbox has been moved into the actor's
	// draining buffer but no TurnFoldedInto has committed yet. A test cancels the loop
	// here to exercise the draining-buffer abnormal-return sweep.
	if cfg.afterDrain != nil {
		cfg.afterDrain()
	}
	for _, fm := range batch {
		ts.msgs = append(ts.msgs, fm.msg)
		fold := turnCommit{
			Messages: content.AgenticMessages{fm.msg},
			Event: event.TurnFoldedInto{
				Header: event.Header{
					SessionID:         ts.sessionID,
					LoopID:            ts.loopID,
					TurnID:            ts.id,
					CausationID:       fm.inputID,
					TriggeredByLoopID: fm.triggeredBy,
				},
				TurnIndex: ts.index,
				InputID:   fm.inputID,
				Message:   fm.msg,
			},
		}
		if cerr := cfg.commit(ctx, fold); cerr != nil {
			return cerr
		}
	}
	return nil
}

// requestMessages builds the LLM request message slice from the committed base
// clone followed by the turn's staged messages. The result is a fresh slice so the
// request never aliases either input's backing array.
func requestMessages(base, staged content.AgenticMessages) content.AgenticMessages {
	out := make(content.AgenticMessages, 0, len(base)+len(staged))
	out = append(out, base...)
	out = append(out, staged...)
	return out
}

// commitStep sends one completed step's group + its StepDone to the actor through
// the ctx-cancellable cfg.commit handshake. The actor appends the group to
// loopState.msgs and emits the StepDone at the same point. On a cancellation error
// the turn goroutine stops; committed steps stay committed.
func commitStep(ctx context.Context, cfg turnConfig, st stepState) error {
	// The step group is cloned TWICE on purpose: Messages (appended to
	// loopState.msgs as committed history) and the StepDone payload inside
	// stepDoneEvent (the consumer-held event). These two clones are DELIBERATELY
	// independent — the committed-history slice and the consumer-held event payload
	// must not alias each other or st.msgs. Do NOT merge into one shared slice
	// (would reintroduce aliasing).
	return cfg.commit(ctx, turnCommit{
		Messages: cloneMessages(st.msgs),
		Event:    stepDoneEvent(st),
	})
}

// stepDoneEvent builds the Enduring StepDone for one COMPLETED step: its Header is
// stamped from the step's identity (SessionID/LoopID/TurnID/StepID), and Messages
// is the finalized step group (the single AIMessage followed by its
// ToolResultMessages). The Messages slice is a fresh copy so a consumer cannot
// mutate the turn's live history through the event.
func stepDoneEvent(st stepState) event.StepDone {
	group := cloneMessages(st.msgs)
	return event.StepDone{
		Header: event.Header{
			SessionID: st.sessionID,
			LoopID:    st.loopID,
			TurnID:    st.turnID,
			StepID:    st.id,
		},
		Messages: group,
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
// cancelled ctx is an interrupt (no error surfaced); any other error is a
// TurnFailed carrying the typed cause.
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
