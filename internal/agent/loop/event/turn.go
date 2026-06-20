package event

import (
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

// RejectReason explains why a UserInput submit was refused (carried by TurnRejected).
// It is the single source of truth for submit-rejection reasons — the loop publishes
// it on the event stream; there is no command-side copy (the former
// command.Disposition reply, with its command.RejectReason, was removed).
type RejectReason uint8

const (
	// RejectBusy: the loop is running and the submit is not queueable
	// (StartOnly / a non-queueable internal turn).
	RejectBusy RejectReason = iota
	// RejectQueueFull: the loop's inbox is at capacity.
	RejectQueueFull
	// RejectShuttingDown: the loop is shutting down and accepts no new input.
	RejectShuttingDown
	// RejectInternal: a transient internal failure (e.g. id generation); the loop is
	// healthy and the caller MAY retry.
	RejectInternal
)

// InputQueued is the Ephemeral Reply event for a UserInput accepted into the loop
// inbox but not yet assigned to a turn (it later resolves to TurnStarted,
// TurnFoldedInto, or InputCancelled). Header.CausationID == InputID (the submit
// command id). Ephemeral: it self-heals — the authoritative resolution event still
// follows if this is dropped.
type InputQueued struct {
	ephemeral
	loopScoped
	Header
	InputID uuid.UUID
}

// TurnRejected is the Enduring Reply event for a UserInput the loop refused
// (queue-full, shutting-down, busy/non-queueable, or a transient internal failure).
// Enduring: a rejected user message must never silently vanish. Header.CausationID ==
// InputID. It carries an InputID it did not have as a point-to-point reply.
type TurnRejected struct {
	enduring
	loopScoped
	Header
	InputID uuid.UUID
	Reason  RejectReason
}

func (InputQueued) isEvent()  {}
func (TurnRejected) isEvent() {}

func (InputQueued) isReply()  {}
func (TurnRejected) isReply() {}

// TokenDelta is emitted for each streaming chunk from the LLM. TokenDelta and
// the ToolCallStarted/ToolCallCompleted events (in tool.go) are the Ephemeral
// events.
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
	// Message is the complete AI response.
	Message *content.AIMessage
}

// TurnFailed is the terminal event for non-cancellation LLM/provider errors. Err
// carries the typed cause; callers may errors.As it to inspect and retry.
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

// isReply marks the command-outcome events. Together with InputQueued/TurnRejected
// (declared above) these are exactly the five Reply events: a command issuer
// recognises its answer via ReplyTo() == its command id.
func (TurnStarted) isReply()    {}
func (TurnFoldedInto) isReply() {}
func (InputCancelled) isReply() {}
