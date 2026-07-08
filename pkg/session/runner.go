package session

import (
	"context"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/ceiling"
	"github.com/looprig/harness/pkg/foreignloop"
	"github.com/looprig/harness/pkg/journal"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/sessionstore"
	"github.com/looprig/harness/pkg/workspacestore"
)

// NilStoreError reports that Compile was handed a nil *sessionstore.Store. The durable
// backend is a required dependency (DIP): the Runner mints per-run leases/journals/
// appenders from it, so a nil store is rejected at Compile rather than deferring a
// nil-deref to the first Run/Restore.
type NilStoreError struct{}

func (*NilStoreError) Error() string {
	return "session: Runner.Compile requires a non-nil sessionstore.Store"
}

// RunErrorKind classifies a Run failure before the live session exists — the per-run
// durable wiring the Runner builds itself (lease, journal, checked appenders) or the
// session construction that consumes them.
type RunErrorKind string

const (
	// RunContextDone: the Run context was already cancelled.
	RunContextDone RunErrorKind = "context_done"
	// RunIDGenerationFailed: a crypto/rand failure minting the fresh session id.
	RunIDGenerationFailed RunErrorKind = "id_generation_failed"
	// RunLeaseFailed: the single-writer lease could not be acquired (another owner holds
	// it, or a backend read failed). The session must not come up.
	RunLeaseFailed RunErrorKind = "lease_failed"
	// RunJournalFailed: the SessionJournal could not be opened (its opening fence was
	// rejected, or stream setup failed).
	RunJournalFailed RunErrorKind = "journal_failed"
	// RunAppenderFailed: a checked journal appender (event/command/gate) could not be
	// constructed over the opened journal.
	RunAppenderFailed RunErrorKind = "appender_failed"
	// RunSessionFailed: session.New refused to build the live session over the wired deps.
	RunSessionFailed RunErrorKind = "session_failed"
)

// RunError is the typed wrapper for a Run failure. Kind classifies the stage; Cause
// chains the underlying typed error (a *journal.LeaseHeldError, a *SessionError, etc.) so
// a caller can errors.As both this and the cause. On any failure after the lease is
// acquired the Runner releases it best-effort before returning, so a failed Run never
// strands single-writer ownership.
type RunError struct {
	Kind  RunErrorKind
	Cause error
}

func (e *RunError) Error() string {
	msg := "session: run failed (" + string(e.Kind) + ")"
	if e.Cause != nil {
		return msg + ": " + e.Cause.Error()
	}
	return msg
}

func (e *RunError) Unwrap() error { return e.Cause }

// Runner binds a design-time agent definition (cfg) and a durable backend (store) into an
// immutable, reusable factory for live sessions. Compile captures the caller-facing
// options once; Run mints a fresh session id and brings up a brand-new session over
// per-run durable deps; Restore rebuilds a prior session from its journal. It mirrors
// flow's CompileOption/RunOption split: everything the narrow serve.Runner interface needs
// is fixed at Compile, so Run/Restore take no per-call knobs.
//
// A Runner is safe to reuse across many sessions. Each Run/Restore builds its OWN durable
// wiring (lease, journal, appenders) from the shared store, so two live sessions never
// share a lease or a journal.
type Runner struct {
	cfg   loop.Config
	store *sessionstore.Store

	// catalog is the derived session-index the per-run event appender notifies (via
	// journal.WithCatalog) after each durable append, so the replay-free status fold stays
	// live. Built once in Compile from the store (cheap, no I/O). See AMBIGUITY A3.
	catalog *sessionstore.Catalog

	// baseOpts are the session.Options captured at Compile that are IDENTICAL across every
	// Run and Restore: WithLimits, WithConfigFingerprintFields (the fingerprinted one),
	// WithWorkspaceStore, WithForeignBuilder, WithGateCaps. They are forwarded verbatim to
	// both New and Restore. The per-run deps (session id, appenders, lease-release) and the
	// per-run ceiling are appended by Run/Restore on top of these.
	baseOpts []Option

	// allowConfigMismatch is the Compile-time opt-in forwarded to Restore ONLY (as
	// WithAllowConfigMismatch): a fingerprint mismatch resumes instead of rejecting. New
	// never reads it. AMBIGUITY A2: WithAllowConfigMismatch is classified Compile-time for
	// serve.Runner interface minimalism, so it is fixed for the Runner's whole lifetime.
	allowConfigMismatch bool

	// ceilingFactory mints a FRESH *ceiling.State per Run/Restore. AMBIGUITY A1: reusing one
	// Compile-captured cfg across many concurrent Runs would otherwise share ONE mutable
	// ceiling (cfg.Tools.Permission holds the checker that reads it) — wrong for
	// multi-session, where each session must clamp independently. The Runner therefore mints
	// a per-run state here and injects it via WithCeiling so the SESSION's ceiling source is
	// per-run. Rebinding the permission CHECKER (in cfg.Tools.Permission) to this same
	// per-run state is a composition-root (swe) concern the spec DEFERS: the Runner only
	// mints-per-run; swe wires the checker to it. When the factory is nil the Runner falls
	// back to today's behavior — the session default-mints its own internal ceiling state
	// (whatever cfg carries is untouched).
	ceilingFactory func() *ceiling.State
}

// CompileOption configures a Runner at Compile time. Every caller-facing knob is captured
// here (the runtime Run/Restore take none), mirroring flow's CompileOption model. A
// nil/zero argument is ignored (the default is kept), mirroring the session options' own
// fail-safe convention.
type CompileOption func(*Runner)

// WithCompileLimits captures the in-session subagent-spawn safety caps (depth + quota) the
// session enforces. Forwarded to both New and Restore as WithLimits.
func WithCompileLimits(l Limits) CompileOption {
	return func(r *Runner) {
		r.baseOpts = append(r.baseOpts, WithLimits(l))
	}
}

// WithCompileConfigFingerprintFields captures the swarm-level config-fingerprint inputs
// (AgentKind/RuntimeSkills/WorkspaceRoot/…) that do not live on loop.Config. This is THE
// fingerprinted option: it is stamped on New's SessionStarted and re-merged into the LIVE
// fingerprint Restore compares, so it MUST be identical between the Run that created a
// session and the Restore that resumes it — hence its capture-once-at-Compile placement.
func WithCompileConfigFingerprintFields(fields ConfigFingerprintFields) CompileOption {
	return func(r *Runner) {
		r.baseOpts = append(r.baseOpts, WithConfigFingerprintFields(fields))
	}
}

// WithCompileWorkspaceStore captures the workspace snapshot store and root the session
// checkpoints into (and Restore materializes from). A nil store is ignored. Forwarded to
// both New and Restore as WithWorkspaceStore.
func WithCompileWorkspaceStore(ws *workspacestore.Store, root string) CompileOption {
	return func(r *Runner) {
		if ws != nil {
			r.baseOpts = append(r.baseOpts, WithWorkspaceStore(ws, root))
		}
	}
}

// WithCompileForeignBuilder captures the composition-root seams that construct foreign-
// engine loops (live + restored). Either seam being nil leaves foreign engines unsupported,
// so both are captured together. Forwarded to both New and Restore as WithForeignBuilder.
func WithCompileForeignBuilder(b foreignloop.Builder, rb foreignloop.RestoredBuilder) CompileOption {
	return func(r *Runner) {
		if b != nil && rb != nil {
			r.baseOpts = append(r.baseOpts, WithForeignBuilder(b, rb))
		}
	}
}

// WithCompileGateCaps captures the live gate-directory bounds. Zero (the default) means no
// cap. Forwarded to both New and Restore as WithGateCaps.
func WithCompileGateCaps(caps GateCaps) CompileOption {
	return func(r *Runner) {
		r.baseOpts = append(r.baseOpts, WithGateCaps(caps))
	}
}

// WithCompileAllowConfigMismatch captures the restore-only opt-in to resume a session whose
// persisted config fingerprint no longer matches the live config. AMBIGUITY A2: classified
// Compile-time (fixed for the Runner's lifetime) so the narrow serve.Runner interface
// exposes no per-call knob. New ignores it; only Restore honors it.
func WithCompileAllowConfigMismatch() CompileOption {
	return func(r *Runner) {
		r.allowConfigMismatch = true
	}
}

// WithCompileCeilingFactory captures the factory the Runner calls to mint a FRESH
// *ceiling.State for each Run/Restore. A nil factory is ignored (the session default-mints
// its own internal state). See AMBIGUITY A1 on Runner.ceilingFactory for why the ceiling
// must be per-run and what the Runner deliberately leaves to swe.
func WithCompileCeilingFactory(factory func() *ceiling.State) CompileOption {
	return func(r *Runner) {
		if factory != nil {
			r.ceilingFactory = factory
		}
	}
}

// Compile binds cfg and store into an immutable, reusable Runner, capturing the caller-
// facing options once. A nil store is rejected with a typed *NilStoreError (the durable
// backend is required). It does no session I/O — the derived catalog it opens is cheap and
// cannot fail — so the returned Runner is ready to Run/Restore.
func Compile(cfg loop.Config, store *sessionstore.Store, opts ...CompileOption) (*Runner, error) {
	if store == nil {
		return nil, &NilStoreError{}
	}
	r := &Runner{cfg: cfg, store: store}
	for _, opt := range opts {
		opt(r)
	}
	// AMBIGUITY A3: build the derived catalog so each per-run event appender keeps the
	// replay-free status fold live (journal.WithCatalog below). WithCatalogReplayer(store)
	// is passed explicitly; OpenCatalog already defaults the replayer to the owning store,
	// so this is belt-and-suspenders — it names the store as the repair opener rather than
	// relying on the default. OpenCatalog does no I/O and cannot fail.
	r.catalog = store.OpenCatalog(sessionstore.WithCatalogReplayer(store))
	return r, nil
}

// Run mints a fresh session id and brings up a brand-new live session over per-run durable
// deps built from the Runner's store: a single-writer lease, the session journal, and the
// three checked appenders (event — carrying the catalog — command, and gate). It returns
// the minted id, the live session, and a typed *RunError on any failure. On ANY failure
// after the lease is acquired the lease is released best-effort, so a failed Run never
// strands single-writer ownership.
func (r *Runner) Run(ctx context.Context) (uuid.UUID, *Session, error) {
	select {
	case <-ctx.Done():
		return uuid.UUID{}, nil, &RunError{Kind: RunContextDone, Cause: ctx.Err()}
	default:
	}

	sid, err := uuid.New()
	if err != nil {
		return uuid.UUID{}, nil, &RunError{Kind: RunIDGenerationFailed, Cause: err}
	}

	// Per-run durable wiring, mirroring the by-hand persistence pattern: acquire the lease,
	// open the journal fenced on it, then build the three checked appenders over that
	// journal. The event appender carries the Compile-built catalog so the status fold stays
	// live. On any failure past the lease, release it best-effort (releaseLease, shared with
	// Restore) so a successor can re-acquire without waiting out the TTL.
	lease, err := r.store.AcquireLease(ctx, sid)
	if err != nil {
		return uuid.UUID{}, nil, &RunError{Kind: RunLeaseFailed, Cause: err}
	}
	j, err := r.store.OpenJournal(ctx, sid, lease)
	if err != nil {
		releaseLease(lease)
		return uuid.UUID{}, nil, &RunError{Kind: RunJournalFailed, Cause: err}
	}
	evAp, err := journal.NewJournalEventAppenderChecked(j, journal.WithCatalog(r.catalog))
	if err != nil {
		releaseLease(lease)
		return uuid.UUID{}, nil, &RunError{Kind: RunAppenderFailed, Cause: err}
	}
	cmdAp, err := journal.NewJournalCommandAppenderChecked(j)
	if err != nil {
		releaseLease(lease)
		return uuid.UUID{}, nil, &RunError{Kind: RunAppenderFailed, Cause: err}
	}
	gateAp, err := journal.NewJournalGateAppenderChecked(j)
	if err != nil {
		releaseLease(lease)
		return uuid.UUID{}, nil, &RunError{Kind: RunAppenderFailed, Cause: err}
	}

	// The captured base options, then the per-run deps. WithSessionID(sid) makes New adopt
	// the id the journal was already bound to (the journal chicken-and-egg). WithLeaseRelease
	// hands the session the lease's release hook for its clean-Shutdown teardown.
	opts := make([]Option, 0, len(r.baseOpts)+6)
	opts = append(opts, r.baseOpts...)
	opts = append(opts,
		WithSessionID(sid),
		WithEventAppender(evAp),
		WithCommandAppender(cmdAp),
		WithGateAppender(gateAp),
		WithLeaseRelease(lease.Release),
	)
	// AMBIGUITY A1: mint a fresh per-run ceiling state so concurrent sessions never share one
	// mutable clamp. Nil factory falls back to the session's own internal default-mint.
	if r.ceilingFactory != nil {
		opts = append(opts, WithCeiling(r.ceilingFactory()))
	}

	s, err := New(ctx, r.cfg, opts...)
	if err != nil {
		// New failed, so the session never took ownership of the lease-release hook — release
		// it here best-effort so ownership is not stranded.
		releaseLease(lease)
		return uuid.UUID{}, nil, &RunError{Kind: RunSessionFailed, Cause: err}
	}
	return sid, s, nil
}

// Restore rebuilds a live session from its durable journal under the id it was created
// with, delegating to session.Restore with the Runner's captured cfg, store, and base
// options. session.Restore holds the store, so it builds its OWN lease/journal/appenders
// (and installs the lease-release hook) internally — the Runner supplies only the captured
// caller options, NOT the per-run appenders Run builds. It refuses a config-fingerprint
// mismatch (typed *ConfigMismatchError) unless WithCompileAllowConfigMismatch was compiled
// in, and surfaces session.Restore's typed errors unchanged (a *RestoreDiscoveryError for a
// session with no history, a *RestoreError for a lease/journal/replay failure), never a
// panic.
func (r *Runner) Restore(ctx context.Context, id uuid.UUID) (*Session, error) {
	opts := make([]Option, 0, len(r.baseOpts)+2)
	opts = append(opts, r.baseOpts...)
	if r.allowConfigMismatch {
		opts = append(opts, WithAllowConfigMismatch())
	}
	// AMBIGUITY A1: mint a fresh per-run ceiling on restore too (WithCeiling applies to
	// Restore, which re-seeds the injected state from the folded SecurityCeilingChanged
	// events), so a restored session gets its own clamp just like a fresh Run.
	if r.ceilingFactory != nil {
		opts = append(opts, WithCeiling(r.ceilingFactory()))
	}
	return Restore(ctx, r.cfg, id, r.store, opts...)
}
