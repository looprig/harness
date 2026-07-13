package rig

import (
	"time"

	"github.com/looprig/harness/internal/sessionruntime"
	"github.com/looprig/harness/pkg/ceiling"
	"github.com/looprig/harness/pkg/foreignloop"
	"github.com/looprig/harness/pkg/loop"
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
	keyCeilingFactory      singletonKey = "ceiling_factory"
	keySnapshots           singletonKey = "snapshots"
	keyOffloadGC           singletonKey = "offload_gc"
)

type DelegationLimits struct {
	Depth int
	Quota int
}

type GateCaps struct {
	MaxOpen    int
	MaxTimeout time.Duration
}

// CeilingFactory mints a fresh security-ceiling state for each session. A rig may
// invoke it concurrently for separate sessions, so captured mutable state must be
// concurrency-safe.
type CeilingFactory func() *ceiling.State

func WithLoops(definitions ...loop.Definition) Option {
	copyOf := append([]loop.Definition(nil), definitions...)
	return func(state *definitionState) error {
		state.loops = append(state.loops, copyOf...)
		return nil
	}
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

func WithForeignBuilders(builder foreignloop.Builder, restored foreignloop.RestoredBuilder) Option {
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

func WithCeilingFactory(factory CeilingFactory) Option {
	return func(state *definitionState) error {
		if factory == nil {
			return &DefinitionError{Kind: DefinitionInvalidCeilingFactory}
		}
		if state.seen[keyCeilingFactory] {
			return &DefinitionError{Kind: DefinitionDuplicateOption, Name: string(keyCeilingFactory)}
		}
		state.seen[keyCeilingFactory] = true
		state.lifecycleOptions = append(state.lifecycleOptions, sessionruntime.WithLifecycleCeilingFactory(sessionruntime.CeilingFactory(factory)))
		return nil
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
