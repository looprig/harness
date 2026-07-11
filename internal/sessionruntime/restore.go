package sessionruntime

import (
	"github.com/looprig/core/content"
	"github.com/looprig/harness/pkg/ceiling"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/identity"
)

func checkFingerprint(persisted, live event.ConfigFingerprint, allowMismatch bool) error {
	if persisted.Equal(live) {
		return nil
	}
	if allowMismatch {
		return nil
	}
	return &ConfigMismatchError{Persisted: persisted, Live: live}
}

// checkAgentName is the restore root-loop AgentName decision: it returns nil when the
// persisted (root LoopStarted) name and the configured (primary loop.Definition) name are
// equal, a typed *AgentNameMismatchError when they differ, and — when allowMismatch is
// set — nil even on a difference (the operator's explicit opt-in, shared with the
// fingerprint override). It is fail-secure by default and treats an EMPTY persisted name
// vs a non-empty configured one as a mismatch: a legacy/pre-AgentName record is never
// silently accepted as a match (plain string inequality covers this — "" != "operator").
func checkAgentName(persisted, configured identity.AgentName, allowMismatch bool) error {
	if persisted == configured {
		return nil
	}
	if allowMismatch {
		return nil
	}
	return &AgentNameMismatchError{Persisted: persisted, Configured: configured}
}

// firstConfigFingerprint extracts the persisted config fingerprint from the FIRST
// SessionStarted in the replayed event slice (stream-sequence order). A stream with no
// SessionStarted fails closed with a typed *RestoreDiscoveryError — there is nothing to
// check the live config against, so the restore must not proceed.
func firstConfigFingerprint(events []event.Event) (event.ConfigFingerprint, error) {
	for _, ev := range events {
		if ss, ok := ev.(event.SessionStarted); ok {
			return ss.Config, nil
		}
	}
	return event.ConfigFingerprint{}, &RestoreDiscoveryError{Kind: RestoreNoSessionStarted}
}

// findRootLoopStarted locates the session's root LoopStarted: the one whose
// Cause.Coordinates is zero (a root loop has no spawning loop/turn/step). Restore reads
// two facts off it — the primary loop's stable id (the recovered loop comes up under THIS
// id, keeping session identity stable) and its immutable stamped AgentName — so the "what
// is the root loop" rule lives in one place. A stream with no root LoopStarted fails closed
// with a typed *RestoreDiscoveryError.
func findRootLoopStarted(events []event.Event) (event.LoopStarted, error) {
	for _, ev := range events {
		ls, ok := ev.(event.LoopStarted)
		if !ok {
			continue
		}
		if ls.Cause.Coordinates == (identity.Coordinates{}) {
			return ls, nil
		}
	}
	return event.LoopStarted{}, &RestoreDiscoveryError{Kind: RestoreNoPrimaryLoop}
}

func findForeignSID(events []event.Event) string {
	for _, ev := range events {
		if ls, ok := ev.(event.LoopStarted); ok && ls.ForeignSID != "" {
			return ls.ForeignSID
		}
	}
	for _, ev := range events {
		if fb, ok := ev.(event.ForeignSessionBound); ok && fb.ForeignSID != "" {
			return fb.ForeignSID
		}
	}
	return ""
}

// countSpawnedLoops counts the durable NON-ROOT LoopStarted events in the replayed
// stream — those whose Header.Cause.Coordinates is non-zero (a subagent spawn carries the
// spawning loop/turn/step in its Cause). It is the restore-time re-seed of the session's
// cumulative spawn counter: the live counter increments only on a successful NewLoop spawn
// (a rejected or rolled-back spawn emits NO LoopStarted, §6d), so counting the durable
// non-root LoopStarted events reproduces exactly the live `spawned` value at crash time.
// The ROOT LoopStarted (the primary, Cause zero) is excluded — the primary never counts
// toward the quota — using the SAME root/non-root discriminator findRootLoopStarted and the
// live NewLoop counter use, so the restored quota matches the live one and a restart cannot
// grant a fresh budget.
//
// It scans the full-stream replay (the `all` slice restore already drains for discovery,
// which spans EVERY loop's events — it is not loop-scoped), so no extra read is needed.
func countSpawnedLoops(events []event.Event) int {
	n := 0
	for _, ev := range events {
		ls, ok := ev.(event.LoopStarted)
		if !ok {
			continue
		}
		if ls.Cause.Coordinates != (identity.Coordinates{}) {
			n++
		}
	}
	return n
}

// effectiveCurrentWorkspace returns the Ref selected by the LAST durable workspace
// transition, whether checkpoint or restore. It is scanned from the unnarrowed discovery
// drain because both event types are session-scoped. A restore therefore changes the resume
// point without erasing the independently projected LastCheckpoint history.
func effectiveCurrentWorkspace(events []event.Event) (string, bool) {
	ref, ok := "", false
	for _, ev := range events {
		switch e := ev.(type) {
		case event.WorkspaceCheckpointed:
			ref, ok = e.Ref, true
		case event.WorkspaceRestored:
			ref, ok = e.Ref, true
		}
	}
	return ref, ok
}

// lastSecurityCeiling returns the ordinal of the LAST SecurityCeilingChanged event in the
// replay — the live security ceiling to re-seed on resume (last write wins) — and false if
// the session never changed its ceiling (it then resumes at the fail-secure most-restrictive
// default). SecurityCeilingChanged is session-scoped, so it is present in the unnarrowed
// discovery drain; scanning to the end picks the newest (a change appends a fresh event,
// never replacing the prior one). It mirrors effectiveCurrentWorkspace — a single-purpose
// discovery scanner — so the restore constructor stays a straight-line assembly and
// foldPrimaryLoop stays pure.
func lastSecurityCeiling(events []event.Event) (ceiling.Level, bool) {
	level, ok := ceiling.Level(0), false
	for _, ev := range events {
		if sc, isSC := ev.(event.SecurityCeilingChanged); isSC {
			level, ok = sc.Level, true // keep scanning; the LAST one wins
		}
	}
	return level, ok
}

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
