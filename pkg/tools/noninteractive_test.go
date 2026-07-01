package tools

import (
	"context"
	"testing"

	"github.com/ciram-co/looprig/pkg/loop"
	"github.com/ciram-co/looprig/pkg/tool"
)

type stubGate struct {
	effect    loop.Effect
	grantErr  error
	grantSeen bool
}

func (s stubGate) Check(context.Context, tool.InvokableTool, string, string) loop.Effect {
	return s.effect
}
func (s *stubGate) Grant(context.Context, string, string, tool.ApprovalScope) error {
	s.grantSeen = true
	return s.grantErr
}

func TestNonInteractiveGate_Check(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		in   loop.Effect
		want loop.Effect
	}{
		{"ask becomes deny", loop.EffectAsk, loop.EffectDeny},
		{"auto-approve passes through", loop.EffectAutoApprove, loop.EffectAutoApprove},
		{"deny passes through", loop.EffectDeny, loop.EffectDeny},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			g := NonInteractiveGate{Inner: &stubGate{effect: tt.in}}
			if got := g.Check(context.Background(), nil, "X", "{}"); got != tt.want {
				t.Errorf("Check(%v) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}

func TestNonInteractiveGate_GrantPassThrough(t *testing.T) {
	t.Parallel()
	inner := &stubGate{}
	g := NonInteractiveGate{Inner: inner}
	if err := g.Grant(context.Background(), "X", "{}", tool.ApprovalScope(0)); err != nil {
		t.Fatalf("Grant: %v", err)
	}
	if !inner.grantSeen {
		t.Errorf("Grant did not delegate to inner")
	}
}
