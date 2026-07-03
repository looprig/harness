package session

import (
	"context"
	"strings"

	"github.com/looprig/harness/pkg/content"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/uuid"
)

// drainFailedError wraps a TurnFailed.Err terminal: the sub-loop's turn ended on
// a non-cancellation provider/LLM error. Cause is the typed cause the loop
// carried; callers errors.As to this type and errors.Is/As through Unwrap to the
// original cause.
type drainFailedError struct {
	Cause error
}

func (e *drainFailedError) Error() string {
	if e.Cause == nil {
		return "drain: turn failed"
	}
	return "drain: turn failed: " + e.Cause.Error()
}
func (e *drainFailedError) Unwrap() error { return e.Cause }

// drainInterruptedError is returned when the sub-loop's turn ended on a
// TurnInterrupted terminal — the caller went away (ctx cancel) and the helper's
// fail-safe interrupt stopped the loop, or a distributed Interrupt reached it.
// There is no partial result.
type drainInterruptedError struct{}

func (e *drainInterruptedError) Error() string { return "drain: turn interrupted" }

// A submit refused before any turn starts (TurnRejected for the submit's
// Cause.CommandID) surfaces as the package's existing *TurnRejectedError: its
// Reason and event.TurnRejected.Reason are the same type (event.RejectReason),
// so there is no second rejected-error type or duplicated reason→string switch.

// drainLostError is returned when sub.Events() closes before a terminal arrives:
// the hub force-closed the subscription (Cause set to sub.Err()) or the sub-loop
// exited with no terminal (Cause nil). Either way no final text exists.
type drainLostError struct {
	Cause error
}

func (e *drainLostError) Error() string {
	if e.Cause == nil {
		return "drain: subscription closed before terminal event"
	}
	return "drain: subscription lost: " + e.Cause.Error()
}
func (e *drainLostError) Unwrap() error { return e.Cause }

// drainToFinalText drains a (sub-)loop's events from a caller-owned subscription
// to the terminal for the submit identified by commandID, and returns the final
// assistant text (or a typed error per the §5 failure contract).
//
// Correlation is two-phase: phase 1 scans for the opening event.TurnStarted whose
// Header.Cause.CommandID == commandID and captures its TurnID and LoopID; phase 2
// matches StepDone/terminal events by BOTH that TurnID and LoopID (they do not
// carry Cause.CommandID). The LoopID check is fail-secure defense-in-depth: the
// subscription is scoped to one sub-loop so every event should already carry that
// loop's id, and a phase-2 event from a different loop is thereby provably ignored
// rather than merely "can't happen". Events for other commands/turns interleaved
// on the stream are ignored. A TurnRejected for commandID in phase 1 means the
// submit was refused and no turn will ever start, so it short-circuits to the
// package's typed *TurnRejectedError.
//
// The final text is taken from the TurnDone.Message terminal, falling back to the
// latest StepDone's assistant text when Message is nil/empty.
//
// ctx is the calling turn's context and interrupt is the loop-targeted Interrupt
// bound to the sub-loop. Submits carry no ctx, so cancelling ctx cannot reach the
// sub-loop's turn — only an explicit Interrupt can. On ctx.Done() the helper
// therefore calls interrupt() EXACTLY ONCE (fail-safe) and keeps draining to the
// sub-loop's TurnInterrupted terminal so the loop does not orphan on ctx-cancel.
// This fail-safe relies on the sub-loop honouring the interrupt by producing a
// terminal (or the subscription closing); a pathologically wedged provider that
// ignores its turn ctx after the interrupt is a known, pre-existing liveness corner.
//
// CALLER RESPONSIBILITY: subscribe BEFORE submitting. The hub has no replay, so a
// subscription created after the submit could miss the opening TurnStarted and the
// helper would then block until ctx-cancel or subscription loss. The helper does
// not — and cannot — enforce this ordering; it is the one subtlety the caller owns.
func drainToFinalText(ctx context.Context, sub event.Subscription, commandID uuid.UUID, interrupt func()) (string, error) {
	var (
		turnID    uuid.UUID // captured from the opening TurnStarted (phase-1 -> phase-2 edge)
		loopID    uuid.UUID // captured alongside turnID; phase-2 cross-checks it (fail-secure)
		haveTurn  bool
		lastStep  string // latest StepDone assistant text for the matched turn (fallback)
		fired     bool   // guards the single fail-safe interrupt() on ctx.Done()
		ctxClosed bool   // once true, await terminal/close without re-selecting ctx.Done()
	)

	for {
		// After ctx fired and interrupt() ran, stop selecting on ctx.Done() (it
		// stays selectable once cancelled — re-selecting it would busy-loop). Just
		// await the sub-loop's terminal (or a subscription loss).
		if ctxClosed {
			ev, ok := <-sub.Events()
			if !ok {
				return "", &drainLostError{Cause: sub.Err()}
			}
			if text, done, err := handleEvent(ev, commandID, &turnID, &loopID, &haveTurn, &lastStep); done {
				return text, err
			}
			continue
		}

		select {
		case ev, ok := <-sub.Events():
			if !ok {
				return "", &drainLostError{Cause: sub.Err()}
			}
			if text, done, err := handleEvent(ev, commandID, &turnID, &loopID, &haveTurn, &lastStep); done {
				return text, err
			}
		case <-ctx.Done():
			// Boundary cancel: the submit carried no ctx, so translate it into a
			// single loop-targeted Interrupt and keep draining for the resulting
			// TurnInterrupted terminal.
			ctxClosed = true
			if !fired {
				fired = true
				interrupt()
			}
		}
	}
}

// handleEvent applies one event to the two-phase correlation state. It returns
// done=true with the result (text+err) only on the matched turn's terminal (or a
// phase-1 rejection); otherwise done=false and the caller keeps draining. turnID,
// loopID, haveTurn, and lastStep are updated in place across calls.
func handleEvent(
	ev event.Event,
	commandID uuid.UUID,
	turnID *uuid.UUID,
	loopID *uuid.UUID,
	haveTurn *bool,
	lastStep *string,
) (text string, done bool, err error) {
	if !*haveTurn {
		// Phase 1: await the opening resolution event for our submit.
		switch e := ev.(type) {
		case event.TurnStarted:
			if e.Cause.CommandID == commandID {
				*turnID = e.Coordinates.TurnID
				*loopID = e.Coordinates.LoopID
				*haveTurn = true
			}
		case event.TurnRejected:
			if e.Cause.CommandID == commandID {
				return "", true, &TurnRejectedError{Reason: e.Reason}
			}
		}
		return "", false, nil
	}

	// Phase 2: match StepDone/terminal events by the captured TurnID AND LoopID.
	// LoopID is the fail-secure cross-check (the sub is scoped to one loop, so a
	// phase-2 event from a different loop is provably ignored, not "can't happen").
	switch e := ev.(type) {
	case event.StepDone:
		if e.Coordinates.TurnID == *turnID && e.Coordinates.LoopID == *loopID {
			if t := stepDoneText(e.Messages); t != "" {
				*lastStep = t
			}
		}
	case event.TurnDone:
		if e.Coordinates.TurnID == *turnID && e.Coordinates.LoopID == *loopID {
			final := aiText(e.Message)
			if final == "" {
				final = *lastStep
			}
			return final, true, nil
		}
	case event.TurnFailed:
		if e.Coordinates.TurnID == *turnID && e.Coordinates.LoopID == *loopID {
			return "", true, &drainFailedError{Cause: e.Err}
		}
	case event.TurnInterrupted:
		if e.Coordinates.TurnID == *turnID && e.Coordinates.LoopID == *loopID {
			return "", true, &drainInterruptedError{}
		}
	}
	return "", false, nil
}

// aiText concatenates the text of every TextBlock in an AIMessage. A nil message
// or a message with no text blocks yields the empty string.
func aiText(m *content.AIMessage) string {
	if m == nil {
		return ""
	}
	var b strings.Builder
	for _, blk := range m.Blocks {
		if tb, ok := blk.(*content.TextBlock); ok {
			b.WriteString(tb.Text)
		}
	}
	return b.String()
}

// stepDoneText extracts the assistant text from a StepDone's committed group. A
// StepDone carries the step's single AIMessage followed by its ToolResultMessages
// (content.AgenticMessages, a sealed []Conversation union); the fallback text is
// the AIMessage's concatenated TextBlocks. The empty string means no assistant
// text in this step (e.g. a pure tool-use step).
func stepDoneText(msgs content.AgenticMessages) string {
	for _, m := range msgs {
		if ai, ok := m.(*content.AIMessage); ok {
			return aiText(ai)
		}
	}
	return ""
}

// Compile-time assertions that every typed exit error satisfies error.
var (
	_ error = (*drainFailedError)(nil)
	_ error = (*drainInterruptedError)(nil)
	_ error = (*drainLostError)(nil)
)
