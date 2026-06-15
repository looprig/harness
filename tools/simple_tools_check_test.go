package tools

import (
	"context"
	"testing"

	"github.com/inventivepotter/urvi/internal/agent/loop"
	"github.com/inventivepotter/urvi/internal/tool"
)

// simple_tools_check_test.go pins the security CLASSIFICATION of the two SIMPLE,
// AutoApprove tools (AskUser, Todo): they carry NO path/command boundary, so
// classifyTool puts them in classUnknown and Check clears Stages 1–2 (no boundary
// to cross). They therefore reach AutoApprove ONLY via the manifest's HardApprove
// list — and default to Ask when not hard-approved. This is the integration the
// tools/check.go contract requires and is the manifest's responsibility to wire.
func TestSimpleToolsCheckClassification(t *testing.T) {
	t.Parallel()
	ws := t.TempDir()

	tools := []struct {
		name string
		t    tool.InvokableTool
		args string
	}{
		{name: "AskUser", t: NewAskUser(), args: `{"question":"proceed?","choices":["yes","no"]}`},
		{name: "Todo", t: NewTodo(), args: `{"action":"create","title":"x"}`},
	}

	tests := []struct {
		name        string
		hardApprove []string
		want        loop.Effect
	}{
		{name: "not hard-approved defaults to Ask", hardApprove: nil, want: loop.EffectAsk},
		{name: "named in HardApprove auto-approves", hardApprove: nil /*per-tool below*/, want: loop.EffectAutoApprove},
		{name: "wildcard hard-approve auto-approves", hardApprove: []string{wildcardTool}, want: loop.EffectAutoApprove},
	}

	for _, tl := range tools {
		tl := tl
		for _, tt := range tests {
			tt := tt
			t.Run(tl.name+"/"+tt.name, func(t *testing.T) {
				t.Parallel()
				approve := tt.hardApprove
				// The "named in HardApprove" case approves the specific tool by name.
				if tt.want == loop.EffectAutoApprove && approve == nil {
					approve = []string{tl.name}
				}
				pc := NewPermissionChecker(PermissionPolicy{
					WorkspaceRoot: ws,
					HardDeny:      DefaultHardDeny(),
					HardApprove:   HardApproveRules{Tools: approve},
				})
				got := pc.Check(context.Background(), tl.t, tl.name, tl.args)
				if got != tt.want {
					t.Errorf("Check(%s) = %v, want %v", tl.name, got, tt.want)
				}
			})
		}
	}
}
