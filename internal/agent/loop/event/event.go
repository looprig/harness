package event

import "github.com/inventivepotter/urvi/internal/uuid"

// Event is the sealed root of every loop event. SECURITY: any new event carrying
// sensitive payload (tool args, file content, URLs/headers, user text, raw
// provider/LLM error strings) MUST implement Redactable (see tool.go), because
// the loop's sink path forwards every non-Redactable event to observability
// sinks VERBATIM. If in doubt, redact.
type Event interface{ isEvent() }

// TurnIndex identifies a turn within one session.
type TurnIndex int

// SessionStarted is published to sinks when the actor starts.
type SessionStarted struct{ SessionID uuid.UUID }

func (SessionStarted) isEvent() {}
