package tools

import (
	"context"

	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/tool"
)

// NonInteractiveGate makes any loop.PermissionGate safe to run with no human
// present: a would-be prompt (EffectAsk) becomes a fail-secure EffectDeny, so an
// unattended turn continues with a denied tool result and NEVER parks.
// EffectAutoApprove and EffectDeny pass through unchanged, so the inner gate's
// declared allowlist and the non-bypassable safety floor are fully honored. Wrap
// only for the Unattended posture (pair with the WithUnattended checker option).
// It is Open/Closed — it wraps, it does not modify, the checker.
type NonInteractiveGate struct {
	// Inner is the wrapped gate (typically a *PermissionChecker built WithUnattended).
	Inner loop.PermissionGate
}

var _ loop.PermissionGate = NonInteractiveGate{}

func (g NonInteractiveGate) Check(ctx context.Context, t tool.InvokableTool, name, argsJSON string) loop.Effect {
	if e := g.Inner.Check(ctx, t, name, argsJSON); e != loop.EffectAsk {
		return e // EffectAutoApprove / EffectDeny pass through
	}
	return loop.EffectDeny // fail-secure: no one to ask
}

func (g NonInteractiveGate) Grant(ctx context.Context, name, argsJSON string, scope tool.ApprovalScope) error {
	return g.Inner.Grant(ctx, name, argsJSON, scope) // pass-through; no gate is ever opened
}
