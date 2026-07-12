package tools

import "github.com/looprig/harness/pkg/loop"

// Intent is the composition-root selector for how autonomous tool approval is.
// It is NOT session state — neither loop.Definition nor Session stores it; the
// composition root reads it to decide the permission wiring and then discards it.
// Zero value is Interactive (fail-secure: a human answers gates by default).
type Intent uint8

const (
	// Interactive: the full permission pipeline; gates fire and a human answers.
	Interactive Intent = iota
	// Unattended: no permission prompt; the declared allowlist approves and the
	// rest deny fail-secure. Build the inner checker WithUnattended() and Wrap it.
	Unattended
)

// Wrap returns the permission gate for this intent: Interactive returns inner
// unchanged; Unattended wraps it in a NonInteractiveGate. The caller is
// responsible for building `inner` WithUnattended() for the Unattended intent.
func (i Intent) Wrap(inner loop.PermissionGate) loop.PermissionGate {
	if i == Unattended {
		return NonInteractiveGate{Inner: inner}
	}
	return inner
}
