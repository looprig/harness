package loop

import (
	"context"

	"github.com/looprig/harness/pkg/tool"
)

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

// AttenuateBoundPermission returns a private bound view whose permission decision is
// never more permissive than either the child's own gate or its live parent gate.
// A nil parent is the runtime's fail-secure no-gate state and therefore remains nil.
func AttenuateBoundPermission(bound BoundDefinition, parent PermissionGate) BoundDefinition {
	state, ok := bound.(*boundDefinitionState)
	if !ok || state == nil {
		return bound
	}
	clone := *state
	if parent == nil || state.permission == nil {
		clone.permission = nil
	} else {
		clone.permission = &attenuatedPermissionGate{parent: parent, child: state.permission}
	}
	return &clone
}

type attenuatedPermissionGate struct {
	parent PermissionGate
	child  PermissionGate
}

func (g *attenuatedPermissionGate) Check(ctx context.Context, t tool.InvokableTool, name, args string) Effect {
	return restrictiveEffect(g.parent.Check(ctx, t, name, args), g.child.Check(ctx, t, name, args))
}

func (g *attenuatedPermissionGate) CheckDecision(ctx context.Context, t tool.InvokableTool, name, args string) PermissionDecision {
	parent := permissionDecision(ctx, g.parent, t, name, args)
	child := permissionDecision(ctx, g.child, t, name, args)
	if restrictiveEffect(parent.Effect, child.Effect) == parent.Effect {
		return parent
	}
	return child
}

func (g *attenuatedPermissionGate) Grant(ctx context.Context, name, args string, scope tool.ApprovalScope) error {
	if err := g.parent.Grant(ctx, name, args, scope); err != nil {
		return err
	}
	return g.child.Grant(ctx, name, args, scope)
}

func permissionDecision(ctx context.Context, gate PermissionGate, t tool.InvokableTool, name, args string) PermissionDecision {
	if decisionGate, ok := gate.(interface {
		CheckDecision(context.Context, tool.InvokableTool, string, string) PermissionDecision
	}); ok {
		return decisionGate.CheckDecision(ctx, t, name, args)
	}
	return PermissionDecision{Effect: gate.Check(ctx, t, name, args)}
}

func restrictiveEffect(left, right Effect) Effect {
	if !knownEffect(left) || !knownEffect(right) {
		return EffectDeny
	}
	if left == EffectDeny || right == EffectDeny {
		return EffectDeny
	}
	if left == EffectAsk || right == EffectAsk {
		return EffectAsk
	}
	return EffectAutoApprove
}

func knownEffect(effect Effect) bool {
	return effect == EffectAutoApprove || effect == EffectAsk || effect == EffectDeny
}
