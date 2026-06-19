package command

import "github.com/inventivepotter/urvi/internal/uuid"

// DenyToolCall denies a pending tool call identified by CallID. Like
// ApproveToolCall it is a fire-and-route control command with no Ack: the actor
// routes it by GateCallID to the permission gate, which fails the call closed
// (fail-secure). Denial carries no scope — nothing is ever persisted on a deny.
type DenyToolCall struct {
	Header
	// Route addresses the loop (and, for full routing, the turn/step/tool call).
	// The loop's gate routing still matches by CallID for now; Route is carried
	// alongside so routing can migrate to Route-keyed without breaking callers.
	Route  Route
	CallID uuid.UUID
}

func (DenyToolCall) isCommand() {}

// GateCallID returns the tool-call id this command targets, so the actor can
// route it to the matching pending gate.
func (c DenyToolCall) GateCallID() uuid.UUID { return c.CallID }
