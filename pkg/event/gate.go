package event

import (
	"github.com/looprig/core/content"
	"github.com/looprig/harness/pkg/gate"
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
	// Resume is the snapshot a restored session needs to CONTINUE the tool step this
	// gate parks, rather than interrupt its turn: the step's assistant message, as it
	// will be committed, and which of its tool calls the gate belongs to. It is set
	// only for a loop-owned tool gate a successor can resume (a permission gate, or a
	// user-input gate raised by a tool.UserInputReplaySafe tool); nil means restore
	// keeps the closure behaviour this record always had.
	//
	// It carries raw tool arguments, which is why it lives HERE and nowhere public:
	// GatePrepared rides only inside the private journal.GatePreparedRecord, never
	// SSE/history. The same message reaches the public StepDone once the step commits.
	//
	// Additive and backward-compatible on the wire: the field is omitted when nil,
	// and the plain decoder an older Harness uses ignores it, so a journal carrying it
	// replays on an older runtime exactly as before (the gate is closed and the turn
	// interrupted at restore). It is not a one-way upgrade.
	Resume *ToolStepResume `json:"resume,omitempty"`
}

// ToolStepResume is GatePrepared's private resume snapshot. See GatePrepared.Resume.
type ToolStepResume struct {
	// StepIndex is the turn-local index of the parked step.
	StepIndex uint64 `json:"step_index"`
	// Message is the step's assistant message exactly as StepDone would commit it,
	// including every tool call of the step and any provider-opaque reasoning state.
	Message *content.AIMessage `json:"message"`
	// ToolUseID is the provider tool-use id of the call this gate blocks.
	ToolUseID string `json:"tool_use_id"`
}

// Valid reports whether r names a call its own message carries. A snapshot that
// does not is unusable, and restore treats it exactly like an absent one.
func (r *ToolStepResume) Valid() bool {
	if r == nil || r.Message == nil || r.ToolUseID == "" {
		return false
	}
	found := 0
	for _, block := range r.Message.Blocks {
		if use, ok := block.(*content.ToolUseBlock); ok && use.ID == r.ToolUseID {
			found++
		}
	}
	return found == 1
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

// GateResolved is the SINGLE atomic close-with-answer record. The decision
// Action stays in the clear (for a permission gate it is one of the three
// exact gate.ApprovalAction strings); Reason is the close reason; per-kind
// Audit is redaction-aware (requirement and candidate descriptions, never
// grant tokens, never raw tool arguments). A non-answer close (abandon/owner)
// sets Reason with Action="".
type GateResolved struct {
	enduring
	loopScoped
	Header
	GateID gate.ID `json:"gate_id,omitzero"`
	// Resolver is the self-contained scope discriminator the DECODER needs to pick
	// this record's identity profile. GatePrepared and GateOpened embed the full
	// gate.Gate (whose Resolver already names the owner), but GateResolved carries
	// only the GateID and coordinates — so without this field a decoded GateResolved
	// could not tell a host-owned gate (SessionID required; loop/turn/step optional)
	// from a loop-owned one (full step profile). It is additive and omitempty: an old
	// record written before this field existed decodes with an empty Resolver and is
	// validated under the strict loop-owned profile, matching every gate record that
	// could ever restore before this change.
	Resolver gate.ResolverKind   `json:"resolver,omitempty"`
	Reason   gate.CloseReason    `json:"reason,omitempty"`
	Action   string              `json:"action,omitempty"`
	Source   gate.ResponseSource `json:"source,omitzero"`
	// Audit is a sealed interface (gate.ResponseAudit) with no general JSON codec,
	// so it is excluded from direct serialization — like PermissionRequested.Request
	// — and projected through gate.MarshalResponseAudit by the marshaler.
	Audit gate.ResponseAudit `json:"-"`
}

func (GatePrepared) isEvent() {}
func (GateOpened) isEvent()   {}
func (GateResolved) isEvent() {}
