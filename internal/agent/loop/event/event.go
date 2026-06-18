package event

import "github.com/inventivepotter/urvi/internal/uuid"

// Event is the sealed root of every loop event. Every concrete event embeds a
// Header, exactly one lifecycle mixin (ephemeral, enduring, or terminal), and
// exactly one scope mixin (sessionScoped or loopScoped). The lifecycle mixin
// supplies Class()/EndsTurn() and the scope mixin supplies Scope(), so the hub
// gets its delivery policy and consumers get producer identity without a
// transport-only envelope or a concrete type switch. Embedding two lifecycle
// mixins or two scope mixins makes the selectors ambiguous and the type stops
// satisfying Event, so the "exactly one of each" rule is enforced by the
// compiler.
//
// SECURITY: any new event carrying sensitive payload (tool args, file content,
// URLs/headers, user text, raw provider/LLM error strings) MUST implement
// Redactable (see tool.go), because the loop's sink path forwards every
// non-Redactable event to observability sinks VERBATIM. If in doubt, redact.
type Event interface {
	isEvent()
	Class() Class
	Scope() Scope
	EndsTurn() bool // turn-terminal: the last event this turn's per-turn stream carries
	EventHeader() Header
}

// Class is the delivery class of an event. It is semantic — "is this event
// reconstructable from a later authoritative event?" — not a transport flag,
// which is why it belongs on the event rather than on the transport.
type Class uint8

const (
	Ephemeral Class = iota // reconstructable from a later authoritative event -> droppable
	Enduring               // authoritative transition/payload -> never silently dropped
)

// Scope is whether an event is session-global or produced by one loop.
type Scope uint8

const (
	ScopeSession Scope = iota
	ScopeLoop
)

// CancelReason explains why a queued input left the loop queue without
// committing. It is carried by InputCancelled.
type CancelReason uint8

const (
	CancelClientRetracted CancelReason = iota
	CancelTurnInterrupted
	CancelTurnFailed
)

// Header is the producer identity stamped on every event. The producer (the loop
// for loop events, the session for session events) fills it in; fan-in, filter,
// and journal consumers read it without a transport-only envelope.
type Header struct {
	// SessionID is set on every event.
	SessionID uuid.UUID

	// Producer identity. For session-scoped events, LoopID/TurnID/StepID are zero.
	// For loop-scoped events, LoopID is set. TurnID is set for turn events; StepID
	// is set for step/tool scoped events.
	LoopID uuid.UUID
	TurnID uuid.UUID
	StepID uuid.UUID

	// TriggeredByLoopID is set on a turn/input event caused by a SubagentResult
	// (= the producing subagent's loop id); zero otherwise. The publish path
	// releases {wake, TriggeredByLoopID} when it sees TurnStarted/TurnFoldedInto/
	// InputCancelled carrying it (see quiescence).
	TriggeredByLoopID uuid.UUID

	// CausationID is set when an event is directly caused by a command. For
	// UserInput/SubagentResult resolution events (TurnStarted, TurnFoldedInto,
	// InputCancelled), it is the submit command id and equals InputID.
	CausationID uuid.UUID

	// ToolCallID is set on gate/tool lifecycle events when CallID is available.
	ToolCallID uuid.UUID

	// Event identity and parent grouping are present on the type, but detailed
	// wiring and EventEnvelope replacement are sequenced after the
	// journal/redaction follow-on.
	ID           uuid.UUID
	ParentLoopID uuid.UUID
	ParentTurnID uuid.UUID
	ParentStepID uuid.UUID
}

// EventHeader returns the embedded Header so every event satisfies Event without
// per-type boilerplate.
func (h Header) EventHeader() Header { return h }

// ephemeral is the lifecycle mixin for a streaming delta: droppable, never
// turn-terminal.
type ephemeral struct{}

func (ephemeral) Class() Class   { return Ephemeral }
func (ephemeral) EndsTurn() bool { return false }

// enduring is the lifecycle mixin for an authoritative, mid-turn or mid-loop
// transition/payload: never silently dropped, never turn-terminal.
type enduring struct{}

func (enduring) Class() Class   { return Enduring }
func (enduring) EndsTurn() bool { return false }

// terminal is the lifecycle mixin for a turn-ender. It folds in Class()==Enduring,
// so terminal => Enduring holds by construction — a turn-ender can never be
// classified droppable.
type terminal struct{}

func (terminal) Class() Class   { return Enduring }
func (terminal) EndsTurn() bool { return true }

// sessionScoped is the scope mixin for a session-global event.
type sessionScoped struct{}

func (sessionScoped) Scope() Scope { return ScopeSession }

// loopScoped is the scope mixin for a loop-produced event.
type loopScoped struct{}

func (loopScoped) Scope() Scope { return ScopeLoop }

// TurnIndex identifies a turn within one loop. Each loop numbers its own turns
// from 0; it is not unique across loops in a multi-loop session.
type TurnIndex int

// SessionStarted is published when the session's primary loop actor starts.
// Header.SessionID is set; LoopID/TurnID/StepID are zero.
type SessionStarted struct {
	enduring
	sessionScoped
	Header
}

// SessionActive marks the Idle -> Active edge of the session quiescence model.
type SessionActive struct {
	enduring
	sessionScoped
	Header
}

// SessionIdle marks the Active -> Idle edge of the session quiescence model.
type SessionIdle struct {
	enduring
	sessionScoped
	Header
}

// SessionStopped marks the session phase transition on Shutdown.
type SessionStopped struct {
	enduring
	sessionScoped
	Header
}

// LoopIdle is emitted when a loop parks with no active turn. Header.SessionID and
// Header.LoopID are set; TurnID/StepID are zero. It drives session quiescence.
type LoopIdle struct {
	enduring
	loopScoped
	Header
}

func (SessionStarted) isEvent() {}
func (SessionActive) isEvent()  {}
func (SessionIdle) isEvent()    {}
func (SessionStopped) isEvent() {}
func (LoopIdle) isEvent()       {}
