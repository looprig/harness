package tui

import (
	"context"

	tea "charm.land/bubbletea/v2"

	"github.com/inventivepotter/urvi/internal/uuid"
)

// RestoreBacklogError reports a failure to read a restored session's historical
// Enduring backlog for repaint (the Agent.ReplayBacklog call failed). It is a
// NON-FATAL restore error: the live subscription is unaffected, so the Screen
// surfaces it as a faint error notice and continues with an empty transcript rather
// than a dead surface. It wraps the underlying replay cause so a caller can errors.As
// both this and the journal's typed read error.
type RestoreBacklogError struct {
	Cause error
}

func (e *RestoreBacklogError) Error() string {
	if e.Cause == nil {
		return "tui: restore backlog read failed"
	}
	return "tui: restore backlog read failed: " + e.Cause.Error()
}

func (e *RestoreBacklogError) Unwrap() error { return e.Cause }

// restoredMsg carries the result of the background restore fold (restoreBacklogCmd):
// the rebuilt committed transcript + pending-gate interaction model for a cold-restore
// repaint, OR a non-nil err when the backlog read failed. The reducers were applied
// PER-EVENT inside the command (off the update loop), so the Screen applies this state
// ONCE — it never folds per event on the loop. A new (non-restored) session yields an
// empty transcript here, which the Screen installs as a no-op (no repaint). It is a
// tea.Msg.
type restoredMsg struct {
	transcript  transcriptModel
	interaction interactionModel
	err         error
}

// restoreBacklogCmd is the background-fold command (Task 10.1): OFF the Bubble Tea
// update loop, it reads the restored session's historical Enduring backlog via
// Agent.ReplayBacklog, then folds EVERY event through the SAME pure reducers the live
// path uses (transcript.ApplyEvent + interaction.ApplyEvent) to build the FINAL reducer
// state, and returns a SINGLE restoredMsg. This is the no-UI-hang property: a large
// backlog is folded once here, not delivered as N per-event messages through the live
// Subscribe 256-buffer and not folded per event on the update loop. A read failure
// returns a restoredMsg carrying a typed *RestoreBacklogError (non-fatal). A new session
// (empty backlog) folds to an empty transcript, which the Screen installs as a no-op.
//
// primaryLoopID scopes the rebuilt transcript's committed user rows to the primary loop
// exactly as the live path does (transcriptModel.primaryLoopID) — a subagent loop's
// turns in the backlog surface collapsed via StepDone, never as a human user row.
func restoreBacklogCmd(ctx context.Context, agent Agent, primaryLoopID uuid.UUID) tea.Cmd {
	return func() tea.Msg {
		backlog, err := agent.ReplayBacklog(ctx)
		if err != nil {
			return restoredMsg{err: &RestoreBacklogError{Cause: err}}
		}
		tr := transcriptModel{primaryLoopID: primaryLoopID}
		in := newInteractionModel()
		for _, ev := range backlog {
			tr = tr.ApplyEvent(ev)
			in = in.ApplyEvent(ev)
		}
		return restoredMsg{transcript: tr, interaction: in}
	}
}

// compile-time guard: a restoredMsg is a tea.Msg (any value satisfies tea.Msg, but the
// assignment documents intent and fails loudly if the alias ever narrows).
var _ tea.Msg = restoredMsg{}

// compile-time guard: *RestoreBacklogError is an error.
var _ error = (*RestoreBacklogError)(nil)
