package sessionruntime

import (
	"context"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/ceiling"
	"github.com/looprig/harness/pkg/foreignloop"
	"github.com/looprig/harness/pkg/identity"
	"github.com/looprig/harness/pkg/journal"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/sessionstore"
	"github.com/looprig/harness/pkg/workspacestore"
)

// MissingStoreError reports that NewLifecycle was handed a nil *sessionstore.Store. The durable
// backend is a required dependency (DIP): the Lifecycle mints per-session leases/journals/
// appenders from it, so a nil store is rejected at NewLifecycle rather than deferring a
// nil-deref to the first NewSession/RestoreSession.
type MissingStoreError struct{}

func (*MissingStoreError) Error() string {
	return "session: Lifecycle.NewLifecycle requires a non-nil sessionstore.Store"
}

// MissingFingerprintProviderError reports incomplete composition wiring.
type MissingFingerprintProviderError struct{}

func (*MissingFingerprintProviderError) Error() string {
	return "session: fingerprint provider is required"
}

type MissingTopologyError struct{}

func (*MissingTopologyError) Error() string {
	return "session: lifecycle topology has no active primer"
}

// NewSessionErrorKind classifies a NewSession failure before the live session exists — the per-session
// durable wiring the Lifecycle builds itself (lease, journal, checked appenders) or the
// session construction that consumes them.
type NewSessionErrorKind string

const (
	// NewSessionContextDone: the NewSession context was already cancelled.
	NewSessionContextDone NewSessionErrorKind = "context_done"
	// NewSessionIDGenerationFailed: a crypto/rand failure minting the fresh session id.
	NewSessionIDGenerationFailed NewSessionErrorKind = "id_generation_failed"
	// NewSessionLeaseFailed: the single-writer lease could not be acquired (another owner holds
	// it, or a backend read failed). The session must not come up.
	NewSessionLeaseFailed NewSessionErrorKind = "lease_failed"
	// NewSessionJournalFailed: the SessionJournal could not be opened (its opening fence was
	// rejected, or stream setup failed).
	NewSessionJournalFailed NewSessionErrorKind = "journal_failed"
	// NewSessionAppenderFailed: a checked journal appender (event/command/gate) could not be
	// constructed over the opened journal.
	NewSessionAppenderFailed NewSessionErrorKind = "appender_failed"
	// NewSessionRuntimeFailed: NewSession refused to build the live session over the wired dependencies.
	NewSessionRuntimeFailed NewSessionErrorKind = "session_failed"
)

// NewSessionError is the typed wrapper for a NewSession failure. Kind classifies the stage; Cause
// chains the underlying typed error (a *journal.LeaseHeldError, a *SessionError, etc.) so
// a caller can errors.As both this and the cause. On any failure after the lease is
// acquired the Lifecycle releases it best-effort before returning, so a failed NewSession never
// strands single-writer ownership.
type NewSessionError struct {
	Kind  NewSessionErrorKind
	Cause error
}

func (e *NewSessionError) Error() string {
	msg := "session: creation failed (" + string(e.Kind) + ")"
	if e.Cause != nil {
		return msg + ": " + e.Cause.Error()
	}
	return msg
}

func (e *NewSessionError) Unwrap() error { return e.Cause }

// Lifecycle binds a design-time agent definition (cfg) and a durable backend (store) into an
// immutable, reusable factory for live sessions. NewLifecycle captures the caller-facing
// options once; NewSession mints a fresh session id and brings up a brand-new session over
// per-session durable dependencies; RestoreSession rebuilds a prior session from its journal.
// Everything the narrow serve.Rig interface needs
// is fixed at NewLifecycle, so NewSession/RestoreSession take no per-call knobs.
//
// A Lifecycle is safe to reuse across many sessions. Each NewSession/RestoreSession builds its OWN durable
// wiring (lease, journal, appenders) from the shared store, so two live sessions never
// share a lease or a journal.
type Lifecycle struct {
	cfg      loop.Definition // compatibility alias for the active primer
	topology Topology
	store    *sessionstore.Store

	// catalog is the derived session-index each session's event appender notifies (via
	// journal.WithCatalog) after each durable append, so the replay-free status fold stays
	// live. Built once in NewLifecycle from the store (cheap, no I/O). See AMBIGUITY A3.
	catalog *sessionstore.Catalog

	// baseOpts are the session.Options captured at NewLifecycle that are IDENTICAL across every
	// NewSession and RestoreSession: limits, fingerprint projection,
	// WithWorkspaceStore, WithForeignBuilder, WithGateCaps. They are forwarded verbatim to
	// both NewSession and RestoreSession. The per-session dependencies (session ID,
	// appenders, lease release, and ceiling) are appended by each lifecycle call.
	baseOpts []Option

	// allowConfigMismatch is the NewLifecycle-time opt-in forwarded to RestoreSession ONLY (as
	// WithAllowConfigMismatch): a fingerprint mismatch resumes instead of rejecting. NewSession
	// never reads it. AMBIGUITY A2: WithAllowConfigMismatch is classified NewLifecycle-time for
	// serve.Rig interface minimalism, so it is fixed for the Lifecycle's whole lifetime.
	allowConfigMismatch bool

	// ceilingFactory mints a FRESH *ceiling.State per NewSession/RestoreSession. AMBIGUITY A1: reusing one
	// NewLifecycle-captured cfg across many concurrent NewSession calls would otherwise share ONE mutable
	// ceiling (cfg.Tools.Permission holds the checker that reads it) — wrong for
	// multi-session, where each session must clamp independently. The Lifecycle therefore mints
	// a per-session state here and injects it via WithCeiling so the SESSION's ceiling source is
	// session-isolated. Every loop PermissionFactory receives this exact live source through
	// tool.Bindings, so native permission checkers can select consumer-defined postures on
	// each Check. When the factory is nil the Lifecycle falls
	// back to today's behavior — the session default-mints its own internal ceiling state
	// (whatever cfg carries is untouched).
	ceilingFactory CeilingFactory
	fingerprint    FingerprintProvider

	// placement is the OPTIONAL managed-workspace placement (exclusive/per-session/shared)
	// resolved per session by NewSession/RestoreSession. The zero value (PlacementNone) means
	// no managed workspace: no root resolution, no lease, no coordinator — the historical
	// default. Captured once via WithLifecyclePlacement.
	placement WorkspacePlacement
}

// WithLifecyclePlacement captures the managed-workspace placement the rig declared. The
// unconfigured zero value is ignored (no managed workspace). Forwarded to NewSession and
// RestoreSession, which resolve it per-session after acquiring the session lease.
func WithLifecyclePlacement(p WorkspacePlacement) LifecycleOption {
	return func(r *Lifecycle) {
		if p.Configured() {
			r.placement = p
		}
	}
}

// CeilingFactory mints a fresh security-ceiling state. The Lifecycle calls it once per
// NewSession/RestoreSession so each session gets its own independent clamp (AMBIGUITY A1 on
// Lifecycle.ceilingFactory). It is a named type per the codebase's prefer-named-types rule.
type CeilingFactory func() *ceiling.State

// LifecycleOption configures a Lifecycle at NewLifecycle time. Every caller-facing knob is captured
// here (the runtime NewSession/RestoreSession take none), mirroring flow's LifecycleOption model. A
// nil/zero argument is ignored (the default is kept), mirroring the session options' own
// fail-safe convention.
type LifecycleOption func(*Lifecycle)

// WithLifecycleLimits captures the in-session subagent-spawn safety caps (depth + quota) the
// session enforces. Forwarded to both NewSession and RestoreSession as WithLimits.
func WithLifecycleLimits(l Limits) LifecycleOption {
	return func(r *Lifecycle) {
		r.baseOpts = append(r.baseOpts, WithLimits(l))
	}
}

// WithLifecycleFingerprintProvider captures the deterministic projection used by both
// NewSession and RestoreSession. The provider may be called concurrently for different
// sessions and must be concurrency-safe.
func WithLifecycleFingerprintProvider(provider FingerprintProvider) LifecycleOption {
	return func(r *Lifecycle) {
		r.fingerprint = provider
	}
}

// WithLifecycleWorkspaceStore captures the workspace snapshot store and root the session
// checkpoints into (and RestoreSession materializes from). A nil store is ignored. Forwarded to
// both NewSession and RestoreSession as WithWorkspaceStore.
func WithLifecycleWorkspaceStore(ws *workspacestore.Store, root string) LifecycleOption {
	return func(r *Lifecycle) {
		if ws != nil {
			r.baseOpts = append(r.baseOpts, WithWorkspaceStore(ws, root))
		}
	}
}

// WithLifecycleForeignBuilder captures the composition-root seams that construct foreign-
// engine loops (live + restored). Either seam being nil leaves foreign engines unsupported,
// so both are captured together. Forwarded to both NewSession and RestoreSession as WithForeignBuilder.
func WithLifecycleForeignBuilder(b foreignloop.Builder, rb foreignloop.RestoredBuilder) LifecycleOption {
	return func(r *Lifecycle) {
		if b != nil && rb != nil {
			r.baseOpts = append(r.baseOpts, WithForeignBuilder(b, rb))
		}
	}
}

// WithLifecycleGateCaps captures the live gate-directory bounds. Zero (the default) means no
// cap. Forwarded to both NewSession and RestoreSession as WithGateCaps.
func WithLifecycleGateCaps(caps GateCaps) LifecycleOption {
	return func(r *Lifecycle) {
		r.baseOpts = append(r.baseOpts, WithGateCaps(caps))
	}
}

// WithLifecycleAllowConfigMismatch captures the restore-only opt-in to resume a session whose
// persisted config fingerprint no longer matches the live config. AMBIGUITY A2: classified
// NewLifecycle-time (fixed for the Lifecycle's lifetime) so the narrow serve.Rig interface
// exposes no per-call knob. NewSession ignores it; only RestoreSession honors it.
func WithLifecycleAllowConfigMismatch() LifecycleOption {
	return func(r *Lifecycle) {
		r.allowConfigMismatch = true
	}
}

// WithLifecycleCeilingFactory captures the factory the Lifecycle calls to mint a FRESH
// *ceiling.State for each NewSession/RestoreSession. A nil factory is ignored (the session default-mints
// its own internal state). See AMBIGUITY A1 on Lifecycle.ceilingFactory for why the ceiling
// must be per-session and what the Lifecycle deliberately leaves to swe.
func WithLifecycleCeilingFactory(factory CeilingFactory) LifecycleOption {
	return func(r *Lifecycle) {
		if factory != nil {
			r.ceilingFactory = factory
		}
	}
}

// NewLifecycle binds cfg and store into an immutable, reusable Lifecycle, capturing the caller-
// facing options once. A nil store is rejected with a typed *MissingStoreError (the durable
// backend is required). It does no session I/O — the derived catalog it opens is cheap and
// cannot fail — so the returned Lifecycle is ready to NewSession/RestoreSession.
func NewLifecycle(cfg loop.Definition, store *sessionstore.Store, opts ...LifecycleOption) (*Lifecycle, error) {
	return NewTopologyLifecycle(Topology{Definitions: []loop.Definition{cfg}, Primers: []identity.AgentName{cfg.Name()}, ActivePrimer: cfg.Name()}, store, opts...)
}

// NewTopologyLifecycle binds an immutable, validated multi-primer graph to storage.
func NewTopologyLifecycle(topology Topology, store *sessionstore.Store, opts ...LifecycleOption) (*Lifecycle, error) {
	if store == nil {
		return nil, &MissingStoreError{}
	}
	topology = cloneTopology(topology)
	active, ok := topology.definition(topology.ActivePrimer)
	if !ok {
		return nil, &MissingTopologyError{}
	}
	r := &Lifecycle{cfg: active, topology: topology, store: store}
	for _, opt := range opts {
		opt(r)
	}
	if r.fingerprint == nil {
		return nil, &MissingFingerprintProviderError{}
	}
	// AMBIGUITY A3: build the derived catalog so each session event appender keeps the
	// replay-free status fold live (journal.WithCatalog below). WithCatalogReplayer(store)
	// is passed explicitly; OpenCatalog already defaults the replayer to the owning store,
	// so this is belt-and-suspenders — it names the store as the repair opener rather than
	// relying on the default. OpenCatalog does no I/O and cannot fail.
	r.catalog = store.OpenCatalog(sessionstore.WithCatalogReplayer(store))
	return r, nil
}

// NewSession mints a fresh session ID and brings up a brand-new live session over per-session durable
// deps built from the Lifecycle's store: a single-writer lease, the session journal, and the
// three checked appenders (event — carrying the catalog — command, and gate). It returns
// the minted id, the live session, and a typed *NewSessionError on any failure. On ANY failure
// after the lease is acquired the lease is released best-effort, so a failed NewSession never
// strands single-writer ownership.
func (r *Lifecycle) NewSession(ctx context.Context, seed workspacestore.Ref) (*Session, error) {
	select {
	case <-ctx.Done():
		return nil, &NewSessionError{Kind: NewSessionContextDone, Cause: ctx.Err()}
	default:
	}

	sid, err := uuid.New()
	if err != nil {
		return nil, &NewSessionError{Kind: NewSessionIDGenerationFailed, Cause: err}
	}

	// Per-run durable wiring, mirroring the by-hand persistence pattern: acquire the lease,
	// open the journal fenced on it, then build the three checked appenders over that
	// journal. The event appender carries the NewLifecycle-built catalog so the status fold stays
	// live. On any failure past the lease, release it best-effort (releaseLease, shared with
	// RestoreSession) so a successor can re-acquire without waiting out the TTL.
	lease, err := r.store.AcquireLease(ctx, sid)
	if err != nil {
		return nil, &NewSessionError{Kind: NewSessionLeaseFailed, Cause: err}
	}
	j, err := r.store.OpenJournal(ctx, sid, lease)
	if err != nil {
		releaseLease(lease)
		return nil, &NewSessionError{Kind: NewSessionJournalFailed, Cause: err}
	}
	evAp, err := journal.NewJournalEventAppenderChecked(j, journal.WithCatalog(r.catalog))
	if err != nil {
		releaseLease(lease)
		return nil, &NewSessionError{Kind: NewSessionAppenderFailed, Cause: err}
	}
	cmdAp, err := journal.NewJournalCommandAppenderChecked(j)
	if err != nil {
		releaseLease(lease)
		return nil, &NewSessionError{Kind: NewSessionAppenderFailed, Cause: err}
	}
	gateAp, err := journal.NewJournalGateAppenderChecked(j)
	if err != nil {
		releaseLease(lease)
		return nil, &NewSessionError{Kind: NewSessionAppenderFailed, Cause: err}
	}

	// The captured base options, then the per-session dependencies. WithSessionID(sid) makes NewSession adopt
	// the id the journal was already bound to (the journal chicken-and-egg). WithLeaseRelease
	// hands the session the lease's release hook for its clean-Shutdown teardown.
	opts := make([]Option, 0, len(r.baseOpts)+6)
	opts = append(opts, r.baseOpts...)
	opts = append(opts,
		WithFingerprintProvider(r.fingerprint),
		WithSessionID(sid),
		WithEventAppender(evAp),
		WithCommandAppender(cmdAp),
		WithGateAppender(gateAp),
		WithLeaseRelease(lease.Release),
	)
	// AMBIGUITY A1: mint a fresh per-session ceiling state so concurrent sessions never share one
	// mutable clamp. Nil factory falls back to the session's own internal default-mint.
	if r.ceilingFactory != nil {
		opts = append(opts, WithCeiling(r.ceilingFactory()))
	}

	// Resolve the managed-workspace placement (design §"Placement details"). The session
	// lease is already held (above), so the exclusive root lease is acquired AFTER it, as
	// the design mandates. On root contention the session lease is released and the typed
	// *WorkspaceRootBusyError surfaces (fail closed). A per-session/shared placement takes
	// no lease. When there is no placement, resolved stays nil and nothing changes.
	var resolved *resolvedPlacement
	if r.placement.Configured() {
		resolved, err = r.placement.resolveForNew(ctx, sid)
		if err != nil {
			releaseLease(lease)
			return nil, &NewSessionError{Kind: NewSessionLeaseFailed, Cause: err}
		}
		opts = append(opts, withResolvedPlacement(resolved))
	}

	// Seeding (design §"Seeding"): materialize the seed BEFORE constructing the session so
	// it lands in the (empty) root and becomes the first workspace checkpoint. Valid only
	// for per-session and an EMPTY exclusive root; never shared; the ref must resolve.
	if seed != "" {
		if err := r.placement.materializeSeed(ctx, resolved, seed); err != nil {
			releaseResolvedRoot(ctx, resolved)
			releaseLease(lease)
			return nil, &NewSessionError{Kind: NewSessionRuntimeFailed, Cause: err}
		}
	}

	s, err := NewTopology(ctx, r.topology, opts...)
	if err != nil {
		// NewSession failed, so the session never took ownership of the lease-release hook — release
		// it here best-effort so ownership is not stranded.
		releaseResolvedRoot(ctx, resolved)
		releaseLease(lease)
		return nil, &NewSessionError{Kind: NewSessionRuntimeFailed, Cause: err}
	}
	// The session now owns both leases (via WithLeaseRelease + withResolvedPlacement); start
	// the exclusive root-lease loss watcher so ownership loss faults the session.
	s.watchRootLease()
	// Journal the seed as the first workspace checkpoint AFTER SessionStarted/LoopStarted.
	// Failing to record it faults durability, so shut the session down (releasing both
	// leases) and fail closed.
	if seed != "" {
		if err := s.recordSeedCheckpoint(ctx, seed); err != nil {
			_ = s.Shutdown(ctx)
			return nil, &NewSessionError{Kind: NewSessionRuntimeFailed, Cause: err}
		}
	}
	return s, nil
}

// releaseResolvedRoot releases a resolved placement's exclusive root lease best-effort on
// a NewSession failure path (before the session takes ownership). Nil-safe.
func releaseResolvedRoot(ctx context.Context, resolved *resolvedPlacement) {
	if resolved == nil || resolved.rootRelease == nil {
		return
	}
	_ = resolved.rootRelease(ctx)
}

// RestoreSession rebuilds a live session from its durable journal under the id it was created
// with, delegating to runtime restoration with the Lifecycle's captured cfg, store, and base
// options. runtime restoration holds the store, so it builds its OWN lease/journal/appenders
// (and installs the lease-release hook) internally — the Lifecycle supplies only the captured
// caller options, NOT the per-session appenders NewSession builds. It refuses a config-fingerprint
// mismatch (typed *ConfigMismatchError) unless WithLifecycleAllowConfigMismatch was compiled
// in, and surfaces runtime restoration's typed errors unchanged (a *RestoreDiscoveryError for a
// session with no history, a *RestoreError for a lease/journal/replay failure), never a
// panic.
func (r *Lifecycle) RestoreSession(ctx context.Context, id uuid.UUID) (*Session, error) {
	opts := make([]Option, 0, len(r.baseOpts)+2)
	opts = append(opts, r.baseOpts...)
	opts = append(opts, WithFingerprintProvider(r.fingerprint))
	if r.allowConfigMismatch {
		opts = append(opts, WithAllowConfigMismatch())
	}
	// AMBIGUITY A1: mint a fresh per-session ceiling on restore too (WithCeiling applies to
	// RestoreSession, which re-seeds the injected state from the folded SecurityCeilingChanged
	// events), so a restored session gets its own clamp just like a fresh NewSession.
	if r.ceilingFactory != nil {
		opts = append(opts, WithCeiling(r.ceilingFactory()))
	}
	if r.placement.Configured() {
		opts = append(opts, withPlacementSpec(r.placement))
	}
	return RestoreTopology(ctx, r.topology, id, r.store, opts...)
}
