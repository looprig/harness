package codex

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/looprig/harness/pkg/foreignloop"
)

const codexIntegrationEnv = "LOOPRIG_CODEX_INTEGRATION"

type resumeFlagSupport struct {
	flag          string
	before, after bool
}

var resumeFlagProbes = []struct {
	flag  string
	value string
}{
	{flag: "--cd", value: "."},
	{flag: "--sandbox", value: "read-only"},
	{flag: "--add-dir", value: "."},
}

func TestResumeFlagProbeArgs(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		flag   string
		value  string
		before []string
		after  []string
	}{
		{
			name:   "change directory",
			flag:   "--cd",
			value:  ".",
			before: []string{"exec", "--cd", ".", "resume", "--help"},
			after:  []string{"exec", "resume", "--cd", ".", "--help"},
		},
		{
			name:   "sandbox",
			flag:   "--sandbox",
			value:  "read-only",
			before: []string{"exec", "--sandbox", "read-only", "resume", "--help"},
			after:  []string{"exec", "resume", "--sandbox", "read-only", "--help"},
		},
		{
			name:   "additional directory",
			flag:   "--add-dir",
			value:  ".",
			before: []string{"exec", "--add-dir", ".", "resume", "--help"},
			after:  []string{"exec", "resume", "--add-dir", ".", "--help"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := resumeFlagProbeArgs(tt.flag, tt.value, true); !slices.Equal(got, tt.before) {
				t.Fatalf("before-resume probe args = %q, want %q", got, tt.before)
			}
			if got := resumeFlagProbeArgs(tt.flag, tt.value, false); !slices.Equal(got, tt.after) {
				t.Fatalf("after-resume probe args = %q, want %q", got, tt.after)
			}
		})
	}
}

func TestResumeFlagFallback(t *testing.T) {
	t.Parallel()

	tests := []struct {
		flag string
		want []string
	}{
		{"--cd", []string{"working directory", "-c key=value", "persist"}},
		{"--sandbox", []string{"sandbox policy", "-c key=value", "persist"}},
		{"--ask-for-approval", []string{"approval policy", "-c key=value", "persist"}},
		{"--add-dir", []string{"additional directory", "-c key=value", "persist"}},
	}

	for _, tt := range tests {
		t.Run(tt.flag, func(t *testing.T) {
			got := resumeFlagFallback(tt.flag)
			for _, want := range tt.want {
				if !strings.Contains(got, want) {
					t.Errorf("resumeFlagFallback(%q) = %q, want %q", tt.flag, got, want)
				}
			}
			if strings.Contains(got, "sandbox_mode") || strings.Contains(got, "approval_policy") {
				t.Errorf("resumeFlagFallback(%q) assumes a version-specific config key: %q", tt.flag, got)
			}
		})
	}
}

func TestUnsupportedResumeFlagFailures(t *testing.T) {
	t.Parallel()

	got := unsupportedResumeFlagFailures([]resumeFlagSupport{
		{flag: "--cd", before: false, after: false},
		{flag: "--sandbox", before: true, after: false},
		{flag: "--add-dir", before: false, after: true},
	})
	if len(got) != 1 {
		t.Fatalf("unsupported resume flag failures = %q, want only the placement-agnostic failure", got)
	}

	for _, want := range []string{
		"--cd does not parse before or after resume",
		"working directory",
	} {
		found := false
		for _, failure := range got {
			if strings.Contains(failure, want) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("unsupported resume flag failures = %q, missing %q", got, want)
		}
	}
}

func TestResumeFlagProbeGate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		support   resumeFlagSupport
		wantBlock bool
	}{
		{
			name:      "before resume parses",
			support:   resumeFlagSupport{flag: "--cd", before: true, after: false},
			wantBlock: false,
		},
		{
			name:      "after resume parses",
			support:   resumeFlagSupport{flag: "--sandbox", before: false, after: true},
			wantBlock: false,
		},
		{
			name:      "neither placement parses",
			support:   resumeFlagSupport{flag: "--add-dir", before: false, after: false},
			wantBlock: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotBlock := !resumeFlagProbesSupported([]resumeFlagSupport{tt.support})
			if gotBlock != tt.wantBlock {
				t.Fatalf("live Codex contract blocked = %t, want %t for %+v", gotBlock, tt.wantBlock, tt.support)
			}
		})
	}
}

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

	if !probeResumeFlagSupport(t, ctx, codexPath) {
		t.Fatal("codex exec resume parser probes failed; live codex exec start/resume commands were not attempted")
	}

	start := runCodex(t, ctx, codexPath, buildStartArgs(foreignloop.ForeignTurn{}, runConfig{
		cwd:              t.TempDir(),
		sandbox:          SandboxReadOnly,
		approval:         ApprovalNever,
		skipGitRepoCheck: true,
	}, "Reply with exactly: ok"))
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

}

func TestIntegrationCodexProductionStartArgsParse(t *testing.T) {
	if os.Getenv(codexIntegrationEnv) != "1" {
		t.Skipf("set %s=1 to run Codex CLI contract tests", codexIntegrationEnv)
	}

	codexPath, err := exec.LookPath("codex")
	if err != nil {
		t.Skip("codex CLI not found on PATH; install Codex CLI or add it to PATH to run this integration test")
	}
	cwd := t.TempDir()
	args := buildStartArgs(foreignloop.ForeignTurn{}, runConfig{
		cwd:      cwd,
		sandbox:  SandboxReadOnly,
		approval: ApprovalNever,
	}, "--help")
	want := []string{
		"exec",
		"--json",
		"--cd", cwd,
		"--sandbox", "read-only",
		"-c", "approval_policy=\"never\"",
		"--help",
	}
	if !slices.Equal(args, want) {
		t.Fatalf("production start parser argv = %q, want %q", args, want)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	probe := runCodexParserProbe(ctx, codexPath, args, sanitizedCodexEnv(t, codexPath))
	if !probe.ok {
		t.Fatalf("codex parser rejected production start argv %q: %s", args, probe.detail)
	}
	if !strings.Contains(probe.detail, "Usage: codex exec") {
		t.Fatalf("codex parser argv did not take the --help path: %s", probe.detail)
	}
}

func probeResumeFlagSupport(t *testing.T, parent context.Context, codexPath string) bool {
	t.Helper()

	env := sanitizedCodexEnv(t, codexPath)
	support := make([]resumeFlagSupport, 0, len(resumeFlagProbes))
	var results []string
	for _, probe := range resumeFlagProbes {
		before := runCodexParserProbe(parent, codexPath, resumeFlagProbeArgs(probe.flag, probe.value, true), env)
		after := runCodexParserProbe(parent, codexPath, resumeFlagProbeArgs(probe.flag, probe.value, false), env)
		support = append(support, resumeFlagSupport{
			flag:   probe.flag,
			before: before.ok,
			after:  after.ok,
		})
		results = append(results, fmt.Sprintf("%s: before resume=%t, after resume=%t", probe.flag, before.ok, after.ok))
		if !before.ok {
			results = append(results, fmt.Sprintf("  before resume: %s", before.detail))
		}
		if !after.ok {
			results = append(results, fmt.Sprintf("  after resume: %s", after.detail))
		}
	}

	if !resumeFlagProbesSupported(support) {
		failures := unsupportedResumeFlagFailures(support)
		t.Logf("codex exec resume parser probe results (all probes end in --help and do not create or resume a session):\n%s\nunsupported flag fallbacks:\n%s", strings.Join(results, "\n"), strings.Join(failures, "\n"))
		return false
	}
	t.Logf("codex exec resume parser probe results:\n%s", strings.Join(results, "\n"))
	return true
}

type codexParserProbe struct {
	ok     bool
	detail string
}

func runCodexParserProbe(parent context.Context, codexPath string, args, env []string) codexParserProbe {
	ctx, cancel := context.WithTimeout(parent, 5*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, codexPath, args...)
	cmd.Env = env

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if ctx.Err() != nil {
		return codexParserProbe{detail: fmt.Sprintf("timed out after 5s; stdout: %s; stderr: %s", conciseOutput(stdout.String()), conciseOutput(stderr.String()))}
	}
	if err != nil {
		return codexParserProbe{detail: fmt.Sprintf("failed: %v; stdout: %s; stderr: %s", err, conciseOutput(stdout.String()), conciseOutput(stderr.String()))}
	}
	return codexParserProbe{ok: true, detail: conciseOutput(stdout.String() + "\n" + stderr.String())}
}

func sanitizedCodexEnv(t *testing.T, codexPath string) []string {
	t.Helper()
	home := t.TempDir()
	codexHome := filepath.Join(home, "codex")
	if err := os.Mkdir(codexHome, 0o700); err != nil {
		t.Fatalf("create isolated CODEX_HOME: %v", err)
	}
	return []string{
		"HOME=" + home,
		"CODEX_HOME=" + codexHome,
		"TMPDIR=" + t.TempDir(),
		"PATH=" + filepath.Dir(codexPath) + string(os.PathListSeparator) + "/usr/bin:/bin",
		"NO_COLOR=1",
	}
}

func resumeFlagProbeArgs(flag, value string, beforeResume bool) []string {
	if beforeResume {
		return []string{"exec", flag, value, "resume", "--help"}
	}
	return []string{"exec", "resume", flag, value, "--help"}
}

func unsupportedResumeFlagFailures(support []resumeFlagSupport) []string {
	var failures []string
	for _, flag := range support {
		if flag.before || flag.after {
			continue
		}
		failures = append(failures, fmt.Sprintf("%s does not parse before or after resume; %s", flag.flag, resumeFlagFallback(flag.flag)))
	}
	return failures
}

func resumeFlagProbesSupported(support []resumeFlagSupport) bool {
	return len(unsupportedResumeFlagFailures(support)) == 0
}

func resumeFlagFallback(flag string) string {
	const generic = "use the CLI's version-supported -c key=value override or persist the setting in the Codex profile/config used for resume; exact config keys vary by CLI version"

	switch flag {
	case "--cd":
		return "working directory fallback: " + generic
	case "--sandbox":
		return "sandbox policy fallback: " + generic
	case "--ask-for-approval":
		return "approval policy fallback: " + generic
	case "--add-dir":
		return "additional directory fallback: " + generic
	default:
		return "Codex configuration fallback: " + generic
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
