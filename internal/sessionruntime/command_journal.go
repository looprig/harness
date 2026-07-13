package sessionruntime

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/ceiling"
	"github.com/looprig/harness/pkg/command"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/foreignloop"
	"github.com/looprig/harness/pkg/journal"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/workspacestore"
)

// commandAppender is the session's narrow durable-write seam for the INTENT LOG:
// append one command (the session dispatched to a loop) to the session's durable
// journal. The session depends only on this one method (Interface Segregation) —
// never on the full SessionJournal, its stream management, or the record codec. The
// composition root (Phase 10) wires a real adapter over SessionJournal
// (journal.JournalCommandAppender); the default is the nop appender so existing tests
// and headless/no-persistence mode are unchanged.
//
// Ordinary interactive/control commands use this seam as AUDIT-ONLY: append errors are
// logged and dispatch proceeds. Machine NoFold delegate requests are the deliberate
// exception: their intent record is restore state, so append failure is propagated and
// dispatch is refused. The same narrow seam supports both policies at their call sites.
type commandAppender interface {
	AppendCommand(ctx context.Context, rec journal.CommandRecord) error
}

// nopCommandAppender is the default appender wired into a session built without an
// injected one. It persists nothing and never fails, so the audit-only append path is
// a pure no-op in no-persistence mode — every command is dispatched exactly as before
// the intent log landed. Headless runs and existing tests use this.
type nopCommandAppender struct{}

func (nopCommandAppender) AppendCommand(context.Context, journal.CommandRecord) error { return nil }

// Option configures an optional session dependency at construction. The bare
// New(ctx, cfg) installs the nop command appender; an Option overrides it. This mirrors
// the hub's Option pattern so the composition root injects the durable intent-log
// appender (Phase 10) without New growing a positional parameter.
type Option func(*Session)

// WithCommandAppender injects the intent-log appender (the composition
// root's adapter over SessionJournal). A nil appender is ignored (the nop default stays
// installed) so a caller can never accidentally null out the field and nil-deref the
// dispatch path.
func WithCommandAppender(a commandAppender) Option {
	return func(s *Session) {
		if a != nil {
			s.cmdAppender = a
		}
	}
}

// WithCeiling injects the session's SECURITY-CEILING source — the SAME *ceiling.State the
// composition root wires into the permission checker via tools.WithCeilingPostures.
// Sharing ONE state is what makes SetSecurityCeiling's clamp visible to the checker on the
// next Check (SPEC §8). Without this option the session mints its own internal state
// (SetSecurityCeiling still journals/applies/emits, but no checker observes it). A nil
// state is ignored (the default internal state stays), so a wiring slip can never null the
// field. It applies to both New and Restore — Restore re-seeds the injected state from the
// folded SecurityCeilingChanged events, so the checker sees the recovered ceiling on
// resume.
func WithCeiling(cs *ceiling.State) Option {
	return func(s *Session) {
		if cs != nil {
			s.ceiling = cs
		}
	}
}

// WithSessionID injects an externally-minted sessionID for New to adopt instead of
// minting its own. It resolves the journal chicken-and-egg: the durable journal needs
// the sessionID (to bind the per-session stream and write the opening LeaseFence)
// BEFORE the session exists, so the composition root mints the id first, builds the
// journal/lease/appenders from it, then hands the SAME id to New here. A zero id is
// ignored (New mints one) so a wiring slip can never produce a zero-id session. Restore
// takes the sessionID positionally and ignores this option.
func WithSessionID(id uuid.UUID) Option {
	return func(s *Session) {
		if !id.IsZero() {
			s.injectedSessionID = id
		}
	}
}

// WithEventAppender injects the hub's REQUIRED durable event tap (the composition
// root's adapter over SessionJournal — journal.JournalEventAppender). New forwards it
// into the hub (hub.WithAppender) so every Enduring event is durably appended before
// fan-out (fail-secure: an append failure faults the session). A nil appender is ignored
// (the hub's nop default stays installed) so a caller can never null out the tap and
// silently persist nothing. This is the event-side counterpart to WithCommandAppender
// (the audit-only intent log).
func WithEventAppender(a eventAppender) Option {
	return func(s *Session) {
		if a != nil {
			s.injectedEventAppender = a
		}
	}
}

// WithLeaseRelease installs the single-writer-lease release hook the session calls ONCE
// at the end of Shutdown (after the loops have drained, so the journal's last append is
// durable before ownership is relinquished). The composition root passes lease.Release
// for a NEW session; Restore installs it from the lease it acquired, so both paths free
// ownership on a clean exit and a successor can re-acquire without waiting out the TTL. A
// nil hook is ignored (headless mode stays a no-op). It takes a context so the release I/O
// is bounded by Shutdown's ctx.
func WithLeaseRelease(release func(context.Context) error) Option {
	return func(s *Session) {
		if release != nil {
			s.leaseRelease = release
		}
	}
}

// WithLimits sets the in-session subagent-spawn safety caps (depth + quota) NewLoop
// enforces. A zero (or negative) field in the supplied Limits adopts the package default
// (Depth 3 / Quota 64) when newSession applies withDefaults, so a caller can never disable
// a cap with a missing or bad value. Without this option a session uses the defaults. It
// applies on both New and Restore (the restore path re-seeds the spawn counter from the
// durable log, then enforces these caps against it).
func WithLimits(l Limits) Option {
	return func(s *Session) {
		s.limits = l
	}
}

// WithAllowConfigMismatch is the restore-only opt-in to resume a session whose
// persisted config fingerprint no longer matches the live config (a different model,
// system prompt, or tool policy). Restore is fail-secure by DEFAULT — a mismatch
// rejects with *ConfigMismatchError so a conversation never silently resumes under
// behavior it never ran with — so this option exists for an operator who knowingly
// accepts the drift. New ignores it (only Restore checks fingerprints).
func WithAllowConfigMismatch() Option {
	return func(s *Session) {
		s.allowConfigMismatch = true
	}
}

// WithInterruptReleasePolicy installs the pluggable admission-barrier release policy (the
// Dependency-Inversion seam of the interrupt machinery). After an interrupt cancels a running
// turn, the session holds the interrupt-pending marks until the policy's AwaitRelease returns,
// then clears them. Without this option the session uses the default (sessionIdleRelease):
// release once the session next reaches idle (SessionIdle durably appended). Task 16 injects a
// workspace-aware policy that may hold the barrier through a checkpoint. A nil policy is ignored
// (the default stays installed). See interrupt.go.
func WithInterruptReleasePolicy(p InterruptReleasePolicy) Option {
	return func(s *Session) {
		if p != nil {
			s.interruptRelease = p
		}
	}
}

// withOffloadGCRunner installs the session's offload-blob GC runner (built at the
// composition root over the session lease + the journal-admission gate). A nil runner is
// ignored so a wiring slip cannot null the field. The session STARTS it after the hub
// exists and STOPS it first on Shutdown.
func withOffloadGCRunner(runner *offloadGCRunner) Option {
	return func(s *Session) {
		if runner != nil {
			s.offloadGC = runner
		}
	}
}

// withOffloadGCPolicy carries the offload-GC cadence into the restore path so
// restoreTopologySession can build the runner from the lease it acquires. NewSession builds
// the runner in the Lifecycle and uses withOffloadGCRunner directly. An unconfigured policy
// is ignored.
func withOffloadGCPolicy(policy OffloadGCPolicy) Option {
	return func(s *Session) {
		if policy.Configured() {
			s.offloadGCPolicy = policy
		}
	}
}

// FingerprintProvider projects a bound loop into the immutable behavior fingerprint
// used for both SessionStarted and restore validation. It must be deterministic and safe
// for concurrent calls from separate sessions.
type FingerprintProvider func(loop.BoundDefinition) event.ConfigFingerprint

// WithFingerprintProvider installs the composition root's immutable projection.
func WithFingerprintProvider(provider FingerprintProvider) Option {
	return func(s *Session) {
		s.fingerprint = provider
	}
}

// WithFingerprint installs a definition-time frozen compatibility fingerprint. Unlike
// a provider, it is available before any loop definition is bound during restore.
func WithFingerprint(fingerprint event.ConfigFingerprint) Option {
	return func(s *Session) {
		copy := fingerprint
		s.frozenFingerprint = &copy
	}
}

// WithForeignBuilders wires the composition-root seam that constructs foreign-engine
// loops (live + restored). Without it, a foreign-engine definition fails closed at newLoop
// (SessionForeignBuilderMissing) and at restore (RestoreForeignBuilderMissing) — a
// foreign engine never silently resolves to a native loop. The two seams travel
// together (a live build and a restored build of the same agent), so they are wired as
// one option; either being nil leaves foreign engines unsupported for that path.
func WithForeignBuilders(b foreignloop.Builder, rb foreignloop.RestoredBuilder) Option {
	return func(s *Session) {
		s.foreignBuild = b
		s.foreignBuildRestored = rb
	}
}

// WithWorkspaceCheckpointing wires the workspace snapshot store and the workspace root this
// session checkpoints. Both are required for CheckpointWorkspace; without this option the
// capability is unconfigured and CheckpointWorkspace fails closed with a typed
// *WorkspaceNotConfiguredError. The composition root decides WHEN to checkpoint (a
// quiescence point); looprig only exposes the capability. A nil store is ignored (the
// default unconfigured state stays), so a wiring slip can never install a store the
// capability would nil-deref on.
func WithWorkspaceCheckpointing(ws *workspacestore.Store, root string) Option {
	return func(s *Session) {
		if ws != nil {
			s.ws = ws
			s.wsRoot = root
		}
	}
}

// WithSnapshotPolicy carries the already-validated rig policy into one session.
// It is meaningful only with a managed placement; rig enforces that pairing.
func WithSnapshotPolicy(policy SnapshotPolicy) Option {
	resolved := policy.internal()
	return func(s *Session) { s.snapshotPolicy = &resolved }
}

// withResolvedPlacement installs a resolved managed-workspace placement: the workspace
// store + root, the ONE session-scoped mutation coordinator every loop's tools serialize
// through, the exclusive root-lease release hook (nil for non-leased modes), and the
// lease-loss channel the session watches to fault on ownership loss. It is the internal
// composition seam the Lifecycle populates after acquiring the root lease and materializing
// any seed. A nil argument is ignored (the no-placement default stays). This is the ONLY
// path that populates the coordinator, so a session without a placement leaves
// tool.Bindings.Workspace nil at every bind site.
func withResolvedPlacement(r *resolvedPlacement) Option {
	return func(s *Session) {
		if r == nil || r.coordinator == nil {
			return
		}
		s.ws = r.store
		s.wsRoot = r.root
		s.wsMode = r.mode
		s.wsCoordinator = r.coordinator
		s.wsRootRelease = r.rootRelease
		s.wsLeaseLost = r.leaseLost
	}
}

// withPlacementSpec carries the UNRESOLVED managed-workspace placement into RestoreTopology,
// which resolves it per-session after acquiring the session lease (so the exclusive root
// lease is acquired AFTER the session lease on the restore path too). NewSession resolves
// its placement in the Lifecycle instead (seeding must materialize before construction), so
// it uses withResolvedPlacement directly. A zero/unconfigured spec is ignored.
func withPlacementSpec(p WorkspacePlacement) Option {
	return func(s *Session) {
		if p.Configured() {
			s.placementSpec = p
		}
	}
}

// stampNow returns the session clock's current time, defaulting to the wall clock if
// the clock seam is unset (a struct-literal test session). The session stamps this onto
// every dispatched command's Header.CreatedAt at the dispatch boundary, so a journaled
// intent-log record carries its creation time minted from the SAME seam as the
// session's events.
func (s *Session) stampNow() time.Time {
	if s.now == nil {
		return time.Now()
	}
	return s.now()
}

// appendCommand is the session's DRY, AUDIT-ONLY intent-log write, called at every
// command-dispatch site BEFORE the command is sent to the loop. It wraps cmd in a
// journal.CommandRecord targeting (sessionID, loopID) — the dispatch target the command
// itself may not carry (Interrupt/Shutdown route per-loop) — and appends it.
//
// On a non-nil append error it LOGS LOUDLY and RETURNS (the caller proceeds with the
// dispatch): losing a command record must never block the user's action or fault the
// session. This is the single deliberate proceed-on-failure persistence path. The
// appender is nil-guarded (a struct-literal test session leaves it unset) so the path
// is a safe no-op in no-persistence mode.
func (s *Session) appendCommand(ctx context.Context, loopID uuid.UUID, cmd command.Command) {
	s.appendCommandWithPolicy(ctx, loopID, cmd, false)
}

// appendDelegateCommand is the load-bearing delegate request intent append. Unlike
// ordinary interactive audit records, failure prevents dispatch so restore can classify
// every accepted machine NoFold request deterministically.
func (s *Session) appendDelegateCommand(ctx context.Context, loopID uuid.UUID, cmd command.UserInput) error {
	if s.cmdAppender == nil {
		return nil
	}
	record := journal.NewCommandRecord(s.sessionID, loopID, cmd)
	if err := journal.ValidateCommandRecordRoute(record); err != nil {
		return &SessionError{Kind: SessionDelegateIntentAppendFailed, Cause: err}
	}
	if err := s.cmdAppender.AppendCommand(ctx, record); err != nil {
		return &SessionError{Kind: SessionDelegateIntentAppendFailed, Cause: err}
	}
	return nil
}

// appendShutdownCommand is the shutdown-path intent-log write. It is identical to
// appendCommand except that a TYPED lease-lost append failure is EXPECTED: Shutdown releases
// the single-writer lease as part of teardown (or the heartbeat already observed the loss),
// so a final shutdown-command append refused for lease loss is benign — not a fault. That
// one path is logged at debug; every OTHER append failure (and an ordinary, non-shutdown
// lease loss) still logs loudly at error. It does not change dispatch semantics.
func (s *Session) appendShutdownCommand(ctx context.Context, loopID uuid.UUID, cmd command.Command) {
	s.appendCommandWithPolicy(ctx, loopID, cmd, true)
}

// appendCommandWithPolicy performs the audit-only intent-log append and applies the
// failure-log policy: a lease-lost failure is downgraded to debug only when
// leaseLostExpected (the shutdown path); otherwise any failure logs loudly. It always
// proceeds — the append is never load-bearing for dispatch.
func (s *Session) appendCommandWithPolicy(ctx context.Context, loopID uuid.UUID, cmd command.Command, leaseLostExpected bool) {
	app := s.cmdAppender
	if app == nil {
		return
	}
	rec := journal.NewCommandRecord(s.sessionID, loopID, cmd)
	err := app.AppendCommand(ctx, rec)
	if err == nil {
		return
	}

	if leaseLostExpected && isJournalLeaseLost(err) {
		// Expected on a clean shutdown: ownership is relinquished during teardown, so a final
		// shutdown-command append refused for lease loss is benign. Log at debug, not error —
		// this is the incident's false-alarm path.
		slog.DebugContext(ctx, "session: shutdown intent-log append skipped after lease loss (expected)",
			"session", s.sessionID,
			"loop", loopID,
			"command_id", cmd.CommandHeader().CommandID,
			"err", err,
		)
		return
	}

	// Audit-only: log loudly and proceed. Never block the dispatch, never fault the
	// session — a lost intent-log record is recoverable; a blocked user action is not.
	slog.ErrorContext(ctx, "session: intent-log command append failed (audit-only, proceeding)",
		"session", s.sessionID,
		"loop", loopID,
		"command_id", cmd.CommandHeader().CommandID,
		"err", err,
	)
}

// isJournalLeaseLost reports whether err is (or wraps) a *journal.JournalLeaseLostError —
// the typed "append refused because ownership was lost" signal.
func isJournalLeaseLost(err error) bool {
	var leaseLost *journal.JournalLeaseLostError
	return errors.As(err, &leaseLost)
}
