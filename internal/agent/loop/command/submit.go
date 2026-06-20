package command

import (
	"github.com/inventivepotter/urvi/internal/agent/loop/event"
	"github.com/inventivepotter/urvi/internal/agent/loop/identity"
	"github.com/inventivepotter/urvi/internal/content"
)

// InputMode lets the caller say whether queueing behind a running turn is
// allowed. The loop decides the actual disposition on its own live state; the
// mode only constrains what the loop is permitted to do when it is already busy.
type InputMode uint8

const (
	// AllowFold is the interactive mode: queue while a turn runs (the queued
	// input may later fold into a tool-continuation request or start a later
	// turn). Submit commands never carry a turn context; a queued input can start
	// much later.
	AllowFold InputMode = iota
	// StartOnly is the programmatic single-shot mode (Invoke/Stream): the submit
	// must start a turn or be rejected. A busy loop returns TurnRejected{RejectBusy}.
	StartOnly
)

// UserInput is interactive input. The loop decides its outcome; the caller never
// assumes a turn was created. Submit commands DO NOT carry a context (no Ctx
// field): a queued input can start much later, fold, be cancelled, or be returned,
// so the loop derives the turn context from its own loopCtx only when a turn
// actually starts.
//
// The loop announces the outcome by PUBLISHING a typed Reply event onto the normal
// session fan-in (event.TurnStarted / event.InputQueued / event.TurnRejected, each
// carrying Cause.CommandID == this command's id), NOT a point-to-point reply.
//
// Events/Abandoned are the OPTIONAL per-turn stream: StartOnly callers
// (Invoke/Stream) set them to observe the outcome (and, on success, the turn's
// events) on a dedicated channel — the loop delivers the same outcome event there
// before the terminal; a fan-in-only submit leaves them nil and observes results on
// the session event fan-in.
type UserInput struct {
	Header
	Blocks []content.Block `json:"blocks,omitempty"`
	Mode   InputMode       `json:"mode,omitzero"`
	// Events/Abandoned are live channels for the optional per-turn stream; channels
	// have no JSON representation, so they are tagged json:"-" (journal-prep is for
	// the durable command shape, not the in-memory stream wiring).
	Events    chan<- event.Event `json:"-"`
	Abandoned <-chan struct{}    `json:"-"`
}

// SubagentResult delivers a finished subagent's output to its parent loop (the
// hand-back). It shares UserInput's submit semantics but is always AllowFold and
// carries no per-turn stream — the parent loop's events go to the session fan-in.
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
