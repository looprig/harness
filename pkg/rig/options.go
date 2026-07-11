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

type DelegationLimits struct {
	Depth int
	Quota int
}

type GateCaps struct {
	MaxOpen    int
	MaxTimeout time.Duration
}

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
	return singleton("active_primer", func(state *definitionState) { state.activePrimer = name })
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
	return singletonCompile("delegation_limits", sessionruntime.WithLifecycleLimits(sessionruntime.Limits{Depth: limits.Depth, Quota: limits.Quota}))
}

func WithConfigFingerprintFields(fields ConfigFingerprintFields) Option {
	return singletonCompile("config_fingerprint", sessionruntime.WithLifecycleConfigFingerprintFields(fields))
}

func WithForeignBuilder(builder foreignloop.Builder, restored foreignloop.RestoredBuilder) Option {
	return singletonCompile("foreign_builder", sessionruntime.WithLifecycleForeignBuilder(builder, restored))
}

func WithGateCaps(caps GateCaps) Option {
	return singletonCompile("gate_caps", sessionruntime.WithLifecycleGateCaps(sessionruntime.GateCaps{MaxOpen: caps.MaxOpen, MaxTimeout: caps.MaxTimeout}))
}

func WithAllowConfigMismatch() Option {
	return singletonCompile("allow_config_mismatch", sessionruntime.WithLifecycleAllowConfigMismatch())
}

func WithCeilingFactory(factory CeilingFactory) Option {
	return singletonCompile("ceiling_factory", sessionruntime.WithLifecycleCeilingFactory(sessionruntime.CeilingFactory(factory)))
}

func singleton(name string, apply func(*definitionState)) Option {
	return func(state *definitionState) error {
		if state.seen[name] {
			return &DefinitionError{Kind: DefinitionDuplicateOption, Name: name}
		}
		state.seen[name] = true
		apply(state)
		return nil
	}
}

func singletonCompile(name string, option sessionruntime.LifecycleOption) Option {
	return singleton(name, func(state *definitionState) { state.lifecycleOptions = append(state.lifecycleOptions, option) })
}
