package tools

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/looprig/core/content"
	"github.com/looprig/harness/pkg/tool"
)

// fakeCommandRunner is a tool.CommandRunner test double: it records the
// (dir, command) it was asked to run and returns a canned (output, exit, err).
type fakeCommandRunner struct {
	calls      int
	gotDir     string
	gotCommand string
	out        []byte
	exit       int
	err        error
}

func (f *fakeCommandRunner) RunCommand(_ context.Context, dir, command string) ([]byte, int, error) {
	f.calls++
	f.gotDir = dir
	f.gotCommand = command
	return f.out, f.exit, f.err
}

// fakeArgvRunner is a tool.ArgvRunner test double: it records the (dir, argv) it
// was asked to run and returns a canned (output, exit, err).
type fakeArgvRunner struct {
	calls   int
	gotDir  string
	gotArgv []string
	out     []byte
	exit    int
	err     error
}

func (f *fakeArgvRunner) RunArgv(_ context.Context, dir string, argv []string) ([]byte, int, error) {
	f.calls++
	f.gotDir = dir
	f.gotArgv = append([]string(nil), argv...)
	return f.out, f.exit, f.err
}

// Compile-time assertions that the fakes satisfy the stdlib-only runner
// interfaces (so the sandbox Executor satisfies them structurally too).
var (
	_ tool.CommandRunner = (*fakeCommandRunner)(nil)
	_ tool.ArgvRunner    = (*fakeArgvRunner)(nil)
)

// bashRunnerText runs Bash with the given command and returns the single text
// block; it fails the test on a Go error (Bash returns tool-result strings).
func bashRunnerText(t *testing.T, b *BashTool, command string) string {
	t.Helper()
	res, err := b.InvokableRun(context.Background(), `{"command":`+strconvQuote(command)+`}`)
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

// strconvQuote is a tiny JSON-string quoter for the test args.
func strconvQuote(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// TestBashWithRunnerRoutesCommand asserts NewBash(root, WithRunner(fake)) sends
// the command through the injected runner (never runShellCommand) and that the
// runner's output + exit code propagate into the tool result.
func TestBashWithRunnerRoutesCommand(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	fake := &fakeCommandRunner{out: []byte("ROUTED-OUTPUT\n"), exit: 7}
	b := NewBash(root, WithRunner(fake))

	out := bashRunnerText(t, b, "echo hi")

	if fake.calls != 1 {
		t.Fatalf("runner calls = %d, want 1 (command must route through the runner)", fake.calls)
	}
	if fake.gotCommand != "echo hi" {
		t.Errorf("runner saw command %q, want %q", fake.gotCommand, "echo hi")
	}
	if fake.gotDir != root {
		t.Errorf("runner saw dir %q, want %q", fake.gotDir, root)
	}
	if !strings.Contains(out, "ROUTED-OUTPUT") {
		t.Errorf("result %q missing the runner's output", out)
	}
	if !strings.Contains(out, "[exit code: 7]") {
		t.Errorf("result %q missing the runner's exit code", out)
	}
}

// TestBashWithRunnerTimeout asserts a runner error that is (or wraps)
// context.DeadlineExceeded surfaces as the timed-out tool-result, not a start
// error (the sandbox executor folds a timeout into its returned err).
func TestBashWithRunnerTimeout(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		err  error
	}{
		{name: "direct deadline exceeded", err: context.DeadlineExceeded},
		{name: "wrapped deadline exceeded", err: errors.Join(errors.New("killed"), context.DeadlineExceeded)},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			fake := &fakeCommandRunner{err: tt.err}
			b := NewBash(t.TempDir(), WithRunner(fake))
			out := bashRunnerText(t, b, "sleep 999")
			if !strings.HasPrefix(out, "error:") || !strings.Contains(out, "timed out") {
				t.Fatalf("result = %q, want a timed-out error", out)
			}
		})
	}
}

// TestBashWithRunnerStartError asserts a non-timeout runner error surfaces as the
// "could not run command" tool-result (still never a Go error).
func TestBashWithRunnerStartError(t *testing.T) {
	t.Parallel()
	fake := &fakeCommandRunner{err: errors.New("sandbox refused to start")}
	b := NewBash(t.TempDir(), WithRunner(fake))
	out := bashRunnerText(t, b, "echo hi")
	if !strings.HasPrefix(out, "error:") || !strings.Contains(out, "could not run command") {
		t.Fatalf("result = %q, want a start error", out)
	}
	if !strings.Contains(out, "sandbox refused to start") {
		t.Errorf("result %q missing the underlying error", out)
	}
}

// TestBashNilRunnerDirectExec asserts NewBash(root) (no runner) still direct-execs
// via sh -c, byte-for-byte the pre-existing behavior.
func TestBashNilRunnerDirectExec(t *testing.T) {
	t.Parallel()
	requireSh(t)
	if NewBash(t.TempDir()).runner != nil {
		t.Fatal("NewBash without WithRunner must leave runner nil (direct-exec default)")
	}
	out := runBash(t, t.TempDir(), map[string]any{"command": "echo hello"})
	if !strings.Contains(out, "hello") || !strings.Contains(out, "[exit code: 0]") {
		t.Errorf("nil-runner Bash did not direct-exec; got %q", out)
	}
}

// TestGrepWithArgvRunnerRoutesArgv asserts the rg backend, given an injected
// ArgvRunner, routes the built rg argv (binary + flags) through it and adapts the
// runner's canned output into rendered matches.
func TestGrepWithArgvRunnerRoutesArgv(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "a.go"), "package main\nfunc target() {}\n")

	// Canned rg-style "path:line:text" line whose path is the resolved absolute
	// path collectRgLines relativizes against the workspace root.
	absA := resolvedJoin(t, root, "a.go")
	fake := &fakeArgvRunner{out: []byte(absA + ":2:func target() {}\n"), exit: 0}

	// Force the rg backend AND inject the argv runner (nil argvRunner would exec rg).
	g := newGrepWithBackend(root, newFakeReadGuard(1<<20), true, WithArgvRunner(fake))
	res, err := g.InvokableRun(context.Background(), `{"pattern":"target"}`)
	if err != nil {
		t.Fatalf("InvokableRun returned a Go error %v; read tools return tool-result strings", err)
	}
	tb, ok := res.Content[0].(*content.TextBlock)
	if !ok {
		t.Fatalf("block type = %T, want *content.TextBlock", res.Content[0])
	}

	if fake.calls != 1 {
		t.Fatalf("argv runner calls = %d, want 1 (argv must route through the runner)", fake.calls)
	}
	if fake.gotDir != root {
		t.Errorf("argv runner saw dir %q, want %q", fake.gotDir, root)
	}
	if len(fake.gotArgv) == 0 || fake.gotArgv[0] != rgBinary {
		t.Fatalf("argv runner argv = %v, want it to start with %q", fake.gotArgv, rgBinary)
	}
	assertAdjacent(t, fake.gotArgv, "--regexp", "target")
	if !strings.Contains(tb.Text, "a.go") || !strings.Contains(tb.Text, "func target") {
		t.Errorf("result %q did not adapt the runner's output into matches", tb.Text)
	}
}

// TestGrepWithArgvRunnerTimeout asserts a runner error paired with a cancelled ctx
// surfaces the timeout tool-result (the executor folds a cancel into its err).
func TestGrepWithArgvRunnerTimeout(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "a.go"), "// findme\n")

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // expired before the run.

	fake := &fakeArgvRunner{err: context.Canceled}
	g := newGrepWithBackend(root, newFakeReadGuard(1<<20), true, WithArgvRunner(fake))
	res, err := g.InvokableRun(ctx, `{"pattern":"findme"}`)
	if err != nil {
		t.Fatalf("InvokableRun returned a Go error %v", err)
	}
	tb := res.Content[0].(*content.TextBlock)
	if !strings.Contains(tb.Text, "timed out") {
		t.Errorf("result %q, want the timeout tool-result", tb.Text)
	}
}

// TestGrepNilArgvRunnerDirectExec asserts the rg backend with no injected runner
// still execs real ripgrep, byte-for-byte the pre-existing behavior.
func TestGrepNilArgvRunnerDirectExec(t *testing.T) {
	t.Parallel()
	if !rgOnPath() {
		t.Skip("ripgrep not on PATH; nil-runner direct-exec needs the real binary")
	}
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "a.go"), "package main\nfunc target() {}\n")

	g := newGrepWithBackend(root, newFakeReadGuard(1<<20), true) // rg backend, nil argvRunner.
	if g.argvRunner != nil {
		t.Fatal("newGrepWithBackend without WithArgvRunner must leave argvRunner nil")
	}
	res, err := g.InvokableRun(context.Background(), `{"pattern":"target"}`)
	if err != nil {
		t.Fatalf("InvokableRun returned a Go error %v", err)
	}
	tb := res.Content[0].(*content.TextBlock)
	if !strings.Contains(tb.Text, "a.go") || !strings.Contains(tb.Text, "func target") {
		t.Errorf("nil-runner Grep did not direct-exec rg; got %q", tb.Text)
	}
}

// rgOnPath reports whether ripgrep is resolvable (mirrors NewGrep's probe).
func rgOnPath() bool {
	return NewGrep(".", newFakeReadGuard(1<<20)).useRg
}
