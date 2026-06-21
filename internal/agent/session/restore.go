package session

import (
	"github.com/inventivepotter/urvi/internal/agent/loop/event"
	"github.com/inventivepotter/urvi/internal/content"
)

// foldResult is the reconstruction of one loop's committed conversation from its
// ordered Enduring event sequence. It is what the Restore constructor (Task 8.3)
// seeds a re-created loop with: the committed message history, the next live
// turn's index, and whether the history ends on a crash-seam (a turn that was
// started but never terminated).
//
// Msgs is the committed conversation only — it does NOT include a SystemMessage.
// The constructor re-seeds the system prompt from the live config, so the fold
// produces solely what the loop committed (TurnStarted/StepDone/TurnFoldedInto).
//
// TurnIndex is the count of TurnStarted events, which equals loopState.turnIndex
// after the last started turn — the loop increments turnIndex as it installs each
// turn, so the next live turn numbers correctly when the constructor resumes.
//
// OpenTurn reports the crash seam: true when the sequence ends with a turn that
// was started but never reached a terminal (TurnDone/TurnFailed/TurnInterrupted).
// The Task 8.3 constructor closes such a turn by synthesizing a TurnInterrupted
// before resuming, so a resumed loop never observes a half-open turn.
type foldResult struct {
	Msgs      content.AgenticMessages
	TurnIndex event.TurnIndex
	OpenTurn  bool
}

// foldPrimaryLoop reconstructs a loop's committed msgs + turnIndex from an ordered
// slice of its Enduring events, mirroring runLoop's commit arm EXACTLY so a fold
// reproduces loopState.msgs byte-for-byte:
//
//   - TurnStarted    -> append its UserMessage (the loop commits qi.msg as the
//     turn's first message), and increment the turn counter
//     (the loop bumps turnIndex as it installs the turn).
//   - StepDone       -> append its Messages, the finalized step group in order
//     (the single AIMessage followed by its ToolResultMessages),
//     exactly the slice the loop appends at the commit point.
//   - TurnFoldedInto -> append its UserMessage at the fold point (the loop commits
//     the folded user message after that step's tool results).
//   - Everything else (the terminals TurnDone/TurnFailed/TurnInterrupted, and the
//     lifecycle/queue events LoopStarted/LoopIdle/Session*/Restore*/
//     InputQueued/InputCancelled/TurnRejected/TokenDelta) does NOT
//     contribute to msgs — the terminal's AIMessage was already
//     committed via its StepDone, and lifecycle/queue events never
//     mutate loopState.msgs.
//
// It is a PURE function: no I/O, no error. The events are already-typed, journaled
// payloads (each TurnStarted/StepDone/TurnFoldedInto carries its committed
// message[s] verbatim), so there is no malformed-group failure mode to surface — a
// nil Message or empty Messages folds to the same nil/empty the loop itself
// committed. The constructor (Task 8.3) wires the EventReplayer that feeds the
// slice; this function only folds it.
//
// Open-turn (crash-seam) detection rides the same single pass: a TurnStarted opens
// the turn (openTurn = true) and a terminal closes it (openTurn = false). After the
// last event, openTurn is true iff the loop crashed mid-turn — a TurnStarted with no
// later terminal. The interrupted-turn contract follows directly from mirroring the
// commit arm: only StepDone groups (completed steps) and TurnFoldedInto user
// messages reach msgs, so a turn that crashed after some completed steps yields the
// committed UserMessage + those completed step groups and NO partial assistant step
// (the in-flight step never emitted a StepDone, so it never entered the slice); a
// turn that crashed before its first step yields just the committed UserMessage.
func foldPrimaryLoop(events []event.Event) foldResult {
	// Start non-nil so an empty sequence reconstructs as an empty (not nil) thread,
	// matching content.AgenticMessages' documented empty zero value and the loop's
	// own freshly-seeded msgs.
	msgs := content.AgenticMessages{}
	var turnIndex event.TurnIndex
	openTurn := false

	for _, ev := range events {
		switch e := ev.(type) {
		case event.TurnStarted:
			// The loop increments turnIndex then commits the initial UserMessage. A turn
			// is now open until a terminal closes it.
			turnIndex++
			msgs = append(msgs, e.Message)
			openTurn = true
		case event.StepDone:
			// The loop appends the finalized step group (AIMessage + ToolResultMessages).
			msgs = append(msgs, e.Messages...)
		case event.TurnFoldedInto:
			// The loop commits the folded user message at the tool-continuation point.
			msgs = append(msgs, e.Message)
		case event.TurnDone, event.TurnFailed, event.TurnInterrupted:
			// A terminal closes the open turn. Its AIMessage (for TurnDone) was already
			// committed via that step's StepDone, so the terminal adds nothing to msgs.
			openTurn = false
		default:
			// Lifecycle/queue/ephemeral events (LoopStarted/LoopIdle/Session*/Restore*/
			// InputQueued/InputCancelled/TurnRejected/TokenDelta) never mutate msgs and
			// never open or close a turn.
		}
	}

	return foldResult{Msgs: msgs, TurnIndex: turnIndex, OpenTurn: openTurn}
}
