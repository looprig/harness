package command

import (
	"github.com/inventivepotter/urvi/internal/tool"
	"github.com/inventivepotter/urvi/internal/uuid"
)

// ApproveToolCall approves a pending tool call identified by CallID, granting it
// at the requested persistence Scope. It is a fire-and-route control command: the
// actor routes it by GateCallID to the permission gate blocked on that call, so
// there is no Ack (the gate's unblocking and the subsequent ToolCallStarted event
// are the observable effect, not a reply on this command).
type ApproveToolCall struct {
	Header
	CallID uuid.UUID
	Scope  tool.ApprovalScope
}

func (ApproveToolCall) isCommand() {}

// GateCallID returns the tool-call id this command targets, so the actor can
// route it to the matching pending gate.
func (c ApproveToolCall) GateCallID() uuid.UUID { return c.CallID }
