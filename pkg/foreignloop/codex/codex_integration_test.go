package codex

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/looprig/harness/pkg/foreignloop"
)

const codexIntegrationEnv = "LOOPRIG_CODEX_INTEGRATION"

func TestIntegrationCodexCLIContract(t *testing.T) {
	if os.Getenv(codexIntegrationEnv) != "1" {
		t.Skipf("set %s=1 to run Codex CLI contract tests", codexIntegrationEnv)
	}

	codexPath, err := exec.LookPath("codex")
	if err != nil {
		t.Skip("codex CLI not found on PATH; install Codex CLI or add it to PATH to run this integration test")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	start := runCodex(t, ctx, codexPath, []string{
		"exec",
		"--json",
		"--sandbox", "read-only",
		"--ask-for-approval", "never",
		"Reply with exactly: ok",
	})
	if start.threadID == "" {
		t.Fatalf("codex exec did not emit %s; stdout:\n%s\nstderr:\n%s", eventThreadStarted, start.stdout, start.stderr)
	}

	resume := runCodex(t, ctx, codexPath, []string{
		"exec",
		"resume",
		start.threadID,
		"--json",
		"Reply with exactly: continued",
	})
	if !resumeConfirmsContinuation(resume, start.threadID) {
		t.Fatalf("codex exec resume did not resume or clearly confirm continuation of thread %q; stdout:\n%s\nstderr:\n%s", start.threadID, resume.stdout, resume.stderr)
	}

	help := runCodex(t, ctx, codexPath, []string{"exec", "resume", "--help"})
	for _, flag := range []string{"--cd", "--sandbox", "--ask-for-approval", "--add-dir"} {
		if !strings.Contains(help.stdout, flag) && !strings.Contains(help.stderr, flag) {
			t.Fatalf("codex exec resume --help does not advertise %s; resume-specific flags may need the Codex CLI -c fallback, for example -c sandbox_mode=read-only or -c approval_policy=never. stdout:\n%s\nstderr:\n%s", flag, help.stdout, help.stderr)
		}
		t.Logf("codex exec resume accepts %s", flag)
	}
}

type codexRun struct {
	stdout   string
	stderr   string
	threadID string
}

func runCodex(t *testing.T, parent context.Context, codexPath string, args []string) codexRun {
	t.Helper()

	ctx, cancel := context.WithTimeout(parent, 30*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, codexPath, args...)
	cmd.Env = os.Environ()

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if ctx.Err() != nil {
		t.Fatalf("codex %s timed out after 30s; stdout:\n%s\nstderr:\n%s", strings.Join(args, " "), conciseOutput(stdout.String()), conciseOutput(stderr.String()))
	}
	if err != nil {
		t.Fatalf("codex %s failed: %v; stdout:\n%s\nstderr:\n%s", strings.Join(args, " "), err, conciseOutput(stdout.String()), conciseOutput(stderr.String()))
	}

	out := stdout.String()
	return codexRun{
		stdout:   conciseOutput(out),
		stderr:   conciseOutput(stderr.String()),
		threadID: firstThreadID(t, out),
	}
}

func firstThreadID(t *testing.T, stdout string) string {
	t.Helper()

	scanner := bufio.NewScanner(strings.NewReader(stdout))
	for scanner.Scan() {
		events, err := decodeLine(scanner.Bytes())
		if err != nil {
			var de *foreignloop.DecodeError
			if errors.As(err, &de) {
				continue
			}
			t.Fatalf("decodeLine() unexpected error = %v", err)
		}
		for _, ev := range events {
			if ev.Kind == foreignloop.ForeignInit {
				return ev.SessionID
			}
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("scan codex stdout: %v", err)
	}
	return ""
}

func resumeConfirmsContinuation(run codexRun, wantThreadID string) bool {
	if run.threadID == wantThreadID {
		return true
	}
	combined := strings.ToLower(run.stdout + "\n" + run.stderr)
	return strings.Contains(combined, strings.ToLower(wantThreadID)) &&
		(strings.Contains(combined, "resume") || strings.Contains(combined, "continu"))
}

func conciseOutput(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	const max = 4096
	if len(s) <= max {
		return s
	}
	return fmt.Sprintf("%s\n... truncated %d bytes ...", s[:max], len(s)-max)
}
