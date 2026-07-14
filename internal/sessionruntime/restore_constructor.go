package sessionruntime

import (
	"context"
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

// RestoreTopology reconstructs the durable loop topology and brings it up idle. It does
// not publish SessionStarted.
//
// Order (per the design — RestoreStarted is the FIRST restore mutation, after the lease
// fence the journal writes at construction):
//
//  1. Acquire the single-writer lease, construct the SessionJournal (writes the opening
//     LeaseFence — the handover boundary) and the EventReplayer.
//  2. Replay the stream; read the persisted config fingerprint and compare to the live
//     config. On mismatch → *ConfigMismatchError UNLESS WithAllowConfigMismatch is set.
//  3. Append RestoreStarted (the first restore mutation).
//  4. Validate, bind, and independently fold every durable declared loop.
//  5. Append checked crash closures for every open turn.
//  6. Materialize the workspace, build all loops, and validate the active loop.
//  7. Append RestoreDone as the final commit point and return the controller.
//
// On ANY failure in steps 2–7 (a config mismatch, a discovery/replay/decode/object
// failure, or an append failure) Restore durably records a RestoreErrored and returns a
// typed error; the session does NOT come up (fail-secure / no silent drift). On a FAILED
// restore the acquired lease is released here before returning (so a successor can
// re-acquire). On SUCCESS the journal HOLDS the lease for the live session's lifetime;
// releasing it at session teardown is the Phase-10 composition root's wiring (mirroring
// how the lease is composition-root-owned in the durable-tap path), out of scope here.
// RestoreTopology reconstructs the entire topology in one leased replay transaction.
func RestoreTopology(ctx context.Context, topology Topology, sessionID uuid.UUID, store *sessionstore.Store, opts ...Option) (*Session, error) {
	return restoreTopologySession(ctx, topology, sessionID, store, uuid.New, time.Now, opts...)
}

func (s *Session) attachRestoredLoop(started event.LoopStarted, parent loop.Provenance, bound loop.BoundDefinition, folded foldResult, ri restoredInference, foreignSID string) error {
	loopCtx, cancel := context.WithCancel(s.sessionCtx)
	var backend loop.Backend
	var err error
	switch bound.Engine() {
	case loop.EngineNative:
		backend, err = loopruntime.NewRestored(loopCtx, s.sessionID, started.LoopID, parent, s, bound, restoredStateFrom(folded, ri))
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
	liveMode, liveModel := liveViewFor(bound, ri)
	s.loopsMu.Lock()
	s.loops[started.LoopID] = &loopHandle{id: started.LoopID, owner: s, bound: bound, backend: backend, parent: parent, cancel: cancel, liveMode: liveMode, liveModel: liveModel, state: tool.DelegateStatusIdle}
	s.loopsMu.Unlock()
	return nil
}

func restoreTopologySession(
	ctx context.Context,
	topology Topology,
	sessionID uuid.UUID,
	store *sessionstore.Store,
	newID idGenerator,
	now event.Clock,
	opts ...Option,
) (*Session, error) {
	activeDefinition, topologyOK := topology.definition(topology.ActivePrimer)
	if !topologyOK {
		return nil, &RestoreError{Kind: RestoreLoopFailed, Cause: &MissingTopologyError{}}
	}
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
	constructionAbortTimeout := probe.constructionAbortTimeout
	if constructionAbortTimeout <= 0 {
		constructionAbortTimeout = defaultConstructionAbortTimeout
	}
	if probe.ceiling == nil {
		probe.ceiling = ceiling.New()
	}
	allowMismatch := probe.allowConfigMismatch
	if probe.fingerprint == nil && probe.frozenFingerprint == nil {
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
	// Offload GC (symmetric with NewSession): when the restore-forwarded policy is armed,
	// wrap the journal with the admission gate BEFORE any appender (restore-lifecycle appends
	// or the rebuilt session's appenders) is built over it. The runner is started only after
	// the session is rebuilt.
	j, gcRunner, err := wrapJournalWithOffloadGC(store, sessionID, lease, j, probe.offloadGCPolicy)
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
	// resolved holds the per-session managed-workspace placement (exclusive root lease +
	// coordinator) once resolved below. recordErrored releases the root lease BEFORE the
	// session lease (LIFO) so a failed restore never strands root-lease ownership.
	var resolved *resolvedPlacement
	recordErrored := func(restoreErr error) (*Session, error) {
		runRestoreFailureCleanup(constructionAbortTimeout, func(appendCtx context.Context) {
			_ = appendRestoreEvent(appendCtx, j, factory, event.RestoreErrored{
				Header: event.Header{Coordinates: identity.Coordinates{SessionID: sessionID}},
				Err:    restoreErr,
			})
		}, func() {
			releaseResolvedRoot(context.Background(), resolved)
			releaseLease(lease)
		})
		return nil, restoreErr
	}
	abortAccepted := func(s *Session, restoreErr error) (*Session, error) {
		s.abortConstructionAfter(restoreErr, func(appendCtx context.Context) {
			_ = appendRestoreEvent(appendCtx, j, factory, event.RestoreErrored{
				Header: event.Header{Coordinates: identity.Coordinates{SessionID: sessionID}},
				Err:    restoreErr,
			})
		})
		return nil, restoreErr
	}

	// (2) Replay the whole stream once for discovery (the persisted fingerprint + the
	// root loop id + the subagent spawn count) and gate recovery. A ZERO LoopID leaves
	// the event projection UNNARROWED — every loop's events — so findRootLoopStarted and
	// countSpawnedLoops see the subagent LoopStarted events, not just the root's. Fail
	// closed on any error.
	allRecords, err := drainRecordReplay(ctx, replayer, journal.ReplayRequest{Follow: false})
	if err != nil {
		return recordErrored(&RestoreError{Kind: RestoreReplayFailed, Cause: err})
	}
	all := eventsFromRecords(allRecords, uuid.UUID{})
	// Privileged lifecycle records are interpreted only for crash-consistency
	// validation. Unmatched starts remain in the journal as interrupted audit
	// evidence; no queue, worker, request, finalizer, activity, or synthetic
	// terminal is reconstructed.
	if _, auditErr := foldRestoredHustleAudit(all); auditErr != nil {
		return recordErrored(&RestoreError{Kind: RestoreReplayFailed, Cause: auditErr})
	}
	ceilingLevel, hasCeiling := lastSecurityCeiling(all)
	if hasCeiling {
		probe.ceiling.Set(ceilingLevel)
	}

	persisted, err := firstConfigFingerprint(all)
	if err != nil {
		return recordErrored(err)
	}
	// A rig supplies a frozen fingerprint, allowing the mismatch decision to happen
	// immediately after replay and before root acquisition, binding, or reconstruction.
	if probe.frozenFingerprint != nil {
		if err := checkFingerprint(persisted, *probe.frozenFingerprint, allowMismatch); err != nil {
			return recordErrored(err)
		}
	}
	// (4a) Discover every durable root (zero-Cause LoopStarted) and the ordered starts, and
	// validate that every configured primer maps to a root (single-primer recovery honored
	// under allowMismatch). (4b) Bind + independently fold every durable loop into a plan
	// and pick the active primer's plan. Both fail closed through recordErrored.
	roots, starts, err := discoverRoots(all, topology, allowMismatch)
	if err != nil {
		return recordErrored(err)
	}
	// Stand up the delegation manager BEFORE binding any loop so each restored loop's
	// Subagent tool is bound against a parent-scoped controller; it is attached to the
	// live session once buildRestoredSession creates it. Seed its durable request→terminal
	// index from the full stream so a wait for a wait:false request submitted before the
	// restart resolves after restore.
	// Resolve the managed-workspace placement AFTER the session lease (acquired in step 1),
	// honoring the session-lease-before-root-lease ordering on the restore path. It
	// populates probe.ws/wsRoot/wsCoordinator so the loop bind below serializes the
	// restored session's tools through the live coordinator, and re-fences an exclusive
	// root. A root-lease contention fails the restore closed.
	if probe.placementSpec.Configured() {
		r, placeErr := probe.placementSpec.resolveForNew(ctx, sessionID)
		if placeErr != nil {
			return recordErrored(&RestoreError{Kind: RestoreLeaseFailed, Cause: placeErr})
		}
		resolved = r
		withResolvedPlacement(resolved)(probe)
	}

	manager := newDelegationManager(topology)
	plans, activePlan, err := planLoops(sessionCtx, sessionID, topology, activeDefinition, roots, starts, allRecords, allowMismatch, manager, probe.ceiling, probe.newWorkspaceBinding)
	if err != nil {
		return recordErrored(err)
	}
	if probe.frozenFingerprint == nil {
		if err := checkFingerprint(persisted, probe.projectFingerprint(activePlan.bound), allowMismatch); err != nil {
			return recordErrored(err)
		}
	}
	rootLoopID := activePlan.started.LoopID
	bound := activePlan.bound

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

	// Every loop was independently folded from its loop-scoped projection above.
	rootEvents := activePlan.events
	folded := activePlan.folded
	// Fold the root loop's mode + direct inference changes so it resumes under the
	// effective config it crashed under (last write wins, live precedence).
	activeInference := foldLoopInference(rootEvents)

	// (6) Crash-seam: an open turn (a TurnStarted with no terminal) is closed durably
	// with a TurnInterrupted carrying the open turn's id + index, so the resumed loop
	// never observes a half-open turn.
	var crashClosures []event.Event
	for _, plan := range plans {
		if !plan.folded.OpenTurn {
			continue
		}
		turnID, turnIdx := openTurnCoords(plan.events)
		closure := event.TurnInterrupted{
			Header: event.Header{Coordinates: identity.Coordinates{
				SessionID: sessionID, LoopID: plan.started.LoopID, TurnID: turnID,
			}},
			TurnIndex: turnIdx,
		}
		if err := appendRestoreEvent(ctx, j, factory, closure); err != nil {
			return recordErrored(&RestoreError{Kind: RestoreAppendFailed, Cause: err})
		}
		crashClosures = append(crashClosures, closure)
	}
	// Correlation is seeded only after every checked crash closure committed, so an open
	// wait:false request resolves as Interrupted after restore rather than unknown.
	if err := seedResolvedDelegateRecords(manager, allRecords, all, crashClosures); err != nil {
		return recordErrored(&RestoreError{Kind: RestoreReplayFailed, Cause: err})
	}

	// (6b) Materialize the workspace ref selected by the latest durable transition BEFORE
	// declaring the restore done, so
	// RestoreDone is appended only if the workspace is also restored (fail closed — the
	// session never comes up on a workspace that does not match the durable pointer). It
	// uses probe.ws/probe.wsRoot because the live Session is not built yet. Skipped
	// when no workspace store is wired (a conversation-only restore —
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

	// Build the Session, reusing sessionID + the active primer loop id (identity stable).
	// It mirrors newSession's wiring EXCEPT: no SessionStarted is published (the start
	// was recorded on the original run), and the root loop is SEEDED via NewRestored
	// rather than spawned empty. The lease Restore acquired is handed to the session as its
	// release-on-Shutdown hook (the Phase-10 composition wiring): the journal holds the
	// lease for the live lifetime, and a clean Shutdown releases it so a successor can
	// re-acquire without waiting out the TTL. We append WithLeaseRelease AFTER the caller's
	// opts so the restore owns the lease lifecycle (a caller cannot accidentally override
	// the releaser with a stale one).
	leaseOpts := append(append([]Option(nil), opts...), WithCeiling(probe.ceiling), WithLeaseRelease(lease.Release))
	if resolved != nil {
		// Hand the restored session the coordinator + exclusive root-lease release so its
		// Shutdown releases the root lease before the session lease (LIFO), and so its loops'
		// tools serialize through the live coordinator.
		leaseOpts = append(leaseOpts, withResolvedPlacement(resolved))
	}
	if gcRunner != nil {
		// Hand the restored session its offload-GC runner (started after the hub exists,
		// below) so its Shutdown stops+joins it before SessionStopped and lease release.
		leaseOpts = append(leaseOpts, withOffloadGCRunner(gcRunner))
	}
	// Recover the foreign session id from the root loop's events. Prebound adapters
	// stamped it on LoopStarted; late-bound adapters record it with ForeignSessionBound.
	// buildRestoredSession fails closed on an empty sid for a foreign engine.
	foreignSID := findForeignSID(rootEvents)
	s, err := buildRestoredSession(sessionCtx, sessionCancel, bound, sessionID, rootLoopID, foreignSID, spawnedCount, ceilingLevel, hasCeiling, folded, activeInference, restoredGates.open, j, factory, newID, now, leaseOpts...)
	if err != nil {
		if constructionCleanupOwned(err) {
			return nil, err
		}
		return recordErrored(err)
	}
	// Attach the delegation manager the loop tools were bound against, so the restored
	// session's scoped controllers can spawn + address children. Ownership survives
	// restore through the loop registry's parent links (re-seeded by attachAndActivate).
	s.topology = cloneTopology(topology)
	s.delegation = manager
	manager.attach(s)
	// (7) Post-build wiring: register every non-root restored loop, resolve + validate the
	// durable active selection, and install it as the session's active loop. Any failure cancels the
	// session context (tearing down the seeded loops) before recording a RestoreErrored.
	if err := attachAndActivate(s, all, plans, rootLoopID); err != nil {
		return abortAccepted(s, err)
	}
	// RestoreDone is the commit point: every loop is bound, crash-closed, built,
	// attached, and the active selection has been validated before this append.
	if err := appendRestoreEvent(ctx, j, factory, event.RestoreDone{
		Header: event.Header{Coordinates: identity.Coordinates{SessionID: sessionID}},
	}); err != nil {
		return abortAccepted(s, &RestoreError{Kind: RestoreAppendFailed, Cause: err})
	}
	// Start the exclusive root-lease loss watcher now that the restored session owns the
	// root lease (via leaseOpts). Nil-safe for per-session/shared/no placement. Arm the
	// offload-GC runner now the hub exists (bound to hub.IsIdle).
	s.watchRootLease()
	s.startOffloadGC()
	contextTransferred = true
	return s, nil
}

func runRestoreFailureCleanup(timeout time.Duration, appendErrored func(context.Context), release func()) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer cancel()
		appendErrored(ctx)
		release()
	}()
	select {
	case <-done:
	case <-ctx.Done():
		// The sole cleanup owner goroutine retains raw ownership while a
		// context-ignoring append remains inside the journal.
	}
}

// loopPlan is a single durable loop staged for restore: its root LoopStarted, its bound
// definition, its loop-scoped events, and the fold of those events (committed msgs +
// turnIndex + open-turn flag). planLoops builds one per durable loop; the restore
// constructor crash-closes, seeds, and attaches from these.
type loopPlan struct {
	started event.LoopStarted
	bound   loop.BoundDefinition
	events  []event.Event
	folded  foldResult
}

// discoverRoots scans the full UNNARROWED replay for every LoopStarted, collecting the
// ordered `starts` and the root (zero-Cause) LoopStarted per AgentName. For a single-primer
// topology it maps the sole configured primer onto the first root (name-agnostic
// compatibility), and — under WithAllowConfigMismatch — recovers a single primer whose name
// does not match the persisted root. It fails closed if the stream carries no loop
// (RestoreNoPrimerLoop) or a configured primer has no matching root in a topology that
// cannot be recovered, returning a typed error the caller records as a RestoreErrored.
func discoverRoots(all []event.Event, topology Topology, allowMismatch bool) (map[identity.AgentName]event.LoopStarted, []event.LoopStarted, error) {
	roots := make(map[identity.AgentName]event.LoopStarted)
	starts := make([]event.LoopStarted, 0)
	for _, ev := range all {
		if started, ok := ev.(event.LoopStarted); ok {
			starts = append(starts, started)
			if started.Cause.Coordinates == (identity.Coordinates{}) {
				if _, duplicate := roots[started.AgentName]; !duplicate {
					roots[started.AgentName] = started
				}
			}
		}
	}
	if len(topology.Primers) == 1 {
		for _, started := range starts {
			if started.Cause.Coordinates == (identity.Coordinates{}) {
				roots[topology.Primers[0]] = started
				break
			}
		}
	}
	if len(starts) == 0 {
		return nil, nil, &RestoreDiscoveryError{Kind: RestoreNoPrimerLoop}
	}
	for _, primer := range topology.Primers {
		if _, ok := roots[primer]; !ok {
			if allowMismatch && len(topology.Primers) == 1 {
				for _, started := range starts {
					if started.Cause.Coordinates == (identity.Coordinates{}) {
						roots[primer] = started
						break
					}
				}
				if _, recovered := roots[primer]; recovered {
					continue
				}
			}
			return nil, nil, &RestoreError{Kind: RestoreLoopFailed, Cause: &AgentNameMismatchError{Persisted: "", Configured: primer}}
		}
	}
	return roots, starts, nil
}

// planLoops binds and independently folds every durable loop into a loopPlan (in stream
// order) and identifies the active primer's plan. For a single-definition topology it maps
// the configured root onto the sole definition and skips non-root loops whose AgentName is
// unknown (subagents of a single-definition run). It is the single Bind of each loop,
// performed inside the restore lease. It returns the ordered plans and the active plan, or a
// typed error the caller records as a RestoreErrored.
func planLoops(sessionCtx context.Context, sessionID uuid.UUID, topology Topology, activeDefinition loop.Definition, roots map[identity.AgentName]event.LoopStarted, starts []event.LoopStarted, allRecords []journal.JournalRecord, allowMismatch bool, manager *delegationManager, ceilingSource ceiling.Source, wsBind func() *tool.WorkspaceBinding) ([]loopPlan, loopPlan, error) {
	plans := make([]loopPlan, 0, len(starts))
	boundByLoop := make(map[uuid.UUID]loop.BoundDefinition, len(starts))
	activeIndex := -1
	for _, started := range starts {
		definition, ok := topology.definition(started.AgentName)
		isConfiguredRoot := started.LoopID == roots[topology.ActivePrimer].LoopID
		if !ok && isConfiguredRoot && len(topology.Definitions) == 1 {
			definition, ok = activeDefinition, true
		}
		if !ok && !isConfiguredRoot && len(topology.Definitions) == 1 {
			continue
		}
		if !ok {
			// WithAllowConfigMismatch recovery is intentionally single-primer/fail-secure: a
			// multi-definition topology hard-fails on ANY unknown loop AgentName (allowMismatch
			// only relaxes the name check for the sole root above via checkAgentName — it never
			// invents a missing definition here).
			return nil, loopPlan{}, &RestoreError{Kind: RestoreLoopFailed, Cause: &AgentNameMismatchError{Persisted: started.AgentName}}
		}
		bound, bindErr := definition.Bind(sessionCtx, tool.Bindings{SessionID: sessionID, LoopID: started.LoopID, Ceiling: ceilingSource, Workspace: wsBind(), Delegate: manager.controllerFor(started.LoopID, definition), ExtraTools: delegateExtraTools(definition, manager)})
		if bindErr != nil {
			return nil, loopPlan{}, &RestoreError{Kind: RestoreLoopFailed, Cause: bindErr}
		}
		if nameErr := checkAgentName(started.AgentName, bound.Name(), allowMismatch); nameErr != nil {
			return nil, loopPlan{}, nameErr
		}
		if parentID := started.Cause.Coordinates.LoopID; !parentID.IsZero() {
			parentBound := boundByLoop[parentID]
			var parentPermission loop.PermissionGate
			if parentBound != nil {
				parentPermission = parentBound.Permission()
			}
			bound = loop.AttenuateBoundPermission(bound, parentPermission)
		}
		boundByLoop[started.LoopID] = bound
		loopEvents := eventsFromRecords(allRecords, started.LoopID)
		plans = append(plans, loopPlan{started: started, bound: bound, events: loopEvents, folded: foldLoop(loopEvents)})
		if started.LoopID == roots[topology.ActivePrimer].LoopID {
			activeIndex = len(plans) - 1
		}
	}
	if activeIndex < 0 {
		return nil, loopPlan{}, &RestoreError{Kind: RestoreLoopFailed, Cause: &SessionError{Kind: SessionLoopNotFound}}
	}
	return plans, plans[activeIndex], nil
}

// attachAndActivate registers every non-root restored loop, resolves the durable active
// selection (the last ActiveLoopChanged, else the initially active root), validates it is
// registered, and sets it as the session's active loop under loopsMu — the post-build wiring performed after the
// root loop is seeded and before RestoreDone. It returns a typed *RestoreError on any
// failure (an attach failure, an unregistered active target, or a latched persistence
// fault); the caller cancels the session context and records a RestoreErrored. On success
// s.activeLoopID reflects the durable active loop.
func attachAndActivate(s *Session, all []event.Event, plans []loopPlan, rootLoopID uuid.UUID) error {
	for _, plan := range plans {
		if plan.started.LoopID == rootLoopID {
			continue
		}
		parent := loop.Provenance{LoopID: plan.started.Cause.Coordinates.LoopID, TurnID: plan.started.Cause.Coordinates.TurnID, StepID: plan.started.Cause.Coordinates.StepID}
		if err := s.attachRestoredLoop(plan.started, parent, plan.bound, plan.folded, foldLoopInference(plan.events), findForeignSID(plan.events)); err != nil {
			return err
		}
	}
	activeID := rootLoopID
	for _, ev := range all {
		if changed, ok := ev.(event.ActiveLoopChanged); ok {
			activeID = changed.ActiveLoopID
		}
	}
	if _, ok := s.Loop(activeID); !ok {
		return &RestoreError{Kind: RestoreLoopFailed, Cause: &SessionError{Kind: SessionLoopNotFound}}
	}
	s.loopsMu.Lock()
	s.activeLoopID = activeID
	if s.faulted {
		fault := s.faultErr
		s.loopsMu.Unlock()
		return &RestoreError{Kind: RestoreAppendFailed, Cause: fault}
	}
	s.loopsMu.Unlock()
	return nil
}

// buildRestoredSession assembles the live Session for a successful restore: the hub
// wired with the journal-backed event appender, the shared Factory, and the session as
// the hub's FaultReporter; the journal-backed command appender; and the root loop
// seeded (NewRestored) with the folded committed state under its ORIGINAL id, idle. It
// publishes NO SessionStarted and spawns NO empty loop — both are the deliberate
// difference from New.
func buildRestoredSession(
	sessionCtx context.Context,
	sessionCancel context.CancelFunc,
	cfg loop.BoundDefinition,
	sessionID, rootLoopID uuid.UUID,
	foreignSID string,
	spawnedCount int,
	ceilingLevel ceiling.Level,
	hasCeiling bool,
	folded foldResult,
	ri restoredInference,
	restoredGates map[gate.ID]gateEntry,
	j journal.SessionJournal,
	factory *event.Factory,
	newID idGenerator,
	now event.Clock,
	opts ...Option,
) (*Session, error) {
	s := &Session{
		sessionID:                sessionID,
		sessionCtx:               sessionCtx,
		sessionCancel:            sessionCancel,
		constructionAbortTimeout: defaultConstructionAbortTimeout,
		loops:                    make(map[uuid.UUID]*loopHandle),
		newID:                    newID,
		now:                      now,
		cmdAppender:              nopCommandAppender{},
		factory:                  factory,
		// Re-seed the cumulative spawn counter from the durable non-root LoopStarted count
		// so the quota survives restart (§16.3). Set before the session is reachable, so no
		// lock is needed yet; once up, NewLoop enforces the quota against this restored base.
		spawned: spawnedCount,
		gates:   cloneGateEntries(restoredGates),
		// Gate directory: restored sessions own a journal-backed appender by default.
		// Caller opts may replace it below for same-package tests.
		gateTimers:          map[gate.ID]*time.Timer{},
		gateAppender:        nopGateAppender{},
		checkpointAdmission: newCheckpointAdmissionGate(),
	}
	// Apply the same opts the probe read (WithCommandAppender wires the durable intent
	// log; WithAllowConfigMismatch is a no-op here — already consumed; WithLimits sets the
	// spawn caps the restored session enforces against the re-seeded counter). A nil
	// appender option leaves the nop default installed.
	for _, opt := range opts {
		opt(s)
	}
	abort := func(restoreErr error) (*Session, error) {
		s.abortConstructionAfter(restoreErr, func(appendCtx context.Context) {
			_ = appendRestoreEvent(appendCtx, j, factory, event.RestoreErrored{
				Header: event.Header{Coordinates: identity.Coordinates{SessionID: sessionID}},
				Err:    restoreErr,
			})
		})
		return nil, &constructionCleanupOwnedError{cause: restoreErr}
	}
	gateAppender, err := journal.NewJournalGateAppenderChecked(j)
	if err != nil {
		return abort(&RestoreError{Kind: RestoreJournalFailed, Cause: err})
	}
	s.gateAppender = gateAppender
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
		return abort(&RestoreError{Kind: RestoreJournalFailed, Cause: err})
	}
	hubOpts := []hub.Option{hub.WithAppender(appender), hub.WithFactory(factory), hub.WithFaultReporter(s)}
	s.hub = hub.New(sessionID, hubOpts...)
	s.gateAppender = &liveGateAppender{prepared: gateAppender, publisher: s}
	if err := s.bindSessionHustles(); err != nil {
		return abort(&RestoreError{Kind: RestoreLoopFailed, Cause: err})
	}
	if s.snapshotPolicy != nil && s.ws != nil && s.wsCoordinator != nil {
		s.checkpoints = newCheckpointController(checkpointControllerConfig{
			SessionID: sessionID, Policy: *s.snapshotPolicy, Store: s.ws, Root: s.wsRoot,
			Mode: s.wsMode, Coordinator: s.wsCoordinator, Publisher: s, Factory: s.factory,
			Idle: s.hub.IsIdle, Fault: s.latchWorkspaceCheckpointFault,
			Recover: s.recoverWorkspaceCheckpointFault, Faulted: s.faultIfFaulted,
			Admission:    s.checkpointAdmission.enterCheckpoint,
			ObserveError: s.observeBestEffortCheckpointError,
		})
	}

	// Seed the root loop under its ORIGINAL id (identity stable), coming up idle with
	// the folded committed history + turnIndex. No empty loop is spawned and no
	// LoopStarted is published — the loop already exists in the durable record. The Engine
	// switch mirrors newLoop's: a native bound definition seeds through
	// loopruntime.NewRestored; a foreign engine reconstructs through the injected RestoredBuilder,
	// carrying the recovered foreign session id. It fails CLOSED on an empty recovered sid
	// (the foreign session could not be --resumed) or a missing builder (never silently
	// rebuild the root as a native loop). Every error path cancels the loopCtx and the
	// session backstop, exactly like the native path.
	loopCtx, cancel := context.WithCancel(sessionCtx)
	var l loop.Backend
	switch cfg.Engine() {
	case loop.EngineNative:
		l, err = loopruntime.NewRestored(loopCtx, sessionID, rootLoopID, loop.Provenance{}, s, cfg,
			restoredStateFrom(folded, ri))
	default:
		if foreignSID == "" {
			cancel()
			restoreErr := &RestoreError{Kind: RestoreForeignSIDMissing}
			return abort(restoreErr)
		}
		if s.foreignBuildRestored == nil {
			cancel()
			restoreErr := &RestoreError{Kind: RestoreForeignBuilderMissing}
			return abort(restoreErr)
		}
		l, err = s.foreignBuildRestored(loopCtx, sessionID, rootLoopID, loop.Provenance{}, s, cfg,
			func() (uuid.UUID, error) { return newID() }, factory,
			foreignloop.RestoredForeign{ForeignSID: foreignSID, TurnIndex: folded.TurnIndex, Msgs: folded.Msgs})
	}
	if err != nil {
		cancel()
		restoreErr := &RestoreError{Kind: RestoreLoopFailed, Cause: err}
		return abort(restoreErr)
	}
	liveMode, liveModel := liveViewFor(cfg, ri)
	s.loops[rootLoopID] = &loopHandle{id: rootLoopID, owner: s, bound: cfg, backend: l, parent: loop.Provenance{}, cancel: cancel, liveMode: liveMode, liveModel: liveModel, state: tool.DelegateStatusIdle}
	s.activeLoopID = rootLoopID
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

// openTurnCoords returns the TurnID + TurnIndex of the last TurnStarted in one loop's
// events — the open (unterminated) turn when foldLoop reports OpenTurn.
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
	rctx, cancel := context.WithTimeout(context.Background(), leaseReleaseTimeout)
	defer cancel()
	_ = lease.Release(rctx)
}
