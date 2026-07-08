package event

import (
	"github.com/looprig/harness/pkg/gate"
	"github.com/looprig/harness/pkg/tool"
)

// GatePrepared is the private/internal prepared projection inside a
// GatePreparedRecord. It is durable so restore can validate a later GateOpened,
// but it is NOT fanned out to SSE/history and does not make the gate answerable.
// It must never be appended through NewEventRecord or hub.PublishEvent; it is
// only valid inside journal.GatePreparedRecord.
type GatePrepared struct {
	enduring
	loopScoped
	Header
	Gate gate.Gate `json:"gate,omitzero"`
}

// GateOpened is the PUBLIC activation event for a gate. It carries the pure
// public envelope (the Gate) and NO private payload — the typed payload stays
// server-private inside the GatePreparedRecord. It fans out to SSE/history and
// makes the gate listable/answerable.
type GateOpened struct {
	enduring
	loopScoped
	Header
	Gate gate.Gate `json:"gate,omitzero"`
}

// GateResolved is the SINGLE atomic close-with-answer record. Decision fields
// (Action, ApprovalScope) stay in the clear; Reason is the close reason; per-kind
// Audit is redaction-aware (grant descriptions, not tokens). A non-answer close
// (abandon/owner) sets Reason with Action="".
//
// The approval-scope field is named ApprovalScope (not Scope) because the embedded
// loopScoped mixin promotes a Scope() method — a field named Scope would shadow
// that method and fail to satisfy the Event interface.
type GateResolved struct {
	enduring
	loopScoped
	Header
	GateID        gate.ID             `json:"gate_id,omitzero"`
	Reason        gate.CloseReason    `json:"reason,omitempty"`
	Action        string              `json:"action,omitempty"`
	ApprovalScope tool.ApprovalScope  `json:"scope,omitzero"`
	Source        gate.ResponseSource `json:"source,omitzero"`
	// Audit is a sealed interface (gate.ResponseAudit) with no general JSON codec,
	// so it is excluded from direct serialization — like PermissionRequested.Request
	// — and projected through gate.MarshalResponseAudit by the marshaler.
	Audit gate.ResponseAudit `json:"-"`
}

func (GatePrepared) isEvent() {}
func (GateOpened) isEvent()   {}
func (GateResolved) isEvent() {}
