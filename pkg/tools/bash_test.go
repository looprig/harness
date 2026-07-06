package tools

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/looprig/core/content"
	"github.com/looprig/harness/pkg/tool"
)

// requireSh skips a test when no POSIX sh is on PATH (Bash tests exec it).
func requireSh(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skipf("sh not available: %v", err)
	}
}

// runBash invokes Bash and extracts the single text block.
func runBash(t *testing.T, root string, args map[string]any) string {
	t.Helper()
	b, err := json.Marshal(args)
	if err != nil {
		t.Fatalf("marshal args: %v", err)
	}
	res, err := NewBash(root).InvokableRun(context.Background(), string(b))
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

func TestBashInfo(t *testing.T) {
	t.Parallel()
	info, err := NewBash(t.TempDir()).Info(context.Background())
	if err != nil {
		t.Fatalf("Info() error = %v", err)
	}
	if info.Name != "Bash" {
		t.Errorf("Info().Name = %q, want %q", info.Name, "Bash")
	}
	var schema map[string]json.RawMessage
	if err := json.Unmarshal(info.Schema, &schema); err != nil {
		t.Fatalf("Schema is not a JSON object: %v", err)
	}
}

func TestBash(t *testing.T) {
	t.Parallel()
	requireSh(t)

	tests := []struct {
		name        string
		args        map[string]any
		wantContain []string
		wantAbsent  []string
		wantErr     bool // result begins with "error:"
	}{
		{
			name:        "stdout captured with exit 0",
			args:        map[string]any{"command": "echo hello"},
			wantContain: []string{"hello", "[exit code: 0]"},
		},
		{
			name:        "non-zero exit code is captured (not an error)",
			args:        map[string]any{"command": "echo oops 1>&2; exit 3"},
			wantContain: []string{"oops", "[exit code: 3]"},
		},
		{
			name:        "combined stdout and stderr",
			args:        map[string]any{"command": "echo OUT; echo ERR 1>&2"},
			wantContain: []string{"OUT", "ERR"},
		},
		{
			name:        "pipes work (shell feature)",
			args:        map[string]any{"command": "printf 'a\\nb\\nc\\n' | wc -l | tr -d ' '"},
			wantContain: []string{"3", "[exit code: 0]"},
		},
		{
			name:    "missing command is rejected",
			args:    map[string]any{},
			wantErr: true,
		},
		{
			name:    "escaping workdir is rejected",
			args:    map[string]any{"command": "echo x", "workdir": "../.."},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			out := runBash(t, t.TempDir(), tt.args)
			gotErr := strings.HasPrefix(out, "error:")
			if gotErr != tt.wantErr {
				t.Fatalf("result = %q, wantErr = %v", out, tt.wantErr)
			}
			for _, sub := range tt.wantContain {
				if !strings.Contains(out, sub) {
					t.Errorf("result %q missing %q", out, sub)
				}
			}
			for _, sub := range tt.wantAbsent {
				if strings.Contains(out, sub) {
					t.Errorf("result %q unexpectedly contains %q", out, sub)
				}
			}
		})
	}
}

// TestBashTimeout runs a command that sleeps past a short timeout and asserts the
// timed-out tool-result.
func TestBashTimeout(t *testing.T) {
	t.Parallel()
	requireSh(t)
	out := runBash(t, t.TempDir(), map[string]any{"command": "sleep 5", "timeout": 1})
	if !strings.HasPrefix(out, "error:") || !strings.Contains(out, "timed out") {
		t.Fatalf("result = %q, want a timed-out error", out)
	}
}

// TestBashOutputTruncation generates more than 32 KiB of output and asserts the
// truncation notice is present and the capture is bounded.
func TestBashOutputTruncation(t *testing.T) {
	t.Parallel()
	requireSh(t)
	// yes prints an endless stream; head bounds it well above 32 KiB.
	out := runBash(t, t.TempDir(), map[string]any{"command": "yes AAAAAAAAAA | head -c 100000"})
	if !strings.Contains(out, "truncated") {
		t.Fatalf("result missing truncation notice; got %d bytes", len(out))
	}
	// The captured body must be bounded near the cap (allow room for the notice
	// and exit-code line).
	if len(out) > maxBashOutputBytes+256 {
		t.Errorf("output length = %d, want <= %d", len(out), maxBashOutputBytes+256)
	}
}

// TestBashWorkdir confirms the command runs in the resolved workdir under root.
func TestBashWorkdir(t *testing.T) {
	t.Parallel()
	requireSh(t)
	root := t.TempDir()
	sub := filepath.Join(root, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(sub, "marker.txt"), []byte("x"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	out := runBash(t, root, map[string]any{"command": "ls", "workdir": "sub"})
	if !strings.Contains(out, "marker.txt") {
		t.Errorf("result %q does not show the workdir contents", out)
	}
}

func TestBashBuildRequest(t *testing.T) {
	t.Parallel()
	req, err := NewBash(t.TempDir()).BuildRequest(`{"command":"go test ./..."}`, nil)
	if err != nil {
		t.Fatalf("BuildRequest err = %v", err)
	}
	br, ok := req.(tool.BashRequest)
	if !ok {
		t.Fatalf("request type = %T, want tool.BashRequest", req)
	}
	if br.Command != "go test ./..." {
		t.Errorf("BashRequest.Command = %q, want %q", br.Command, "go test ./...")
	}
	if _, err := NewBash(t.TempDir()).BuildRequest(`{}`, nil); err == nil {
		t.Errorf("BuildRequest(no command) err = nil, want non-nil")
	}
	if _, err := NewBash(t.TempDir()).BuildRequest(`not json`, nil); err == nil {
		t.Errorf("BuildRequest(bad json) err = nil, want non-nil")
	}
}

// plainRunner is a bare tool.CommandRunner with NO escalation-planner methods. The
// Bash tool's structural probe must find it is not a planner and plan no grants.
type plainRunner struct{}

func (plainRunner) RunCommand(context.Context, string, string) ([]byte, int, error) {
	return nil, 0, nil
}

// fakeGrantRunner is a test double implementing tool.CommandRunner plus the
// structural PlanGrants/DescribeGrant escalation-planner methods the Bash tool
// probes for (no sandbox import). plan maps a command to the candidate tokens
// PlanGrants mints; describe maps a token to its bound description — a token ABSENT
// from describe returns (,false), simulating a fabricated/tampered token that fails
// MAC verification. gotDir records the last dir PlanGrants was called with so a test
// can assert dir derivation.
type fakeGrantRunner struct {
	plan     map[string][]string
	describe map[string]string
	gotDir   string
}

func (f *fakeGrantRunner) RunCommand(context.Context, string, string) ([]byte, int, error) {
	return nil, 0, nil
}

func (f *fakeGrantRunner) PlanGrants(dir, command string) []string {
	f.gotDir = dir
	return f.plan[command]
}

func (f *fakeGrantRunner) DescribeGrant(token string) (string, bool) {
	desc, ok := f.describe[token]
	return desc, ok
}

// TestBashBuildRequestGrants proves BuildRequest attaches MAC-verified escalation
// grant descriptions to the BashRequest when (and only when) the runner is a
// structural escalation planner, and FAILS the build if any planned token cannot be
// verified (SPEC §10.7 — an unverifiable token must never reach a prompt).
func TestBashBuildRequestGrants(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		runner     tool.CommandRunner // nil => no runner
		args       string
		wantGrants []tool.GrantDisplay
		wantErr    bool
	}{
		{
			name: "planner attaches a MAC-verified grant",
			runner: &fakeGrantRunner{
				plan:     map[string][]string{"git push": {"tok-net"}},
				describe: map[string]string{"tok-net": "allow network egress for: git push"},
			},
			args:       `{"command":"git push"}`,
			wantGrants: []tool.GrantDisplay{{Token: "tok-net", Description: "allow network egress for: git push"}},
		},
		{
			name: "multiple verified grants preserved in plan order",
			runner: &fakeGrantRunner{
				plan:     map[string][]string{"deploy": {"tok-net", "tok-write"}},
				describe: map[string]string{"tok-net": "allow network egress", "tok-write": "allow write to /out"},
			},
			args: `{"command":"deploy"}`,
			wantGrants: []tool.GrantDisplay{
				{Token: "tok-net", Description: "allow network egress"},
				{Token: "tok-write", Description: "allow write to /out"},
			},
		},
		{
			name: "an unverifiable token fails the build (never prompts)",
			runner: &fakeGrantRunner{
				plan:     map[string][]string{"git push": {"tok-real", "tok-forged"}},
				describe: map[string]string{"tok-real": "allow network egress"},
			},
			args:    `{"command":"git push"}`,
			wantErr: true,
		},
		{
			name: "planner mints no tokens leaves Grants nil",
			runner: &fakeGrantRunner{
				plan:     map[string][]string{},
				describe: map[string]string{},
			},
			args:       `{"command":"echo hi"}`,
			wantGrants: nil,
		},
		{
			name:       "plain CommandRunner is not a planner",
			runner:     plainRunner{},
			args:       `{"command":"git push"}`,
			wantGrants: nil,
		},
		{
			name:       "nil runner plans no grants",
			runner:     nil,
			args:       `{"command":"git push"}`,
			wantGrants: nil,
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var opts []BashOption
			if tt.runner != nil {
				opts = append(opts, WithRunner(tt.runner))
			}
			b := NewBash(t.TempDir(), opts...)
			req, err := b.BuildRequest(tt.args, nil)
			if (err != nil) != tt.wantErr {
				t.Fatalf("BuildRequest() err = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				if req != nil {
					t.Fatalf("BuildRequest() returned request %v on error; an unverifiable grant must never reach a prompt", req)
				}
				return
			}
			br, ok := req.(tool.BashRequest)
			if !ok {
				t.Fatalf("request type = %T, want tool.BashRequest", req)
			}
			if !reflect.DeepEqual(br.Grants, tt.wantGrants) {
				t.Errorf("BashRequest.Grants = %#v, want %#v", br.Grants, tt.wantGrants)
			}
		})
	}
}

// TestBashBuildRequestGrantsUsesResolvedWorkdir proves PlanGrants is asked about the
// SAME resolved working directory InvokableRun would run in — so grants are planned
// for the call's actual dir, not the workspace root.
func TestBashBuildRequestGrantsUsesResolvedWorkdir(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	sub := filepath.Join(root, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	wantDir, err := containedPath(root, "sub")
	if err != nil {
		t.Fatalf("containedPath: %v", err)
	}
	fr := &fakeGrantRunner{
		plan:     map[string][]string{"git push": {"tok"}},
		describe: map[string]string{"tok": "egress"},
	}
	if _, err := NewBash(root, WithRunner(fr)).BuildRequest(`{"command":"git push","workdir":"sub"}`, nil); err != nil {
		t.Fatalf("BuildRequest() err = %v", err)
	}
	if fr.gotDir != wantDir {
		t.Errorf("PlanGrants dir = %q, want resolved workdir %q", fr.gotDir, wantDir)
	}
}

func TestBashAuditSummary(t *testing.T) {
	t.Parallel()
	b := NewBash(t.TempDir())
	got := b.AuditSummary(`{"command":"rm -rf build"}`)
	if got != "Bash: rm -rf build" {
		t.Errorf("AuditSummary = %q, want %q", got, "Bash: rm -rf build")
	}
	if got := b.AuditSummary("not json"); !strings.Contains(got, "unparsable") {
		t.Errorf("AuditSummary(bad) = %q, want an unparsable note", got)
	}
}

func TestBashClampTimeout(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		seconds int
		want    string // human duration
	}{
		{name: "zero uses default", seconds: 0, want: defaultBashTimeout.String()},
		{name: "negative uses default", seconds: -5, want: defaultBashTimeout.String()},
		{name: "in range is honored", seconds: 10, want: "10s"},
		{name: "over cap is clamped", seconds: 9999, want: maxBashTimeout.String()},
		{name: "exactly cap", seconds: 120, want: maxBashTimeout.String()},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := clampBashTimeout(tt.seconds).String(); got != tt.want {
				t.Errorf("clampBashTimeout(%d) = %s, want %s", tt.seconds, got, tt.want)
			}
		})
	}
}
