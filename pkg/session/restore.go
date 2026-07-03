package session

import (
	"strconv"

	"github.com/looprig/harness/pkg/content"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/identity"
	"github.com/looprig/harness/pkg/uuid"
)

// ConfigMismatchError is the fail-secure rejection a restore returns when the live
// config's fingerprint does not match the one the session was started under
// (persisted on its SessionStarted). Restoring a session against a materially changed
// config (a different model, system prompt, or tool policy) would silently resume a
// conversation under behavior it never ran with, so the restore refuses by default. An
// operator who knowingly wants to proceed passes WithAllowConfigMismatch. Persisted is
// the fingerprint from the journal; Live is the fingerprint of the config Restore was
// called with.
type ConfigMismatchError struct {
	Persisted event.ConfigFingerprint
	Live      event.ConfigFingerprint
}

func (e *ConfigMismatchError) Error() string {
	return "session: restore config mismatch: persisted model=" + e.Persisted.ModelID +
		" != live model=" + e.Live.ModelID +
		" (system/tool digests may also differ); pass WithAllowConfigMismatch to override"
}

// AgentNameMismatchError is the fail-secure rejection a restore returns when the root
// loop's stamped AgentName (from its LoopStarted) does not match the AgentName of the
// live primary loop.Config. The AgentName is the loop's immutable attribution identity;
// resuming a session under a different agent name — including resuming a pre-AgentName
// record (empty Persisted) under a now-named config, which is treated as a mismatch, not
// silently accepted — would silently re-attribute the conversation, so the restore
// refuses by default. An operator who knowingly wants to proceed passes the same
// WithAllowConfigMismatch override the fingerprint check honors. Persisted is the name
// from the journal's root LoopStarted; Configured is the name on the config Restore was
// called with.
type AgentNameMismatchError struct {
	Persisted  identity.AgentName
	Configured identity.AgentName
}

func (e *AgentNameMismatchError) Error() string {
	return "session: restore agent name mismatch: persisted=" + strconv.Quote(string(e.Persisted)) +
		" != configured=" + strconv.Quote(string(e.Configured)) +
		"; pass WithAllowConfigMismatch to override"
}

// RestoreDiscoveryErrorKind classifies a failure to extract a required fact from the
// replayed durable stream during restore (the persisted config fingerprint, or the
// primary loop's original id). A stream missing either cannot be restored.
type RestoreDiscoveryErrorKind string

const (
	// RestoreNoSessionStarted means the replayed stream carried no SessionStarted, so
	// there is no persisted config fingerprint to check against.
	RestoreNoSessionStarted RestoreDiscoveryErrorKind = "no_session_started"
	// RestoreNoPrimaryLoop means the replayed stream carried no root LoopStarted (one
	// whose Cause.Coordinates is zero), so the primary loop's id cannot be recovered.
	RestoreNoPrimaryLoop RestoreDiscoveryErrorKind = "no_primary_loop"
)

// RestoreDiscoveryError reports that a required fact could not be extracted from the
// replayed stream. It fails the restore closed (RestoreErrored + no come-up) rather
// than guessing a missing id or fingerprint.
type RestoreDiscoveryError struct {
	Kind      RestoreDiscoveryErrorKind
	SessionID uuid.UUID
}

func (e *RestoreDiscoveryError) Error() string {
	switch e.Kind {
	case RestoreNoSessionStarted:
		return "session: restore: no SessionStarted in stream for " + e.SessionID.String()
	case RestoreNoPrimaryLoop:
		return "session: restore: no root LoopStarted in stream for " + e.SessionID.String()
	default:
		return "session: restore: discovery failed for " + e.SessionID.String()
	}
}

// checkFingerprint is the restore config-fingerprint decision: it returns nil when the
// persisted and live fingerprints are Equal, a typed *ConfigMismatchError when they
// differ, and — when allowMismatch is set — nil even on a difference (the operator's
// explicit opt-in to resume under a changed config). It is fail-secure by default: a
// mismatch blocks unless overridden.
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
// persisted (root LoopStarted) name and the configured (primary loop.Config) name are
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
