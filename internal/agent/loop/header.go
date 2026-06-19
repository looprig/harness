package loop

import (
	"github.com/inventivepotter/urvi/internal/agent/loop/event"
	"github.com/inventivepotter/urvi/internal/uuid"
)

// stampLoopHeader returns ev with its producer-identity Header completed from the
// loop's identity for the session fan-in. It fills ONLY zero header fields (so an
// event that already carries finer identity — TurnStarted/StepDone/TurnFoldedInto/
// InputCancelled stamp SessionID/LoopID/TurnID/StepID/CausationID/TriggeredByLoopID at
// their producers — is preserved exactly). Header-less events (TokenDelta, the turn
// terminals, and gate/tool lifecycle events) get SessionID + LoopID + TurnID from the
// loop and active turn here. ToolCallID is filled from the event's own CallID for gate/
// tool events so a consumer can correlate without an envelope.
//
// This is the LOOP stamping its own producer identity (the actor is the loop's
// authoritative producer), NOT the hub inferring it — the fan-in never repairs
// identity. loopID is loopState.id; turnID is the active turn id (zero between turns).
func stampLoopHeader(ev event.Event, sessionID, loopID, turnID uuid.UUID) event.Event {
	switch e := ev.(type) {
	case event.TokenDelta:
		e.Header = fillTurnScoped(e.Header, sessionID, loopID, turnID)
		return e
	case event.TurnDone:
		e.Header = fillTurnScoped(e.Header, sessionID, loopID, turnID)
		return e
	case event.TurnFailed:
		e.Header = fillTurnScoped(e.Header, sessionID, loopID, turnID)
		return e
	case event.TurnInterrupted:
		e.Header = fillTurnScoped(e.Header, sessionID, loopID, turnID)
		return e
	case event.PermissionRequested:
		e.Header = fillToolScoped(e.Header, sessionID, loopID, turnID, e.CallID)
		return e
	case event.UserInputRequested:
		e.Header = fillToolScoped(e.Header, sessionID, loopID, turnID, e.CallID)
		return e
	case event.ToolCallStarted:
		e.Header = fillToolScoped(e.Header, sessionID, loopID, turnID, e.CallID)
		return e
	case event.ToolCallCompleted:
		e.Header = fillToolScoped(e.Header, sessionID, loopID, turnID, e.CallID)
		return e
	case event.TurnStarted:
		e.Header = fillLoopScoped(e.Header, sessionID, loopID)
		return e
	case event.StepDone:
		e.Header = fillLoopScoped(e.Header, sessionID, loopID)
		return e
	case event.TurnFoldedInto:
		e.Header = fillLoopScoped(e.Header, sessionID, loopID)
		return e
	case event.InputCancelled:
		e.Header = fillLoopScoped(e.Header, sessionID, loopID)
		return e
	default:
		// Session-scoped events (SessionStarted/Active/Idle/Stopped) and any future
		// event: only ensure SessionID. SessionStarted never reaches publishHub, but a
		// defensive fill keeps the helper total.
		if h := e.EventHeader(); h.SessionID.IsZero() {
			// No generic setter on the sealed interface; an unhandled type is returned
			// as-is. Concrete loop events are all enumerated above.
			return ev
		}
		return ev
	}
}

// fillLoopScoped ensures SessionID + LoopID are present without disturbing the
// already-stamped TurnID/StepID/CausationID/TriggeredByLoopID a producer set.
func fillLoopScoped(h event.Header, sessionID, loopID uuid.UUID) event.Header {
	if h.SessionID.IsZero() {
		h.SessionID = sessionID
	}
	if h.LoopID.IsZero() {
		h.LoopID = loopID
	}
	return h
}

// fillTurnScoped fills SessionID + LoopID + TurnID for a header-less turn-scoped
// event (TokenDelta, terminals). Only zero fields are filled.
func fillTurnScoped(h event.Header, sessionID, loopID, turnID uuid.UUID) event.Header {
	h = fillLoopScoped(h, sessionID, loopID)
	if h.TurnID.IsZero() {
		h.TurnID = turnID
	}
	return h
}

// fillToolScoped fills SessionID + LoopID + TurnID + ToolCallID for a gate/tool
// lifecycle event from the loop identity and the event's own CallID.
func fillToolScoped(h event.Header, sessionID, loopID, turnID, callID uuid.UUID) event.Header {
	h = fillTurnScoped(h, sessionID, loopID, turnID)
	if h.ToolCallID.IsZero() {
		h.ToolCallID = callID
	}
	return h
}
