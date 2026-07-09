package codex

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
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
		"--ask-for-approval", "on-request",
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
		"--model", "gpt-5",
		"--ignore-user-config",
		"--ignore-rules",
		"--skip-git-repo-check",
		"codex-thread-previous",
		wantPrompt,
	}
	if got := fake.argv(t); !reflect.DeepEqual(got, wantArgv) {
		t.Fatalf("argv = %#v, want %#v", got, wantArgv)
	}
	assertCodexEvents(t, events)
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
