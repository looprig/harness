package rig

import (
	"time"

	"github.com/looprig/harness/internal/sessionruntime"
	"github.com/looprig/harness/pkg/foreign"
	"github.com/looprig/harness/pkg/hustle"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/session"
	"github.com/looprig/harness/pkg/sessionstore"
)

type Option func(*definitionState) error

// singletonKey identifies an at-most-once rig option in definitionState.seen. The keys are
// the single source of truth shared by the option setters below and the auto-active-primer
// selection in definition.go, so a rename cannot silently desynchronize the two (a mismatch
// would defeat duplicate-option detection or the single-primer auto-active default).
type singletonKey string

const (
	keyActivePrimer        singletonKey = "active_primer"
	keyDelegationLimits    singletonKey = "delegation_limits"
	keyConfigFingerprint   singletonKey = "config_fingerprint"
	keyForeignBuilder      singletonKey = "foreign_builders"
	keyGateCaps            singletonKey = "gate_caps"
	keyAllowConfigMismatch singletonKey = "allow_config_mismatch"
	keyRestoreDecider      singletonKey = "restore_decider"
	keySnapshots           singletonKey = "snapshots"
	keyOffloadGC           singletonKey = "offload_gc"
	keyHustleLimits        singletonKey = "hustle_limits"
)

// MaxHustleQueued is the largest configured waiting capacity for either hustle
// lane. The execution controller may allocate no queue larger than this bound.
const MaxHustleQueued = 10_000

type DelegationLimits struct {
	Depth int
	Quota int
}

type GateCaps struct {
	MaxOpen    int
	MaxTimeout time.Duration
}

// HustleLimits bounds the two independent execution lanes and their audit,
// finalization, and worker-drain operations.
type HustleLimits struct {
	BlockingConcurrent   int
	BlockingQueued       int
	BackgroundConcurrent int
	BackgroundQueued     int
	AuditTimeout         time.Duration
	FinalizationTimeout  time.Duration
	WorkerDrainTimeout   time.Duration
}

func WithLoops(definitions ...loop.Definition) Option {
	copyOf := append([]loop.Definition(nil), definitions...)
	return func(state *definitionState) error {
		state.loops = append(state.loops, copyOf...)
		return nil
	}
}

// WithHustles adds immutable hustle definitions to the rig.
func WithHustles(definitions ...hustle.Definition) Option {
	copyOf := append([]hustle.Definition(nil), definitions...)
	return func(state *definitionState) error {
		state.hustles = append(state.hustles, copyOf...)
		return nil
	}
}

// WithHustleLimits configures the required singleton lane bounds.
func WithHustleLimits(limits HustleLimits) Option {
	return func(state *definitionState) error {
		if state.seen[keyHustleLimits] {
			return &DefinitionError{Kind: DefinitionDuplicateOption, Name: string(keyHustleLimits)}
		}
		if invalidHustleLimits(limits) {
			return &DefinitionError{Kind: DefinitionInvalidHustleLimits}
		}
		return singleton(keyHustleLimits, func(state *definitionState) { state.hustleLimits = limits })(state)
	}
}

func invalidHustleLimits(limits HustleLimits) bool {
	return limits.BlockingConcurrent <= 0 ||
		limits.BlockingQueued < 0 || limits.BlockingQueued > MaxHustleQueued ||
		limits.BackgroundConcurrent <= 0 ||
		limits.BackgroundQueued < 0 || limits.BackgroundQueued > MaxHustleQueued ||
		limits.AuditTimeout <= 0 || limits.FinalizationTimeout <= 0 || limits.WorkerDrainTimeout <= 0
}

func WithPrimers(names ...string) Option {
	copyOf := append([]string(nil), names...)
	return func(state *definitionState) error {
		state.primers = append(state.primers, copyOf...)
		return nil
	}
}

func WithActivePrimer(name string) Option {
	return singleton(keyActivePrimer, func(state *definitionState) { state.activePrimer = name })
}

func WithSessionStore(store *sessionstore.Store) Option {
	return func(state *definitionState) error {
		if store == nil {
			return &DefinitionError{Kind: DefinitionInvalidSessionStore}
		}
		if state.storeSet {
			return &DefinitionError{Kind: DefinitionDuplicateOption, Name: "session_store"}
		}
		state.storeSet = true
		state.store = store
		return nil
	}
}

func WithDelegationLimits(limits DelegationLimits) Option {
	return func(state *definitionState) error {
		if limits.Depth < 0 || limits.Quota < 0 {
			return &DefinitionError{Kind: DefinitionInvalidDelegationLimits}
		}
		return singletonCompile(keyDelegationLimits, sessionruntime.WithLifecycleLimits(sessionruntime.Limits{Depth: limits.Depth, Quota: limits.Quota}))(state)
	}
}

func WithFingerprintFields(fields ConfigFingerprintFields) Option {
	return singleton(keyConfigFingerprint, func(state *definitionState) { state.fingerprintFields = fields })
}

func WithForeignBuilders(builder foreign.Builder, restored foreign.RestoredBuilder) Option {
	return func(state *definitionState) error {
		if builder == nil || restored == nil {
			return &DefinitionError{Kind: DefinitionInvalidForeignBuilders}
		}
		return singletonCompile(keyForeignBuilder, sessionruntime.WithLifecycleForeignBuilders(builder, restored))(state)
	}
}

func WithGateCaps(caps GateCaps) Option {
	return func(state *definitionState) error {
		if caps.MaxOpen < 0 || caps.MaxTimeout < 0 {
			return &DefinitionError{Kind: DefinitionInvalidGateCaps}
		}
		return singletonCompile(keyGateCaps, sessionruntime.WithLifecycleGateCaps(sessionruntime.GateCaps{MaxOpen: caps.MaxOpen, MaxTimeout: caps.MaxTimeout}))(state)
	}
}

func WithAllowConfigMismatch() Option {
	return singletonCompile(keyAllowConfigMismatch, sessionruntime.WithLifecycleAllowConfigMismatch())
}

// WithRestoreDecider installs the application policy that decides whether a
// configuration-drifted restore proceeds. It is the successor to
// WithAllowConfigMismatch: rather than a blanket override, the decider inspects the
// typed drift assessment and accepts or rejects. Omitting it leaves restore on the
// fail-secure session.DefaultPolicyDecider (reject on any Warn). A nil decider is
// rejected at definition time so the option cannot silently disarm the default.
func WithRestoreDecider(decider session.RestoreDecider) Option {
	return func(state *definitionState) error {
		if decider == nil {
			return &DefinitionError{Kind: DefinitionInvalidRestoreDecider}
		}
		return singletonCompile(keyRestoreDecider, sessionruntime.WithLifecycleRestoreDecider(decider))(state)
	}
}

func singleton(name singletonKey, apply func(*definitionState)) Option {
	return func(state *definitionState) error {
		if state.seen[name] {
			return &DefinitionError{Kind: DefinitionDuplicateOption, Name: string(name)}
		}
		state.seen[name] = true
		apply(state)
		return nil
	}
}

func singletonCompile(name singletonKey, option sessionruntime.LifecycleOption) Option {
	return singleton(name, func(state *definitionState) { state.lifecycleOptions = append(state.lifecycleOptions, option) })
}
