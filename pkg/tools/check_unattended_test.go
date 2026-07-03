package tools

import (
	"context"
	"testing"

	"github.com/looprig/harness/pkg/loop"
)

func TestUnattended_SuppressesEffectCheckerAutoApprove(t *testing.T) {
	t.Parallel()
	tool := staticEffect{name: "SelfApprove", effect: loop.EffectAutoApprove, handled: true}
	// Not on HardApprove.Tools -> under Unattended it must NOT auto-approve;
	// it falls through to Stage 7 -> EffectAsk.
	c, err := NewPermissionChecker(PermissionPolicy{}, WithUnattended())
	if err != nil {
		t.Fatal(err)
	}
	if got := c.Check(context.Background(), tool, "SelfApprove", "{}"); got == loop.EffectAutoApprove {
		t.Errorf("Unattended must not honor EffectChecker auto-approve; got %v", got)
	}
	// A plain checker DOES honor it (proves the suppression is option-scoped).
	plain, err := NewPermissionChecker(PermissionPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	if got := plain.Check(context.Background(), tool, "SelfApprove", "{}"); got != loop.EffectAutoApprove {
		t.Errorf("interactive checker should honor EffectChecker auto-approve; got %v", got)
	}
}

func TestUnattended_HonorsEffectCheckerDeny(t *testing.T) {
	t.Parallel()
	tool := staticEffect{name: "SelfDeny", effect: loop.EffectDeny, handled: true}
	c, err := NewPermissionChecker(PermissionPolicy{}, WithUnattended())
	if err != nil {
		t.Fatal(err)
	}
	if got := c.Check(context.Background(), tool, "SelfDeny", "{}"); got != loop.EffectDeny {
		t.Errorf("EffectChecker deny must still be honored under Unattended; got %v", got)
	}
}
