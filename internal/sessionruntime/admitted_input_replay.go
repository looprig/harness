package sessionruntime

import (
	"context"
	"log/slog"

	"github.com/looprig/core/uuid"

	"github.com/looprig/harness/pkg/command"
	"github.com/looprig/harness/pkg/journal"
	"github.com/looprig/harness/pkg/runtimecommand"
)

// admittedInputReplay is one Host-admitted input the journal says this runtime OWES:
// its disposition is durably `applied`, and no event it caused is durable.
type admittedInputReplay struct {
	loopID uuid.UUID
	cmd    command.UserInput
}

// planAppliedAdmittedInputs finds every admitted input whose `applied` disposition
// is durable but whose effect is not, in disposition order.
//
// `applied` for an input means the runtime durably accepted it into its execution
// path. The loop's inbox is memory, so a runtime that dies — or shuts down — between
// that acceptance and the input's TurnStarted/TurnFoldedInto leaves a command
// settled `applied` whose input never ran. Nothing else can recover it: the record is
// terminal in the store, so no successor may close it, and redelivery deduplicates
// on the prefix. Restore is therefore the owner of the debt, and this is its ledger.
//
// THE PREDICATE IS THE RECOVERY SCAN'S (ScanCommandEffect): any durable event whose
// Cause.CommandID is the runtime command id counts as the input having been dealt
// with — started, folded, rejected, or cancelled — at any sequence. Using the same
// predicate as the closure guard keeps the two authorities from disagreeing about
// one journal, and it is the conservative direction for replay: an input answered
// by anything durable is never applied twice.
//
// It reads journals v0.36.0 wrote. That writer appended the intent record, the
// prefix and the applied disposition in that order and left no new record kind, so
// its crash-window journals are exactly the shape this plan replays. A v0.36.0
// journal whose shutdown returned the input as InputCancelled is NOT replayed — the
// cancellation is durable and indistinguishable from an interrupt's — which is the
// released behaviour for that journal, not a new loss.
//
// A disposition with no intent record carries nothing to replay: a create with no
// first message has no blocks, and a v0.36.0 intent append was audit-only and may be
// missing. Both are skipped. v0.37.0 makes the intent append load-bearing for every
// attempt-bearing input, so for journals it writes the second case cannot arise.
func planAppliedAdmittedInputs(records []journal.JournalRecord) []admittedInputReplay {
	var order []uuid.UUID
	applied := make(map[uuid.UUID]struct{})
	caused := make(map[uuid.UUID]struct{})
	intents := make(map[uuid.UUID]journal.CommandRecord)
	for _, rec := range records {
		switch r := rec.(type) {
		case journal.CommandDispositionRecord:
			d := r.Disposition()
			if d.Disposition != runtimecommand.DispositionApplied {
				continue
			}
			if d.Kind != runtimecommand.KindInput && d.Kind != runtimecommand.KindCreate {
				continue
			}
			if _, seen := applied[d.RuntimeCommandID]; !seen {
				applied[d.RuntimeCommandID] = struct{}{}
				order = append(order, d.RuntimeCommandID)
			}
		case journal.EventRecord:
			if id := r.Event().EventHeader().Cause.CommandID; !id.IsZero() {
				caused[id] = struct{}{}
			}
		case journal.CommandRecord:
			input, ok := r.Command().(command.UserInput)
			if !ok {
				continue
			}
			if _, seen := intents[input.CommandID]; !seen {
				intents[input.CommandID] = r
			}
		}
	}
	var plan []admittedInputReplay
	for _, id := range order {
		if _, done := caused[id]; done {
			continue
		}
		intent, ok := intents[id]
		if !ok {
			continue
		}
		input := intent.Command().(command.UserInput)
		if len(input.Blocks) == 0 {
			continue
		}
		plan = append(plan, admittedInputReplay{loopID: intent.LoopID(), cmd: input})
	}
	return plan
}

// replayAppliedAdmittedInputs re-offers each planned input to its loop, in order,
// once the restored session is live. The original command crosses the actor boundary
// again with its original id, so its events correlate to the same runtime command.
// No intent record is appended — one is already durable — and nothing is committed:
// the `applied` disposition is already durable, so the Admission carries a nil
// Commit and exists only to mark the input carry-over and to learn the loop's answer.
//
// A replay that fails is logged and left: the journal is unchanged, so the next
// restore plans it again.
func (s *Session) replayAppliedAdmittedInputs(ctx context.Context, plan []admittedInputReplay) {
	for _, entry := range plan {
		if err := s.replayAdmittedInput(ctx, entry); err != nil {
			slog.ErrorContext(ctx, "session: replaying an applied admitted input failed; the next restore will retry it",
				"session", s.sessionID, "command_id", entry.cmd.CommandID, "err", err)
		}
	}
}

func (s *Session) replayAdmittedInput(ctx context.Context, entry admittedInputReplay) error {
	l, ok := s.loopFor(entry.loopID)
	if !ok || l == nil {
		// The input's loop did not survive restore; the active loop is where an
		// admitted input is delivered, so it is where the debt is paid.
		s.loopsMu.RLock()
		active := s.activeLoopID
		s.loopsMu.RUnlock()
		if l, ok = s.loopFor(active); !ok {
			return &SessionError{Kind: SessionLoopNotFound}
		}
		if l == nil {
			return &SessionError{Kind: SessionLoopExited}
		}
	}
	result := make(chan error, 1)
	cmd := entry.cmd
	cmd.Accepted = nil
	cmd.Admission = &command.Admission{Result: result}
	if _, err := s.sendUserInput(ctx, l, cmd); err != nil {
		return err
	}
	select {
	case err := <-result:
		return err
	case <-l.DoneChan():
		select {
		case err := <-result:
			return err
		default:
			return &SessionError{Kind: SessionLoopExited}
		}
	}
}
