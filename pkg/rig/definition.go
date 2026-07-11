package rig

import (
	"github.com/looprig/harness/internal/sessionruntime"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/sessionstore"
)

type definitionState struct {
	loops            []loop.Definition
	primers          []string
	activePrimer     string
	store            *sessionstore.Store
	storeSet         bool
	seen             map[string]bool
	lifecycleOptions []sessionruntime.LifecycleOption
}

// Rig is an immutable design-time assembly that creates and restores sessions.
type Rig struct{ lifecycle *sessionruntime.Lifecycle }

func Define(options ...Option) (*Rig, error) {
	state := &definitionState{seen: make(map[string]bool)}
	for _, option := range options {
		if option == nil {
			return nil, &DefinitionError{Kind: DefinitionNilOption}
		}
		if err := option(state); err != nil {
			return nil, err
		}
	}
	if !state.storeSet || state.store == nil {
		return nil, &DefinitionError{Kind: DefinitionMissingSessionStore}
	}
	if len(state.loops) == 0 {
		return nil, &DefinitionError{Kind: DefinitionMissingLoop}
	}
	if len(state.loops) != 1 {
		return nil, &DefinitionError{Kind: DefinitionInvalidLoop, Name: "Task 7 supports exactly one loop"}
	}
	name := string(state.loops[0].Name())
	if name == "" {
		return nil, &DefinitionError{Kind: DefinitionInvalidLoop}
	}
	if len(state.primers) == 0 {
		return nil, &DefinitionError{Kind: DefinitionMissingPrimer}
	}
	if len(state.primers) != 1 || state.primers[0] != name {
		return nil, &DefinitionError{Kind: DefinitionInvalidPrimer}
	}
	if state.activePrimer != "" && state.activePrimer != name {
		return nil, &DefinitionError{Kind: DefinitionInvalidActivePrimer, Name: state.activePrimer}
	}
	lifecycle, err := sessionruntime.NewLifecycle(state.loops[0], state.store, state.lifecycleOptions...)
	if err != nil {
		return nil, &DefinitionError{Kind: DefinitionInvalidSessionStore, Cause: err}
	}
	return &Rig{lifecycle: lifecycle}, nil
}
