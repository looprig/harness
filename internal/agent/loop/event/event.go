package event

import "github.com/inventivepotter/urvi/internal/uuid"

type Event interface{ isEvent() }

// TurnIndex identifies a turn within one session.
type TurnIndex int

// SessionStarted is published to sinks when the actor starts.
type SessionStarted struct{ SessionID uuid.UUID }

func (SessionStarted) isEvent() {}
