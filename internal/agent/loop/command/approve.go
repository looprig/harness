package command

import (
	"github.com/inventivepotter/urvi/internal/tool"
	"github.com/inventivepotter/urvi/internal/uuid"
)

// ApproveToolCall approves a pending tool call identified by ToolExecutionID, granting it
// at the requested persistence Scope. It is a fire-and-route control command: the
// actor routes it by GateToolExecutionID to the permission gate blocked on that call, so
// there is no Ack (the gate's unblocking and the subsequent ToolCallStarted event
// are the observable effect, not a reply on this command).
type ApproveToolCall struct {
	Header
	// GateRoute locates the loop (the session dispatches by LoopID) and names the
	// pending gate (ToolExecutionID), which the actor matches against.
	GateRoute
	Scope tool.ApprovalScope `json:"scope,omitzero"`
}

func (ApproveToolCall) isCommand() {}

// GateToolExecutionID returns the tool-call id this command targets, so the actor can
// route it to the matching pending gate.
func (c ApproveToolCall) GateToolExecutionID() uuid.UUID { return c.GateRoute.ToolExecutionID }
