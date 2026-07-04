package tools

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
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
