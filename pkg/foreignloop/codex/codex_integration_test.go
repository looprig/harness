package codex

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
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
	{flag: "--ask-for-approval", value: "never"},
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
			name:   "approval",
			flag:   "--ask-for-approval",
			value:  "never",
			before: []string{"exec", "--ask-for-approval", "never", "resume", "--help"},
			after:  []string{"exec", "resume", "--ask-for-approval", "never", "--help"},
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
		{flag: "--ask-for-approval", before: true, after: true},
		{flag: "--add-dir", before: false, after: true},
	})
	if len(got) != 3 {
		t.Fatalf("unsupported resume flag failures = %q, want 3 failures", got)
	}

	for _, want := range []string{
		"--cd does not parse before or after resume",
		"working directory",
		"--sandbox does not parse after resume",
		"sandbox policy",
		"--add-dir does not parse before resume",
		"additional directory",
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

	probeResumeFlagSupport(t, ctx, codexPath)

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

}

func probeResumeFlagSupport(t *testing.T, parent context.Context, codexPath string) {
	t.Helper()

	support := make([]resumeFlagSupport, 0, len(resumeFlagProbes))
	var results []string
	for _, probe := range resumeFlagProbes {
		before := runCodexParserProbe(parent, codexPath, resumeFlagProbeArgs(probe.flag, probe.value, true))
		after := runCodexParserProbe(parent, codexPath, resumeFlagProbeArgs(probe.flag, probe.value, false))
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

	if failures := unsupportedResumeFlagFailures(support); len(failures) > 0 {
		t.Errorf("codex exec resume parser probe results (all probes end in --help and do not create or resume a session):\n%s\nunsupported flag fallbacks:\n%s", strings.Join(results, "\n"), strings.Join(failures, "\n"))
		return
	}
	t.Logf("codex exec resume parser probe results:\n%s", strings.Join(results, "\n"))
}

type codexParserProbe struct {
	ok     bool
	detail string
}

func runCodexParserProbe(parent context.Context, codexPath string, args []string) codexParserProbe {
	ctx, cancel := context.WithTimeout(parent, 5*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, codexPath, args...)
	cmd.Env = os.Environ()

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
	return codexParserProbe{ok: true}
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
		var placement string
		switch {
		case !flag.before && !flag.after:
			placement = "before or after"
		case !flag.before:
			placement = "before"
		case !flag.after:
			placement = "after"
		default:
			continue
		}
		failures = append(failures, fmt.Sprintf("%s does not parse %s resume; %s", flag.flag, placement, resumeFlagFallback(flag.flag)))
	}
	return failures
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
