package command

import "github.com/inventivepotter/urvi/internal/uuid"

// ProvideUserInput supplies the user's Answer to a pending AskUser request
// identified by CallID. Like the approve/deny pair it is a fire-and-route control
// command with no Ack: the actor routes it by GateCallID to the user-input gate
// blocked on that call, which delivers Answer to the waiting tool.
type ProvideUserInput struct {
	Header
	// Route addresses the loop (and, for full routing, the turn/step/tool call).
	// The loop's gate routing still matches by CallID for now; Route is carried
	// alongside so routing can migrate to Route-keyed without breaking callers.
	Route  Route
	CallID uuid.UUID
	Answer string
}

func (ProvideUserInput) isCommand() {}

// GateCallID returns the tool-call id this command targets, so the actor can
// route it to the matching pending gate.
func (c ProvideUserInput) GateCallID() uuid.UUID { return c.CallID }
