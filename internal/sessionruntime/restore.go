package sessionruntime

import (
	"sort"

	"github.com/looprig/core/content"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/internal/loopruntime"
	"github.com/looprig/harness/pkg/ceiling"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/hustle"
	"github.com/looprig/harness/pkg/identity"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/inference"
)

type restoredHustleAuditErrorKind string

const (
	restoredHustleAuditDuplicateStart       restoredHustleAuditErrorKind = "duplicate_start"
	restoredHustleAuditTerminalWithoutStart restoredHustleAuditErrorKind = "terminal_without_start"
	restoredHustleAuditAttributionMismatch  restoredHustleAuditErrorKind = "attribution_mismatch"
	restoredHustleAuditInvalidStart         restoredHustleAuditErrorKind = "invalid_start"
	restoredHustleAuditCountOverflow        restoredHustleAuditErrorKind = "count_overflow"
)

type restoredHustleAuditError struct {
	Kind  restoredHustleAuditErrorKind
	RunID hustle.RunID
}

func (e *restoredHustleAuditError) Error() string {
	return "sessionruntime: invalid restored hustle audit: " + string(e.Kind)
}

// restoredHustleInterrupted is a bounded diagnostic classification. The
// unmatched HustleStarted remains the durable audit record; restore neither
// appends a synthetic terminal nor reconstructs runtime work.
type restoredHustleInterrupted struct {
	Name          hustle.Name
	ModelSource   hustle.ModelSource
	NamedModelKey inference.ModelKey
	Runs          uint64
}

type restoredHustleAudit struct {
	Interrupted []restoredHustleInterrupted
}

type restoredHustleStart struct {
	descriptor hustle.DefinitionDescriptor
	sessionID  uuid.UUID
	cause      identity.Cause
}

// foldRestoredHustleAudit validates exact one-start/one-terminal lifecycle
// attribution by RunID. Only unmatched starts survive as canonically sorted
// counts keyed by immutable definition identity; no per-run restore state is
// returned.
func foldRestoredHustleAudit(events []event.Event) (restoredHustleAudit, error) {
	starts := make(map[hustle.RunID]restoredHustleStart)
	seen := make(map[hustle.RunID]struct{})
	for _, ev := range events {
		switch typed := ev.(type) {
		case event.HustleStarted:
			if typed.Visibility() != event.Internal || typed.SessionID.IsZero() || uuid.UUID(typed.Run.RunID).IsZero() || typed.Run.Runtime != (event.ModelRuntime{}) || typed.Run.Definition.Validate() != nil {
				return restoredHustleAudit{}, &restoredHustleAuditError{Kind: restoredHustleAuditInvalidStart, RunID: typed.Run.RunID}
			}
			if _, exists := seen[typed.Run.RunID]; exists {
				return restoredHustleAudit{}, &restoredHustleAuditError{Kind: restoredHustleAuditDuplicateStart, RunID: typed.Run.RunID}
			}
			seen[typed.Run.RunID] = struct{}{}
			starts[typed.Run.RunID] = restoredHustleStart{descriptor: typed.Run.Definition, sessionID: typed.SessionID, cause: typed.Cause}
		case event.HustleCompleted:
			if err := consumeRestoredHustleTerminal(starts, typed.Run, typed.SessionID, typed.Cause); err != nil {
				return restoredHustleAudit{}, err
			}
		case event.HustleFailed:
			if err := consumeRestoredHustleTerminal(starts, typed.Run, typed.SessionID, typed.Cause); err != nil {
				return restoredHustleAudit{}, err
			}
		}
	}
	return projectRestoredHustleInterruptions(starts)
}

func projectRestoredHustleInterruptions(starts map[hustle.RunID]restoredHustleStart) (restoredHustleAudit, error) {
	type interruptedKey struct {
		name          hustle.Name
		modelSource   hustle.ModelSource
		namedModelKey inference.ModelKey
	}
	counts := make(map[interruptedKey]uint64)
	for _, start := range starts {
		key := interruptedKey{name: start.descriptor.Name, modelSource: start.descriptor.ModelSource}
		if start.descriptor.ModelSource == hustle.ModelSourceNamed {
			key.namedModelKey = start.descriptor.NamedModelKey
		}
		var err error
		counts[key], err = incrementRestoredHustleCount(counts[key])
		if err != nil {
			return restoredHustleAudit{}, err
		}
	}
	interrupted := make([]restoredHustleInterrupted, 0, len(counts))
	for key, runs := range counts {
		interrupted = append(interrupted, restoredHustleInterrupted{Name: key.name, ModelSource: key.modelSource, NamedModelKey: key.namedModelKey, Runs: runs})
	}
	sort.Slice(interrupted, func(i, j int) bool {
		left, right := interrupted[i], interrupted[j]
		if left.Name != right.Name {
			return left.Name < right.Name
		}
		if left.ModelSource != right.ModelSource {
			return left.ModelSource < right.ModelSource
		}
		if left.NamedModelKey.Provider != right.NamedModelKey.Provider {
			return left.NamedModelKey.Provider < right.NamedModelKey.Provider
		}
		return left.NamedModelKey.Model < right.NamedModelKey.Model
	})
	return restoredHustleAudit{Interrupted: interrupted}, nil
}

func incrementRestoredHustleCount(value uint64) (uint64, error) {
	if value == ^uint64(0) {
		return 0, &restoredHustleAuditError{Kind: restoredHustleAuditCountOverflow}
	}
	return value + 1, nil
}

func consumeRestoredHustleTerminal(starts map[hustle.RunID]restoredHustleStart, run event.HustleRunDescriptor, sessionID uuid.UUID, cause identity.Cause) error {
	start, exists := starts[run.RunID]
	if !exists {
		return &restoredHustleAuditError{Kind: restoredHustleAuditTerminalWithoutStart, RunID: run.RunID}
	}
	if start.descriptor != run.Definition || start.sessionID != sessionID || start.cause != cause {
		return &restoredHustleAuditError{Kind: restoredHustleAuditAttributionMismatch, RunID: run.RunID}
	}
	if run.Definition.ModelSource == hustle.ModelSourceNamed && run.Runtime != (event.ModelRuntime{}) && run.Runtime.Key != run.Definition.NamedModelKey {
		return &restoredHustleAuditError{Kind: restoredHustleAuditAttributionMismatch, RunID: run.RunID}
	}
	delete(starts, run.RunID)
	return nil
}

func checkFingerprint(persisted, live event.ConfigFingerprint, allowMismatch bool) error {
	if persisted.Equal(live) {
		return nil
	}
	if allowMismatch {
		return nil
	}
	return &ConfigMismatchError{Persisted: persisted, Live: live}
}

// restoredContextDisposition validates restore compatibility and reports whether
// an explicitly overridden, actual config mismatch makes a durable context
// measurement stale. Merely enabling the override does not discard matching state.
func restoredContextDisposition(persisted, live event.ConfigFingerprint, allowMismatch bool) (bool, error) {
	if err := checkFingerprint(persisted, live, allowMismatch); err != nil {
		return false, err
	}
	return !persisted.Equal(live), nil
}

// checkAgentName is the restore root-loop AgentName decision: it returns nil when the
// persisted (root LoopStarted) name and the configured (root loop.Definition) name are
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
// two facts off it — the root loop's stable id (the recovered loop comes up under THIS
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
	return event.LoopStarted{}, &RestoreDiscoveryError{Kind: RestoreNoPrimerLoop}
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
// The root LoopStarted (Cause zero) is excluded — the initial loop never counts
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
// foldLoop stays pure.
func lastSecurityCeiling(events []event.Event) (ceiling.Level, bool) {
	level, ok := ceiling.Level(0), false
	for _, ev := range events {
		if sc, isSC := ev.(event.SecurityCeilingChanged); isSC {
			level, ok = sc.Level, true // keep scanning; the LAST one wins
		}
	}
	return level, ok
}

// restoredInference is the fold of one loop's selected mode and latest resolved
// runtime. Every lifecycle event replaces Runtime, matching the actor's live
// precedence, so restore does not consult a mutable model catalog for identity,
// limits, or effort.
type restoredInference struct {
	Mode       loop.ModeName
	HasMode    bool
	Runtime    event.ModelRuntime
	HasRuntime bool
}

// foldLoopInference folds a loop's ordered lifecycle events into its durable mode
// and resolved runtime, last-write-wins. Legacy LoopStarted records without the
// additive runtime retain the live bound definition fallback.
func foldLoopInference(events []event.Event) restoredInference {
	var ri restoredInference
	for _, ev := range events {
		switch e := ev.(type) {
		case event.LoopStarted:
			// Seed the mode BASELINE from the durable start mode: a mode-selective spawn
			// records the selected mode on LoopStarted and emits NO LoopModeChanged, so
			// without this a child spawned in a non-default mode would resume at the
			// definition's initial mode (wrong model/effort/tools/instructions). An empty
			// InitialMode means the definition default; a later LoopModeChanged overrides
			// this baseline (a mode change also resets any inference override, handled below).
			if e.InitialMode != "" {
				ri.Mode = loop.ModeName(e.InitialMode)
				ri.HasMode = true
			}
			if e.Runtime != (event.ModelRuntime{}) {
				ri.Runtime = e.Runtime
				ri.HasRuntime = true
			}
		case event.LoopModeChanged:
			ri.Mode = loop.ModeName(e.Mode)
			ri.HasMode = true
			// A pre-runtime mode change still authoritatively selects its named
			// mode, but its model must be re-resolved from the bound definition.
			// Clearing a prior direct inference override is therefore intentional.
			ri.Runtime = e.Runtime
			ri.HasRuntime = e.Runtime != (event.ModelRuntime{})
		case event.LoopInferenceChanged:
			ri.Runtime = e.Runtime
			ri.HasRuntime = e.Runtime != (event.ModelRuntime{})
		}
	}
	return ri
}

// restoredStateFrom builds the loopruntime restore seed from a loop's committed-conversation
// fold and its mode/inference fold, so a re-created loop comes up with both its history AND
// the effective config it crashed under.
func restoredStateFrom(folded foldResult, ri restoredInference) loopruntime.RestoredState {
	return loopruntime.RestoredState{
		Msgs:       folded.Msgs,
		TurnIndex:  folded.TurnIndex,
		Mode:       ri.Mode,
		HasMode:    ri.HasMode,
		Runtime:    ri.Runtime,
		HasRuntime: ri.HasRuntime,
		Context:    folded.Context,
		HasContext: folded.HasContext,
		Basis:      folded.Basis,
		HasBasis:   folded.HasBasis,

		AutomaticBasis:    folded.AutomaticBasis,
		HasAutomaticBasis: folded.HasAutomaticBasis,
	}
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
	Msgs       content.AgenticMessages
	TurnIndex  event.TurnIndex
	OpenTurn   bool
	Runtime    event.ModelRuntime
	HasRuntime bool
	Context    event.ContextMeasurement
	HasContext bool
	Basis      event.ContextBasis
	HasBasis   bool

	AutomaticBasis    event.ContextBasis
	HasAutomaticBasis bool
	Err               error
}

type restoredContextRevisionOverflowError struct{}

func (*restoredContextRevisionOverflowError) Error() string {
	return "sessionruntime: restored context revision overflow"
}

type restoredCompactionErrorKind string

const (
	restoredCompactionDuplicateTerminal restoredCompactionErrorKind = "duplicate_terminal"
	restoredCompactionWaiterMismatch    restoredCompactionErrorKind = "waiter_mismatch"
)

// restoredCompactionError reports contradictory durable compaction membership.
// Restore wraps it as RestoreReplayFailed before any repaired outcome is appended.
type restoredCompactionError struct {
	Kind      restoredCompactionErrorKind
	AttemptID event.CompactAttemptID
	CommandID uuid.UUID
}

func (e *restoredCompactionError) Error() string {
	return "sessionruntime: invalid restored compaction: " + string(e.Kind)
}

type restoredCompactionTerminal struct {
	event    event.Event
	attempt  event.CompactAttemptID
	waiters  []uuid.UUID
	resolved bool
}

type restoredCompactionWaiterKey struct {
	attempt event.CompactAttemptID
	command uuid.UUID
}

// planCompactWaiterRepairs validates canonical terminal uniqueness and returns
// only terminal members whose exact durable reply is absent. It is a pure fold:
// the authoritative replay remains untouched and in its original order.
func planCompactWaiterRepairs(events []event.Event) ([]event.Event, error) {
	terminals := make(map[event.CompactAttemptID]struct{})
	ordered := make([]restoredCompactionTerminal, 0)
	outcomes := make(map[restoredCompactionWaiterKey][]event.Event)
	for _, ev := range events {
		switch typed := ev.(type) {
		case event.CompactionCommitted:
			if _, exists := terminals[typed.AttemptID]; exists {
				return nil, &restoredCompactionError{Kind: restoredCompactionDuplicateTerminal, AttemptID: typed.AttemptID}
			}
			terminals[typed.AttemptID] = struct{}{}
			ordered = append(ordered, restoredCompactionTerminal{
				event: typed, attempt: typed.AttemptID, waiters: typed.WaiterCommandIDs, resolved: true,
			})
		case event.CompactionRejected:
			if _, exists := terminals[typed.AttemptID]; exists {
				return nil, &restoredCompactionError{Kind: restoredCompactionDuplicateTerminal, AttemptID: typed.AttemptID}
			}
			terminals[typed.AttemptID] = struct{}{}
			ordered = append(ordered, restoredCompactionTerminal{
				event: typed, attempt: typed.AttemptID, waiters: typed.WaiterCommandIDs,
			})
		case event.CompactWaiterResolved:
			key := restoredCompactionWaiterKey{attempt: typed.AttemptID, command: typed.Cause.CommandID}
			outcomes[key] = append(outcomes[key], typed)
		case event.CompactWaiterRejected:
			key := restoredCompactionWaiterKey{attempt: typed.AttemptID, command: typed.Cause.CommandID}
			outcomes[key] = append(outcomes[key], typed)
		}
	}

	var repairs []event.Event
	for _, terminal := range ordered {
		for _, commandID := range terminal.waiters {
			key := restoredCompactionWaiterKey{attempt: terminal.attempt, command: commandID}
			existing := outcomes[key]
			if len(existing) == 0 {
				repairs = append(repairs, restoredCompactionRepair(terminal, commandID))
				continue
			}
			for _, outcome := range existing {
				if !restoredCompactionOutcomeMatches(terminal, commandID, outcome) {
					return nil, &restoredCompactionError{
						Kind: restoredCompactionWaiterMismatch, AttemptID: terminal.attempt, CommandID: commandID,
					}
				}
			}
		}
	}
	return repairs, nil
}

func restoredCompactionRepair(terminal restoredCompactionTerminal, commandID uuid.UUID) event.Event {
	header := event.Header{
		Coordinates: terminal.event.EventHeader().Coordinates,
		Cause:       identity.Cause{CommandID: commandID},
		EventID:     event.CompactWaiterReplyID(terminal.attempt, commandID, terminal.resolved),
	}
	if terminal.resolved {
		committed := terminal.event.(event.CompactionCommitted)
		return event.CompactWaiterResolved{
			Header: header, AttemptID: terminal.attempt, CommittedEventID: committed.EventID,
		}
	}
	rejected := terminal.event.(event.CompactionRejected)
	return event.CompactWaiterRejected{
		Header: header, AttemptID: terminal.attempt, Reason: rejected.RejectReason,
	}
}

func restoredCompactionOutcomeMatches(terminal restoredCompactionTerminal, commandID uuid.UUID, outcome event.Event) bool {
	header := outcome.EventHeader()
	if header.Coordinates != terminal.event.EventHeader().Coordinates || header.Cause.CommandID != commandID {
		return false
	}
	if terminal.resolved {
		resolved, ok := outcome.(event.CompactWaiterResolved)
		committed := terminal.event.(event.CompactionCommitted)
		return ok && resolved.AttemptID == terminal.attempt && resolved.CommittedEventID == committed.EventID &&
			resolved.EventID == event.CompactWaiterReplyID(terminal.attempt, commandID, true)
	}
	rejectedOutcome, ok := outcome.(event.CompactWaiterRejected)
	rejectedTerminal := terminal.event.(event.CompactionRejected)
	return ok && rejectedOutcome.AttemptID == terminal.attempt && rejectedOutcome.Reason == rejectedTerminal.RejectReason &&
		rejectedOutcome.EventID == event.CompactWaiterReplyID(terminal.attempt, commandID, false)
}

func advanceFoldedContextBasis(current event.ContextBasis, hasCurrent bool, eventID uuid.UUID) (event.ContextBasis, bool, error) {
	if eventID.IsZero() {
		return current, hasCurrent, nil
	}
	if hasCurrent && current.Revision == ^event.ContextRevision(0) {
		return event.ContextBasis{}, false, &restoredContextRevisionOverflowError{}
	}
	revision := event.ContextRevision(1)
	if hasCurrent {
		revision = current.Revision + 1
	}
	return event.ContextBasis{Revision: revision, ThroughEventID: eventID}, true, nil
}

// RestoredContextModelMismatchError reports a replay projection that would
// seed a current measurement under a different restored runtime model.
type RestoredContextModelMismatchError struct {
	Runtime     inference.ModelKey
	Measurement inference.ModelKey
}

func (e *RestoredContextModelMismatchError) Error() string {
	return "sessionruntime: restored context measurement model does not match runtime"
}

// foldLoop reconstructs a loop's committed msgs + turnIndex from an ordered
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
// It is a PURE function: no I/O. The events are already-typed, journaled
// payloads (each TurnStarted/StepDone/TurnFoldedInto carries its committed
// message[s] verbatim), so there is no malformed-group failure mode to surface — a
// nil Message or empty Messages folds to the same nil/empty the loop itself
// committed. Cross-event runtime/context consistency is reported through Err so
// the restore constructor can reject the replay before seeding a loop.
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
func foldLoop(events []event.Event) foldResult {
	// Start non-nil so an empty sequence reconstructs as an empty (not nil) thread,
	// matching content.AgenticMessages' documented empty zero value and the loop's
	// own freshly-seeded msgs.
	msgs := content.AgenticMessages{}
	var turnIndex event.TurnIndex
	openTurn := false
	var runtime event.ModelRuntime
	hasRuntime := false
	var contextMeasurement event.ContextMeasurement
	hasContext := false
	var basis event.ContextBasis
	hasBasis := false
	var automaticBasis event.ContextBasis
	hasAutomaticBasis := false
	var foldErr error
	advanceBasis := func(header event.Header) {
		if foldErr != nil {
			return
		}
		basis, hasBasis, foldErr = advanceFoldedContextBasis(basis, hasBasis, header.EventID)
	}

	for _, ev := range events {
		switch e := ev.(type) {
		case event.TurnStarted:
			// The loop increments turnIndex then commits the initial UserMessage. A turn
			// is now open until a terminal closes it.
			turnIndex++
			msgs = append(msgs, e.Message)
			openTurn = true
			contextMeasurement = event.ContextMeasurement{}
			hasContext = false
			advanceBasis(e.Header)
		case event.StepDone:
			// The loop appends the finalized step group (AIMessage + ToolResultMessages).
			msgs = append(msgs, e.Messages...)
			contextMeasurement = event.ContextMeasurement{}
			hasContext = false
			advanceBasis(e.Header)
		case event.TurnFoldedInto:
			// The loop commits the folded user message at the tool-continuation point.
			msgs = append(msgs, e.Message)
			contextMeasurement = event.ContextMeasurement{}
			hasContext = false
			advanceBasis(e.Header)
		case event.LoopStarted:
			runtime = e.Runtime
			hasRuntime = e.Runtime != (event.ModelRuntime{})
			contextMeasurement = event.ContextMeasurement{}
			hasContext = false
		case event.LoopInferenceChanged:
			runtime = e.Runtime
			hasRuntime = e.Runtime != (event.ModelRuntime{})
			contextMeasurement = event.ContextMeasurement{}
			hasContext = false
			advanceBasis(e.Header)
		case event.LoopModeChanged:
			runtime = e.Runtime
			hasRuntime = e.Runtime != (event.ModelRuntime{})
			contextMeasurement = event.ContextMeasurement{}
			hasContext = false
			advanceBasis(e.Header)
		case event.ContextMeasured:
			contextMeasurement = e.Measurement
			hasContext = true
			basis = e.Measurement.Basis
			hasBasis = true
		case event.CompactionRejected:
			if !hasBasis {
				basis = e.Basis
				hasBasis = true
			}
			if e.Reason == event.CompactionReasonAutomatic {
				automaticBasis = e.Basis
				hasAutomaticBasis = true
			}
		case event.CompactionCommitted:
			msgs = content.AgenticMessages{e.Summary}
			contextMeasurement = e.PostContext
			hasContext = true
			basis = e.PostContext.Basis
			hasBasis = true
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

	if foldErr == nil && hasContext && hasRuntime && contextMeasurement.Model != runtime.Key {
		foldErr = &RestoredContextModelMismatchError{Runtime: runtime.Key, Measurement: contextMeasurement.Model}
		contextMeasurement = event.ContextMeasurement{}
		hasContext = false
	}
	return foldResult{
		Msgs: msgs, TurnIndex: turnIndex, OpenTurn: openTurn,
		Runtime: runtime, HasRuntime: hasRuntime,
		Context: contextMeasurement, HasContext: hasContext, Err: foldErr,
		Basis: basis, HasBasis: hasBasis,
		AutomaticBasis: automaticBasis, HasAutomaticBasis: hasAutomaticBasis,
	}
}

func foldLoopForRestore(bound loop.BoundDefinition, events []event.Event, discardContext bool) (foldResult, error) {
	folded := foldLoop(events)
	if folded.Err != nil {
		return foldResult{}, &RestoreError{Kind: RestoreReplayFailed, Cause: folded.Err}
	}
	if discardContext {
		folded.Context = event.ContextMeasurement{}
		folded.HasContext = false
		return folded, nil
	}
	if folded.HasContext {
		_, effectiveModel := liveViewFor(bound, foldLoopInference(events))
		if folded.Context.Model != effectiveModel.Key() {
			folded.Err = &RestoredContextModelMismatchError{
				Runtime: effectiveModel.Key(), Measurement: folded.Context.Model,
			}
		}
	}
	if folded.Err != nil {
		return foldResult{}, &RestoreError{Kind: RestoreReplayFailed, Cause: folded.Err}
	}
	return folded, nil
}
