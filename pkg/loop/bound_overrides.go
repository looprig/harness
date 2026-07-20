package loop

// SelectBoundMode returns a private bound view whose default accessors resolve the
// selected effective mode. It retains every declared mode for later trusted changes.
func SelectBoundMode(bound BoundDefinition, mode ModeName) (BoundDefinition, error) {
	state, ok := bound.(*boundDefinitionState)
	if !ok || state == nil {
		return nil, &BindError{Kind: BindInvalidDefinition, Name: string(mode), Index: -1}
	}
	if _, exists := state.Mode(mode); !exists {
		return nil, &BindError{Kind: BindInvalidDefinition, Name: string(mode), Index: -1}
	}
	clone := *state
	definition := *state.definition
	definition.initialMode = mode
	clone.definition = &definition
	return &clone, nil
}

// OverrideBoundAccess returns a private bound view whose Access() resolves the
// given gate instead of the definition's own. It is the binding-time seam a
// composition root uses to give ONE bound loop a different combined access gate
// (for example a restricted evaluator for a reviewer role) without mutating the
// immutable definition.
//
// Authority differences between loops are expressed by the CONSUMER passing
// different evaluators — there is no harness-side attenuation, and a bound loop
// without an override always resolves its own definition's gate, never another
// loop's. A nil gate is rejected: overriding to "no gate" would silently turn a
// gated loop into a fail-closed-only loop through a side door; configure the
// definition without WithAccessGate instead.
func OverrideBoundAccess(bound BoundDefinition, access AccessGate) (BoundDefinition, error) {
	state, ok := bound.(*boundDefinitionState)
	if !ok || state == nil {
		return nil, &BindError{Kind: BindInvalidDefinition, Index: -1}
	}
	if nilLike(access) {
		return nil, &BindError{Kind: BindInvalidAccessGate, Index: -1}
	}
	clone := *state
	clone.accessOverride = access
	return &clone, nil
}
