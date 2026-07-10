package codex

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/looprig/core/content"
	"github.com/looprig/harness/pkg/foreignloop"
)

// Agent must satisfy the foreign-agent port.
var _ foreignloop.ForeignAgent = (*Agent)(nil)

func TestAgentSpawnFirstTurnExecJSONL(t *testing.T) {
	t.Parallel()
	fake := newFakeCodex(t)
	cwd := t.TempDir()
	agent := &Agent{
		ExecPath:         fake.path,
		Model:            "gpt-5",
		Profile:          "looprig",
		AdditionalDirs:   []string{"/deps/one", "/deps/two"},
		Sandbox:          SandboxWorkspaceWrite,
		Approval:         ApprovalOnRequest,
		Env:              fake.env("KEEP_ME=1", "STDERR_LINES=20000"),
		IgnoreUserConfig: true,
		IgnoreRules:      true,
		SkipGitRepoCheck: true,
	}
	turn := foreignloop.ForeignTurn{
		Cwd:          cwd,
		StartNew:     true,
		SystemPrompt: "system rules",
		Input:        []content.Block{&content.TextBlock{Text: "write the adapter"}},
	}

	stream, err := agent.Spawn(context.Background(), turn)
	if err != nil {
		t.Fatalf("Spawn() error = %v", err)
	}
	events := collectEvents(t, stream)
	if err := stream.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if err := stream.Close(); err != nil {
		t.Fatalf("second Close() error = %v, want idempotent nil", err)
	}
	if tp := stream.TranscriptPath(); tp != "" {
		t.Fatalf("TranscriptPath() = %q, want empty", tp)
	}

	wantPrompt := "<looprig-system>system rules</looprig-system>\n\n<user-task>write the adapter</user-task>"
	wantArgv := []string{
		"exec",
		"--json",
		"--cd", cwd,
		"--model", "gpt-5",
		"--profile", "looprig",
		"--sandbox", "workspace-write",
		"-c", "approval_policy=\"on-request\"",
		"--add-dir", "/deps/one",
		"--add-dir", "/deps/two",
		"--ignore-user-config",
		"--ignore-rules",
		"--skip-git-repo-check",
		wantPrompt,
	}
	if got := fake.argv(t); !reflect.DeepEqual(got, wantArgv) {
		t.Fatalf("argv = %#v, want %#v", got, wantArgv)
	}
	if got := cleanPath(t, strings.TrimSpace(readFile(t, fake.cwdFile))); got != cleanPath(t, cwd) {
		t.Fatalf("child cwd = %q, want %q", got, cleanPath(t, cwd))
	}
	env := readFile(t, fake.envFile)
	if !strings.Contains(env, "KEEP_ME=1\n") {
		t.Fatalf("child env missing whitelisted var: %q", env)
	}
	if strings.Contains(env, "UNRELATED_PARENT_SHOULD_NOT_LEAK=") {
		t.Fatalf("child env leaked unrelated parent env: %q", env)
	}
	if stdin := readFile(t, fake.stdinFile); stdin != "" {
		t.Fatalf("stdin = %q, want empty", stdin)
	}
	assertCodexEvents(t, events)
}

func TestAgentSpawnResumeTurnExecJSONL(t *testing.T) {
	t.Parallel()
	fake := newFakeCodex(t)
	cwd := t.TempDir()
	agent := &Agent{
		ExecPath:         fake.path,
		Model:            "gpt-5",
		Profile:          "ignored-on-resume",
		AdditionalDirs:   []string{"/ignored"},
		Sandbox:          SandboxDangerFullAccess,
		Approval:         ApprovalNever,
		Env:              fake.env(),
		IgnoreUserConfig: true,
		IgnoreRules:      true,
		SkipGitRepoCheck: true,
	}
	turn := foreignloop.ForeignTurn{
		Cwd:          cwd,
		ForeignSID:   "codex-thread-previous",
		StartNew:     false,
		SystemPrompt: "resume system",
		Input:        []content.Block{&content.TextBlock{Text: "continue"}},
	}

	stream, err := agent.Spawn(context.Background(), turn)
	if err != nil {
		t.Fatalf("Spawn() error = %v", err)
	}
	events := collectEvents(t, stream)
	if err := stream.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	wantPrompt := "<looprig-system>resume system</looprig-system>\n\n<user-task>continue</user-task>"
	wantArgv := []string{
		"exec",
		"resume",
		"--json",
		"codex-thread-previous",
		"--model", "gpt-5",
		"--ignore-user-config",
		"--ignore-rules",
		"--skip-git-repo-check",
		wantPrompt,
	}
	if got := fake.argv(t); !reflect.DeepEqual(got, wantArgv) {
		t.Fatalf("argv = %#v, want %#v", got, wantArgv)
	}
	assertCodexEvents(t, events)
}

func TestAgentSpawnContextCancelClosesEvents(t *testing.T) {
	t.Parallel()
	fake := newFakeCodex(t)
	ctx, cancel := context.WithCancel(context.Background())
	stream, err := (&Agent{
		ExecPath: fake.path,
		Env:      fake.env("FAKE_MODE=long_running"),
	}).Spawn(ctx, foreignloop.ForeignTurn{
		Cwd:      t.TempDir(),
		StartNew: true,
		Input:    []content.Block{&content.TextBlock{Text: "wait"}},
	})
	if err != nil {
		t.Fatalf("Spawn() error = %v", err)
	}

	cancel()
	assertEventsClosePromptly(t, stream)
	err1 := stream.Close()
	if err2 := stream.Close(); err2 != err1 {
		t.Fatalf("second Close() error = %v, want same error %v", err2, err1)
	}
}

func TestAgentSpawnCloseClosesEventsWithoutDrain(t *testing.T) {
	t.Parallel()
	fake := newFakeCodex(t)
	stream, err := (&Agent{
		ExecPath: fake.path,
		Env:      fake.env("FAKE_MODE=block_on_event"),
	}).Spawn(context.Background(), foreignloop.ForeignTurn{
		Cwd:      t.TempDir(),
		StartNew: true,
		Input:    []content.Block{&content.TextBlock{Text: "do not drain"}},
	})
	if err != nil {
		t.Fatalf("Spawn() error = %v", err)
	}
	waitForBlockedDecoderSend(t)

	err1 := stream.Close()
	assertEventsAlreadyClosed(t, stream)
	if err2 := stream.Close(); err2 != err1 {
		t.Fatalf("second Close() error = %v, want same error %v", err2, err1)
	}
}

func TestAgentSpawnCloseReturnsDecodeError(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		mode string
	}{
		{name: "malformed json", mode: "malformed_json"},
		{name: "oversized line", mode: "oversized_line"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			fake := newFakeCodex(t)
			stream, err := (&Agent{
				ExecPath: fake.path,
				Env:      fake.env("FAKE_MODE=" + tt.mode),
			}).Spawn(context.Background(), foreignloop.ForeignTurn{
				Cwd:      t.TempDir(),
				StartNew: true,
				Input:    []content.Block{&content.TextBlock{Text: "decode"}},
			})
			if err != nil {
				t.Fatalf("Spawn() error = %v", err)
			}
			_ = collectEvents(t, stream)
			err = stream.Close()
			var de *foreignloop.DecodeError
			if !errors.As(err, &de) {
				t.Fatalf("Close() error = %T %[1]v, want *foreignloop.DecodeError", err)
			}
		})
	}
}

func TestAgentSpawnCloseReturnsForeignExitError(t *testing.T) {
	t.Parallel()
	fake := newFakeCodex(t)
	agent := &Agent{
		ExecPath: fake.path,
		Env:      fake.env("EXIT_CODE=7"),
	}
	turn := foreignloop.ForeignTurn{
		Cwd:      t.TempDir(),
		StartNew: true,
		Input:    []content.Block{&content.TextBlock{Text: "fail after output"}},
	}

	stream, err := agent.Spawn(context.Background(), turn)
	if err != nil {
		t.Fatalf("Spawn() error = %v", err)
	}
	_ = collectEvents(t, stream)
	err = stream.Close()
	var ee *foreignloop.ForeignExitError
	if !errors.As(err, &ee) {
		t.Fatalf("Close() error = %T %[1]v, want *foreignloop.ForeignExitError", err)
	}
	if ee.Code != 7 {
		t.Fatalf("ForeignExitError.Code = %d, want 7", ee.Code)
	}
	if err2 := stream.Close(); err2 != err {
		t.Fatalf("second Close() error = %v, want same error %v", err2, err)
	}
}

func TestAgentSpawnCloseJoinsDecodeAndForeignExitErrors(t *testing.T) {
	t.Parallel()
	fake := newFakeCodex(t)
	stream, err := (&Agent{
		ExecPath: fake.path,
		Env:      fake.env("FAKE_MODE=malformed_json", "EXIT_CODE=7"),
	}).Spawn(context.Background(), foreignloop.ForeignTurn{
		Cwd:      t.TempDir(),
		StartNew: true,
		Input:    []content.Block{&content.TextBlock{Text: "decode then exit"}},
	})
	if err != nil {
		t.Fatalf("Spawn() error = %v", err)
	}
	_ = collectEvents(t, stream)

	err = stream.Close()
	var de *foreignloop.DecodeError
	if !errors.As(err, &de) {
		t.Fatalf("Close() error = %T %[1]v, want DecodeError", err)
	}
	var ee *foreignloop.ForeignExitError
	if !errors.As(err, &ee) || ee.Code != 7 {
		t.Fatalf("Close() error = %T %[1]v, want ForeignExitError code 7", err)
	}
	if got, want := err.Error(), de.Error()+"\n"+ee.Error(); got != want {
		t.Fatalf("Close() error = %q, want deterministic decode-first ordering %q", got, want)
	}
	if err2 := stream.Close(); err2 != err {
		t.Fatalf("second Close() error = %v, want same error %v", err2, err)
	}
}

func TestAgentSpawnErrorPaths(t *testing.T) {
	t.Parallel()
	t.Run("empty exec path fails closed with config error", func(t *testing.T) {
		t.Parallel()
		stream, err := (&Agent{}).Spawn(context.Background(), foreignloop.ForeignTurn{StartNew: true})
		if err == nil {
			if stream != nil {
				_ = stream.Close()
			}
			t.Fatal("Spawn() error = nil, want error")
		}
		var se *foreignloop.SpawnError
		if errors.As(err, &se) {
			t.Fatalf("Spawn() error = %T %[1]v, want direct config error", err)
		}
		if got := reflect.TypeOf(err).String(); got != "*codex.SpawnConfigError" {
			t.Fatalf("Spawn() error type = %s, want *codex.SpawnConfigError", got)
		}
		if got, want := err.Error(), "codex: spawn config: ExecPath: empty"; got != want {
			t.Fatalf("Spawn() error = %q, want %q", got, want)
		}
	})
	t.Run("bogus exec path surfaces a spawn error", func(t *testing.T) {
		t.Parallel()
		stream, err := (&Agent{ExecPath: "/nonexistent/codex-binary-xyz-not-here"}).Spawn(context.Background(), foreignloop.ForeignTurn{StartNew: true})
		if err == nil {
			if stream != nil {
				_ = stream.Close()
			}
			t.Fatal("Spawn() error = nil, want error")
		}
		var se *foreignloop.SpawnError
		if !errors.As(err, &se) {
			t.Fatalf("Spawn() error = %T %[1]v, want *foreignloop.SpawnError", err)
		}
	})
}

func assertCodexEvents(t *testing.T, got []foreignloop.ForeignEvent) {
	t.Helper()
	if len(got) != 3 {
		t.Fatalf("events = %d, want 3: %#v", len(got), got)
	}
	if got[0].Kind != foreignloop.ForeignInit {
		t.Fatalf("event[0].Kind = %v, want ForeignInit", got[0].Kind)
	}
	if got[0].SessionID != "codex-thread-from-jsonl" {
		t.Fatalf("event[0].SessionID = %q, want codex-thread-from-jsonl", got[0].SessionID)
	}
	assertStepText(t, got[1], "decoded assistant text")
	if got[2].Kind != foreignloop.ForeignTerminalOK {
		t.Fatalf("event[2].Kind = %v, want ForeignTerminalOK", got[2].Kind)
	}
}

func collectEvents(t *testing.T, stream foreignloop.ForeignStream) []foreignloop.ForeignEvent {
	t.Helper()
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	var events []foreignloop.ForeignEvent
	for {
		select {
		case ev, ok := <-stream.Events():
			if !ok {
				return events
			}
			events = append(events, ev)
		case <-timer.C:
			_ = stream.Close()
			t.Fatal("timed out waiting for stream events")
		}
	}
}

func assertEventsClosePromptly(t *testing.T, stream foreignloop.ForeignStream) {
	t.Helper()
	timer := time.NewTimer(500 * time.Millisecond)
	defer timer.Stop()
	for {
		select {
		case _, ok := <-stream.Events():
			if !ok {
				return
			}
		case <-timer.C:
			_ = stream.Close()
			t.Fatal("timed out waiting for events to close after context cancellation")
		}
	}
}

func assertEventsAlreadyClosed(t *testing.T, stream foreignloop.ForeignStream) {
	t.Helper()
	timer := time.NewTimer(500 * time.Millisecond)
	defer timer.Stop()
	select {
	case ev, ok := <-stream.Events():
		if ok {
			t.Fatalf("Events() yielded %#v after Close without a drain, want closed channel", ev)
		}
	case <-timer.C:
		t.Fatal("timed out waiting for Events() to close after Close without a drain")
	}
}

func waitForBlockedDecoderSend(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	buf := make([]byte, 1<<20)
	for time.Now().Before(deadline) {
		n := runtime.Stack(buf, true)
		stacks := string(buf[:n])
		if strings.Contains(stacks, "github.com/looprig/harness/pkg/foreignloop/codex.decodeJSONL.func") &&
			(strings.Contains(stacks, "[chan send]") || strings.Contains(stacks, "[select]")) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("timed out waiting for decoder goroutine to block on event send")
}

type fakeCodex struct {
	path      string
	argvFile  string
	envFile   string
	cwdFile   string
	stdinFile string
}

func newFakeCodex(t *testing.T) fakeCodex {
	t.Helper()
	dir := t.TempDir()
	f := fakeCodex{
		path:      filepath.Join(dir, "codex"),
		argvFile:  filepath.Join(dir, "argv.bin"),
		envFile:   filepath.Join(dir, "env.txt"),
		cwdFile:   filepath.Join(dir, "cwd.txt"),
		stdinFile: filepath.Join(dir, "stdin.txt"),
	}
	script := `#!/bin/sh
set -eu
: > "$ARGV_FILE"
for arg in "$@"; do
  printf '%s\000' "$arg" >> "$ARGV_FILE"
done
env | sort > "$ENV_FILE"
pwd > "$CWD_FILE"
cat > "$STDIN_FILE"
case "${FAKE_MODE:-happy}" in
  malformed_json)
    printf '%s\n' '{"type":"thread.started"'
    exit "${EXIT_CODE:-0}"
    ;;
  oversized_line)
    head -c 1048577 /dev/zero | tr '\000' x
    printf '\n'
    exit 0
    ;;
  long_running)
    trap 'exit 0' INT TERM
    printf '%s\n' '{"type":"thread.started","thread_id":"codex-thread-from-jsonl"}'
    sleep 60
    exit 0
    ;;
  block_on_event)
    trap 'exit 0' INT TERM
    printf '%s\n' '{"type":"thread.started","thread_id":"codex-thread-from-jsonl"}'
    sleep 60
    exit 0
    ;;
esac
i=0
while [ "$i" -lt "${STDERR_LINES:-0}" ]; do
  printf 'stderr line %s abcdefghijklmnopqrstuvwxyz abcdefghijklmnopqrstuvwxyz abcdefghijklmnopqrstuvwxyz\n' "$i" >&2
  i=$((i + 1))
done
printf '%s\n' '{"type":"thread.started","thread_id":"codex-thread-from-jsonl"}'
printf '%s\n' '{"type":"turn.started"}'
printf '%s\n' '{"type":"item.completed","item":{"type":"agent_message","text":"decoded assistant text"}}'
printf '%s\n' '{"type":"turn.completed"}'
exit "${EXIT_CODE:-0}"
`
	if err := os.WriteFile(f.path, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake codex: %v", err)
	}
	return f
}

func (f fakeCodex) env(extra ...string) []string {
	env := []string{
		"ARGV_FILE=" + f.argvFile,
		"ENV_FILE=" + f.envFile,
		"CWD_FILE=" + f.cwdFile,
		"STDIN_FILE=" + f.stdinFile,
	}
	return append(env, extra...)
}

func (f fakeCodex) argv(t *testing.T) []string {
	t.Helper()
	raw, err := os.ReadFile(f.argvFile)
	if err != nil {
		t.Fatalf("read argv file: %v", err)
	}
	raw = bytes.TrimSuffix(raw, []byte{0})
	if len(raw) == 0 {
		return nil
	}
	parts := bytes.Split(raw, []byte{0})
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		out = append(out, string(p))
	}
	return out
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

func cleanPath(t *testing.T, path string) string {
	t.Helper()
	clean, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatalf("eval symlinks for %s: %v", path, err)
	}
	return clean
}
