package tool

import (
	"context"
	"testing"

	"github.com/looprig/harness/pkg/uuid"
)

// capableTool implements every optional capability interface, proving each is
// satisfiable. fakeTool (in tool_test.go) deliberately implements NONE of them,
// proving the optionals are not folded into BaseTool/InvokableTool.
type capableTool struct{}

func (capableTool) Info(ctx context.Context) (*ToolInfo, error) { return &ToolInfo{}, nil }
func (capableTool) InvokableRun(ctx context.Context, argsJSON string) (*ToolResult, error) {
	return TextResult("ok"), nil
}
func (capableTool) Sequential() bool                    { return true }
func (capableTool) AuditSummary(argsJSON string) string { return "summary" }
func (capableTool) BuildRequest(argsJSON string, _ PreparedArtifact) (PermissionRequest, error) {
	return UnknownRequest{Tool: "capable", Summary: "does a thing"}, nil
}
func (capableTool) WriteTarget(argsJSON string) (string, bool, error) {
	return "/tmp/x", true, nil
}
func (capableTool) Prepare(ctx context.Context, callID uuid.UUID, argsJSON string) (PreparedArtifact, error) {
	return TokenArtifact{Token: callID.String()}, nil
}

// Compile-time assertions: a tool implementing each optional capability is
// assignable to that interface.
var (
	_ InvokableTool      = capableTool{}
	_ Sequential         = capableTool{}
	_ Auditable          = capableTool{}
	_ PermissionPrompter = capableTool{}
	_ WriteTarget        = capableTool{}
	_ Preparer           = capableTool{}
	_ PreparedArtifact   = TokenArtifact{}
)

// TestCapabilityInterfaces verifies, via type assertion (the runner's real
// access path), that capableTool satisfies each optional interface and that the
// minimal fakeTool satisfies NONE of them while still being an InvokableTool.
func TestCapabilityInterfaces(t *testing.T) {
	t.Parallel()

	var capable InvokableTool = capableTool{}
	var minimal InvokableTool = fakeTool{name: "minimal"}

	tests := []struct {
		name          string
		tool          InvokableTool
		wantOptionals bool
	}{
		{name: "capable tool implements all optionals", tool: capable, wantOptionals: true},
		{name: "minimal tool implements no optionals", tool: minimal, wantOptionals: false},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if _, ok := tt.tool.(Sequential); ok != tt.wantOptionals {
				t.Errorf("Sequential assertion = %v, want %v", ok, tt.wantOptionals)
			}
			if _, ok := tt.tool.(Auditable); ok != tt.wantOptionals {
				t.Errorf("Auditable assertion = %v, want %v", ok, tt.wantOptionals)
			}
			if _, ok := tt.tool.(PermissionPrompter); ok != tt.wantOptionals {
				t.Errorf("PermissionPrompter assertion = %v, want %v", ok, tt.wantOptionals)
			}
			if _, ok := tt.tool.(WriteTarget); ok != tt.wantOptionals {
				t.Errorf("WriteTarget assertion = %v, want %v", ok, tt.wantOptionals)
			}
			if _, ok := tt.tool.(Preparer); ok != tt.wantOptionals {
				t.Errorf("Preparer assertion = %v, want %v", ok, tt.wantOptionals)
			}
		})
	}
}

// TestMiddlewareChaining verifies ToolMiddleware/ToolExecuteFunc compose: a
// middleware can wrap a next func and produce a result.
func TestMiddlewareChaining(t *testing.T) {
	t.Parallel()

	var calls []string
	base := func(ctx context.Context, argsJSON string) (*ToolResult, error) {
		calls = append(calls, "base")
		return TextResult("base-ran"), nil
	}
	var mw ToolMiddleware = func(ctx context.Context, tl InvokableTool, argsJSON string, next ToolExecuteFunc) (*ToolResult, error) {
		calls = append(calls, "before")
		res, err := next(ctx, argsJSON)
		calls = append(calls, "after")
		return res, err
	}

	res, err := mw(context.Background(), capableTool{}, `{}`, base)
	if err != nil {
		t.Fatalf("middleware error = %v, want nil", err)
	}
	if res == nil || len(res.Content) != 1 {
		t.Fatalf("middleware result = %v, want 1 block", res)
	}
	want := []string{"before", "base", "after"}
	if len(calls) != len(want) {
		t.Fatalf("call order = %v, want %v", calls, want)
	}
	for i := range want {
		if calls[i] != want[i] {
			t.Fatalf("call order = %v, want %v", calls, want)
		}
	}
}
