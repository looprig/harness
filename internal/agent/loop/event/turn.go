package event

import (
	"encoding/json"

	"github.com/inventivepotter/urvi/internal/content"
	"github.com/inventivepotter/urvi/internal/uuid"
)

// TurnStarted is emitted when runLoop commits a turn's initial UserMessage. It is
// the first enduring turn event. Header.CausationID == InputID (the submit command
// id). Message is the exact UserMessage committed as the first message of the turn.
type TurnStarted struct {
	enduring
	loopScoped
	Header
	TurnIndex TurnIndex
	InputID   uuid.UUID
	Message   *content.UserMessage
}

// StepDone is the enduring event emitted when a completed step's finalized group
// is committed: the step's single AIMessage followed by its ToolResultMessages.
// It is emitted at step completion (the actor-owned commit point once the commit
// handshake lands), so it is never a lie.
//
// SECURITY: StepDone carries message content (an AIMessage whose ToolUseBlocks
// hold raw tool arguments, and ToolResultMessages holding tool output). The
// loop-machine spec DELIBERATELY does NOT add a Redactable projection here — the
// "Redaction deferred and risk accepted" decision routes this (and the other
// content-bearing events TurnStarted/TurnFoldedInto/InputCancelled) to existing
// best-effort sinks unredacted, with re-homing owned by the journal/redaction
// follow-on (TODO(Open Items B)). Do NOT make StepDone implement Redactable in
// this phase; TestRedactableImplementations enforces that.
type StepDone struct {
	enduring
	loopScoped
	Header
	Messages content.AgenticMessages
}

// TurnFoldedInto is emitted when queued input folds into a mandatory
// tool-continuation request. Header.CausationID == InputID; Header.TriggeredByLoopID
// is set for a SubagentResult hand-back. Message is the folded user message.
type TurnFoldedInto struct {
	enduring
	loopScoped
	Header
	TurnIndex TurnIndex
	InputID   uuid.UUID
	Message   *content.UserMessage
}

// InputCancelled is emitted when a queued input leaves the loop queue without
// committing — client retract, or a return after an abnormal turn end.
// Header.CausationID == InputID; Header.TurnID is the active turn that caused a
// return, or zero for a pure client retract outside a turn. Message is the
// returned/retracted user message.
type InputCancelled struct {
	enduring
	loopScoped
	Header
	TurnIndex TurnIndex
	InputID   uuid.UUID
	Reason    CancelReason
	Message   *content.UserMessage
}

// TokenDelta is emitted for each streaming chunk from the LLM. It is the only
// Ephemeral event.
type TokenDelta struct {
	ephemeral
	loopScoped
	Header
	TurnIndex TurnIndex
	Chunk     content.Chunk
}

// TurnDone is the terminal success event for a turn.
type TurnDone struct {
	terminal
	loopScoped
	Header
	TurnIndex TurnIndex
	// Message is the complete AI response. It is retained for the current sink
	// projection; the loop-machine refactor removes it in a later phase once the
	// step group is the authoritative payload.
	Message *content.AIMessage
}

// TurnFailed is the terminal event for non-cancellation LLM/provider errors. Err
// carries the typed cause; callers may errors.As it to inspect and retry.
//
// SECURITY: TurnFailed is intentionally NOT Redactable, so Err is forwarded to
// sinks UN-redacted — an audit log legitimately needs the failure cause. This is
// safe ONLY while every cause is a typed, secret-free error constructed in this
// package (EmptyResponseError, ToolLimitError, TurnPanicError). A future change
// that wraps a provider/LLM error string into TurnFailed.Err MUST redact it (or
// make TurnFailed implement Redactable), because such strings can carry request
// bodies, headers, or tokens.
type TurnFailed struct {
	terminal
	loopScoped
	Header
	TurnIndex TurnIndex
	Err       error
}

// TurnInterrupted is the terminal event when the turn context is cancelled.
type TurnInterrupted struct {
	terminal
	loopScoped
	Header
	TurnIndex TurnIndex
}

func (TurnStarted) isEvent()     {}
func (StepDone) isEvent()        {}
func (TurnFoldedInto) isEvent()  {}
func (InputCancelled) isEvent()  {}
func (TokenDelta) isEvent()      {}
func (TurnDone) isEvent()        {}
func (TurnFailed) isEvent()      {}
func (TurnInterrupted) isEvent() {}

// SinkProjection redacts a tool-call delta before it reaches a sink. A
// *content.ToolUseChunk carries partial argument JSON (InputJSON) — the same
// secret ToolCallStarted.Summary redacts — so the projection keeps Index/ID/Name
// and drops InputJSON. A TextChunk/ThinkingChunk TokenDelta is model output, not
// a secret, so it is returned unchanged. The Header is carried through unchanged.
// TODO(Open Items B): journal/redaction follow-on re-homes redaction as a
// subscriber; new content-bearing events get no projection here by design.
func (e TokenDelta) SinkProjection() Event {
	tu, ok := e.Chunk.(*content.ToolUseChunk)
	if !ok {
		return e
	}
	return TokenDelta{
		Header:    e.Header,
		TurnIndex: e.TurnIndex,
		Chunk: &content.ToolUseChunk{
			Index: tu.Index,
			ID:    tu.ID,
			Name:  tu.Name,
			// InputJSON deliberately dropped: raw tool arguments never reach a sink.
		},
	}
}

// SinkProjection redacts a completed message before it reaches a sink. Every
// ToolUseBlock.Input is replaced with `{}` so raw tool arguments (a WriteFile
// body, a Bash command with an inline token, Fetch headers) never log. Text and
// thinking blocks are model output and pass through unchanged. The projection is
// a deep-enough copy that the original message — still referenced by the stream
// and conversation history — is never mutated. The Header is carried through
// unchanged.
// TODO(Open Items B): journal/redaction follow-on re-homes redaction as a
// subscriber; new content-bearing events get no projection here by design.
func (e TurnDone) SinkProjection() Event {
	if e.Message == nil {
		return e
	}
	blocks := make([]content.Block, len(e.Message.Blocks))
	for i, b := range e.Message.Blocks {
		if tu, ok := b.(*content.ToolUseBlock); ok {
			blocks[i] = &content.ToolUseBlock{
				ID:   tu.ID,
				Name: tu.Name,
				// Fresh allocation per block: no two projected events may share a
				// backing array a sink could mutate. An empty JSON object carries
				// no argument bytes.
				Input: json.RawMessage(`{}`),
			}
			continue
		}
		blocks[i] = b
	}
	return TurnDone{
		Header:    e.Header,
		TurnIndex: e.TurnIndex,
		Message: &content.AIMessage{Message: content.Message{
			Role:   e.Message.Role,
			Blocks: blocks,
		}},
	}
}
