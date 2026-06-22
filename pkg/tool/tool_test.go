package tool

import (
	"context"
	"testing"

	"github.com/ciram-co/looprig/pkg/content"
)

// fakeTool is a minimal InvokableTool used to prove the contract is satisfiable
// without depending on any concrete tool implementation. It implements ONLY the
// required BaseTool + InvokableTool methods (no optional capability interfaces).
type fakeTool struct {
	name   string
	result *ToolResult
}

func (f fakeTool) Info(ctx context.Context) (*ToolInfo, error) {
	return &ToolInfo{Name: f.name, Desc: "fake", Schema: []byte(`{"type":"object"}`)}, nil
}

func (f fakeTool) InvokableRun(ctx context.Context, argsJSON string) (*ToolResult, error) {
	return f.result, nil
}

// Compile-time assertions: fakeTool satisfies both contract interfaces.
var (
	_ BaseTool      = fakeTool{}
	_ InvokableTool = fakeTool{}
)

func TestTextResult(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "happy path", in: "hello", want: "hello"},
		{name: "empty string", in: "", want: ""},
		{name: "whitespace", in: "  \n\t", want: "  \n\t"},
		{name: "unicode", in: "héllo 世界", want: "héllo 世界"},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := TextResult(tt.in)
			if got == nil {
				t.Fatalf("TextResult(%q) = nil, want non-nil *ToolResult", tt.in)
			}
			if len(got.Content) != 1 {
				t.Fatalf("TextResult(%q) produced %d blocks, want exactly 1", tt.in, len(got.Content))
			}
			tb, ok := got.Content[0].(*content.TextBlock)
			if !ok {
				t.Fatalf("TextResult(%q) block type = %T, want *content.TextBlock", tt.in, got.Content[0])
			}
			if tb.Text != tt.want {
				t.Errorf("TextResult(%q).Text = %q, want %q", tt.in, tb.Text, tt.want)
			}
		})
	}
}

func TestFakeToolSatisfiesInvokable(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		tool    fakeTool
		wantTxt string
	}{
		{name: "returns text result", tool: fakeTool{name: "echo", result: TextResult("ok")}, wantTxt: "ok"},
		{name: "returns empty text result", tool: fakeTool{name: "echo", result: TextResult("")}, wantTxt: ""},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var it InvokableTool = tt.tool
			info, err := it.Info(context.Background())
			if err != nil {
				t.Fatalf("Info() error = %v, want nil", err)
			}
			if info == nil || info.Name != tt.tool.name {
				t.Fatalf("Info().Name = %v, want %q", info, tt.tool.name)
			}
			res, err := it.InvokableRun(context.Background(), `{}`)
			if err != nil {
				t.Fatalf("InvokableRun() error = %v, want nil", err)
			}
			if res == nil || len(res.Content) != 1 {
				t.Fatalf("InvokableRun() result = %v, want 1 block", res)
			}
			tb, ok := res.Content[0].(*content.TextBlock)
			if !ok {
				t.Fatalf("result block type = %T, want *content.TextBlock", res.Content[0])
			}
			if tb.Text != tt.wantTxt {
				t.Errorf("result text = %q, want %q", tb.Text, tt.wantTxt)
			}
		})
	}
}
