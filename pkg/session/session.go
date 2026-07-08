package session

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/looprig/core/content"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/ceiling"
	"github.com/looprig/harness/pkg/command"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/foreignloop"
	"github.com/looprig/harness/pkg/gate"
	"github.com/looprig/harness/pkg/hub"
	"github.com/looprig/harness/pkg/identity"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/workspacestore"
)

type SessionErrorKind string

const (
	SessionIDGenerationFailed     SessionErrorKind = "id_generation_failed"
	SessionLoopIDGenerationFailed SessionErrorKind = "loop_id_generation_failed"
	SessionLoopExited             SessionErrorKind = "loop_exited"
	SessionLoopNotFound           SessionErrorKind = "loop_not_found"
	SessionEventChannelClosed     SessionErrorKind = "event_channel_closed"
	SessionContextDone            SessionErrorKind = "context_done"
	SessionClosing                SessionErrorKind = "session_closing"
	SessionFaulted                SessionErrorKind = "session_faulted"
	SessionLoopDepthExceeded      SessionErrorKind = "loop_depth_exceeded"
	SessionLoopQuotaExceeded      SessionErrorKind = "loop_quota_exceeded"
	// SessionForeignBuilderMissing: a loop.Config selected a foreign Engine but no
	// foreign Builder was wired (WithForeignBuilder). newLoop fails closed rather than
	// silently falling back to a native loop — a foreign engine must never resolve to a
	// different backend than the caller asked for.
	SessionForeignBuilderMissing SessionErrorKind = "foreign_builder_missing"
)

// SessionError is returned when a session method cannot complete.
// Cause is non-nil when there is an underlying error to chain.
type SessionError struct {
	Kind  SessionErrorKind
	Cause error
}

func (e *SessionError) Error() string {
	var msg string
	switch e.Kind {
	case SessionIDGenerationFailed:
		msg = "session: id generation failed"
	case SessionLoopIDGenerationFailed:
		msg = "session: loop id generation failed"
	case SessionLoopExited:
		msg = "session: loop exited"
	case SessionLoopNotFound:
		msg = "session: loop not found"
	case SessionEventChannelClosed:
		msg = "session: event channel closed without terminal event"
	case SessionContextDone:
		msg = "session: context done"
	case SessionClosing:
		msg = "session: closing"
	case SessionFaulted:
		msg = "session: faulted (durable persistence failure)"
	case SessionLoopDepthExceeded:
		msg = "session: loop spawn depth limit exceeded"
	case SessionLoopQuotaExceeded:
		msg = "session: loop spawn quota exceeded"
	case SessionForeignBuilderMissing:
		msg = "session: foreign engine selected but no foreign builder wired"
	default:
		msg = "session: error"
	}
	if e.Cause == nil {
		return msg
	}
	return msg + ": " + e.Cause.Error()
}
func (e *SessionError) Unwrap() error { return e.Cause }

// TurnRejectedError is returned by drainToFinalText when the loop refuses to start
// a turn for a subagent submit — a phase-1 event.TurnRejected for the submit's
// Cause.CommandID means no turn will ever start, so the drain surfaces it as this
// typed error. Reason carries the event RejectReason
// (QueueFull/ShuttingDown/Internal) so callers can errors.As and branch (e.g.
// retry-on-queue-full, or retry a transient RejectInternal).
type TurnRejectedError struct {
	Reason event.RejectReason
}

func (e *TurnRejectedError) Error() string {
	switch e.Reason {
	case event.RejectQueueFull:
		return "session: turn rejected: queue full"
	case event.RejectShuttingDown:
		return "session: turn rejected: loop shutting down"
	case event.RejectInternal:
		// Transient internal failure (e.g. id generation); the loop is healthy and
		// the caller MAY retry. Distinct from RejectShuttingDown.
		return "session: turn rejected: transient internal failure"
	default:
		// RejectUnspecified (the zero-value sentinel) or any unknown reason.
		return "session: turn rejected"
	}
}

// idGenerator mints a fresh UUID. It defaults to uuid.New; tests inject a
// failing generator to exercise the crypto/rand failure branch.
type idGenerator func() (uuid.UUID, error)

type Session struct {
	// SessionID is shared by every loop participating in this session.
	SessionID uuid.UUID

	// hub is the session-level event fan-in. Loops publish through it (via the
	// session's PublishEvent, which delegates here); consumers subscribe via
	// SubscribeEvents. The hub also owns the federated-quiescence model that
	// WaitIdle reads. It is constructed in New before any loop, so a loop
	// never publishes into a nil hub.
	hub *hub.Hub

	// sessionCtx is the shared lifetime root for the session; every loop gets a
	// loopCtx derived from it. sessionCancel is the final backstop, cancelled by
	// the construction context (today) or future explicit teardown.
	sessionCtx    context.Context
	sessionCancel context.CancelFunc

	// loopsMu protects loops and primaryLoopID. There is no session goroutine, so
	// session methods serialize registry access with a normal RWMutex.
	loopsMu sync.RWMutex

	// loops are the loop handles in this session, keyed by loop id. Each entry
	// pairs the loop handle with the provenance of whatever spawned it (zero for
	// the primary loop). Today this map holds one entry; multi-agent
	// orchestration adds subagent loops with a non-zero parent.
	loops map[uuid.UUID]*loopHandle

	// primaryLoopID is the default target for Submit and the gate-answer methods
	// (and the loop Interrupt/Shutdown fan out across, starting from).
	primaryLoopID uuid.UUID

	// closing is the fail-secure latch: once set, NewLoop refuses to create or
	// register any further loop. It is guarded by loopsMu — set by Shutdown
	// (which flips it and snapshots the loops under the same lock), read by
	// NewLoop's registration critical section. Because both the set and the
	// authoritative NewLoop check happen under loopsMu, a loop can never be
	// registered after Shutdown's snapshot has been taken.
	closing bool

	// faulted is the persistence fail-secure latch: set by ReportFault when the hub
	// raises a SessionPersistenceFault (a required durable append failed). Once set,
	// every new Submit/NewLoop is refused with SessionFaulted, so no further work is
	// admitted to a session whose durable log is no longer trustworthy. faultErr is
	// the fault that latched it (chained as the refusal's Cause). Both are guarded by
	// loopsMu — the same lock that gates closing and the NewLoop registration check —
	// so a fault and a NewLoop can never interleave incorrectly.
	faulted  bool
	faultErr error

	// limits are the in-session subagent-spawn safety caps NewLoop enforces (depth +
	// quota). Defaulted in newSession (withDefaults) so the live values are always
	// positive caps, and overridable via WithLimits. Read under loopsMu inside NewLoop's
	// authoritative critical section (the same lock that gates closing/faulted), so the
	// caps are evaluated atomically with the reservation. It is set once at construction
	// and never mutated, so the lock is only for read-coherence with the spawned counter.
	limits Limits

	// spawned is the running count of sub-loops this session has spawned via the
	// quota-counted NewLoop path (the primary loop, built by New, does NOT count). It is
	// the quota's reservation counter: NewLoop reserves a slot (spawned++) under loopsMu
	// once the depth + quota + closing/faulted checks pass, and ROLLS BACK (spawned--)
	// under the same lock on every later failure (id-mint, loop.New, the registration-time
	// closing re-check, publish failure). Restore re-seeds it by counting the durable
	// non-root LoopStarted events, so the quota survives a restart. Guarded by loopsMu.
	spawned int

	// newID mints command-Header IDs and loop ids. It defaults to uuid.New; kept
	// as a field only so tests can inject failure and prove the session never
	// sends zero-id commands and never registers a zero-id loop.
	newID idGenerator

	// now is the clock the session's event Factory mints CreatedAt from. It
	// defaults to time.Now; kept as a field so tests can pin it deterministically
	// (mirrors newID).
	now event.Clock

	// factory mints the EventID + CreatedAt stamped onto the session-scoped Enduring
	// events the session itself produces — SessionStarted (in New) and the loop-tree
	// record LoopStarted (in NewLoop). It is built in New from closures over the live
	// newID + now fields, so a test that swaps either after construction pins the
	// stamp too. The LOOP owns minting for its own loop events (loopConfig.eventFactory);
	// this Factory is only for the session's own creation sites.
	factory *event.Factory

	// cmdAppender is the AUDIT-ONLY intent-log seam: every command the session
	// dispatches to a loop is appended here BEFORE the send (appendCommand). It defaults
	// to the nop appender (no-persistence/headless mode); the composition root (Phase 10)
	// injects the real journal.JournalCommandAppender via WithCommandAppender. Unlike the
	// hub's required durable tap, an append failure here is logged-and-swallowed — the
	// dispatch always proceeds (a lost command record must never block the user).
	cmdAppender commandAppender

	// allowConfigMismatch is the restore-only opt-in (set by WithAllowConfigMismatch)
	// to resume a session whose persisted config fingerprint no longer matches the live
	// config. It is read ONLY by Restore (before the session comes up); New never
	// consults it. Default false = fail-secure (a mismatch rejects the restore).
	allowConfigMismatch bool

	// configFingerprintFields are the swarm-level fingerprint inputs not on loop.Config
	// (AgentKind, RuntimeSkills, WorkspaceRoot), injected via WithConfigFingerprintFields.
	// New merges them onto the loop-derived fingerprint it stamps on SessionStarted;
	// Restore merges them onto the LIVE fingerprint it compares against the persisted one
	// (so a different skill-trust mode or workspace can't silently resume). Zero (the
	// default, no option) leaves them empty — a non-swarm/legacy session is unaffected.
	configFingerprintFields ConfigFingerprintFields

	// injectedSessionID is the externally-minted sessionID the composition root supplies
	// via WithSessionID, read ONLY by newSession to resolve the journal chicken-and-egg:
	// the journal needs the sid before session construction, so the composition root mints
	// it first, builds the journal/lease/appenders from it, then passes it in here. Zero
	// (the default, no option) means New mints its own. It is never consulted by Restore
	// (which already takes the sessionID as a positional argument).
	injectedSessionID uuid.UUID

	// injectedEventAppender is the hub's REQUIRED durable tap the composition root supplies
	// via WithEventAppender, read ONLY by newSession when it builds the hub: the journal's
	// JournalEventAppender wired as hub.WithAppender so every Enduring event is appended
	// before fan-out (fail-secure). Nil (the default, no option) leaves the hub's nop
	// appender installed — headless/no-persistence mode is unchanged. Restore builds its
	// own appender from the journal it constructs, so it does not read this.
	injectedEventAppender eventAppender

	// leaseRelease is the single-writer-lease release hook the composition root installs
	// via WithLeaseRelease (New) or that Restore installs from the lease it acquires. It is
	// called ONCE at the END of Shutdown (releaseOnce) — after the loops have drained, so
	// the journal's last append precedes the release — so a clean exit relinquishes
	// ownership and a successor can re-acquire without waiting out the TTL. Nil (headless /
	// no-persistence) is a no-op. The session owns this seam (DIP): it never holds the
	// concrete lease, only the narrow release closure.
	leaseRelease func(context.Context) error
	releaseOnce  sync.Once

	// foreignBuild and foreignBuildRestored are the composition-root seams newLoop and
	// the restore path use to construct a foreign-engine loop (live + restored). They are
	// wired via WithForeignBuilder; nil (the default) means foreign engines are not
	// supported, so a foreign cfg.Engine fails closed (SessionForeignBuilderMissing /
	// RestoreForeignBuilderMissing). The session depends only on these narrow function
	// seams, never on the foreignloop concrete loop (Dependency Inversion): loop.New
	// itself only ever builds native, and the foreign backend is injected here.
	foreignBuild         foreignloop.Builder
	foreignBuildRestored foreignloop.RestoredBuilder

	// ws is the workspace snapshot store CheckpointWorkspace archives the session's
	// working tree into, and wsRoot is the directory it archives. Both are wired
	// together by WithWorkspaceStore; nil ws (the default, no option) leaves the
	// capability unconfigured, so CheckpointWorkspace fails closed with a typed
	// *WorkspaceNotConfiguredError. The session depends only on the narrow *Store
	// (Dependency Inversion): it never sees the Blobs backend beneath it.
	ws     *workspacestore.Store // nil unless WithWorkspaceStore wired it; gates CheckpointWorkspace
	wsRoot string                // the workspace directory Snapshot archives

	// ceiling is the session's live SECURITY-CEILING ordinal source (SPEC §8/§10.2): the
	// clamp SetSecurityCeiling mutates and CeilingSource exposes. It is default-minted at
	// construction (New/Restore) so it is NEVER nil for a constructed session; the
	// composition root overrides it via WithCeiling with the SAME *ceiling.State it wires
	// into the permission checker (tools.WithCeilingPostures), so a ceiling change is
	// visible to the checker on the next Check. On restore it is re-seeded from the folded
	// SecurityCeilingChanged events (last write wins). Concurrency-safe (atomic) — a
	// checker reads Current on a loop goroutine while SetSecurityCeiling applies on the
	// dispatch goroutine.
	ceiling *ceiling.State

	// gatesMu protects gates and the per-entry state transitions. The gate
	// directory is session-owned (the session is the source of truth for which
	// gates are preparing/open/claiming/closed), so all directory mutations
	// serialize behind this mutex.
	gatesMu sync.Mutex

	// gates is the authoritative gate directory: gate.ID -> gateEntry. An entry
	// exists from PrepareGateOpen (state=preparing) through ActivateGate (open)
	// and RespondGate/CloseGate (claiming -> closed/removed). ListGates returns
	// only open entries.
	gates map[gate.ID]gateEntry

	// gateAppender is the STRICT durable append seam for gate prepare/open/resolve.
	// Unlike the hub's PublishEvent (which faults and returns nil on append
	// failure), this seam returns the append error so PrepareGateOpen/ActivateGate
	// can fail closed — a failed prepare installs no directory entry, a failed
	// activate leaves the gate preparing. The nop default (nopGateAppender) keeps
	// headless/no-persistence mode unchanged; the composition root wires the real
	// journal+hub adapter via WithGateAppender.
	gateAppender gateAppender

	// gateCaps bounds the live gate directory. The cap counts preparing + open +
	// claiming so failed activations cannot accumulate invisible prepared entries.
	// Zero (the default) means no cap — the session accepts unlimited gates.
	gateCaps GateCaps

	// gateTimers holds activation-time response-policy timers keyed by GateID.
	// Protected by gatesMu alongside gates.
	gateTimers map[gate.ID]*time.Timer
}

// eventAppender is the session's narrow view of the hub's REQUIRED durable event tap:
// append one Enduring event to the durable journal, returning a typed error if it did
// not commit. The session holds it only to FORWARD it into the hub at construction
// (hub.WithAppender); the session never calls AppendEvent itself (the hub owns the
// durable tap). It mirrors the hub's own unexported eventAppender method-set, so the
// concrete journal.JournalEventAppender satisfies both structurally and the session
// never imports the journal's appender type. Defined here (where it is consumed) per
// Dependency Inversion, exactly like commandAppender.
type eventAppender interface {
	AppendEvent(ctx context.Context, ev event.Event) error
}

// loopHandle is the session's registry entry: the loop's channel handle, the
// provenance of the turn/step that spawned it (zero for the primary loop), and
// the cancel for this loop's loopCtx (a session-owned backstop).
type loopHandle struct {
	backend loop.Backend
	parent  loop.Provenance
	cancel  context.CancelFunc
}

// eventSubscriber is the consumer-facing half of the session fan-in: a TUI/CLI (or
// later a durable journal) attaches here to receive filtered events. It is defined
// where it is consumed (the session), per Dependency Inversion. *Session
// satisfies it by delegating to the hub.
type eventSubscriber interface {
	SubscribeEvents(event.EventFilter) (event.Subscription, error)
}

// Compile-time proof that *Session is the consumer-facing eventSubscriber.
// Its publisher half (PublishEvent) is asserted by loop.New accepting s as its
// eventPublisher at the NewLoop call site.
var _ eventSubscriber = (*Session)(nil)

// Compile-time proof that *Session is the hub's FaultReporter: on a required durable
// append failure the hub calls ReportFault, and the session fails secure.
var _ hub.FaultReporter = (*Session)(nil)

// ReportFault is the session's fail-secure response to a hub persistence fault (a
// required durable append failed): it latches the faulted state (so every new
// Submit/NewLoop is refused) and wakes every blocked WaitIdle waiter with the fault.
// It is the hub's FaultReporter — the hub calls it inline (outside the hub lock) when
// AppendEvent fails the durable tap. It is idempotent: the FIRST fault latches and
// records the cause; a later fault still wakes any new waiters but keeps the first
// recorded cause (the root failure). The session is NOT torn down here — restore /
// operator action owns recovery; this only stops admitting new work and unblocks
// callers stuck waiting on a session that can no longer reach idle durably.
func (s *Session) ReportFault(_ context.Context, fault *hub.SessionPersistenceFault) {
	s.loopsMu.Lock()
	if !s.faulted {
		s.faulted = true
		s.faultErr = fault
	}
	s.loopsMu.Unlock()

	// Wake blocked WaitIdle waiters with the fault (outside loopsMu — FailWaiters
	// takes the hub lock; loopsMu and the hub lock are never held together).
	s.hub.FailWaiters(fault)
}

// faultIfFaulted returns a typed SessionFaulted error (chaining the latched fault) if
// the session has faulted, else nil. It is the fail-secure gate every new-work entry
// point (Submit/submitToLoop/NewLoop) checks before admitting work. Read under
// loopsMu — the same lock ReportFault latches under.
func (s *Session) faultIfFaulted() error {
	s.loopsMu.RLock()
	defer s.loopsMu.RUnlock()
	if s.faulted {
		return &SessionError{Kind: SessionFaulted, Cause: s.faultErr}
	}
	return nil
}

// PublishEvent is the session's eventPublisher implementation passed to loop.New.
// It delegates to the hub, which fans the event out to matching subscribers and
// applies any quiescence transition the event implies. The loop depends only on
// the narrow eventPublisher interface; it never sees the hub, its subscriber set,
// or its shutdown state (Interface Segregation / least privilege).
func (s *Session) PublishEvent(ctx context.Context, ev event.Event) error {
	return s.hub.PublishEvent(ctx, ev)
}

// SubscribeEvents attaches a consumer to the session fan-in with the given filter.
// The returned subscription's Events() channel yields the filtered stream; the
// caller must Close it when done. It delegates to the hub.
func (s *Session) SubscribeEvents(filter event.EventFilter) (event.Subscription, error) {
	return s.hub.SubscribeEvents(filter)
}

// PrimaryLoopID returns the session's primary loop id — the default target for
// Submit and the loop whose live Ephemeral tokens a single-loop TUI streams.
// A whole-session subscriber builds its EventFilter from it (primary-only Ephemeral
// + all-loop Enduring). It is read-only identity, safe to call concurrently.
func (s *Session) PrimaryLoopID() uuid.UUID {
	s.loopsMu.RLock()
	defer s.loopsMu.RUnlock()
	return s.primaryLoopID
}

// WaitIdle blocks until the session is quiescent, ctx is done, or the session has
// stopped (hub.ErrSessionStopped). It is the headless caller's "is the whole
// interaction at rest?" primitive; it delegates to the hub's quiescence model.
func (s *Session) WaitIdle(ctx context.Context) error {
	return s.hub.WaitIdle(ctx)
}

// expectTurn takes a hand-back wake token for a subagent loop at spawn so its
// in-flight result cannot empty the quiescence set and fire a false idle. It is
// session-internal — loops never call it (they hold only the narrow eventPublisher);
// only the session's subagent orchestration does.
//
// TODO(Open Items A): async subagent spawn must call expectTurn(subagentLoopID)
// before the child can complete its first turn, so the {wake} token guards the
// quiescence set across the hand-back. That async-spawn orchestration is deferred;
// when it lands, NewLoop's async-spawn path is where this call wires in. Today no loop
// spawns an async subagent, so this method has no production caller yet — it is
// exercised by the round-trip and the session+hub quiescence tests.
func (s *Session) expectTurn(ctx context.Context, subagentLoopID uuid.UUID) {
	s.hub.ExpectTurn(ctx, subagentLoopID)
}

// cancelExpectTurn releases a subagent's wake token off the publish path. It is
// session-internal (loops never call it) and is NO LONGER on the SubagentResult
// hand-back path: a SubagentResult is never rejected, so its {wake} token always
// releases on the publish path via the resulting TurnStarted/TurnFoldedInto/
// InputCancelled carrying Cause.LoopID. cancelExpectTurn remains for the future
// async-spawn DISCARD path (a child spawned but abandoned before it ever hands back,
// so no event ever carries its Cause.LoopID). Today it has no production caller;
// it is exercised by the session+hub quiescence tests.
func (s *Session) cancelExpectTurn(ctx context.Context, subagentLoopID uuid.UUID) {
	s.hub.CancelExpectTurn(ctx, subagentLoopID)
}

// deliverSubagentResult is the session-owned SubagentResult hand-back: it routes a
// finished subagent's output (blocks) to its parent loop as a command.SubagentResult
// and returns only a transport error (the loop is gone, or ctx is done). It is
// FIRE-AND-FORGET: a SubagentResult is NEVER rejected, so there is no outcome to wait
// for off the publish path. The parent loop always starts (idle) or queues
// (running/shutting-down) the hand-back, and its quiescence {wake, fromLoopID} token
// is ALWAYS released on the publish path by the resulting Enduring event — a
// TurnStarted/TurnFoldedInto carrying Cause.LoopID == fromLoopID, or an
// InputCancelled (also carrying it) if the loop ends before the hand-back commits (the
// shutdown terminal's returnQueuedInbox, or an idle-time id-gen failure to start). The
// session no longer reads a disposition and no longer releases the token off the
// publish path.
//
// parentLoopID selects the parent loop's command channel — it rides as the command's
// embedded Coordinates.LoopID (the delivery target). fromLoopID is the producing
// subagent (the CHILD); it rides as Header.Cause.LoopID and is stamped onto the events
// the hand-back causes. The submit carries no per-turn stream — the parent's events flow
// to the session fan-in. ctx governs the send only (the loop derives the turn ctx from
// its own loopCtx).
func (s *Session) deliverSubagentResult(ctx context.Context, parentLoopID, fromLoopID uuid.UUID, blocks []content.Block) error {
	l, ok := s.loopFor(parentLoopID)
	if !ok {
		return &SessionError{Kind: SessionLoopNotFound}
	}
	id, err := s.newCommandID()
	if err != nil {
		return err
	}
	cmd := command.SubagentResult{
		Coordinates: identity.Coordinates{LoopID: parentLoopID}, // delivery target (PARENT)
		Header: command.Header{
			CommandID: id,
			Cause:     identity.Cause{Coordinates: identity.Coordinates{LoopID: fromLoopID}}, // CHILD (wake token)
			CreatedAt: s.stampNow(),
		},
		Blocks: blocks,
	} // Agency left default AgencyMachine — a hand-back is machine-originated
	// Intent log (audit-only): append BEFORE dispatch; an append failure is logged and
	// the hand-back proceeds. Targets the PARENT loop (the command's delivery target).
	s.appendCommand(ctx, parentLoopID, cmd)
	select {
	case l.CommandSink() <- cmd:
		return nil
	case <-l.DoneChan():
		return &SessionError{Kind: SessionLoopExited}
	case <-ctx.Done():
		return &SessionError{Kind: SessionContextDone, Cause: ctx.Err()}
	}
}

// NewLoop creates another loop inside this session. The new loop shares
// SessionID but receives its own loop id and loop goroutine. parent is the
// provenance of the spawning turn/step (zero for the primary loop); the session
// records it in the registry and passes it to loop.New. The session stores the
// loop handle and returns only the loop id, because callers route through
// session methods rather than writing to a loop command channel directly.
func (s *Session) NewLoop(parent loop.Provenance, cfg loop.Config) (uuid.UUID, error) {
	// A plain NewLoop is never spawned by a Subagent tool call, so it carries no
	// parent tool-use id; the private newLoop does the real work. RunSubagent is the
	// only path that threads a non-empty id (its child's LoopStarted correlates back
	// to the spawning tool call).
	return s.newLoop(parent, cfg, "")
}

// newLoop is the private loop-creation core behind NewLoop and RunSubagent. It is
// identical to the public NewLoop except that parentToolUseID is stamped onto the new
// loop's LoopStarted (event.LoopStarted.ParentToolUseID): the durable carrier that
// correlates a tool-spawned child loop back to its parent Subagent tool call. NewLoop
// passes ""; RunSubagent passes the provider tool-use id of the spawning call. The id
// rides as a plain parameter into the LoopStarted build only — it touches no identity /
// Provenance / Header struct, so it never perturbs the loop tree or the quota/depth math.
func (s *Session) newLoop(parent loop.Provenance, cfg loop.Config, parentToolUseID string) (uuid.UUID, error) {
	// Whether this spawn counts toward the cumulative spawn quota. The PRIMARY loop is
	// built by newSession via NewLoop with ZERO provenance (parent.LoopID zero) and must
	// NOT consume a quota slot (design §6d: "primary excluded"); every subagent spawn
	// carries a non-zero parent.LoopID. This is the SAME root/non-root discriminator the
	// durable recount uses on restore (a non-root LoopStarted has a non-zero Header.Cause),
	// so the live counter and the restored counter count exactly the same set.
	counts := !parent.LoopID.IsZero()

	// One authoritative critical section folds the closing/faulted gate, the PURE depth
	// check, and the quota RESERVATION under a single loopsMu.Lock — so the cap decisions
	// and the spawned++ reservation are atomic with respect to a concurrent NewLoop,
	// Shutdown (sets closing), or ReportFault (sets faulted). On success it reserves a
	// quota slot (spawned++) and unlocks; that reservation is ROLLED BACK by release()
	// (below) on every later failure. Defaults are resolved here (withDefaults) so the
	// caps are always positive even on a struct-literal Session that bypassed
	// newSession/restore (test seams) — a zero-limits session behaves with the production
	// defaults, never rejecting every spawn. Depth is final once it passes: it is a
	// function of the FIXED parent chain (already-registered ancestors never change), so
	// there is no later depth race to re-check.
	s.loopsMu.Lock()
	if s.faulted {
		fe := s.faultErr
		s.loopsMu.Unlock()
		return uuid.UUID{}, &SessionError{Kind: SessionFaulted, Cause: fe}
	}
	if s.closing {
		s.loopsMu.Unlock()
		return uuid.UUID{}, &SessionError{Kind: SessionClosing}
	}
	limits := s.limits.withDefaults()
	if s.depthUnderLock(parent.LoopID) >= limits.Depth {
		s.loopsMu.Unlock()
		return uuid.UUID{}, &SessionError{Kind: SessionLoopDepthExceeded}
	}
	if counts && s.spawned >= limits.Quota {
		s.loopsMu.Unlock()
		return uuid.UUID{}, &SessionError{Kind: SessionLoopQuotaExceeded}
	}
	if counts {
		s.spawned++ // reserve the quota slot; release() rolls it back on any later failure
	}
	s.loopsMu.Unlock()

	// release rolls back the quota reservation made above. It is called on EVERY failure
	// path after the reservation (id mint, loop.New, the registration-time closing/faulted
	// re-check, and publish failure) ALONGSIDE the loop's cancel(), so a spawn that does
	// not complete never permanently consumes a slot. It is a no-op when this spawn did not
	// reserve (the primary / a zero-parent spawn). A SUCCESSFUL spawn never calls it, so
	// the reservation stands permanently (loops are retained — the cumulative budget never
	// decrements on idle, only on a rolled-back spawn).
	release := func() {
		if !counts {
			return
		}
		s.loopsMu.Lock()
		s.spawned--
		s.loopsMu.Unlock()
	}

	loopID, err := s.newID()
	if err != nil {
		release()
		return uuid.UUID{}, &SessionError{Kind: SessionLoopIDGenerationFailed, Cause: err}
	}

	// Stamp the LoopStarted header (minting its EventID + CreatedAt via the Factory)
	// BEFORE building or registering the loop, so a crypto/rand failure fails NewLoop
	// cleanly (typed error) before any loop exists — we never leave a registered loop
	// behind a returned error. This is the 2nd mint of NewLoop (the loop id was the
	// 1st), so an id-gen failure here is SessionIDGenerationFailed (the loop id was
	// already minted). Coordinates/Cause are the loop-tree record the Factory
	// preserves; only EventID + CreatedAt are added.
	startedHeader, err := s.factory.Stamp(event.Header{
		Coordinates: identity.Coordinates{SessionID: s.SessionID, LoopID: loopID},
		// AgentName is the loop's immutable attribution name, stamped from its Config so
		// the durable LoopStarted records which agent drove this loop. Empty for a plain
		// loop; the primary loop carries its configured name through this same path.
		AgentName: cfg.AgentName,
		Cause: identity.Cause{
			Coordinates: identity.Coordinates{LoopID: parent.LoopID, TurnID: parent.TurnID, StepID: parent.StepID},
			Agency:      identity.AgencyMachine,
		},
	})
	if err != nil {
		release()
		return uuid.UUID{}, &SessionError{Kind: SessionIDGenerationFailed, Cause: err}
	}

	// Engine switch — the single loop-construction chokepoint for BOTH the primary loop
	// (built via newSession → NewLoop with zero provenance) and every subagent. A native
	// cfg.Engine (the zero value) builds through loop.New exactly as before. A foreign
	// cfg.Engine routes to the injected foreign Builder seam and fails CLOSED if none is
	// wired (a foreign engine must never silently resolve to a native loop). The minted
	// foreign sid the builder returns is stamped onto the published LoopStarted below;
	// it is "" for native (omitzero drops it). Every failure path rolls back the quota
	// reservation (release()) and cancels the loopCtx, exactly like the surrounding code.
	loopCtx, cancel := context.WithCancel(s.sessionCtx)
	var b loop.Backend
	var foreignSID string
	switch cfg.Engine {
	case loop.EngineNative:
		b, err = loop.New(loopCtx, s.SessionID, loopID, parent, s, cfg)
	default:
		if s.foreignBuild == nil {
			release()
			cancel()
			return uuid.UUID{}, &SessionError{Kind: SessionForeignBuilderMissing}
		}
		b, foreignSID, err = s.foreignBuild(loopCtx, s.SessionID, loopID, parent, s, cfg,
			func() (uuid.UUID, error) { return s.newID() }, s.factory)
	}
	if err != nil {
		release()
		cancel()
		return uuid.UUID{}, err
	}

	s.loopsMu.Lock()
	// Authoritative fail-secure check: re-test faulted AND closing under the SAME
	// lock acquire that registers the loop. Shutdown sets closing and ReportFault
	// sets faulted under this lock, so checking-then-registering atomically here
	// guarantees a loop is never registered after a fault or after Shutdown's
	// snapshot — it either makes it into the snapshot (registered before the latch)
	// or is refused here. On refusal: register nothing, release the lock, roll back the
	// quota reservation (release()), tear down the already-built loop with cancel(), and
	// return before the publish so no LoopStarted is ever emitted. Faulted is checked first
	// (the stronger condition). release() takes loopsMu, so it is called AFTER Unlock (the
	// mutex is not reentrant).
	if s.faulted {
		fe := s.faultErr
		s.loopsMu.Unlock()
		release()
		cancel()
		return uuid.UUID{}, &SessionError{Kind: SessionFaulted, Cause: fe}
	}
	if s.closing {
		s.loopsMu.Unlock()
		release()
		cancel()
		return uuid.UUID{}, &SessionError{Kind: SessionClosing}
	}
	s.loops[loopID] = &loopHandle{backend: b, parent: parent, cancel: cancel}
	s.loopsMu.Unlock()

	// Announce the new loop to subscribers active at creation time. Published AFTER
	// releasing loopsMu — never under the registry lock — because a hub publish fans
	// out and must not hold the registry lock. LoopStarted is a pure announcement: it
	// is not one of the active-mutating events (TurnStarted/LoopIdle/TurnFoldedInto/
	// InputCancelled), so it never perturbs session quiescence. Header.Coordinates is
	// the NEW loop (SessionID+LoopID; Turn/Step zero); Header.Cause is the spawning
	// loop/turn/step (zero for the primary = root), machine-originated. There is no
	// ctx param, so it publishes on the session lifetime (s.sessionCtx). The header
	// (Coordinates/Cause + minted EventID/CreatedAt) was stamped above before the loop
	// was built.
	ev := event.LoopStarted{Header: startedHeader, ParentToolUseID: parentToolUseID, ForeignSID: foreignSID}
	if err := s.PublishEvent(s.sessionCtx, ev); err != nil {
		// Mirror New's cleanup-on-publish-failure: the loop is already registered and
		// its loopCtx cancel is live, so a bare return would leak a cancel-orphaned
		// loop. Unregister it, roll back the quota reservation (release()), run its cancel,
		// then surface the typed error. (hub PublishEvent returns nil today, so this path is
		// presently unreachable — but it is correct-and-safe rather than dead-and-unsafe if
		// that ever changes.)
		s.loopsMu.Lock()
		delete(s.loops, loopID)
		s.loopsMu.Unlock()
		release()
		cancel()
		return uuid.UUID{}, &SessionError{Kind: SessionContextDone, Cause: err}
	}
	return loopID, nil
}

// depthUnderLock returns the ancestor-chain length a NEW loop spawned under parentLoopID
// would have: it walks parentLoopID up the registry's parent links (loopHandle.parent.LoopID)
// and counts each registered ancestor. A zero parentLoopID (a root spawn — the primary,
// built by New) has no ancestors and returns 0; a child of the primary returns 1, its child
// 2, and so on. NewLoop rejects when this count >= limits.Depth, so the deepest spawnable
// loop has an ancestor chain of limits.Depth-1.
//
// It is a PURE read over the FIXED parent chain (ancestors never change), so the caller's
// depth decision needs no later re-check. The caller MUST hold loopsMu (the registry read
// must be coherent); the cycle guard (capping the walk at len(s.loops)) is defensive — the
// registry is a tree (each loop has at most one parent, set once at creation), so a cycle
// is impossible, but an unbounded walk must never be possible from a corrupt registry.
func (s *Session) depthUnderLock(parentLoopID uuid.UUID) int {
	depth := 0
	cur := parentLoopID
	max := len(s.loops)
	for !cur.IsZero() && depth <= max {
		h, ok := s.loops[cur]
		if !ok {
			break
		}
		depth++
		cur = h.parent.LoopID
	}
	return depth
}

// loopFor returns the loop's channel handle for command routing. The registry
// stores *loopHandle; this derefs to the handle's loop. The parent provenance is
// read only by future tree walks, which read s.loops directly.
func (s *Session) loopFor(loopID uuid.UUID) (loop.Backend, bool) {
	s.loopsMu.RLock()
	defer s.loopsMu.RUnlock()
	h, ok := s.loops[loopID]
	if !ok {
		return nil, false
	}
	return h.backend, true
}

// newCommandID mints a fresh correlation ID for a command Header. Any
// crypto/rand failure is mapped onto the session's typed error path rather than
// swallowed, so callers never send an unidentifiable (zero-ID) command.
func (s *Session) newCommandID() (uuid.UUID, error) {
	id, err := s.newID()
	if err != nil {
		return uuid.UUID{}, &SessionError{Kind: SessionIDGenerationFailed, Cause: err}
	}
	return id, nil
}

// New constructs a Session and starts its primary loop's actor
// goroutine. It owns the session fan-in hub and emits the session-scoped
// SessionStarted through it.
//
// This SessionStarted (the s.hub.PublishEvent below) is the SOLE SessionStarted:
// the session publishes it through the HUB to its SUBSCRIBERS (TUI/CLI fan-in),
// and the loop never emits one. It is published before any subscriber attaches,
// so a subscriber that connects later does not observe it; reliable delivery of
// the session start to late subscribers is a separate future follow-on.
func New(ctx context.Context, cfg loop.Config, opts ...Option) (*Session, error) {
	// Production seams: crypto/rand id-gen + the wall clock. newSession is the
	// unexported core that lets a same-package test inject a failing newID (or a
	// pinned now) that is IN EFFECT during the construction-time SessionStarted
	// stamp — the only way to exercise New's mint-error failure branch — mirroring
	// how the loop injects idGen/now via Config before loop.New. opts are the optional
	// dependency injections (e.g. WithCommandAppender) the composition root supplies.
	return newSession(ctx, cfg, uuid.New, time.Now, opts...)
}

// newSession is the construction core of New with the id-gen and clock seams made
// explicit. New calls it with the production defaults (uuid.New, time.Now); a
// same-package test calls it with a failing newID to drive the SessionStarted
// mint-error branch (no zero-EventID SessionStarted is ever published; New returns
// nil + a typed *SessionError). newID also mints the session id itself, so a
// generator that fails on its FIRST call aborts before any event is stamped.
func newSession(ctx context.Context, cfg loop.Config, newID idGenerator, now event.Clock, opts ...Option) (*Session, error) {
	select {
	case <-ctx.Done():
		return nil, &SessionError{Kind: SessionContextDone, Cause: ctx.Err()}
	default:
	}

	// Resolve the composition-root options on a probe BEFORE minting the session id, so
	// WithSessionID can be consulted to adopt an externally-minted id (the journal
	// chicken-and-egg) instead of minting one here. The same opts are re-applied to the
	// real session below so the other injections (WithCommandAppender/WithEventAppender)
	// take effect on it. A probe (not the real session) is used because the id and the
	// hub must be built from the resolved values, before the struct is finalized.
	probe := &Session{}
	for _, opt := range opts {
		opt(probe)
	}

	// SessionID: adopt the externally-minted id when WithSessionID supplied a non-zero
	// one (so the composition root's journal and this session share the same id); else
	// mint one. The id-gen path is preserved for the no-injection case so a crypto/rand
	// failure still fails New closed.
	id := probe.injectedSessionID
	if id.IsZero() {
		minted, err := newID()
		if err != nil {
			return nil, &SessionError{Kind: SessionIDGenerationFailed, Cause: err}
		}
		id = minted
	}

	sessionCtx, sessionCancel := context.WithCancel(ctx)
	s := &Session{
		SessionID:     id,
		sessionCtx:    sessionCtx,
		sessionCancel: sessionCancel,
		loops:         make(map[uuid.UUID]*loopHandle),
		newID:         newID,
		now:           now,
		// Audit-only intent-log appender: nop by default (no-persistence/headless mode).
		// The composition root (Phase 10) overrides it via WithCommandAppender below.
		cmdAppender: nopCommandAppender{},
		// Gate directory: nop appender + empty directory by default; the composition
		// root wires the real journal+hub adapter via WithGateAppender.
		gates:        map[gate.ID]gateEntry{},
		gateTimers:   map[gate.ID]*time.Timer{},
		gateAppender: nopGateAppender{},
	}
	// Apply optional dependency injections (e.g. WithCommandAppender, WithEventAppender)
	// before any command can be dispatched or the hub is built, so an injected appender is
	// in effect from the first dispatch/publish. A nil appender option is ignored (the nop
	// default stays installed). WithSessionID is a no-op here (already consumed above).
	for _, opt := range opts {
		opt(s)
	}
	// Default-mint the security-ceiling source when the composition root did not inject
	// one via WithCeiling, so SetSecurityCeiling/CeilingSource are always safe (never a
	// nil-deref). A fresh session starts at the fail-secure most-restrictive ordinal (0)
	// until a SetSecurityCeiling command changes it.
	if s.ceiling == nil {
		s.ceiling = ceiling.New()
	}
	// Apply the spawn-cap defaults AFTER the options so an unset (or WithLimits-supplied)
	// Limits resolves to positive caps before the first NewLoop — a zero or negative
	// configured value never silently disables the depth/quota backstop.
	s.limits = s.limits.withDefaults()
	// The Factory mints from closures over the LIVE newID + now fields, so a test
	// that swaps either after construction pins the stamp too (the same seam the
	// session's command-id minting uses).
	s.factory = event.NewFactory(func() (uuid.UUID, error) { return s.newID() }, func() time.Time { return s.now() })

	// The hub is built AFTER s so the session can wire itself as the hub's
	// FaultReporter (fail-secure: on a required-durable-append failure the hub calls
	// s.ReportFault). The hub shares the session's Factory so a hub-synthesized
	// session event (SessionActive/Idle/Stopped) is stamped from the same pinned
	// newID/now seam as the session's own events. The durable event tap is the injected
	// appender (WithEventAppender) when the composition root supplied one — wrapping the
	// SessionJournal so every Enduring event is appended before fan-out — else the hub's
	// nop default (headless/no-persistence). A nil appender is passed through to
	// hub.WithAppender, which ignores it (the nop default stays), so the no-injection
	// path is unchanged.
	hubOpts := []hub.Option{hub.WithFactory(s.factory), hub.WithFaultReporter(s)}
	if s.injectedEventAppender != nil {
		hubOpts = append(hubOpts, hub.WithAppender(s.injectedEventAppender))
	}
	s.hub = hub.New(id, hubOpts...)

	// SessionStarted is an Enduring, session-scoped event: stamp it with a minted
	// EventID + CreatedAt so the journal sees a stable idempotency key and creation
	// time. A crypto/rand failure aborts construction (fail-secure) rather than
	// publishing a zero-EventID SessionStarted.
	startedHeader, err := s.factory.Stamp(event.Header{Coordinates: identity.Coordinates{SessionID: id}})
	if err != nil {
		sessionCancel()
		return nil, &SessionError{Kind: SessionIDGenerationFailed, Cause: err}
	}

	// The hub is built before any loop, so a loop publishing through the session's
	// PublishEvent never sees a nil hub. With no subscribers yet, this delivers to
	// nobody (a no-op), but it is the session's authoritative session-scoped start.
	// Config is the fingerprint of the agent configuration this session started
	// under, stamped here so a durable journal can detect a config change on restore.
	// It merges the loop-derived fingerprint with the swarm-level fields the composition
	// root injected (AgentKind/RuntimeSkills/WorkspaceRoot), via the SAME fingerprintWith
	// the restore comparison uses, so the stamped and compared-against fingerprints match.
	if err := s.hub.PublishEvent(sessionCtx, event.SessionStarted{Header: startedHeader, Config: fingerprintWith(cfg, s.configFingerprintFields)}); err != nil {
		sessionCancel()
		return nil, &SessionError{Kind: SessionContextDone, Cause: err}
	}

	primaryLoopID, err := s.NewLoop(loop.Provenance{}, cfg)
	if err != nil {
		sessionCancel()
		return nil, err
	}
	s.primaryLoopID = primaryLoopID
	return s, nil
}

// interruptLoop sends a best-effort Interrupt to the loop to cancel its active
// turn, escaping on the loop's Done so a stopped loop never wedges the send. It is
// the loop-targeted cancel primitive interruptLoopID delegates to (the subagent
// drain's ctx-cancel fail-safe). The ack is buffered(1) and unread here: the
// caller observes the cancellation through the resulting TurnInterrupted terminal,
// not this command's reply. An id-gen failure is swallowed (best-effort): the worst
// case is the turn runs to its natural terminal instead of being interrupted.
//
// loopID is the dispatch target, passed in for the intent-log append (the Interrupt
// itself carries no addressing); it is appended audit-only before the send on the
// session lifetime ctx (interruptLoop has no ctx of its own — it is best-effort).
func (s *Session) interruptLoop(loopID uuid.UUID, l loop.Backend) {
	id, err := s.newID()
	if err != nil {
		return
	}
	ack := make(chan bool, 1)
	cmd := command.Interrupt{Header: command.Header{CommandID: id, CreatedAt: s.stampNow()}, Ack: ack}
	// Intent log (audit-only): append before dispatch; failure is logged and the
	// best-effort interrupt proceeds.
	s.appendCommand(s.sessionCtx, loopID, cmd)
	select {
	case l.CommandSink() <- cmd:
	case <-l.DoneChan():
	}
}

// interruptLoopID interrupts a SPECIFIC loop's active turn by id (machine-originated,
// fire-and-forget). It resolves loopID then delegates to interruptLoop. Returns
// SessionLoopNotFound if no such loop is registered. It is the per-loop lever the
// subagent drain uses as a fail-safe (drainToFinalText, later task), and the
// loop-targeted counterpart to the distributed Session.Interrupt.
//
// There is deliberately no ctx parameter: interruptLoop is best-effort and already
// escapes on the loop's Done, and the drain calls this on its own ctx.Done(), so a
// ctx here would arrive already-cancelled and useless. Like interruptLoop the send
// stays Agency=AgencyMachine (the zero value) — a programmatic per-loop interrupt is
// a machine action, never falsely attributed to a human.
func (s *Session) interruptLoopID(loopID uuid.UUID) error {
	l, ok := s.loopFor(loopID)
	if !ok {
		return &SessionError{Kind: SessionLoopNotFound}
	}
	s.interruptLoop(loopID, l)
	return nil
}

// Submit is the HUMAN-ONLY submit entry point: it stamps Agency=AgencyUser (a
// person authored this input). Programmatic/machine callers go through
// submitToLoop with Agency=AgencyMachine (the subagent path).
//
// Submit sends input as a queueable UserInput to the primary loop,
// FIRE-AND-FORGET: it returns the InputID (the submit command's id, == the
// Cause.CommandID on the resulting Reply events) and a transport error only if the
// command could not be handed to the loop. The outcome — InputQueued /
// TurnStarted / TurnFoldedInto / TurnRejected / InputCancelled — is observed on
// the event fan-in (each Reply carries Cause.CommandID == this returned id), NOT
// returned here.
//
// A submit while a turn is running QUEUES rather than rejecting; a submit while idle
// starts a turn. Submit never reads a reply, so it returns the instant the command
// is accepted by the loop.
//
// The send carries the standard escapes: ctx.Done() →
// SessionContextDone, the loop's Done → SessionLoopExited, and a missing primary
// loop → SessionLoopNotFound. On any of those the returned id is the zero UUID,
// because nothing was sent and there is no correlation to hand back.
func (s *Session) Submit(ctx context.Context, input []content.Block) (uuid.UUID, error) {
	// Submit IS the primary-loop, human-authored (AgencyUser) case of submitToLoop:
	// the interactive submit targets the primary loop and stamps user agency. The
	// loop-targeted core (a sub-loop, machine agency) is the subagent path.
	return s.submitToLoop(ctx, s.primaryLoopID, input, identity.AgencyUser)
}

// submitToLoop submits a UserInput to a SPECIFIC loop with the given Agency,
// returning the minted CommandID (correlate Reply events via Cause.CommandID).
// It is the loop-targeted core of Submit: public Submit is the primary-loop,
// AgencyUser case; the subagent path targets a sub-loop with AgencyMachine.
//
// Like Submit it is FIRE-AND-FORGET: the outcome —
// InputQueued / TurnStarted / TurnFoldedInto / TurnRejected / InputCancelled — is
// observed on the event fan-in (each Reply carries Cause.CommandID == the returned
// id), not returned here. The send carries the same escapes: ctx.Done() →
// SessionContextDone, the loop's Done → SessionLoopExited, and an unknown loop id →
// SessionLoopNotFound. On any of those the returned id is the zero UUID, because
// nothing was sent and there is no correlation to hand back.
func (s *Session) submitToLoop(ctx context.Context, loopID uuid.UUID, blocks []content.Block, agency identity.Agency) (uuid.UUID, error) {
	// Fail-secure: a faulted session (a required durable append failed) admits no new
	// work. Checked before any loop lookup or id mint so nothing is sent.
	if err := s.faultIfFaulted(); err != nil {
		return uuid.UUID{}, err
	}
	l, ok := s.loopFor(loopID)
	if !ok {
		return uuid.UUID{}, &SessionError{Kind: SessionLoopNotFound}
	}
	id, err := s.newCommandID()
	if err != nil {
		return uuid.UUID{}, err
	}
	// Queueable submit: Cause.CommandID is zero (root); the outcome is observed on the
	// session fan-in. Agency is caller-chosen — AgencyUser for the interactive human
	// Submit, AgencyMachine for the subagent task submit — so a machine path never
	// claims user agency.
	cmd := command.UserInput{Header: command.Header{CommandID: id, Agency: agency, CreatedAt: s.stampNow()}, Blocks: blocks}
	// Intent log (audit-only): append BEFORE dispatch; an append failure is logged and
	// the submit proceeds (a lost record must never block the user's input).
	s.appendCommand(ctx, loopID, cmd)
	select {
	case l.CommandSink() <- cmd:
		return id, nil
	case <-ctx.Done():
		return uuid.UUID{}, &SessionError{Kind: SessionContextDone, Cause: ctx.Err()}
	case <-l.DoneChan():
		return uuid.UUID{}, &SessionError{Kind: SessionLoopExited}
	}
}

// RunSubagent creates an in-session sub-loop, runs one turn on it for the given
// blocks (machine-originated), and returns the sub-loop's final assistant text. It
// is the SOLE exported entry point of the subagent composition: it wires together
// the unexported building blocks (NewLoop + SubscribeEvents + submitToLoop +
// drainToFinalText + interruptLoopID) so the Subagent tool's injected capability
// (a later task) has one method to call and the blocks stay package-private.
//
// cfg is the sub-loop's loop.Config — the CALLER builds a FRESH cfg per call (its
// own ToolSet/PermissionChecker) so each sub-loop has independent approval state;
// RunSubagent never reuses a shared ToolSet. parent is the spawning loop/turn/step
// provenance (recorded on the sub-loop's registry entry and stamped on its
// LoopStarted). The submit is stamped Agency=AgencyMachine — a subagent turn is a
// machine action, never falsely attributed to a human.
//
// The sub-loop PERSISTS idle after the turn (loops are never deleted, design §8):
// RunSubagent closes only the SUBSCRIPTION, never the loop. The ordering is
// load-bearing: it subscribes (scoped to the new sub-loop) BEFORE submitting, so the
// opening TurnStarted the drain correlates on cannot be missed — the hub has no
// replay (design §4).
//
// Errors propagate from the first block that fails: NewLoop (e.g.
// SessionClosing while shutting down, or id-gen failure), SubscribeEvents, the
// submit, or the drain's typed §5 failures (*drainFailedError / *TurnRejectedError /
// *drainInterruptedError / *drainLostError). ctx is the calling turn's context;
// because submits carry no ctx, a ctx cancel cannot reach the sub-loop's turn — the
// drain translates it into a single loop-targeted Interrupt (the closure below) and
// drains to the resulting TurnInterrupted terminal.
func (s *Session) RunSubagent(ctx context.Context, parent loop.Provenance, cfg loop.Config, blocks []content.Block, parentToolUseID string) (string, error) {
	// newLoop publishes LoopStarted and fails SessionClosing if the session is
	// shutting down; either way no sub-loop is left behind a returned error.
	// parentToolUseID is stamped onto that LoopStarted so the sub-loop correlates back
	// to the spawning Subagent tool call across persist/restore.
	subLoopID, err := s.newLoop(parent, cfg, parentToolUseID)
	if err != nil {
		return "", err
	}

	// Subscribe BEFORE submitting (design §4): the hub has no replay, so a
	// subscription created after the submit could miss the opening TurnStarted and the
	// drain would then block until ctx-cancel or subscription loss. The filter is
	// scoped to JUST this sub-loop (Enduring carries StepDone + terminals the drain
	// needs); Ephemeral is left empty so the sub-loop's token firehose never enters
	// this subscription's egress buffer.
	sub, err := s.SubscribeEvents(event.EventFilter{
		Enduring: event.LoopScope{Loops: map[uuid.UUID]struct{}{subLoopID: {}}},
	})
	if err != nil {
		return "", err
	}
	// Close the SUBSCRIPTION (not the sub-loop — it persists idle, design §8).
	// EventSubscription.Close is documented to always return nil (idempotent,
	// records no error), so there is nothing to handle; this mirrors every other
	// sub.Close() call site in the package.
	defer func() { _ = sub.Close() }()

	// Machine-originated submit: a subagent turn is our code's action, so it stamps
	// AgencyMachine (the submitToLoop core, not the human-only Submit).
	cmdID, err := s.submitToLoop(ctx, subLoopID, blocks, identity.AgencyMachine)
	if err != nil {
		return "", err
	}

	// Drain to the sub-loop's terminal. The interrupt closure is the drain's
	// ctx-cancel fail-safe: it is loop-targeted to THIS sub-loop. interruptLoopID only
	// errors SessionLoopNotFound, which is impossible for a sub-loop we just created
	// and never delete; so a non-nil error here is best-effort-logged (never swallowed
	// with _) rather than failing the drain.
	return drainToFinalText(ctx, sub, cmdID, func() {
		if ierr := s.interruptLoopID(subLoopID); ierr != nil {
			slog.WarnContext(ctx, "subagent interrupt failed", "loop", subLoopID, "err", ierr)
		}
	})
}

// loopSnapshot pairs a loop id with its handle, captured under loopsMu by the
// Interrupt/Shutdown fan-outs. The loop id is carried alongside the handle (the map
// key is otherwise lost on snapshot) so the per-loop intent-log append can target the
// right loop's command subject — Interrupt/Shutdown carry no addressing of their own.
type loopSnapshot struct {
	loopID uuid.UUID
	handle *loopHandle
}

// interruptTarget pairs a loop with the Ack channel of the command.Interrupt the
// session sent it, so the ack-wait phase can drain each loop's reply in turn. It
// is Interrupt-internal: the send phase records one per loop actually reached, and
// the wait phase reads them back. The Ack is chan bool (cancelled?), distinct from
// shutdownTarget's chan error (graceful-exit error).
type interruptTarget struct {
	loop loop.Backend
	ack  chan bool
}

// Interrupt is the human "stop everything": it cancels the running turn of EVERY
// loop in the session — the primary AND every sub-loop — not just the primary.
// Sub-loops each run their own actor and turn, so a single human interrupt must
// fan out to all of them. Idle loops (or loops already shutting down) ack false
// and are harmless; Interrupt returns true iff ANY loop reported it cancelled a
// running turn. ctx bounds the whole fan-out so a slow actor cannot wedge it.
//
// Unlike Shutdown, Interrupt does NOT latch closing and does NOT tear loops down:
// it only cancels in-flight turns. A loop created concurrently with an Interrupt
// is simply not in the snapshot and so is not interrupted — acceptable, because a
// brand-new loop has no turn to cancel.
//
// The structure mirrors Shutdown's: snapshot the loops under loopsMu, send one
// Interrupt per loop recording each reached loop's (loop, ack), then wait every
// recorded ack and aggregate. Per-loop id-gen failure SKIPS that loop (best-effort,
// consistent with Shutdown — one loop's failure never aborts the whole Interrupt).
func (s *Session) Interrupt(ctx context.Context) (bool, error) {
	// Snapshot the loops under loopsMu (no closing latch — Interrupt does not tear
	// loops down). The snapshot is the set we fan the Interrupt out to; it carries the
	// loop id so the per-loop intent-log append can target each loop's command subject.
	s.loopsMu.RLock()
	snapshot := make([]loopSnapshot, 0, len(s.loops))
	for lid, h := range s.loops {
		snapshot = append(snapshot, loopSnapshot{loopID: lid, handle: h})
	}
	s.loopsMu.RUnlock()

	// Send an Interrupt to every loop in the snapshot, recording each reached loop's
	// (loop, ack) pair.
	targets := make([]interruptTarget, 0, len(snapshot))
	for _, ls := range snapshot {
		id, err := s.newCommandID()
		if err != nil {
			// id-gen failure for ONE loop must not abort the whole Interrupt: skip its
			// interrupt rather than failing the human's "stop everything". The worst
			// case is that loop's turn runs to its natural terminal.
			continue
		}
		// A manual Interrupt is a human-origination point (the human pressed interrupt),
		// so it stamps Agency=AgencyUser. The programmatic per-loop interrupt
		// (interruptLoop, the subagent drain's ctx-cancel fail-safe) is a SEPARATE method
		// and stays machine — we never falsely attribute that machine action to a user.
		ack := make(chan bool, 1)
		cmd := command.Interrupt{Header: command.Header{CommandID: id, Agency: identity.AgencyUser, CreatedAt: s.stampNow()}, Ack: ack}
		// Intent log (audit-only): one record per loop (the command is per-loop), appended
		// BEFORE this loop's send; an append failure is logged and the fan-out proceeds.
		s.appendCommand(ctx, ls.loopID, cmd)
		select {
		case ls.handle.backend.CommandSink() <- cmd:
			targets = append(targets, interruptTarget{loop: ls.handle.backend, ack: ack})
		case <-ls.handle.backend.DoneChan():
			// Loop already exited; nothing to cancel.
		case <-ctx.Done():
			return false, &SessionError{Kind: SessionContextDone, Cause: ctx.Err()}
		}
	}

	// Wait for each reached loop's ack, aggregating: Interrupt returns true iff ANY
	// loop reported it cancelled a running turn.
	var any bool
	for _, t := range targets {
		select {
		case cancelled := <-t.ack:
			any = any || cancelled
		case <-t.loop.DoneChan():
			// Actor exited without (or before we read) an ack; nothing was cancelled
			// by us — leave any unchanged.
		case <-ctx.Done():
			return false, &SessionError{Kind: SessionContextDone, Cause: ctx.Err()}
		}
	}
	return any, nil
}

// releaseLease invokes the lease-release hook EXACTLY ONCE (releaseOnce) on the bounded
// ctx, swallowing the error (the bucket TTL is the backstop and Shutdown's own error is
// the caller-facing one). It is nil-safe: a headless session (no WithLeaseRelease, no
// Restore-installed releaser) has no hook and this is a no-op. Idempotent so a second
// Shutdown never double-releases.
func (s *Session) releaseLease(ctx context.Context) {
	s.releaseOnce.Do(func() {
		if s.leaseRelease == nil {
			return
		}
		if err := s.leaseRelease(ctx); err != nil {
			slog.WarnContext(ctx, "session: lease release on shutdown failed (TTL is the backstop)",
				"session", s.SessionID, "err", err)
		}
	})
}

// shutdownTarget pairs a loop with the Ack channel of the command.Shutdown the
// session sent it, so the ack-wait phase can drain each loop's reply in turn. It
// is Shutdown-internal: the send phase records one per loop actually reached, and
// the wait phase reads them back.
type shutdownTarget struct {
	loop loop.Backend
	ack  chan error
}

// Shutdown drives the WHOLE session to its stopped phase and blocks until every
// loop's actor exits. Sub-loops are retained (each runs its own goroutine and
// loopCtx), so Shutdown must reach EVERY loop, not just the primary. The order is
// deliberate:
//
//  1. Latch closing AND snapshot the loops in ONE loopsMu critical section. This
//     is the atomicity NewLoop's registration check pairs with (it re-tests
//     closing under the same lock): a loop is either already in this snapshot or
//     refused by NewLoop — never registered after the snapshot is taken.
//  2. THEN hub.StopSession — flip the session phase to SessionStopped, wake every
//     WaitIdle waiter with ErrSessionStopped, and deliver SessionStopped to
//     subscribers. After the snapshot, before the sends, so shutdown-induced loop
//     terminals are published but no longer mutate quiescence (post-stop publishes
//     never derive SessionIdle).
//  3. defer sessionCancel as the FINAL backstop on ALL paths (graceful waits, an
//     id-gen-skipped loop, or a ctx timeout) — it releases every loopCtx derived
//     from sessionCtx. It is deferred (not called before the graceful waits) so it
//     never hard-cancels loops mid-shutdown; it runs last, on every return.
//  4. Send command.Shutdown to EVERY loop in the snapshot, recording each reached
//     loop's (loop, ack) pair. Per loop: mint a CommandID; on id-gen failure SKIP
//     that loop's graceful shutdown (the deferred sessionCancel hard-cancels it)
//     rather than aborting the whole Shutdown. The send keeps Done/ctx escapes so
//     an unbuffered send can never wedge; a loop already exited (Done) is skipped.
//  5. Wait for every recorded ack, aggregating the first non-nil error (a loop's
//     root ctx cancelled before cleanup finished), bounded by ctx and each loop's
//     Done.
//
// Calling Shutdown twice is safe: StopSession is idempotent, closing already true
// is fine, and loops that already exited hit the <-Done cases.
func (s *Session) Shutdown(ctx context.Context) error {
	// (1) Latch closing and snapshot the loops atomically under loopsMu — the same
	// lock NewLoop's registration check re-tests closing under, so no loop can be
	// registered after this snapshot is taken.
	s.loopsMu.Lock()
	s.closing = true
	snapshot := make([]loopSnapshot, 0, len(s.loops))
	for lid, h := range s.loops {
		snapshot = append(snapshot, loopSnapshot{loopID: lid, handle: h})
	}
	s.loopsMu.Unlock()

	// (2) Flip the session phase to stopped after the snapshot, before the sends.
	s.hub.StopSession(ctx)

	// (3) Final backstop on every path: released last, after the graceful waits or
	// on a ctx timeout. Deferred so it never hard-cancels loops mid-shutdown.
	defer s.sessionCancel()

	// Release the single-writer lease on every Shutdown return path, ONCE — deferred
	// AFTER sessionCancel so it runs BEFORE it (LIFO): the graceful waits have completed
	// (the journal's last append is durable) before ownership is relinquished, and the
	// release happens before the root context is cancelled. Nil in headless mode (no-op).
	defer s.releaseLease(ctx)

	// (4) Send a graceful Shutdown to every loop in the snapshot.
	targets := make([]shutdownTarget, 0, len(snapshot))
	for _, ls := range snapshot {
		id, err := s.newCommandID()
		if err != nil {
			// id-gen failure for ONE loop must not abort the whole Shutdown: skip
			// its graceful shutdown; the deferred sessionCancel hard-cancels it.
			continue
		}
		ack := make(chan error, 1)
		cmd := command.Shutdown{Header: command.Header{CommandID: id, CreatedAt: s.stampNow()}, Ack: ack}
		// Intent log (audit-only): one record per loop (the command is per-loop), appended
		// BEFORE this loop's send; an append failure is logged and the fan-out proceeds. This
		// is the shutdown-aware append: a typed lease-lost failure here is expected (ownership
		// is relinquished during teardown) and logged below error, not as a false alarm.
		s.appendShutdownCommand(ctx, ls.loopID, cmd)
		select {
		case ls.handle.backend.CommandSink() <- cmd:
			targets = append(targets, shutdownTarget{loop: ls.handle.backend, ack: ack})
		case <-ls.handle.backend.DoneChan():
			// Loop already exited; nothing to wait for.
		case <-ctx.Done():
			return &SessionError{Kind: SessionContextDone, Cause: ctx.Err()}
		}
	}

	// (5) Wait for each reached loop's ack, aggregating the first non-nil error.
	var firstErr error
	for _, t := range targets {
		select {
		case e := <-t.ack:
			// e is non-nil when that loop's root ctx was cancelled before the actor
			// finished cleanup. Keep the first such error to report.
			if e != nil && firstErr == nil {
				firstErr = e
			}
		case <-t.loop.DoneChan():
			// Actor exited without (or before we read) an ack; nothing to wait for.
		case <-ctx.Done():
			return &SessionError{Kind: SessionContextDone, Cause: ctx.Err()}
		}
	}

	if firstErr != nil {
		return &SessionError{Kind: SessionContextDone, Cause: firstErr}
	}
	return nil
}

// routeGate sends a fire-and-route gate command to the resolved target loop. These
// commands carry no Ack, so routeGate returns nil as soon as the send completes
// and never waits for a reply. It selects on ctx.Done() and the loop's Done
// channel alongside the unbuffered send so the call can never block forever when
// the actor is busy (ctx times out) or has already exited (Done is closed).
//
// loopID is the resolved dispatch target (route.LoopID), passed in for the intent-log
// append — appended audit-only BEFORE the send; an append failure is logged and the
// gate reply proceeds (a lost record must never block a human's approval/denial).
func (s *Session) routeGate(ctx context.Context, loopID uuid.UUID, l loop.Backend, cmd command.Command) error {
	s.appendCommand(ctx, loopID, cmd)
	select {
	case l.CommandSink() <- cmd:
		return nil
	case <-l.DoneChan():
		return &SessionError{Kind: SessionLoopExited}
	case <-ctx.Done():
		return &SessionError{Kind: SessionContextDone, Cause: ctx.Err()}
	}
}
