package command

import (
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/tool"
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

	// AcceptedGrants carries the opaque escalation grant TOKENS the operator
	// accepted by approving this call — the same tokens shown as GrantDisplay in
	// the permission prompt (SPEC §9.3, §10.7). The runner places them on the
	// FIRST spawn's per-call ctx so the delta is applied without a run-fail-rerun,
	// and hands the grant-bearing ctx to Permission.Grant so a non-Once approval
	// persists the MAC-verified grant DELTA DESCRIPTIONS (never the single-mint
	// tokens). The tokens themselves are opaque: harness carries them, never mints
	// or interprets them.
	//
	// omitempty keeps a grant-free ApproveToolCall byte-identical to the
	// pre-Grants wire form (durable backward compatibility): an old journal record
	// decodes with a nil AcceptedGrants, and a grant-free command marshals without
	// the key. The tag "accepted_grants" must NOT be renamed (SPEC §10.7).
	AcceptedGrants []string `json:"accepted_grants,omitempty"`
}

func (ApproveToolCall) isCommand() {}

// GateToolExecutionID returns the tool-call id this command targets, so the actor can
// route it to the matching pending gate.
func (c ApproveToolCall) GateToolExecutionID() uuid.UUID { return c.GateRoute.ToolExecutionID }
