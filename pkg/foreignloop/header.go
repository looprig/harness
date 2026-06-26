package foreignloop

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/ciram-co/looprig/pkg/event"
	"github.com/ciram-co/looprig/pkg/uuid"
)

// fillForeignHeader stamps the producer COORDINATES onto a foreign event from the
// loop/turn/step identity, mirroring the native loop's stampLoopHeader but over only
// the foreign event set. The turn-scoped terminals (TurnStarted/TurnDone/TurnFailed/
// TurnInterrupted) get SessionID+LoopID+TurnID and leave StepID ZERO (their
// validate profile forbids it); the step-scoped events (StepDone/TokenDelta/
// ToolCall*) additionally get StepID. The tool events already carry their
// ToolExecutionID on the body (set by the mapper). Any un-enumerated type is
// returned unchanged.
func fillForeignHeader(ev event.Event, sessionID, loopID, turnID, stepID uuid.UUID) event.Event {
	switch e := ev.(type) {
	case event.TurnStarted:
		e.Header.SessionID, e.Header.LoopID, e.Header.TurnID = sessionID, loopID, turnID
		return e
	case event.TurnDone:
		e.Header.SessionID, e.Header.LoopID, e.Header.TurnID = sessionID, loopID, turnID
		return e
	case event.TurnFailed:
		e.Header.SessionID, e.Header.LoopID, e.Header.TurnID = sessionID, loopID, turnID
		return e
	case event.TurnInterrupted:
		e.Header.SessionID, e.Header.LoopID, e.Header.TurnID = sessionID, loopID, turnID
		return e
	case event.StepDone:
		e.Header.SessionID, e.Header.LoopID, e.Header.TurnID, e.Header.StepID = sessionID, loopID, turnID, stepID
		return e
	case event.TokenDelta:
		e.Header.SessionID, e.Header.LoopID, e.Header.TurnID, e.Header.StepID = sessionID, loopID, turnID, stepID
		return e
	case event.ToolCallStarted:
		e.Header.SessionID, e.Header.LoopID, e.Header.TurnID, e.Header.StepID = sessionID, loopID, turnID, stepID
		return e
	case event.ToolCallCompleted:
		e.Header.SessionID, e.Header.LoopID, e.Header.TurnID, e.Header.StepID = sessionID, loopID, turnID, stepID
		return e
	default:
		return ev
	}
}

// withForeignHeader replaces the embedded Header on the ENDURING foreign events —
// the only events the publish chokepoint stamps (EventID+CreatedAt). It is the
// write-back counterpart to fillForeignHeader for the Factory-stamped class; an
// Ephemeral event never reaches this path, so the default returns ev unchanged.
func withForeignHeader(ev event.Event, h event.Header) event.Event {
	switch e := ev.(type) {
	case event.TurnStarted:
		e.Header = h
		return e
	case event.StepDone:
		e.Header = h
		return e
	case event.TurnDone:
		e.Header = h
		return e
	case event.TurnFailed:
		e.Header = h
		return e
	case event.TurnInterrupted:
		e.Header = h
		return e
	default:
		return ev
	}
}

// publisher returns a per-turn publish closure used by BOTH the actor goroutine
// (terminals it owns) and the turn goroutine (live Ephemeral events + the
// transcript-derived StepDone/terminal). They never call it concurrently for the
// same turn, and it touches only immutable loop fields (sessionID/loopID/pub/fac),
// so there is no shared-state race. It fills producer coordinates, stamps EventID+
// CreatedAt for the Enduring class (fail-secure: drop on mint error rather than
// publish a zero-EventID Enduring event), and fans the event into the session
// publisher.
func (l *Loop) publisher(ctx context.Context, turnID, stepID uuid.UUID) func(event.Event) {
	return func(ev event.Event) {
		ev = fillForeignHeader(ev, l.sessionID, l.loopID, turnID, stepID)
		if ev.Class() == event.Enduring {
			h, err := l.fac.Stamp(ev.EventHeader())
			if err != nil {
				slog.Error("foreignloop: event id mint failed; dropping Enduring event (fail-secure)",
					"event", fmt.Sprintf("%T", ev), "error", err)
				return
			}
			ev = withForeignHeader(ev, h)
		}
		if err := l.pub.PublishEvent(ctx, ev); err != nil {
			slog.Error("foreignloop: event publish to session fan-in failed", "error", err)
		}
	}
}
