package rig

import (
	"strings"

	"github.com/looprig/harness/internal/sessionruntime"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/identity"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/sessionstore"
)

type definitionState struct {
	loops             []loop.Definition
	primers           []string
	activePrimer      string
	store             *sessionstore.Store
	storeSet          bool
	seen              map[singletonKey]bool
	lifecycleOptions  []sessionruntime.LifecycleOption
	fingerprintFields ConfigFingerprintFields
}

// Rig is an immutable design-time assembly that creates and restores sessions.
type Rig struct{ lifecycle *sessionruntime.Lifecycle }

func Define(options ...Option) (*Rig, error) {
	state := &definitionState{seen: make(map[singletonKey]bool)}
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
	byName := make(map[string]loop.Definition, len(state.loops))
	for _, definition := range state.loops {
		name := string(definition.Name())
		if strings.TrimSpace(name) == "" {
			return nil, &DefinitionError{Kind: DefinitionInvalidLoop}
		}
		if _, exists := byName[name]; exists {
			return nil, &DefinitionError{Kind: DefinitionDuplicateLoop, Name: name}
		}
		byName[name] = definition
	}
	if len(state.primers) == 0 {
		return nil, &DefinitionError{Kind: DefinitionMissingPrimer}
	}
	seenPrimers := make(map[string]bool, len(state.primers))
	for _, primer := range state.primers {
		if seenPrimers[primer] {
			return nil, &DefinitionError{Kind: DefinitionInvalidPrimer, Name: primer}
		}
		seenPrimers[primer] = true
		if _, exists := byName[primer]; !exists {
			return nil, &DefinitionError{Kind: DefinitionInvalidPrimer, Name: primer}
		}
	}
	if len(state.primers) == 1 && !state.seen[keyActivePrimer] {
		state.activePrimer = state.primers[0]
	}
	if state.activePrimer == "" || !seenPrimers[state.activePrimer] {
		return nil, &DefinitionError{Kind: DefinitionInvalidActivePrimer, Name: state.activePrimer}
	}
	for _, definition := range state.loops {
		for _, delegate := range definition.Delegates() {
			name := string(delegate)
			if _, exists := byName[name]; !exists {
				return nil, &DefinitionError{Kind: DefinitionInvalidLoop, Name: name}
			}
		}
	}
	queue := append([]string(nil), state.primers...)
	visited := make(map[string]bool, len(byName))
	for len(queue) > 0 {
		name := queue[0]
		queue = queue[1:]
		if visited[name] {
			continue
		}
		visited[name] = true
		for _, delegate := range byName[name].Delegates() {
			queue = append(queue, string(delegate))
		}
	}
	for name := range byName {
		if !visited[name] {
			return nil, &DefinitionError{Kind: DefinitionInvalidLoop, Name: name}
		}
	}
	fields := state.fingerprintFields
	provider := sessionruntime.FingerprintProvider(func(definition loop.BoundDefinition) event.ConfigFingerprint {
		return fingerprintWithTopology(definition, fields, state.loops, state.primers, state.activePrimer)
	})
	lifecycleOptions := append([]sessionruntime.LifecycleOption(nil), state.lifecycleOptions...)
	lifecycleOptions = append(lifecycleOptions, sessionruntime.WithLifecycleFingerprintProvider(provider))
	primerNames := make([]identity.AgentName, len(state.primers))
	for i, name := range state.primers {
		primerNames[i] = identity.AgentName(name)
	}
	lifecycle, err := sessionruntime.NewTopologyLifecycle(sessionruntime.Topology{Definitions: append([]loop.Definition(nil), state.loops...), Primers: primerNames, ActivePrimer: identity.AgentName(state.activePrimer)}, state.store, lifecycleOptions...)
	if err != nil {
		return nil, &DefinitionError{Kind: DefinitionInvalidSessionStore, Cause: err}
	}
	return &Rig{lifecycle: lifecycle}, nil
}
