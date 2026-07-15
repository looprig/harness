package rig

import (
	"strings"

	"github.com/looprig/harness/internal/sessionruntime"
	"github.com/looprig/harness/pkg/hustle"
	"github.com/looprig/harness/pkg/identity"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/sessionstore"
)

type definitionState struct {
	loops             []loop.Definition
	hustles           []hustle.Definition
	hustleLimits      HustleLimits
	primers           []string
	activePrimer      string
	store             *sessionstore.Store
	storeSet          bool
	seen              map[singletonKey]bool
	lifecycleOptions  []sessionruntime.LifecycleOption
	fingerprintFields ConfigFingerprintFields
	// placements accumulates every workspace placement option. Define enforces at most
	// one; more than one is a typed rejection.
	placements     []pendingPlacement
	snapshotPolicy *SnapshotPolicy
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
	if err := validateHustleRegistration(state); err != nil {
		return nil, err
	}
	if err := validateCompactionHustles(state.loops, state.hustles); err != nil {
		return nil, err
	}
	// Resolve the (at-most-one) workspace placement: canonicalize the root/base, derive the
	// exclusive root lease name, and enforce non-nil dependencies. A workspace-requiring
	// tool with NO placement makes the rig invalid.
	placement, region, err := resolvePlacement(state.placements)
	if err != nil {
		return nil, err
	}
	if !placement.Configured() && requiresWorkspaceTool(state.loops) {
		return nil, &WorkspacePlacementError{Kind: WorkspaceToolWithoutPlacement}
	}
	if placement.Configured() && state.snapshotPolicy == nil {
		return nil, &SnapshotPolicyError{Kind: SnapshotPolicyRequired}
	}
	if !placement.Configured() && state.snapshotPolicy != nil {
		return nil, &SnapshotPolicyError{Kind: SnapshotPolicyWithoutWorkspace}
	}
	if placement.Mode == sessionruntime.PlacementShared && state.snapshotPolicy != nil && state.snapshotPolicy.Priority == SnapshotRequired {
		return nil, &SnapshotPolicyError{Kind: SnapshotPolicySharedRequired}
	}
	if placement.Configured() {
		if err := checkPersistenceOverlap(state.store, placement, region); err != nil {
			return nil, err
		}
	}

	fields := state.fingerprintFields
	if placement.Configured() {
		// Fold the placement mode + canonical region into the workspace-root fingerprint
		// field so a placement change (mode or path) is a config change.
		fields.WorkspaceRoot = placementFingerprint(placement, region)
	}
	fingerprint := frozenFingerprintWithHustles(fields, state.loops, state.primers, state.activePrimer, state.hustles, state.hustleLimits)
	lifecycleOptions := append([]sessionruntime.LifecycleOption(nil), state.lifecycleOptions...)
	if len(state.hustles) > 0 {
		lifecycleOptions = append(lifecycleOptions, sessionruntime.WithLifecycleHustles(
			append([]hustle.Definition(nil), state.hustles...),
			lifecycleHustleLimits(state.hustleLimits),
		))
	}
	if placement.Configured() {
		lifecycleOptions = append(lifecycleOptions, sessionruntime.WithLifecyclePlacement(placement))
		policy := *state.snapshotPolicy
		internalPolicy := sessionruntime.SnapshotPolicy{Timeout: policy.Timeout}
		switch policy.Trigger {
		case SnapshotManual:
			internalPolicy.Trigger = sessionruntime.SnapshotManual
		case SnapshotOnIdle:
			internalPolicy.Trigger = sessionruntime.SnapshotOnIdle
		case SnapshotOnTurnDone:
			internalPolicy.Trigger = sessionruntime.SnapshotOnTurnDone
		case SnapshotOnStepDone:
			internalPolicy.Trigger = sessionruntime.SnapshotOnStepDone
		}
		if policy.Priority == SnapshotRequired {
			internalPolicy.Priority = sessionruntime.SnapshotRequired
		} else {
			internalPolicy.Priority = sessionruntime.SnapshotBestEffort
		}
		lifecycleOptions = append(lifecycleOptions, sessionruntime.WithLifecycleSnapshotPolicy(internalPolicy))
	}
	lifecycleOptions = append(lifecycleOptions, sessionruntime.WithLifecycleFingerprint(fingerprint))
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

func lifecycleHustleLimits(limits HustleLimits) sessionruntime.HustleLimits {
	return sessionruntime.HustleLimits{
		BlockingConcurrent:   limits.BlockingConcurrent,
		BlockingQueued:       limits.BlockingQueued,
		BackgroundConcurrent: limits.BackgroundConcurrent,
		BackgroundQueued:     limits.BackgroundQueued,
		AuditTimeout:         limits.AuditTimeout,
		FinalizationTimeout:  limits.FinalizationTimeout,
		WorkerDrainTimeout:   limits.WorkerDrainTimeout,
	}
}

func validateHustleRegistration(state *definitionState) error {
	if len(state.hustles) == 0 {
		if state.seen[keyHustleLimits] {
			return &DefinitionError{Kind: DefinitionUnusedHustleLimits}
		}
		return nil
	}
	if !state.seen[keyHustleLimits] {
		return &DefinitionError{Kind: DefinitionMissingHustleLimits}
	}
	seen := make(map[hustle.Name]struct{}, len(state.hustles))
	for _, definition := range state.hustles {
		name := definition.Name()
		if name == "" || definition.PolicyRevision() == "" {
			return &DefinitionError{Kind: DefinitionInvalidHustle, Name: string(name)}
		}
		if _, exists := seen[name]; exists {
			return &DefinitionError{Kind: DefinitionDuplicateHustle, Name: string(name)}
		}
		seen[name] = struct{}{}
	}
	return nil
}

// validateCompactionHustles runs only after loop and hustle registration have
// both been frozen and checked. Task 21 can enforce the definition-time lane and
// model-source contract; Task 25's focused adapter owns concrete XML/output
// validation and deliberately does not widen the generic hustle descriptor here.
func validateCompactionHustles(loops []loop.Definition, definitions []hustle.Definition) error {
	byName := make(map[hustle.Name]hustle.Definition, len(definitions))
	for _, definition := range definitions {
		byName[definition.Name()] = definition
	}
	for _, loopDefinition := range loops {
		policy, configured := loopDefinition.CompactionPolicy()
		if !configured {
			continue
		}
		definition, exists := byName[policy.Hustle]
		if !exists {
			return &DefinitionError{Kind: DefinitionMissingCompactionHustle, Name: string(policy.Hustle)}
		}
		descriptor := definition.Descriptor()
		if err := descriptor.Validate(); err != nil {
			return &DefinitionError{Kind: DefinitionIncompatibleCompactionHustle, Name: string(policy.Hustle), Cause: err}
		}
		if descriptor.Participation != hustle.ParticipationBlocking || descriptor.ModelSource != hustle.ModelSourceCurrentLoop {
			return &DefinitionError{Kind: DefinitionIncompatibleCompactionHustle, Name: string(policy.Hustle)}
		}
	}
	return nil
}
