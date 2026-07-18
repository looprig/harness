package sessionruntime

import (
	"context"
	"time"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/foreign"
	"github.com/looprig/harness/pkg/hustle"
	"github.com/looprig/harness/pkg/journal"
	"github.com/looprig/harness/pkg/security"
	"github.com/looprig/harness/pkg/sessionstore"
	"github.com/looprig/harness/pkg/workspacestore"
)

// MissingStoreError reports that NewTopologyLifecycle was handed a nil *sessionstore.Store. The durable
// backend is a required dependency (DIP): the Lifecycle mints per-session leases/journals/
// appenders from it, so a nil store is rejected at NewTopologyLifecycle rather than deferring a
// nil-deref to the first NewSession/RestoreSession.
type MissingStoreError struct{}

func (*MissingStoreError) Error() string {
	return "session: Lifecycle.NewTopologyLifecycle requires a non-nil sessionstore.Store"
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
	// NewSessionSecurityLimitFailed: the configured factory returned no per-session security.
	NewSessionSecurityLimitFailed NewSessionErrorKind = "ceiling_failed"
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

// NilSecurityLimitError reports a per-session security limit factory that violated its contract.
// Silently minting the default would hide broken security-policy wiring.
type NilSecurityLimitError struct{}

func (*NilSecurityLimitError) Error() string { return "session: security limit factory returned nil" }

// Lifecycle binds a design-time loop topology and durable backend into an immutable,
// reusable factory for live sessions. NewTopologyLifecycle captures the caller-facing
// options once; NewSession mints a fresh session id and brings up a brand-new session over
// per-session durable dependencies; RestoreSession rebuilds a prior session from its journal.
// Everything the narrow serve.Rig interface needs
// is fixed at NewTopologyLifecycle, so NewSession/RestoreSession take no per-call knobs.
//
// A Lifecycle is safe to reuse across many sessions. Each NewSession/RestoreSession builds its OWN durable
// wiring (lease, journal, appenders) from the shared store, so two live sessions never
// share a lease or a journal.
type Lifecycle struct {
	topology Topology
	store    *sessionstore.Store

	// catalog is the derived session-index each session's event appender notifies (via
	// journal.WithCatalog) after each durable append, so the replay-free status fold stays
	// live. Built once in NewTopologyLifecycle from the store (cheap, no I/O). See AMBIGUITY A3.
	catalog *sessionstore.Catalog

	// baseOpts are the session.Options captured at NewTopologyLifecycle that are IDENTICAL across every
	// NewSession and RestoreSession: limits, fingerprint projection,
	// WithWorkspaceCheckpointing, WithForeignBuilders, WithGateCaps. They are forwarded verbatim to
	// both NewSession and RestoreSession. The per-session dependencies (session ID,
	// appenders, lease release, and security limit) are appended by each lifecycle call.
	baseOpts []Option

	// allowConfigMismatch is the NewTopologyLifecycle-time opt-in forwarded to RestoreSession ONLY (as
	// WithAllowConfigMismatch): a fingerprint mismatch resumes instead of rejecting. NewSession
	// never reads it. AMBIGUITY A2: WithAllowConfigMismatch is classified NewTopologyLifecycle-time for
	// serve.Rig interface minimalism, so it is fixed for the Lifecycle's whole lifetime.
	allowConfigMismatch bool

	// security limitFactory mints a FRESH *security.Limit per NewSession/RestoreSession. Reusing one
	// Lifecycle across concurrent sessions must never reuse mutable security limit state. The
	// Lifecycle therefore mints a per-session state here and injects it via WithSecurityLimit so
	// the session's security limit source is isolated. Every loop PermissionFactory receives this
	// exact live source through tool.Bindings, so native permission checkers can select
	// postures on each Check. When the factory is nil the Lifecycle falls
	// back to today's behavior — the session default-mints its own internal security limit state.
	securityLimitFactory SecurityLimitFactory
	fingerprint          FingerprintProvider
	frozenFingerprint    *event.ConfigFingerprint
	// frozenManifest is the rig-assembled ConfigManifest counterpart to
	// frozenFingerprint. When set it is stamped onto the construction-time
	// SessionStarted alongside the legacy fingerprint. Nil leaves the additive
	// Manifest field at its zero value (the deprecation-window default).
	frozenManifest *event.ConfigManifest

	// placement is the OPTIONAL managed-workspace placement (exclusive/per-session/shared)
	// resolved per session by NewSession/RestoreSession. The zero value (PlacementNone) means
	// no managed workspace: no root resolution, no lease, no coordinator — the historical
	// default. Captured once via WithLifecyclePlacement.
	placement WorkspacePlacement

	// offloadGC is the OPTIONAL session offload-blob GC cadence (WithLifecycleOffloadGC). The
	// zero value (unconfigured) wires no gate and no runner — the journal is used undecorated.
	// When configured, NewSession and RestoreSession install a journal-admission gate + GC
	// runner that reaps orphaned offload blobs while the session lease is held.
	offloadGC OffloadGCPolicy

	// hustles and hustleLimits are immutable design-time inputs captured for the
	// session's private hustle controller.
	hustles      []hustle.Definition
	hustleLimits HustleLimits
}

// HustleLimits is sessionruntime's narrow, rig-independent copy of the hustle
// lane and cleanup bounds.
type HustleLimits struct {
	BlockingConcurrent   int
	BlockingQueued       int
	BackgroundConcurrent int
	BackgroundQueued     int
	AuditTimeout         time.Duration
	FinalizationTimeout  time.Duration
	WorkerDrainTimeout   time.Duration
}

// WithLifecycleHustles captures immutable hustle registrations for both
// NewSession and RestoreSession composition.
func WithLifecycleHustles(definitions []hustle.Definition, limits HustleLimits) LifecycleOption {
	captured := append([]hustle.Definition(nil), definitions...)
	return func(r *Lifecycle) {
		r.hustles = append([]hustle.Definition(nil), captured...)
		r.hustleLimits = limits
	}
}

// WithLifecycleOffloadGC captures the session offload-blob GC cadence. An unconfigured
// (zero) policy is ignored. Forwarded to both NewSession and RestoreSession, which wire the
// journal-admission gate + GC runner over the session lease.
func WithLifecycleOffloadGC(policy OffloadGCPolicy) LifecycleOption {
	return func(r *Lifecycle) {
		if policy.Configured() {
			r.offloadGC = policy
		}
	}
}

// buildOffloadGCRunner assembles a session's offload-GC runner from the store, session id,
// and single-writer lease, driving the shared admission gate's writer. It depends only on
// the narrow ObjectGC scanner + the gate + the lease-loss signal (Dependency Inversion),
// with a production time-ticker over the policy interval. The caller wraps the session
// journal with the SAME gate so every append serializes against a GC pass.
func buildOffloadGCRunner(store *sessionstore.Store, id uuid.UUID, lease journal.Lease, gate *journalAdmissionGate, policy OffloadGCPolicy) (*offloadGCRunner, error) {
	objGC, err := store.OpenObjectGC(id, lease)
	if err != nil {
		return nil, err
	}
	interval := policy.Interval
	return newOffloadGCRunner(id, objGC, gate, func() offloadGCTicker { return newTimeTicker(interval) }, lease.Lost(), policy.Timeout), nil
}

// wrapJournalWithOffloadGC is the ONE composition-root seam shared by NewSession and the
// restore constructor: when the policy is armed it mints the admission gate, builds the GC
// runner over the same gate + session lease, and returns the journal wrapped so every append
// serializes against a GC pass. An unconfigured policy returns the journal undecorated and a
// nil runner (unchanged behavior). It never touches the lease lifecycle; the caller releases
// it on error.
func wrapJournalWithOffloadGC(store *sessionstore.Store, id uuid.UUID, lease journal.Lease, j journal.SessionJournal, policy OffloadGCPolicy) (journal.SessionJournal, *offloadGCRunner, error) {
	if !policy.Configured() {
		return j, nil, nil
	}
	admissionGate := newJournalAdmissionGate()
	runner, err := buildOffloadGCRunner(store, id, lease, admissionGate, policy)
	if err != nil {
		return nil, nil, err
	}
	return newGatedJournal(j, admissionGate), runner, nil
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

// SecurityLimitFactory mints a fresh security limit state. The Lifecycle calls it once per
// NewSession/RestoreSession so each session gets its own independent clamp (AMBIGUITY A1 on
// Lifecycle.security limitFactory). It is a named type per the codebase's prefer-named-types rule.
type SecurityLimitFactory func() *security.Limit

// LifecycleOption configures a Lifecycle at NewTopologyLifecycle time. Every caller-facing knob is captured
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

// WithLifecycleFingerprint captures a rig-time frozen compatibility fingerprint.
func WithLifecycleFingerprint(fingerprint event.ConfigFingerprint) LifecycleOption {
	return func(r *Lifecycle) {
		copy := fingerprint
		r.frozenFingerprint = &copy
	}
}

// WithLifecycleManifest captures the rig-assembled ConfigManifest counterpart to the
// frozen fingerprint. It is stamped onto the construction-time SessionStarted's
// additive Manifest field, giving a newly created session a real (SchemaVersion>=1)
// manifest baseline.
func WithLifecycleManifest(manifest event.ConfigManifest) LifecycleOption {
	return func(r *Lifecycle) {
		copy := manifest
		r.frozenManifest = &copy
	}
}

// WithLifecycleWorkspaceCheckpointing captures the workspace snapshot store and root the session
// checkpoints into (and RestoreSession materializes from). A nil store is ignored. Forwarded to
// both NewSession and RestoreSession as WithWorkspaceCheckpointing.
func WithLifecycleWorkspaceCheckpointing(ws *workspacestore.Store, root string) LifecycleOption {
	return func(r *Lifecycle) {
		if ws != nil {
			r.baseOpts = append(r.baseOpts, WithWorkspaceCheckpointing(ws, root))
		}
	}
}

// WithLifecycleSnapshotPolicy captures the validated native checkpoint policy and
// forwards it to every new/restored session. Rig enforces that it is paired with a
// managed placement.
func WithLifecycleSnapshotPolicy(policy SnapshotPolicy) LifecycleOption {
	return func(r *Lifecycle) {
		r.baseOpts = append(r.baseOpts, WithSnapshotPolicy(policy))
	}
}

// WithLifecycleForeignBuilders captures the composition-root seams that construct foreign-
// engine loops (live + restored). Either seam being nil leaves foreign engines unsupported,
// so both are captured together. Forwarded to both NewSession and RestoreSession as WithForeignBuilders.
func WithLifecycleForeignBuilders(b foreign.Builder, rb foreign.RestoredBuilder) LifecycleOption {
	return func(r *Lifecycle) {
		if b != nil && rb != nil {
			r.baseOpts = append(r.baseOpts, WithForeignBuilders(b, rb))
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
// NewTopologyLifecycle-time (fixed for the Lifecycle's lifetime) so the narrow serve.Rig interface
// exposes no per-call knob. NewSession ignores it; only RestoreSession honors it.
func WithLifecycleAllowConfigMismatch() LifecycleOption {
	return func(r *Lifecycle) {
		r.allowConfigMismatch = true
	}
}

// WithLifecycleSecurityLimitFactory captures the factory the Lifecycle calls to mint a FRESH
// *security.Limit for each NewSession/RestoreSession. A nil factory is ignored (the session default-mints
// its own internal state). See AMBIGUITY A1 on Lifecycle.security limitFactory for why the security limit
// must be per-session and what the Lifecycle deliberately leaves to the composition root.
func WithLifecycleSecurityLimitFactory(factory SecurityLimitFactory) LifecycleOption {
	return func(r *Lifecycle) {
		if factory != nil {
			r.securityLimitFactory = factory
		}
	}
}

// NewTopologyLifecycle binds an immutable, validated multi-primer graph to storage.
func NewTopologyLifecycle(topology Topology, store *sessionstore.Store, opts ...LifecycleOption) (*Lifecycle, error) {
	if store == nil {
		return nil, &MissingStoreError{}
	}
	topology = cloneTopology(topology)
	_, ok := topology.definition(topology.ActivePrimer)
	if !ok {
		return nil, &MissingTopologyError{}
	}
	r := &Lifecycle{topology: topology, store: store}
	for _, opt := range opts {
		opt(r)
	}
	if r.fingerprint == nil && r.frozenFingerprint == nil {
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
	// journal. The event appender carries the NewTopologyLifecycle-built catalog so the status fold stays
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
	// Offload GC: wrap the journal with the admission gate BEFORE any appender is built over
	// it, so every append (event/command/gate/fence) serializes against a GC pass. Unconfigured
	// leaves j undecorated and gcRunner nil.
	j, gcRunner, err := wrapJournalWithOffloadGC(r.store, sid, lease, j, r.offloadGC)
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
	opts = append(opts, withSessionHustles(r.hustles, r.hustleLimits))
	opts = append(opts,
		WithSessionID(sid),
		WithEventAppender(evAp),
		WithCommandAppender(cmdAp),
		WithGateAppender(gateAp),
		WithLeaseRelease(lease.Release),
	)
	if gcRunner != nil {
		opts = append(opts, withOffloadGCRunner(gcRunner))
	}
	if r.frozenFingerprint != nil {
		opts = append(opts, WithFingerprint(*r.frozenFingerprint))
	} else {
		opts = append(opts, WithFingerprintProvider(r.fingerprint))
	}
	if r.frozenManifest != nil {
		opts = append(opts, WithManifest(*r.frozenManifest))
	}
	// AMBIGUITY A1: mint a fresh per-session security limit state so concurrent sessions never share one
	// mutable clamp. A configured factory returning nil fails closed; only an absent factory
	// selects the session's internal default.
	if r.securityLimitFactory != nil {
		state := r.securityLimitFactory()
		if state == nil {
			releaseLease(lease)
			return nil, &NewSessionError{Kind: NewSessionSecurityLimitFailed, Cause: &NilSecurityLimitError{}}
		}
		opts = append(opts, WithSecurityLimit(state))
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
		opts = append(opts, withInitialWorkspaceCheckpoint(seed))
	}

	s, err := NewTopology(ctx, r.topology, opts...)
	if err != nil {
		// A failure after the Session accepted its hooks is already owned by its
		// synchronous/background construction cleanup (which stops the runner via
		// abortConstruction). Earlier failures release here; the runner was never started,
		// so stopping it only halts its (not-yet-built) ticker.
		if !constructionCleanupOwned(err) {
			if gcRunner != nil {
				gcRunner.Stop()
			}
			releaseResolvedRoot(ctx, resolved)
			releaseLease(lease)
		}
		return nil, &NewSessionError{Kind: NewSessionRuntimeFailed, Cause: err}
	}
	// Gate prepare records retain their private journal encoding; public open/resolve
	// transitions use the checked hub path so live subscribers see them as well.
	s.gateAppender = &liveGateAppender{prepared: gateAp, publisher: s}
	// The session now owns both leases (via WithLeaseRelease + withResolvedPlacement); start
	// the exclusive root-lease loss watcher so ownership loss faults the session, and arm the
	// offload-GC runner now the hub exists (bound to hub.IsIdle).
	s.watchRootLease()
	s.startOffloadGC()
	return s, nil
}

// releaseResolvedRoot releases a resolved placement's exclusive root lease best-effort on
// a NewSession failure path (before the session takes ownership). Nil-safe.
func releaseResolvedRoot(_ context.Context, resolved *resolvedPlacement) {
	if resolved == nil || resolved.rootRelease == nil {
		return
	}
	releaseCtx, cancel := context.WithTimeout(context.Background(), leaseReleaseTimeout)
	defer cancel()
	_ = resolved.rootRelease(releaseCtx)
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
	opts = append(opts, withSessionHustles(r.hustles, r.hustleLimits))
	if r.frozenFingerprint != nil {
		opts = append(opts, WithFingerprint(*r.frozenFingerprint))
	} else {
		opts = append(opts, WithFingerprintProvider(r.fingerprint))
	}
	if r.frozenManifest != nil {
		opts = append(opts, WithManifest(*r.frozenManifest))
	}
	if r.allowConfigMismatch {
		opts = append(opts, WithAllowConfigMismatch())
	}
	// AMBIGUITY A1: mint a fresh per-session security limit on restore too (WithSecurityLimit applies to
	// RestoreSession, which re-seeds the injected state from the folded SecurityLimitChanged
	// events), so a restored session gets its own clamp just like a fresh NewSession.
	if r.securityLimitFactory != nil {
		state := r.securityLimitFactory()
		if state == nil {
			return nil, &NilSecurityLimitError{}
		}
		opts = append(opts, WithSecurityLimit(state))
	}
	if r.placement.Configured() {
		opts = append(opts, withPlacementSpec(r.placement))
	}
	if r.offloadGC.Configured() {
		opts = append(opts, withOffloadGCPolicy(r.offloadGC))
	}
	return RestoreTopology(ctx, r.topology, id, r.store, opts...)
}
