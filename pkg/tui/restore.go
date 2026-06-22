package tui

import (
	"context"
	"reflect"

	tea "charm.land/bubbletea/v2"

	"github.com/ciram-co/looprig/pkg/event"
	"github.com/ciram-co/looprig/pkg/uuid"
)

// DisplayProjection is the committed TUI projection of a fold over the primary loop's
// Enduring events — the "displayed" transcript the event-persistence design's headline
// property compares (displayed == stored == restored). It bundles the two pure reducer
// states (the scrollback transcript and the pending-gate interaction surface) so the
// restore-repaint path and the persistence property tests build the displayed view
// through one named seam. It is value-typed and immutable; FoldDisplay is its only
// constructor.
type DisplayProjection struct {
	transcript  transcriptModel
	interaction interactionModel
}

// FoldDisplay folds events through the SAME pure reducers the live path and the
// cold-restore repaint use (transcript.ApplyEvent + interaction.ApplyEvent), starting
// from the zero reducer state scoped to primaryLoopID, and returns the resulting
// displayed projection. It is the single fold the TUI uses to turn a slice of Enduring
// events into a repaintable transcript: restoreBacklogCmd folds the restored backlog
// through it, and the persistence property tests fold both a restored ReplayBacklog and
// the original live Enduring sequence through it to assert the two displayed views are
// identical. The fold is order-sensitive and side-effect-free — folding the same events
// twice yields an EqualTranscript pair.
func FoldDisplay(events []event.Event, primaryLoopID uuid.UUID) DisplayProjection {
	tr := transcriptModel{primaryLoopID: primaryLoopID}
	in := newInteractionModel()
	for _, ev := range events {
		tr = tr.ApplyEvent(ev)
		in = in.ApplyEvent(ev)
	}
	return DisplayProjection{transcript: tr, interaction: in}
}

// EqualTranscript reports whether p and other have the byte-for-byte identical
// committed transcript (the displayed scrollback), via reflect.DeepEqual over the
// transcript reducer state. It is the headline-property comparator: a restored
// session's repainted transcript EqualTranscript the original session's live transcript
// iff the repaint reproduced the displayed view exactly. The interaction surface (its
// input editor carries cursor state and completion-panel closures that are not value-
// comparable) is intentionally NOT part of this equality — assert PendingPrompts for
// the pending-gate dimension instead.
func (p DisplayProjection) EqualTranscript(other DisplayProjection) bool {
	return reflect.DeepEqual(p.transcript, other.transcript)
}

// CommittedLen is the number of committed (finalized) transcript entries — the rows the
// repaint flushes to scrollback.
func (p DisplayProjection) CommittedLen() int { return len(p.transcript.committed) }

// PendingPrompts is the number of pending prompts (permission gates + AskUser requests)
// the projection's interaction surface holds — the gate dimension a transcript deep-
// equal does not cover.
func (p DisplayProjection) PendingPrompts() int { return p.interaction.PendingCount() }

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
		proj := FoldDisplay(backlog, primaryLoopID)
		return restoredMsg{transcript: proj.transcript, interaction: proj.interaction}
	}
}

// handleRestored applies the background fold's result ONCE (Task 10.2 cold-restore
// handoff). On a non-nil err it commits a faint, NON-FATAL restore-error notice (the
// live stream is unaffected; history simply did not repaint) and flushes it. On success
// it INSTALLS the rebuilt transcript + interaction wholesale (the state arrived
// pre-folded — no per-event work here) and flushes the committed backlog to scrollback
// ONCE via the print-once engine. A new session's empty transcript installs nothing and
// flush is a no-op, so a fresh session behaves exactly as today (no repaint). The live
// Subscribe path is attached separately (handleSubscribed) and, since cold restore comes
// up idle, live events only follow a user Submit — so there is no backlog/live overlap
// and no dedup is needed.
func (m *Screen) handleRestored(msg restoredMsg) tea.Cmd {
	if msg.err != nil {
		m.transcript = m.transcript.CommitError(msg.err)
		return m.flush()
	}
	m.transcript = msg.transcript
	m.interaction = msg.interaction
	return m.flush()
}

// compile-time guard: a restoredMsg is a tea.Msg (any value satisfies tea.Msg, but the
// assignment documents intent and fails loudly if the alias ever narrows).
var _ tea.Msg = restoredMsg{}

// compile-time guard: *RestoreBacklogError is an error.
var _ error = (*RestoreBacklogError)(nil)
