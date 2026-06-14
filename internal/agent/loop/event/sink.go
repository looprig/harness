package event

import (
	"context"

	"github.com/inventivepotter/urvi/internal/uuid"
)

// EventEnvelope tags an event with session and turn identity for observability.
// TurnIndex is zero for session-level events such as SessionStarted.
type EventEnvelope struct {
	SessionID uuid.UUID
	TurnIndex TurnIndex
	Event     Event
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
