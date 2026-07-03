package tools

import (
	"testing"

	"github.com/looprig/harness/pkg/loop"
)

func TestIntent_Wrap(t *testing.T) {
	t.Parallel()
	inner := &stubGate{effect: loop.EffectAsk}
	// Interactive: no wrapping — returns inner unchanged.
	if got := Interactive.Wrap(inner); got != loop.PermissionGate(inner) {
		t.Errorf("Interactive.Wrap must return inner unchanged")
	}
	// Unattended: wraps with NonInteractiveGate.
	if _, ok := Unattended.Wrap(inner).(NonInteractiveGate); !ok {
		t.Errorf("Unattended.Wrap must return a NonInteractiveGate")
	}
}
