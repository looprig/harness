package command

import (
	"github.com/looprig/core/content"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/identity"
)

// UserInput is interactive input. The loop decides its outcome; the caller never
// assumes a turn was created. Submit commands DO NOT carry a context (no Ctx
// field): a queued input can start much later, fold, be cancelled, or be returned,
// so the loop derives the turn context from its own loopCtx only when a turn
// actually starts. A UserInput may queue behind a running turn (it later folds into
// a tool-continuation request or starts a later turn).
//
// The loop announces the outcome by PUBLISHING a typed Reply event onto the normal
// session fan-in (event.TurnStarted / event.InputQueued / event.TurnRejected, each
// carrying Cause.CommandID == this command's id), NOT a point-to-point reply: every
// submit observes its outcome on the session event fan-in.
type UserInput struct {
	Header
	Blocks []content.Block `json:"blocks,omitempty"`
	// NoFold requests a DISTINCT non-folding turn: the input still queues behind a
	// running turn, but it NEVER folds into that turn at a tool-continuation boundary —
	// it starts its OWN turn (Cause.CommandID = this command's id) when the running turn
	// finishes. It is the delegation follow-up path, where each request is an independent
	// question/answer correlated by command id; the interactive submit path leaves it
	// false so ordinary input keeps its fold-into-turn semantics.
	NoFold bool `json:"no_fold,omitzero"`
	// TargetLoopID durably carries the dispatch target for machine NoFold delegate
	// requests because storage replay cannot recover CommandRecord's transport-only loop.
	TargetLoopID uuid.UUID `json:"target_loop_id,omitzero"`
	// Accepted is the transient durable-acceptance ack used only by managed delegate
	// sends. It is never serialized; prepared starts use LoopStarted.InitialRequestID.
	Accepted chan error `json:"-"`
}

// SubagentResult delivers a finished subagent's output to its parent loop (the
// hand-back). It shares UserInput's submit semantics — the parent loop's events go
// to the session fan-in.
//
// It carries TWO loop ids with distinct jobs:
//
//   - The embedded identity.Coordinates addresses the PARENT loop — the delivery
//     target. The session dispatches the command to loops[Coordinates.LoopID].
//   - Header.Cause.LoopID is the CHILD loop that produced the result. When the
//     parent folds the result into a turn, the loop stamps this Cause.LoopID onto
//     any start/queue/fold/return event the submit causes, which releases the
//     parent's quiescence wake token on the publish path.
//
// Header.Agency stays AgencyMachine (the zero default): a hand-back is
// machine-originated, never user.
//
// A SubagentResult is NEVER rejected, so its wake token is ALWAYS released by a
// published Enduring event (TurnStarted/TurnFoldedInto, or InputCancelled if the
// loop ends before it commits) — there is no off-publish-path reconciliation
// anymore.
type SubagentResult struct {
	Header                               // command.Header; Cause.LoopID = CHILD loop; Agency = AgencyMachine
	identity.Coordinates                 // addresses the PARENT loop (delivery target)
	Blocks               []content.Block `json:"blocks,omitempty"`
}

func (UserInput) isCommand()      {}
func (SubagentResult) isCommand() {}
