package tools

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/looprig/core/content"
	"github.com/looprig/harness/pkg/tool"
)

// fakeGrantedRunner implements BOTH tool.CommandRunner and tool.GrantedRunner. It
// records which method the Bash tool dispatched to and the grant tokens it was
// handed, so a test can assert the grant-aware routing (grants present + supported
// → RunCommandWithGrants; otherwise RunCommand).
type fakeGrantedRunner struct {
	ranPlain   bool
	ranGrants  bool
	gotCommand string
	gotDir     string
	gotGrants  []string
	out        []byte
	exit       int
	err        error
}

func (f *fakeGrantedRunner) RunCommand(_ context.Context, dir, command string) ([]byte, int, error) {
	f.ranPlain = true
	f.gotDir = dir
	f.gotCommand = command
	return f.out, f.exit, f.err
}

func (f *fakeGrantedRunner) RunCommandWithGrants(_ context.Context, dir, command string, grants []string) ([]byte, int, error) {
	f.ranGrants = true
	f.gotDir = dir
	f.gotCommand = command
	f.gotGrants = append([]string(nil), grants...)
	return f.out, f.exit, f.err
}

// Compile-time assertions: the fake satisfies both runner interfaces (so the
// sandbox Executor, which does too, is routed identically).
var (
	_ tool.CommandRunner = (*fakeGrantedRunner)(nil)
	_ tool.GrantedRunner = (*fakeGrantedRunner)(nil)
)

// runBashCtx invokes Bash with an explicit ctx + args map and returns the single
// text block; it fails on a Go error (Bash returns tool-result strings).
func runBashCtx(t *testing.T, ctx context.Context, b *Bash, args map[string]any) string {
	t.Helper()
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatalf("marshal args: %v", err)
	}
	res, err := b.InvokableRun(ctx, string(raw))
	if err != nil {
		t.Fatalf("InvokableRun returned a Go error %v; Bash returns tool-result strings", err)
	}
	if res == nil || len(res.Content) != 1 {
		t.Fatalf("result = %v, want exactly 1 block", res)
	}
	tb, ok := res.Content[0].(*content.TextBlock)
	if !ok {
		t.Fatalf("block type = %T, want *content.TextBlock", res.Content[0])
	}
	return tb.Text
}

// TestBashGrantDispatch exercises the merge + GrantedRunner routing: grants from
// the args, from the ctx (via tool.WithGrants), and both merged (union, dedup,
// order-stable, args first). No grants → the plain RunCommand path (never the
// grants method).
func TestBashGrantDispatch(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		argsGrants  []string // nil => omit the "grants" arg entirely
		ctxGrants   []string // nil => don't call tool.WithGrants
		wantGranted bool     // expect RunCommandWithGrants (not RunCommand)
		wantGrants  []string // tokens the grants method should receive
	}{
		{
			name:        "args grants route to the grants method",
			argsGrants:  []string{"tok-a"},
			wantGranted: true,
			wantGrants:  []string{"tok-a"},
		},
		{
			name:        "ctx grants are merged in",
			ctxGrants:   []string{"tok-ctx"},
			wantGranted: true,
			wantGrants:  []string{"tok-ctx"},
		},
		{
			name:        "args and ctx union, dedup, args first",
			argsGrants:  []string{"tok-a", "tok-b"},
			ctxGrants:   []string{"tok-b", "tok-c"},
			wantGranted: true,
			wantGrants:  []string{"tok-a", "tok-b", "tok-c"},
		},
		{
			name:        "duplicate args grants are de-duplicated",
			argsGrants:  []string{"tok-a", "tok-a"},
			wantGranted: true,
			wantGrants:  []string{"tok-a"},
		},
		{
			name:        "no grants uses the plain RunCommand path",
			wantGranted: false,
		},
		{
			name:        "empty args grants slice uses the plain path",
			argsGrants:  []string{},
			wantGranted: false,
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			fake := &fakeGrantedRunner{out: []byte("ROUTED\n"), exit: 0}
			b := NewBash(root, WithRunner(fake))

			ctx := context.Background()
			if tt.ctxGrants != nil {
				ctx = tool.WithGrants(ctx, tt.ctxGrants)
			}
			args := map[string]any{"command": "echo hi"}
			if tt.argsGrants != nil {
				args["grants"] = tt.argsGrants
			}

			out := runBashCtx(t, ctx, b, args)

			if fake.ranGrants != tt.wantGranted {
				t.Fatalf("ranGrants = %v, want %v", fake.ranGrants, tt.wantGranted)
			}
			if fake.ranPlain != !tt.wantGranted {
				t.Fatalf("ranPlain = %v, want %v", fake.ranPlain, !tt.wantGranted)
			}
			if tt.wantGranted && !slices.Equal(fake.gotGrants, tt.wantGrants) {
				t.Errorf("grants handed to runner = %#v, want %#v", fake.gotGrants, tt.wantGrants)
			}
			if fake.gotCommand != "echo hi" {
				t.Errorf("runner saw command %q, want %q", fake.gotCommand, "echo hi")
			}
			if fake.gotDir != root {
				t.Errorf("runner saw dir %q, want %q", fake.gotDir, root)
			}
			if !strings.Contains(out, "ROUTED") {
				t.Errorf("result %q missing the runner's output", out)
			}
		})
	}
}

// TestBashGrantsCommandRunnerOnlyFallsBack asserts that when grants are present but
// the injected runner implements ONLY tool.CommandRunner (no GrantedRunner), Bash
// falls back to RunCommand without panicking. The grants are simply ignored at the
// exec layer (the gate still saw them) — the correct 17a behavior.
func TestBashGrantsCommandRunnerOnlyFallsBack(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	fake := &fakeCommandRunner{out: []byte("PLAIN\n"), exit: 0}
	b := NewBash(root, WithRunner(fake))

	out := runBashCtx(t, context.Background(), b, map[string]any{
		"command": "echo hi",
		"grants":  []string{"tok-a"},
	})

	if fake.calls != 1 {
		t.Fatalf("RunCommand calls = %d, want 1 (grants present but runner is CommandRunner-only → fall back)", fake.calls)
	}
	if fake.gotCommand != "echo hi" {
		t.Errorf("runner saw command %q, want %q", fake.gotCommand, "echo hi")
	}
	if !strings.Contains(out, "PLAIN") {
		t.Errorf("result %q missing the runner's output", out)
	}
}

// TestBashGrantsNilRunnerDirectExec asserts grants present with NO injected runner
// (the bare-harness default) still direct-execs via sh -c without panicking; the
// grants are ignored at the exec layer.
func TestBashGrantsNilRunnerDirectExec(t *testing.T) {
	t.Parallel()
	requireSh(t)
	b := NewBash(t.TempDir()) // nil runner
	out := runBashCtx(t, context.Background(), b, map[string]any{
		"command": "echo hello",
		"grants":  []string{"tok-a"},
	})
	if !strings.Contains(out, "hello") || !strings.Contains(out, "[exit code: 0]") {
		t.Errorf("nil-runner Bash with grants did not direct-exec; got %q", out)
	}
}

// TestBashSchemaHasGrants asserts the JSON schema advertises the optional "grants"
// array-of-strings property (so the model can attach tokens) and that "grants" is
// NOT in the required set.
func TestBashSchemaHasGrants(t *testing.T) {
	t.Parallel()
	info, err := NewBash(t.TempDir()).Info(context.Background())
	if err != nil {
		t.Fatalf("Info() error = %v", err)
	}
	var schema struct {
		Properties map[string]struct {
			Type  string `json:"type"`
			Items struct {
				Type string `json:"type"`
			} `json:"items"`
		} `json:"properties"`
		Required []string `json:"required"`
	}
	if err := json.Unmarshal(info.Schema, &schema); err != nil {
		t.Fatalf("Schema is not the expected JSON object: %v", err)
	}
	g, ok := schema.Properties["grants"]
	if !ok {
		t.Fatal("schema is missing the 'grants' property")
	}
	if g.Type != "array" || g.Items.Type != "string" {
		t.Errorf("grants property = {type:%q items.type:%q}, want array of string", g.Type, g.Items.Type)
	}
	if slices.Contains(schema.Required, "grants") {
		t.Error("'grants' must be optional (not in required)")
	}
}
