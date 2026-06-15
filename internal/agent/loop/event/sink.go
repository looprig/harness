package event

import (
	"context"

	"github.com/inventivepotter/urvi/internal/uuid"
)

// EventEnvelope tags an event with session and turn identity for observability.
// TurnIndex is zero for session-level events such as SessionStarted.
// TurnID, EventID, CausationID, and CallID are tracing-only correlation metadata
// that live exclusively on the envelope (never on the bare event structs).
// TurnID is zero for session-level events; CausationID is the active StartTurn's
// Header.ID (zero when no turn is active); CallID is zero unless the event
// pertains to a specific tool call.
type EventEnvelope struct {
	SessionID   uuid.UUID
	TurnID      uuid.UUID
	TurnIndex   TurnIndex
	EventID     uuid.UUID
	CausationID uuid.UUID
	CallID      uuid.UUID // zero unless the event pertains to a tool call
	Event       Event
}

// EventSink receives best-effort copies of session events.
// Implementations must not block; slow or durable sinks own their own buffering.
// Implementations must be safe for concurrent calls.
// Sink failures must not affect turn execution.
// The context passed to OnEvent may already be cancelled during a hard loop kill.
// Implementations must not use it for I/O; use an independently-managed context instead.
type EventSink interface {
	OnEvent(context.Context, EventEnvelope)
}
