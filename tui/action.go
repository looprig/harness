package tui

import (
	"github.com/inventivepotter/urvi/internal/tool"
	"github.com/inventivepotter/urvi/internal/uuid"
)

// uiActionKind tags a uiAction. It is a closed enum: the interactionModel
// produces a uiAction, and Screen (a later task) switches on Kind to drive the
// agent. A tagged struct — never an `any` payload — keeps the contract typed end
// to end (CLAUDE.md: strict typing, no interface{} in business logic).
type uiActionKind uint8

const (
	// uiNoop is the zero value: the key was consumed (e.g. an edit) and there is
	// nothing for Screen to act on. It is also the deferral result for non-compose
	// modes until modal routing lands (Task 8).
	uiNoop uiActionKind = iota
	// uiSubmit carries composed prose in Text for Screen to start/queue a turn.
	uiSubmit
	// uiRunSlash carries a known slash command name in Slash for Screen to dispatch.
	uiRunSlash
	// uiApprove resolves a permission gate (CallID) at Scope. (Produced in Task 8.)
	uiApprove
	// uiDeny resolves a permission gate (CallID) fail-secure. (Produced in Task 8.)
	uiDeny
	// uiAnswer supplies the AskUser reply Text for the gate CallID. (Task 8.)
	uiAnswer
	// uiInterrupt requests a turn interrupt. (Produced in Task 8.)
	uiInterrupt
)

// uiAction is the single typed result the interactionModel hands back from an
// Update. It is a tagged union: Kind selects which fields are meaningful, and the
// rest stay at their zero values. Defining every variant up front lets later
// tasks PRODUCE approve/deny/answer/interrupt without changing this contract;
// this task only ever produces uiSubmit / uiRunSlash / uiNoop.
type uiAction struct {
	Kind   uiActionKind
	Text   string             // uiSubmit / uiAnswer payload
	CallID uuid.UUID          // uiApprove / uiDeny / uiAnswer target gate
	Scope  tool.ApprovalScope // uiApprove persistence breadth
	Slash  string             // uiRunSlash command name (e.g. "/help")
}
