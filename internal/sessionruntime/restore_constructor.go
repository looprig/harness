package sessionruntime

import (
	"context"
	"errors"
	"io"
	"time"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/internal/loopruntime"
	"github.com/looprig/harness/pkg/ceiling"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/foreignloop"
	"github.com/looprig/harness/pkg/gate"
	"github.com/looprig/harness/pkg/hub"
	"github.com/looprig/harness/pkg/identity"
	"github.com/looprig/harness/pkg/journal"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/sessionstore"
	"github.com/looprig/harness/pkg/tool"
	"github.com/looprig/harness/pkg/workspacestore"
)

// Restore reconstructs a session's PRIMARY loop from the durable journal and brings it
// up IDLE, ready to continue — the payoff of the persistence layer. It is the parallel
// of New: it reuses the SAME hub/loop/factory wiring but SEEDS the primary loop from the
// folded journal instead of spawning it empty, and it does NOT publish a fresh
// SessionStarted (the session's start was recorded on the original run). The recovered
// session keeps its identity: the same sessionID and the same primary loop id.
//
// Order (per the design — RestoreStarted is the FIRST restore mutation, after the lease
// fence the journal writes at construction):
//
//  1. Acquire the single-writer lease, construct the SessionJournal (writes the opening
//     LeaseFence — the handover boundary) and the EventReplayer.
//  2. Replay the stream; read the persisted config fingerprint and compare to the live
//     config. On mismatch → *ConfigMismatchError UNLESS WithAllowConfigMismatch is set.
//  3. Append RestoreStarted (the first restore mutation).
//  4. Find the primary loop's original id (the root LoopStarted).
//  5. Fold the primary loop's Enduring events into committed msgs + turnIndex.
//  6. Crash-seam: if the fold ends on an open turn, append a TurnInterrupted to close it.
//  7. Append RestoreDone on success.
//  8. Build the Session: journal-backed hub appender + command appender, the session as
//     FaultReporter, and the primary loop SEEDED (NewRestored) under its original id, idle.
//
// On ANY failure in steps 2–7 (a config mismatch, a discovery/replay/decode/object
// failure, or an append failure) Restore durably records a RestoreErrored and returns a
// typed error; the session does NOT come up (fail-secure / no silent drift). On a FAILED
// restore the acquired lease is released here before returning (so a successor can
// re-acquire). On SUCCESS the journal HOLDS the lease for the live session's lifetime;
// releasing it at session teardown is the Phase-10 composition root's wiring (mirroring
// how the lease is composition-root-owned in the durable-tap path), out of scope here.
func Restore(
	ctx context.Context,
	cfg loop.Definition,
	sessionID uuid.UUID,
	store *sessionstore.Store,
	opts ...Option,
) (*Session, error) {
	return restoreSession(ctx, cfg, sessionID, store, uuid.New, time.Now, opts...)
}

// RestoreTopology reconstructs all durable primer roots and their independent
// histories. The active primer is restored through the legacy single-root core;
// remaining roots are attached before the controller is returned.
func RestoreTopology(ctx context.Context, topology Topology, sessionID uuid.UUID, store *sessionstore.Store, opts ...Option) (*Session, error) {
	active, ok := topology.definition(topology.ActivePrimer)
	if !ok {
		return nil, &RestoreError{Kind: RestoreLoopFailed, Cause: &MissingTopologyError{}}
	}
	events, err := replayTopologyEvents(ctx, store, sessionID)
	if err != nil {
		return nil, &RestoreError{Kind: RestoreReplayFailed, Cause: err}
	}
	hasStarted := false
	for _, ev := range events {
		if _, ok := ev.(event.SessionStarted); ok {
			hasStarted = true
			break
		}
	}
	if !hasStarted {
		return Restore(ctx, active, sessionID, store, opts...)
	}
	roots := make(map[identity.AgentName]event.LoopStarted)
	allStarts := make([]event.LoopStarted, 0)
	for _, ev := range events {
		if started, isStarted := ev.(event.LoopStarted); isStarted {
			allStarts = append(allStarts, started)
			if started.Cause.Coordinates == (identity.Coordinates{}) {
				if _, exists := roots[started.AgentName]; !exists {
					roots[started.AgentName] = started
				}
			}
		}
	}
	for _, primer := range topology.Primers {
		if _, exists := roots[primer]; !exists {
			return nil, &RestoreError{Kind: RestoreLoopFailed, Cause: &AgentNameMismatchError{Persisted: "", Configured: primer}}
		}
	}
	for _, started := range allStarts {
		if _, exists := topology.definition(started.AgentName); !exists {
			return nil, &RestoreError{Kind: RestoreLoopFailed, Cause: &AgentNameMismatchError{Persisted: started.AgentName}}
		}
	}
	s, err := Restore(ctx, active, sessionID, store, opts...)
	if err != nil {
		return nil, err
	}
	for _, started := range allStarts {
		if _, exists := s.Loop(started.LoopID); exists {
			continue
		}
		definition, _ := topology.definition(started.AgentName)
		bound, bindErr := definition.Bind(s.sessionCtx, tool.Bindings{SessionID: sessionID, LoopID: started.LoopID})
		if bindErr != nil {
			_ = s.Shutdown(context.Background())
			return nil, &RestoreError{Kind: RestoreLoopFailed, Cause: bindErr}
		}
		loopEvents := filterLoopEvents(events, started.LoopID)
		folded := foldPrimaryLoop(loopEvents)
		if folded.OpenTurn {
			turnID, turnIndex := openTurnCoords(loopEvents)
			header, stampErr := s.factory.Stamp(event.Header{Coordinates: identity.Coordinates{SessionID: sessionID, LoopID: started.LoopID, TurnID: turnID}})
			if stampErr != nil {
				_ = s.Shutdown(context.Background())
				return nil, &RestoreError{Kind: RestoreIDGenerationFailed, Cause: stampErr}
			}
			if publishErr := s.PublishEvent(ctx, event.TurnInterrupted{Header: header, TurnIndex: turnIndex}); publishErr != nil {
				_ = s.Shutdown(context.Background())
				return nil, &RestoreError{Kind: RestoreAppendFailed, Cause: publishErr}
			}
		}
		parent := loop.Provenance{LoopID: started.Cause.Coordinates.LoopID, TurnID: started.Cause.Coordinates.TurnID, StepID: started.Cause.Coordinates.StepID}
		if buildErr := s.attachRestoredLoop(started, parent, bound, folded, findForeignSID(loopEvents)); buildErr != nil {
			_ = s.Shutdown(context.Background())
			return nil, buildErr
		}
	}
	activeID := roots[topology.ActivePrimer].LoopID
	for _, ev := range events {
		if changed, isChanged := ev.(event.ActiveLoopChanged); isChanged {
			activeID = changed.ActiveLoopID
		}
	}
	if _, exists := s.Loop(activeID); !exists {
		_ = s.Shutdown(context.Background())
		return nil, &RestoreError{Kind: RestoreLoopFailed, Cause: &SessionError{Kind: SessionLoopNotFound}}
	}
	s.loopsMu.Lock()
	s.primaryLoopID = activeID
	s.loopsMu.Unlock()
	return s, nil
}

func replayTopologyEvents(ctx context.Context, store *sessionstore.Store, sessionID uuid.UUID) ([]event.Event, error) {
	replayer, err := store.OpenEventReplayer(sessionID, sessionstore.ReplayRequest{FromSeq: 0})
	if err != nil {
		return nil, err
	}
	cursor, err := replayer.Open(ctx, journal.ReplayRequest{SessionID: sessionID, From: journal.Beginning()})
	if err != nil {
		return nil, err
	}
	defer cursor.Close()
	var events []event.Event
	for {
		ev, _, nextErr := cursor.Next(ctx)
		if errors.Is(nextErr, io.EOF) {
			return events, nil
		}
		if nextErr != nil {
			return nil, nextErr
		}
		events = append(events, ev)
	}
}

func filterLoopEvents(events []event.Event, loopID uuid.UUID) []event.Event {
	filtered := make([]event.Event, 0)
	for _, ev := range events {
		if ev.EventHeader().LoopID == loopID || ev.EventHeader().LoopID.IsZero() {
			filtered = append(filtered, ev)
		}
	}
	return filtered
}

func (s *Session) attachRestoredLoop(started event.LoopStarted, parent loop.Provenance, bound loop.BoundDefinition, folded foldResult, foreignSID string) error {
	loopCtx, cancel := context.WithCancel(s.sessionCtx)
	var backend loop.Backend
	var err error
	switch bound.Engine() {
	case loop.EngineNative:
		backend, err = loopruntime.NewRestored(loopCtx, s.sessionID, started.LoopID, parent, s, bound, loopruntime.RestoredState{Msgs: folded.Msgs, TurnIndex: folded.TurnIndex})
	default:
		if foreignSID == "" {
			cancel()
			return &RestoreError{Kind: RestoreForeignSIDMissing}
		}
		if s.foreignBuildRestored == nil {
			cancel()
			return &RestoreError{Kind: RestoreForeignBuilderMissing}
		}
		backend, err = s.foreignBuildRestored(loopCtx, s.sessionID, started.LoopID, parent, s, bound, func() (uuid.UUID, error) { return s.newID() }, s.factory, foreignloop.RestoredForeign{ForeignSID: foreignSID, TurnIndex: folded.TurnIndex, Msgs: folded.Msgs})
	}
	if err != nil {
		cancel()
		return &RestoreError{Kind: RestoreLoopFailed, Cause: err}
	}
	s.loopsMu.Lock()
	s.loops[started.LoopID] = &loopHandle{id: started.LoopID, owner: s, bound: bound, backend: backend, parent: parent, cancel: cancel}
	s.loopsMu.Unlock()
	return nil
}

// restoreSession is the construction core of Restore with the id-gen and clock seams
// made explicit (mirroring newSession), so a same-package test can pin the stamp or
// drive a mint failure. Restore calls it with the production defaults.
func restoreSession(
	ctx context.Context,
	cfg loop.Definition,
	sessionID uuid.UUID,
	store *sessionstore.Store,
	newID idGenerator,
	now event.Clock,
	opts ...Option,
) (*Session, error) {
	select {
	case <-ctx.Done():
		return nil, &RestoreError{Kind: RestoreContextDone, Cause: ctx.Err()}
	default:
	}
	// Allocate the one restored-session lifetime up front. Until successful transfer
	// into Session, every exit below owns cancellation (lease/replay/bind/check/append/
	// build failures included). Bind and the live Session receive this exact context.
	sessionCtx, sessionCancel := context.WithCancel(ctx)
	contextTransferred := false
	defer func() {
		if !contextTransferred {
			sessionCancel()
		}
	}()

	// Resolve restore-time options (the allow-mismatch flag) on a probe session so the
	// fingerprint decision can read it BEFORE the real session is built. The same opts
	// are applied to the real session in step 8.
	probe := &Session{}
	for _, opt := range opts {
		opt(probe)
	}
	allowMismatch := probe.allowConfigMismatch
	if probe.fingerprint == nil {
		return nil, &RestoreError{Kind: RestoreLoopFailed, Cause: &MissingFingerprintProviderError{}}
	}

	// (1) Acquire the single-writer lease, then construct the journal (which writes the
	// opening LeaseFence as its first append — the handover boundary) and the replayer,
	// all through the sessionstore facade over the composition-root-wired backend.
	lease, err := store.AcquireLease(ctx, sessionID)
	if err != nil {
		return nil, &RestoreError{Kind: RestoreLeaseFailed, Cause: err}
	}
	j, err := store.OpenJournal(ctx, sessionID, lease)
	if err != nil {
		releaseLease(lease)
		return nil, &RestoreError{Kind: RestoreJournalFailed, Cause: err}
	}
	// The replayer is bound to the stream BEGINNING (FromSeq 0). Restore intentionally
	// drains RECORDS, not events, so the private GatePreparedRecord is visible while the
	// normal event-based folds are derived from EventRecord payloads. Opening it is a
	// step-1 setup step (parallel to the journal): a failure releases the lease and
	// returns without a RestoreErrored, exactly like the journal-setup failure above (the
	// first restore MUTATION, RestoreStarted, has not been written yet).
	replayer, err := store.OpenRecordReplayer(sessionID, sessionstore.ReplayRequest{FromSeq: 0})
	if err != nil {
		releaseLease(lease)
		return nil, &RestoreError{Kind: RestoreReplayFailed, Cause: err}
	}

	// The restore-lifecycle events (RestoreStarted/RestoreDone/RestoreErrored and the
	// crash-seam TurnInterrupted) are stamped from the SAME id-gen + clock seam the
	// session's own events use, then appended directly through the journal (the hub does
	// not exist yet). A failed mint fails the restore closed.
	factory := event.NewFactory(func() (uuid.UUID, error) { return newID() }, func() time.Time { return now() })

	// recordErrored durably appends a RestoreErrored carrying err, then releases the
	// lease and returns (nil session, the typed restore error). It is the single
	// fail-secure exit for every failure after the journal exists: the session never
	// comes up, and the failure is recorded in the durable log (a best-effort append — a
	// failure to record the failure never masks the original cause).
	recordErrored := func(restoreErr error) (*Session, error) {
		_ = appendRestoreEvent(ctx, j, factory, event.RestoreErrored{
			Header: event.Header{Coordinates: identity.Coordinates{SessionID: sessionID}},
			Err:    restoreErr,
		})
		releaseLease(lease)
		return nil, restoreErr
	}

	// (2) Replay the whole stream once for discovery (the persisted fingerprint + the
	// primary loop id + the subagent spawn count) and gate recovery. A ZERO LoopID leaves
	// the event projection UNNARROWED — every loop's events — so findRootLoopStarted and
	// countSpawnedLoops see the subagent LoopStarted events, not just the primary's. Fail
	// closed on any error.
	allRecords, err := drainRecordReplay(ctx, replayer, journal.ReplayRequest{Follow: false})
	if err != nil {
		return recordErrored(&RestoreError{Kind: RestoreReplayFailed, Cause: err})
	}
	all := eventsFromRecords(allRecords, uuid.UUID{})

	persisted, err := firstConfigFingerprint(all)
	if err != nil {
		return recordErrored(err)
	}
	// The root LoopStarted carries both the primary loop's stable id (restore reuses it)
	// and its immutable stamped AgentName. Validate the stamped name against the live
	// primary config's AgentName — an empty (legacy/pre-AgentName) stored name vs a
	// configured one is a mismatch, not silently accepted — routing through the same
	// WithAllowConfigMismatch opt-in the fingerprint check honors.
	rootLoop, err := findRootLoopStarted(all)
	if err != nil {
		return recordErrored(err)
	}
	primaryLoopID := rootLoop.LoopID
	bound, err := cfg.Bind(sessionCtx, tool.Bindings{SessionID: sessionID, LoopID: primaryLoopID})
	if err != nil {
		return recordErrored(&RestoreError{Kind: RestoreLoopFailed, Cause: err})
	}
	if err := checkFingerprint(persisted, probe.projectFingerprint(bound), allowMismatch); err != nil {
		return recordErrored(err)
	}
	if err := checkAgentName(rootLoop.Header.AgentName, bound.Name(), allowMismatch); err != nil {
		return recordErrored(err)
	}

	// Re-seed the cumulative spawn counter from the durable log so the quota SURVIVES the
	// restart: count the non-root LoopStarted events (subagent spawns). `all` is the
	// full-stream replay (every loop's events, not loop-scoped), so it already carries
	// every subagent LoopStarted — no extra read. Without this, `spawned` would reset to 0
	// and a restart would grant a fresh quota (a trivial cap bypass, design §16.3).
	spawnedCount := countSpawnedLoops(all)

	// The effective durable workspace pointer to materialize on resume (if any). Scanned over
	// the SAME unnarrowed discovery drain; both checkpoint and restore transitions are
	// session-scoped. Consumed at the pre-RestoreDone seam below.
	wsRef, hasWorkspacePointer := effectiveCurrentWorkspace(all)

	// The last durable security-ceiling ordinal to re-seed on resume (if the session ever
	// changed it) — folded from the SAME unnarrowed discovery drain (SecurityCeilingChanged
	// is session-scoped), last write wins. Absent means the session resumes at the fail-
	// secure most-restrictive default. Seeded into the restored session below.
	ceilingLevel, hasCeiling := lastSecurityCeiling(all)

	// Gate recovery folds the same record replay so private GatePreparedRecord payloads are
	// visible. Unsupported or payload-less open gates are durably closed below, after
	// RestoreStarted, before RestoreDone makes the restored session reachable.
	restoredGates := foldRestoredGates(allRecords)

	// (3) RestoreStarted — the FIRST restore mutation (after the lease fence).
	if err := appendRestoreEvent(ctx, j, factory, event.RestoreStarted{
		Header: event.Header{Coordinates: identity.Coordinates{SessionID: sessionID}},
	}); err != nil {
		return recordErrored(&RestoreError{Kind: RestoreAppendFailed, Cause: err})
	}

	if err := appendRestoreUnavailableGates(ctx, j, factory, restoredGates.unavailable); err != nil {
		return recordErrored(&RestoreError{Kind: RestoreAppendFailed, Cause: err})
	}

	// (5) Fold the primary loop's Enduring events (a SCOPED replay) into committed msgs +
	// turnIndex. The primaryLoopID NARROWS the drain to the session-scoped events plus the
	// primary loop's events, dropping every OTHER loop's events — the SAME narrowing the
	// NATS EventReplayer's loop-subject filter gave, and load-bearing: an unnarrowed drain
	// would fold a subagent loop's turns into the primary thread and corrupt it. Re-seeding
	// the system prompt is implicit: it rides cfg.System (the loop never stores it in
	// msgs), so the seeded loop carries it via cfg.
	primaryEvents := eventsFromRecords(allRecords, primaryLoopID)
	folded := foldPrimaryLoop(primaryEvents)

	// (6) Crash-seam: an open turn (a TurnStarted with no terminal) is closed durably
	// with a TurnInterrupted carrying the open turn's id + index, so the resumed loop
	// never observes a half-open turn.
	if folded.OpenTurn {
		turnID, turnIdx := openTurnCoords(primaryEvents)
		if err := appendRestoreEvent(ctx, j, factory, event.TurnInterrupted{
			Header: event.Header{Coordinates: identity.Coordinates{
				SessionID: sessionID, LoopID: primaryLoopID, TurnID: turnID,
			}},
			TurnIndex: turnIdx,
		}); err != nil {
			return recordErrored(&RestoreError{Kind: RestoreAppendFailed, Cause: err})
		}
	}

	// (6b) Materialize the workspace ref selected by the latest durable transition BEFORE
	// declaring the restore done, so
	// RestoreDone is appended only if the workspace is also restored (fail closed — the
	// session never comes up on a workspace that does not match the durable pointer). It
	// uses probe.ws/probe.wsRoot because the real *Session is not built until AFTER
	// RestoreDone. Skipped when no workspace store is wired (a conversation-only restore —
	// the composition root opted out) or the journal carries no checkpoint or restore
	// transition: the root is left untouched in both cases. The journal-sourced
	// ref is validated through ParseRef (a trust boundary — a corrupt log fails closed) and
	// any Materialize failure routes through the SAME recordErrored exit as every other
	// restore failure.
	if probe.ws != nil && hasWorkspacePointer {
		ref, err := workspacestore.ParseRef(wsRef)
		if err != nil {
			return recordErrored(&RestoreError{Kind: RestoreMaterializeFailed, Cause: err})
		}
		if err := probe.ws.Materialize(ctx, ref, probe.wsRoot); err != nil {
			return recordErrored(&RestoreError{Kind: RestoreMaterializeFailed, Cause: err})
		}
	}

	// (7) RestoreDone — the restore succeeded; the session is about to come up.
	if err := appendRestoreEvent(ctx, j, factory, event.RestoreDone{
		Header: event.Header{Coordinates: identity.Coordinates{SessionID: sessionID}},
	}); err != nil {
		return recordErrored(&RestoreError{Kind: RestoreAppendFailed, Cause: err})
	}

	// (8) Build the Session, reusing sessionID + the primary loop id (identity stable).
	// It mirrors newSession's wiring EXCEPT: no SessionStarted is published (the start
	// was recorded on the original run), and the primary loop is SEEDED via NewRestored
	// rather than spawned empty. The lease Restore acquired is handed to the session as its
	// release-on-Shutdown hook (the Phase-10 composition wiring): the journal holds the
	// lease for the live lifetime, and a clean Shutdown releases it so a successor can
	// re-acquire without waiting out the TTL. We append WithLeaseRelease AFTER the caller's
	// opts so the restore owns the lease lifecycle (a caller cannot accidentally override
	// the releaser with a stale one).
	leaseOpts := append(append([]Option(nil), opts...), WithLeaseRelease(lease.Release))
	// Recover the foreign session id from the primary loop's events. Prebound adapters
	// stamped it on LoopStarted; late-bound adapters record it with ForeignSessionBound.
	// buildRestoredSession fails closed on an empty sid for a foreign engine.
	foreignSID := findForeignSID(primaryEvents)
	s, err := buildRestoredSession(sessionCtx, sessionCancel, bound, sessionID, primaryLoopID, foreignSID, spawnedCount, ceilingLevel, hasCeiling, folded, restoredGates.open, j, factory, newID, now, leaseOpts...)
	if err != nil {
		return recordErrored(err)
	}
	contextTransferred = true
	return s, nil
}

// buildRestoredSession assembles the live Session for a successful restore: the hub
// wired with the journal-backed event appender, the shared Factory, and the session as
// the hub's FaultReporter; the journal-backed command appender; and the primary loop
// seeded (NewRestored) with the folded committed state under its ORIGINAL id, idle. It
// publishes NO SessionStarted and spawns NO empty loop — both are the deliberate
// difference from New.
func buildRestoredSession(
	sessionCtx context.Context,
	sessionCancel context.CancelFunc,
	cfg loop.BoundDefinition,
	sessionID, primaryLoopID uuid.UUID,
	foreignSID string,
	spawnedCount int,
	ceilingLevel ceiling.Level,
	hasCeiling bool,
	folded foldResult,
	restoredGates map[gate.ID]gateEntry,
	j journal.SessionJournal,
	factory *event.Factory,
	newID idGenerator,
	now event.Clock,
	opts ...Option,
) (*Session, error) {
	gateAppender, err := journal.NewJournalGateAppenderChecked(j)
	if err != nil {
		return nil, &RestoreError{Kind: RestoreJournalFailed, Cause: err}
	}
	s := &Session{
		sessionID:     sessionID,
		sessionCtx:    sessionCtx,
		sessionCancel: sessionCancel,
		loops:         make(map[uuid.UUID]*loopHandle),
		newID:         newID,
		now:           now,
		cmdAppender:   nopCommandAppender{},
		factory:       factory,
		// Re-seed the cumulative spawn counter from the durable non-root LoopStarted count
		// so the quota survives restart (§16.3). Set before the session is reachable, so no
		// lock is needed yet; once up, NewLoop enforces the quota against this restored base.
		spawned: spawnedCount,
		gates:   cloneGateEntries(restoredGates),
		// Gate directory: restored sessions own a journal-backed appender by default.
		// Caller opts may replace it below for same-package tests.
		gateTimers:   map[gate.ID]*time.Timer{},
		gateAppender: gateAppender,
	}
	// Apply the same opts the probe read (WithCommandAppender wires the durable intent
	// log; WithAllowConfigMismatch is a no-op here — already consumed; WithLimits sets the
	// spawn caps the restored session enforces against the re-seeded counter). A nil
	// appender option leaves the nop default installed.
	for _, opt := range opts {
		opt(s)
	}
	// Default-mint the security-ceiling source (unless WithCeiling injected the shared one),
	// then re-seed it from the folded SecurityCeilingChanged events so the recovered session
	// — and any checker sharing this source — comes up under the ceiling it crashed at (last
	// write wins). No lock is needed: the session is not yet reachable. An absent ceiling
	// (never changed) leaves the fail-secure most-restrictive default.
	if s.ceiling == nil {
		s.ceiling = ceiling.New()
	}
	if hasCeiling {
		s.ceiling.Set(ceilingLevel)
	}
	// Resolve the spawn-cap defaults AFTER the options, mirroring newSession, so a restore
	// (with or without WithLimits) has positive depth/quota caps from the first NewLoop.
	s.limits = s.limits.withDefaults()

	// The hub uses the journal-backed REQUIRED durable tap and the session as its
	// FaultReporter (fail-secure on a required-append failure), sharing the Factory so a
	// hub-synthesized session event is stamped from the same seam.
	appender, err := journal.NewJournalEventAppenderChecked(j)
	if err != nil {
		return nil, &RestoreError{Kind: RestoreJournalFailed, Cause: err}
	}
	s.hub = hub.New(sessionID, hub.WithAppender(appender), hub.WithFactory(factory), hub.WithFaultReporter(s))

	// Seed the primary loop under its ORIGINAL id (identity stable), coming up idle with
	// the folded committed history + turnIndex. No empty loop is spawned and no
	// LoopStarted is published — the loop already exists in the durable record. The Engine
	// switch mirrors newLoop's: a native cfg.Engine seeds through loop.NewRestored exactly
	// as before; a foreign cfg.Engine reconstructs through the injected RestoredBuilder,
	// carrying the recovered foreign session id. It fails CLOSED on an empty recovered sid
	// (the foreign session could not be --resumed) or a missing builder (never silently
	// rebuild the primary as a native loop). Every error path cancels the loopCtx and the
	// session backstop, exactly like the native path.
	loopCtx, cancel := context.WithCancel(sessionCtx)
	var l loop.Backend
	switch cfg.Engine() {
	case loop.EngineNative:
		l, err = loopruntime.NewRestored(loopCtx, sessionID, primaryLoopID, loop.Provenance{}, s, cfg,
			loopruntime.RestoredState{Msgs: folded.Msgs, TurnIndex: folded.TurnIndex})
	default:
		if foreignSID == "" {
			cancel()
			return nil, &RestoreError{Kind: RestoreForeignSIDMissing}
		}
		if s.foreignBuildRestored == nil {
			cancel()
			return nil, &RestoreError{Kind: RestoreForeignBuilderMissing}
		}
		l, err = s.foreignBuildRestored(loopCtx, sessionID, primaryLoopID, loop.Provenance{}, s, cfg,
			func() (uuid.UUID, error) { return newID() }, factory,
			foreignloop.RestoredForeign{ForeignSID: foreignSID, TurnIndex: folded.TurnIndex, Msgs: folded.Msgs})
	}
	if err != nil {
		cancel()
		return nil, &RestoreError{Kind: RestoreLoopFailed, Cause: err}
	}
	s.loops[primaryLoopID] = &loopHandle{id: primaryLoopID, owner: s, bound: cfg, backend: l, parent: loop.Provenance{}, cancel: cancel}
	s.primaryLoopID = primaryLoopID
	return s, nil
}

// appendRestoreEvent stamps ev (EventID + CreatedAt) via the Factory and appends it to
// the journal as an EventRecord. A mint failure surfaces as a typed restore error; an
// append failure returns the journal's typed error unchanged (the caller wraps it). It
// is the single chokepoint for the restore-lifecycle events written before the hub
// exists.
func appendRestoreEvent(ctx context.Context, j journal.SessionJournal, factory *event.Factory, ev event.Event) error {
	stamped, err := factory.Stamp(ev.EventHeader())
	if err != nil {
		return &RestoreError{Kind: RestoreIDGenerationFailed, Cause: err}
	}
	if _, err := j.Append(ctx, journal.NewEventRecord(withRestoreHeader(ev, stamped))); err != nil {
		return err
	}
	return nil
}

// withRestoreHeader returns a copy of a restore-lifecycle event with hdr substituted for
// its Header. The set is exhaustive over the events appendRestoreEvent stamps; an
// unexpected type panics (a programming error — no other event is appended here).
func withRestoreHeader(ev event.Event, hdr event.Header) event.Event {
	switch e := ev.(type) {
	case event.RestoreStarted:
		e.Header = hdr
		return e
	case event.RestoreDone:
		e.Header = hdr
		return e
	case event.RestoreErrored:
		e.Header = hdr
		return e
	case event.TurnInterrupted:
		e.Header = hdr
		return e
	default:
		panic("session: withRestoreHeader called on a non-restore event type")
	}
}

// openTurnCoords returns the TurnID + TurnIndex of the LAST TurnStarted in the primary
// loop's events — the open (unterminated) turn when foldPrimaryLoop reports OpenTurn.
// The crash-seam TurnInterrupted is stamped with these so it closes the exact turn that
// crashed. It is only called when the fold detected an open turn, so a last TurnStarted
// always exists; the zero return is the defensive fall-through.
func openTurnCoords(events []event.Event) (uuid.UUID, event.TurnIndex) {
	for i := len(events) - 1; i >= 0; i-- {
		if ts, ok := events[i].(event.TurnStarted); ok {
			return ts.TurnID, ts.TurnIndex
		}
	}
	return uuid.UUID{}, 0
}

// releaseLease releases the lease on a bounded context, best-effort. On a failed restore
// the lease must not be held (a successor must be able to re-acquire); a release failure
// is swallowed (the bucket TTL is the backstop) since the restore is already failing.
func releaseLease(lease journal.Lease) {
	rctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = lease.Release(rctx)
}
