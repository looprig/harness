package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/ciram-co/looprig/pkg/loop"
	"github.com/ciram-co/looprig/pkg/tool"
)

// check_test.go exercises the security core: the seven-stage fail-secure
// PermissionChecker.Check, the absolute hard-deny matcher, the persisted-approval
// reading path (deny-beats-allow across two out-of-repo files, malformed
// handling), and the ReadGuard. Tests are table-driven and parallel where they do
// not share a fake-home fixture.

// staticEffect is a stub tool whose CheckEffect returns a fixed (effect, handled)
// — used to prove Stage 3 (EffectChecker) and that hard-deny precedes it.
type staticEffect struct {
	name    string
	effect  loop.Effect
	handled bool
}

func (s staticEffect) Info(ctx context.Context) (*tool.ToolInfo, error) {
	return &tool.ToolInfo{Name: s.name}, nil
}
func (s staticEffect) InvokableRun(ctx context.Context, argsJSON string) (*tool.ToolResult, error) {
	return tool.TextResult(""), nil
}
func (s staticEffect) CheckEffect(argsJSON string) (loop.Effect, bool) {
	return s.effect, s.handled
}

// plainTool is an InvokableTool that implements NO optional interface (no
// EffectChecker) — the common case for a built-in tool.
type plainTool struct{ name string }

func (p plainTool) Info(ctx context.Context) (*tool.ToolInfo, error) {
	return &tool.ToolInfo{Name: p.name}, nil
}
func (p plainTool) InvokableRun(ctx context.Context, argsJSON string) (*tool.ToolResult, error) {
	return tool.TextResult(""), nil
}

// writeApprovals serializes an ApprovalsFile to JSON bytes for a test fixture.
func writeApprovals(t *testing.T, recs ...ApprovalRecord) []byte {
	t.Helper()
	b, err := json.Marshal(ApprovalsFile{Version: 1, Approvals: recs})
	if err != nil {
		t.Fatalf("marshal approvals: %v", err)
	}
	return b
}

// fakeHome builds a fake home directory tree and returns its path plus a home-dir
// func that always returns it. wsBytes/userBytes (when non-nil) are written to the
// workspace-hash and user approvals files respectively.
func fakeHome(t *testing.T, wsRoot string, wsBytes, userBytes []byte) (string, func() (string, error)) {
	t.Helper()
	home := t.TempDir()
	homeFn := func() (string, error) { return home, nil }

	if userBytes != nil {
		userFile := filepath.Join(home, urviDirName, userApprovalsName)
		if err := os.MkdirAll(filepath.Dir(userFile), 0o700); err != nil {
			t.Fatalf("mkdir user store: %v", err)
		}
		if err := os.WriteFile(userFile, userBytes, 0o600); err != nil {
			t.Fatalf("write user approvals: %v", err)
		}
	}
	if wsBytes != nil {
		hash, err := workspaceHash(wsRoot)
		if err != nil {
			t.Fatalf("workspaceHash: %v", err)
		}
		wsFile := filepath.Join(home, urviDirName, workspacesDirName, hash, workspaceApprovalsName)
		if err := os.MkdirAll(filepath.Dir(wsFile), 0o700); err != nil {
			t.Fatalf("mkdir ws store: %v", err)
		}
		if err := os.WriteFile(wsFile, wsBytes, 0o600); err != nil {
			t.Fatalf("write ws approvals: %v", err)
		}
	}
	return home, homeFn
}

// newWS creates a temp workspace dir and returns its EvalSymlinks-resolved path
// (the form the checker resolves to for the workspace hash and containment).
func newWS(t *testing.T) string {
	t.Helper()
	ws := t.TempDir()
	resolved, err := filepath.EvalSymlinks(ws)
	if err != nil {
		t.Fatalf("EvalSymlinks ws: %v", err)
	}
	return resolved
}

// TestCheckContainment asserts Stage 1: a path that escapes the workspace (via ..
// or an absolute path outside root) is DENIED regardless of any later stage.
func TestCheckContainment(t *testing.T) {
	t.Parallel()
	ws := newWS(t)

	tests := []struct {
		name     string
		toolName string
		args     string
		want     loop.Effect
	}{
		{name: "in-repo read ok falls to ask", toolName: "ReadFile", args: `{"path":"a/b.go"}`, want: loop.EffectAsk},
		{name: "dotdot escape read denied", toolName: "ReadFile", args: `{"path":"../../etc/passwd"}`, want: loop.EffectDeny},
		// An absolute path is anchored UNDER the workspace root by containedPath
		// (filepath.Join(root, "/etc/passwd") => <root>/etc/passwd), so it does NOT
		// escape — it becomes an in-workspace path and falls through to Ask. The
		// escape protection is for ".." traversal and out-pointing symlinks.
		{name: "absolute path anchored under root falls to ask", toolName: "ReadFile", args: `{"path":"/etc/passwd"}`, want: loop.EffectAsk},
		{name: "dotdot escape write denied", toolName: "WriteFile", args: `{"path":"../escape.txt"}`, want: loop.EffectDeny},
		{name: "bash workdir escape denied", toolName: "Bash", args: `{"command":"ls","workdir":"../.."}`, want: loop.EffectDeny},
		{name: "glob root escape denied", toolName: "Glob", args: `{"pattern":"*.go","root":"../other"}`, want: loop.EffectDeny},
		{name: "grep path escape denied", toolName: "Grep", args: `{"pattern":"x","path":"../../"}`, want: loop.EffectDeny},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			pc := NewPermissionChecker(PermissionPolicy{
				WorkspaceRoot: ws,
				HardDeny:      DefaultHardDeny(),
			})
			pc.SetHomeDir(func() (string, error) { return t.TempDir(), nil })
			got := pc.Check(context.Background(), plainTool{name: tt.toolName}, tt.toolName, tt.args)
			if got != tt.want {
				t.Errorf("Check() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestCheckContainmentSymlinkEscape asserts a symlink INSIDE the workspace that
// points OUTSIDE it is denied at Stage 1 (containedPath resolves symlinks).
func TestCheckContainmentSymlinkEscape(t *testing.T) {
	t.Parallel()
	ws := newWS(t)
	outside := t.TempDir()
	// Create a symlink inside ws pointing to an outside directory.
	link := filepath.Join(ws, "escape")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	pc := NewPermissionChecker(PermissionPolicy{WorkspaceRoot: ws, HardDeny: DefaultHardDeny()})
	pc.SetHomeDir(func() (string, error) { return t.TempDir(), nil })
	got := pc.Check(context.Background(), plainTool{name: "ReadFile"}, "ReadFile", `{"path":"escape/secret"}`)
	if got != loop.EffectDeny {
		t.Errorf("symlink escape Check() = %v, want EffectDeny", got)
	}
}

// TestCheckHardDenyBeatsEverything is the headline fail-secure ordering test: a
// HardDeny-matching read (.env) is DENIED even when an EffectChecker, a
// HardApprove "*", AND a persisted allow would all auto-approve it.
func TestCheckHardDenyBeatsEverything(t *testing.T) {
	t.Parallel()
	ws := newWS(t)
	// A .env file in the workspace (so containment passes; only hard-deny stops it).
	if err := os.WriteFile(filepath.Join(ws, ".env"), []byte("SECRET=1"), 0o600); err != nil {
		t.Fatalf("write .env: %v", err)
	}

	tests := []struct {
		name string
		t    tool.InvokableTool
		pol  PermissionPolicy
		ws   []byte
		user []byte
	}{
		{
			name: "effectchecker auto-approve cannot bypass hard-deny",
			t:    staticEffect{name: "ReadFile", effect: loop.EffectAutoApprove, handled: true},
			pol:  PermissionPolicy{WorkspaceRoot: ws, HardDeny: DefaultHardDeny()},
		},
		{
			name: "hard-approve wildcard cannot bypass hard-deny",
			t:    plainTool{name: "ReadFile"},
			pol:  PermissionPolicy{WorkspaceRoot: ws, HardDeny: DefaultHardDeny(), HardApprove: HardApproveRules{Tools: []string{wildcardTool}}},
		},
		{
			name: "persisted allow cannot bypass hard-deny",
			t:    plainTool{name: "ReadFile"},
			pol:  PermissionPolicy{WorkspaceRoot: ws, HardDeny: DefaultHardDeny()},
			ws:   writeApprovals(t, ApprovalRecord{Tool: "ReadFile", Match: "**/.env", Effect: loop.EffectAutoApprove}),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			pc := NewPermissionChecker(tt.pol)
			_, homeFn := fakeHome(t, ws, tt.ws, tt.user)
			pc.SetHomeDir(homeFn)
			got := pc.Check(context.Background(), tt.t, "ReadFile", `{"path":".env"}`)
			if got != loop.EffectDeny {
				t.Errorf("Check() = %v, want EffectDeny (hard-deny must beat approval)", got)
			}
		})
	}
}

// TestCheckHardDenyBash asserts a denied Bash prefix is denied, beating a wildcard
// hard-approve.
func TestCheckHardDenyBash(t *testing.T) {
	t.Parallel()
	ws := newWS(t)
	tests := []struct {
		name string
		cmd  string
		want loop.Effect
	}{
		{name: "sudo denied beats wildcard approve", cmd: "sudo rm x", want: loop.EffectDeny},
		{name: "rm -rf root denied beats wildcard approve", cmd: "rm -rf /", want: loop.EffectDeny},
		{name: "normalized whitespace still denied", cmd: "sudo    apt   update", want: loop.EffectDeny},
		// A benign command is not hard-denied, so it reaches the wildcard
		// hard-approve and is auto-approved (proving the deny gate, not approval,
		// is what stops the dangerous ones).
		{name: "benign command not denied reaches wildcard approve", cmd: "ls -la", want: loop.EffectAutoApprove},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			pc := NewPermissionChecker(PermissionPolicy{
				WorkspaceRoot: ws,
				HardDeny:      DefaultHardDeny(),
				HardApprove:   HardApproveRules{Tools: []string{wildcardTool}},
			})
			pc.SetHomeDir(func() (string, error) { return t.TempDir(), nil })
			args, _ := json.Marshal(map[string]string{"command": tt.cmd})
			got := pc.Check(context.Background(), plainTool{name: "Bash"}, "Bash", string(args))
			if got != tt.want {
				t.Errorf("Check(%q) = %v, want %v", tt.cmd, got, tt.want)
			}
		})
	}
}

// TestCheckStages walks the remaining stages: EffectChecker, HardApprove,
// persisted, session, default.
func TestCheckStages(t *testing.T) {
	t.Parallel()
	ws := newWS(t)
	if err := os.WriteFile(filepath.Join(ws, "main.go"), []byte("package main"), 0o600); err != nil {
		t.Fatalf("write main.go: %v", err)
	}

	tests := []struct {
		name     string
		tl       tool.InvokableTool
		toolName string
		args     string
		pol      PermissionPolicy
		ws       []byte
		user     []byte
		want     loop.Effect
	}{
		{
			name:     "stage3 effectchecker handled wins",
			tl:       staticEffect{name: "ReadFile", effect: loop.EffectAutoApprove, handled: true},
			toolName: "ReadFile", args: `{"path":"main.go"}`,
			pol:  PermissionPolicy{WorkspaceRoot: ws, HardDeny: DefaultHardDeny()},
			want: loop.EffectAutoApprove,
		},
		{
			name:     "stage3 effectchecker unhandled falls through",
			tl:       staticEffect{name: "ReadFile", effect: loop.EffectAutoApprove, handled: false},
			toolName: "ReadFile", args: `{"path":"main.go"}`,
			pol:  PermissionPolicy{WorkspaceRoot: ws, HardDeny: DefaultHardDeny()},
			want: loop.EffectAsk,
		},
		{
			name:     "stage4 hard-approve exact tool name",
			tl:       plainTool{name: "Glob"},
			toolName: "Glob", args: `{"pattern":"*.go","root":"."}`,
			pol:  PermissionPolicy{WorkspaceRoot: ws, HardDeny: DefaultHardDeny(), HardApprove: HardApproveRules{Tools: []string{"Glob"}}},
			want: loop.EffectAutoApprove,
		},
		{
			name:     "stage4 hard-approve wildcard",
			tl:       plainTool{name: "ReadFile"},
			toolName: "ReadFile", args: `{"path":"main.go"}`,
			pol:  PermissionPolicy{WorkspaceRoot: ws, HardDeny: DefaultHardDeny(), HardApprove: HardApproveRules{Tools: []string{wildcardTool}}},
			want: loop.EffectAutoApprove,
		},
		{
			name:     "stage5 persisted workspace allow",
			tl:       plainTool{name: "ReadFile"},
			toolName: "ReadFile", args: `{"path":"main.go"}`,
			pol:  PermissionPolicy{WorkspaceRoot: ws, HardDeny: DefaultHardDeny()},
			ws:   writeApprovals(t, ApprovalRecord{Tool: "ReadFile", Match: "main.go", Effect: loop.EffectAutoApprove}),
			want: loop.EffectAutoApprove,
		},
		{
			name:     "stage5 persisted user allow empty match means all",
			tl:       plainTool{name: "ReadFile"},
			toolName: "ReadFile", args: `{"path":"main.go"}`,
			pol:  PermissionPolicy{WorkspaceRoot: ws, HardDeny: DefaultHardDeny()},
			user: writeApprovals(t, ApprovalRecord{Tool: "ReadFile", Effect: loop.EffectAutoApprove}),
			want: loop.EffectAutoApprove,
		},
		{
			name:     "stage5 persisted non-matching path falls through",
			tl:       plainTool{name: "ReadFile"},
			toolName: "ReadFile", args: `{"path":"main.go"}`,
			pol:  PermissionPolicy{WorkspaceRoot: ws, HardDeny: DefaultHardDeny()},
			ws:   writeApprovals(t, ApprovalRecord{Tool: "ReadFile", Match: "other.go", Effect: loop.EffectAutoApprove}),
			want: loop.EffectAsk,
		},
		{
			name:     "stage6 session policy allow",
			tl:       plainTool{name: "ReadFile"},
			toolName: "ReadFile", args: `{"path":"main.go"}`,
			pol: PermissionPolicy{WorkspaceRoot: ws, HardDeny: DefaultHardDeny(), Policies: []loop.ToolPolicy{
				{Tool: "ReadFile", Effect: loop.EffectAutoApprove, Match: []string{"main.go"}},
			}},
			want: loop.EffectAutoApprove,
		},
		{
			name:     "stage6 session policy empty match means all",
			tl:       plainTool{name: "ReadFile"},
			toolName: "ReadFile", args: `{"path":"main.go"}`,
			pol: PermissionPolicy{WorkspaceRoot: ws, HardDeny: DefaultHardDeny(), Policies: []loop.ToolPolicy{
				{Tool: "ReadFile", Effect: loop.EffectAutoApprove},
			}},
			want: loop.EffectAutoApprove,
		},
		{
			name:     "stage7 default ask",
			tl:       plainTool{name: "ReadFile"},
			toolName: "ReadFile", args: `{"path":"main.go"}`,
			pol:  PermissionPolicy{WorkspaceRoot: ws, HardDeny: DefaultHardDeny()},
			want: loop.EffectAsk,
		},
		{
			name:     "websearch no fs falls to default ask",
			tl:       plainTool{name: "WebSearch"},
			toolName: "WebSearch", args: `{"query":"golang"}`,
			pol:  PermissionPolicy{WorkspaceRoot: ws, HardDeny: DefaultHardDeny()},
			want: loop.EffectAsk,
		},
		{
			name:     "fetch no fs persisted allow",
			tl:       plainTool{name: "Fetch"},
			toolName: "Fetch", args: `{"url":"https://example.com","method":"GET"}`,
			pol:  PermissionPolicy{WorkspaceRoot: ws, HardDeny: DefaultHardDeny()},
			user: writeApprovals(t, ApprovalRecord{Tool: "Fetch", Match: "GET https://example.com", Effect: loop.EffectAutoApprove}),
			want: loop.EffectAutoApprove,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			pc := NewPermissionChecker(tt.pol)
			_, homeFn := fakeHome(t, ws, tt.ws, tt.user)
			pc.SetHomeDir(homeFn)
			got := pc.Check(context.Background(), tt.tl, tt.toolName, tt.args)
			if got != tt.want {
				t.Errorf("Check() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestCheckDenyBeatsAllow proves a deny in EITHER file (especially the user file)
// overrides an allow in the other.
func TestCheckDenyBeatsAllow(t *testing.T) {
	t.Parallel()
	ws := newWS(t)
	if err := os.WriteFile(filepath.Join(ws, "main.go"), []byte("package main"), 0o600); err != nil {
		t.Fatalf("write main.go: %v", err)
	}

	tests := []struct {
		name string
		ws   []byte
		user []byte
		want loop.Effect
	}{
		{
			name: "user deny overrides workspace allow",
			ws:   writeApprovals(t, ApprovalRecord{Tool: "ReadFile", Match: "main.go", Effect: loop.EffectAutoApprove}),
			user: writeApprovals(t, ApprovalRecord{Tool: "ReadFile", Match: "main.go", Effect: loop.EffectDeny}),
			want: loop.EffectDeny,
		},
		{
			name: "workspace deny overrides user allow",
			ws:   writeApprovals(t, ApprovalRecord{Tool: "ReadFile", Match: "main.go", Effect: loop.EffectDeny}),
			user: writeApprovals(t, ApprovalRecord{Tool: "ReadFile", Effect: loop.EffectAutoApprove}),
			want: loop.EffectDeny,
		},
		{
			name: "deny and allow in same workspace file deny wins",
			ws: writeApprovals(t,
				ApprovalRecord{Tool: "ReadFile", Effect: loop.EffectAutoApprove},
				ApprovalRecord{Tool: "ReadFile", Match: "main.go", Effect: loop.EffectDeny}),
			want: loop.EffectDeny,
		},
		{
			name: "both allow yields auto-approve",
			ws:   writeApprovals(t, ApprovalRecord{Tool: "ReadFile", Match: "main.go", Effect: loop.EffectAutoApprove}),
			user: writeApprovals(t, ApprovalRecord{Tool: "ReadFile", Effect: loop.EffectAutoApprove}),
			want: loop.EffectAutoApprove,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			pc := NewPermissionChecker(PermissionPolicy{WorkspaceRoot: ws, HardDeny: DefaultHardDeny()})
			_, homeFn := fakeHome(t, ws, tt.ws, tt.user)
			pc.SetHomeDir(homeFn)
			got := pc.Check(context.Background(), plainTool{name: "ReadFile"}, "ReadFile", `{"path":"main.go"}`)
			if got != tt.want {
				t.Errorf("Check() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestCheckMalformedApprovals proves a malformed file → EffectAsk (never
// AutoApprove), and a single bad record is skipped while valid records still
// apply.
func TestCheckMalformedApprovals(t *testing.T) {
	t.Parallel()
	ws := newWS(t)
	if err := os.WriteFile(filepath.Join(ws, "main.go"), []byte("package main"), 0o600); err != nil {
		t.Fatalf("write main.go: %v", err)
	}

	// A file whose top-level JSON is corrupt.
	corrupt := []byte(`{"version":1,"approvals":[ this is not json `)
	// A file with one bad record (unknown effect) and one valid allow record.
	badRecordValidRecord := []byte(`{"version":1,"approvals":[` +
		`{"tool":"ReadFile","match":"main.go","effect":"frobnicate"},` +
		`{"tool":"ReadFile","match":"main.go","effect":"allow"}]}`)
	// A file with only a bad record.
	onlyBadRecord := []byte(`{"version":1,"approvals":[{"tool":"ReadFile","effect":"yolo"}]}`)

	tests := []struct {
		name string
		ws   []byte
		want loop.Effect
	}{
		{name: "corrupt file fails open to ask", ws: corrupt, want: loop.EffectAsk},
		{name: "bad record skipped valid record applies", ws: badRecordValidRecord, want: loop.EffectAutoApprove},
		{name: "only bad record yields ask", ws: onlyBadRecord, want: loop.EffectAsk},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			pc := NewPermissionChecker(PermissionPolicy{WorkspaceRoot: ws, HardDeny: DefaultHardDeny()})
			_, homeFn := fakeHome(t, ws, tt.ws, nil)
			pc.SetHomeDir(homeFn)
			got := pc.Check(context.Background(), plainTool{name: "ReadFile"}, "ReadFile", `{"path":"main.go"}`)
			if got != tt.want {
				t.Errorf("Check() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestCheckMalformedApprovalsWarns proves a malformed approvals file emits a
// WARN-level log naming the file PATH but NOT its (potentially sensitive)
// CONTENTS, and that the decision is EffectAsk. This test is intentionally NOT
// parallel: it swaps the process-global default slog logger to capture output.
func TestCheckMalformedApprovalsWarns(t *testing.T) {
	ws := newWS(t)
	if err := os.WriteFile(filepath.Join(ws, "main.go"), []byte("package main"), 0o600); err != nil {
		t.Fatalf("write main.go: %v", err)
	}
	// A corrupt file whose contents include a distinctive secret-looking token we
	// can assert is NEVER logged.
	const secretToken = "TOPSECRET_DO_NOT_LOG"
	corrupt := []byte(`{"version":1,"approvals":[ ` + secretToken + ` not json `)

	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	pc := NewPermissionChecker(PermissionPolicy{WorkspaceRoot: ws, HardDeny: DefaultHardDeny()})
	_, homeFn := fakeHome(t, ws, corrupt, nil)
	pc.SetHomeDir(homeFn)

	got := pc.Check(context.Background(), plainTool{name: "ReadFile"}, "ReadFile", `{"path":"main.go"}`)
	if got != loop.EffectAsk {
		t.Fatalf("Check() = %v, want EffectAsk for malformed file", got)
	}

	logged := buf.String()
	if !strings.Contains(logged, "malformed approvals file") {
		t.Errorf("expected a malformed-file warning; log was:\n%s", logged)
	}
	if !strings.Contains(logged, "approvals.json") {
		t.Errorf("warning should name the file path; log was:\n%s", logged)
	}
	if strings.Contains(logged, secretToken) {
		t.Errorf("warning leaked file CONTENTS (%q); log was:\n%s", secretToken, logged)
	}
}

// TestCheckSkippedRecordsWarn proves that when ≥1 individual record is malformed
// and dropped (while valid records still apply), a SINGLE aggregate WARN is
// emitted carrying the dropped COUNT and the file PATH but NOT the record
// CONTENTS (which could carry sensitive match patterns/secrets). Like the
// malformed-file test it is intentionally NOT parallel — it swaps the global
// slog default to capture output.
func TestCheckSkippedRecordsWarn(t *testing.T) {
	ws := newWS(t)
	if err := os.WriteFile(filepath.Join(ws, "main.go"), []byte("package main"), 0o600); err != nil {
		t.Fatalf("write main.go: %v", err)
	}
	// A distinctive secret embedded in a record's match field. The record's effect
	// is unknown ("frobnicate") so parseApprovalsFile DROPS it (counted as
	// skipped); a sibling valid allow record keeps the stage live.
	const secretToken = "TOPSECRET_MATCH_DO_NOT_LOG"
	skipped := []byte(`{"version":1,"approvals":[` +
		`{"tool":"ReadFile","match":"` + secretToken + `","effect":"frobnicate"},` +
		`{"tool":"ReadFile","match":"main.go","effect":"allow"}]}`)

	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	pc := NewPermissionChecker(PermissionPolicy{WorkspaceRoot: ws, HardDeny: DefaultHardDeny()})
	_, homeFn := fakeHome(t, ws, skipped, nil)
	pc.SetHomeDir(homeFn)

	// The valid allow record must still apply (skip is fail-secure, not fatal).
	got := pc.Check(context.Background(), plainTool{name: "ReadFile"}, "ReadFile", `{"path":"main.go"}`)
	if got != loop.EffectAutoApprove {
		t.Fatalf("Check() = %v, want EffectAutoApprove (valid record applies despite skip)", got)
	}

	logged := buf.String()
	if !strings.Contains(logged, "skipped malformed approval records") {
		t.Errorf("expected the aggregate skipped-records warning; log was:\n%s", logged)
	}
	if !strings.Contains(logged, "count=1") {
		t.Errorf("warning should report the dropped count (count=1); log was:\n%s", logged)
	}
	if !strings.Contains(logged, "approvals.json") {
		t.Errorf("warning should name the file path; log was:\n%s", logged)
	}
	if strings.Contains(logged, secretToken) {
		t.Errorf("warning leaked record CONTENTS (%q); log was:\n%s", secretToken, logged)
	}
}

// TestCheckInRepoApprovalsIgnored proves an in-repo <ws>/.urvi/approvals.json has
// NO effect — only ~/.urvi/workspaces/<hash>/ is read.
func TestCheckInRepoApprovalsIgnored(t *testing.T) {
	t.Parallel()
	ws := newWS(t)
	if err := os.WriteFile(filepath.Join(ws, "main.go"), []byte("package main"), 0o600); err != nil {
		t.Fatalf("write main.go: %v", err)
	}
	// Plant a hostile in-repo approvals file granting Bash everything.
	inRepoDir := filepath.Join(ws, ".urvi")
	if err := os.MkdirAll(inRepoDir, 0o700); err != nil {
		t.Fatalf("mkdir in-repo .urvi: %v", err)
	}
	hostile := writeApprovals(t, ApprovalRecord{Tool: "ReadFile", Effect: loop.EffectAutoApprove})
	if err := os.WriteFile(filepath.Join(inRepoDir, "approvals.json"), hostile, 0o600); err != nil {
		t.Fatalf("write in-repo approvals: %v", err)
	}

	pc := NewPermissionChecker(PermissionPolicy{WorkspaceRoot: ws, HardDeny: DefaultHardDeny()})
	// Fake home is EMPTY (no out-of-repo approvals), so the only approvals on disk
	// are the in-repo hostile ones — which must be ignored.
	_, homeFn := fakeHome(t, ws, nil, nil)
	pc.SetHomeDir(homeFn)
	got := pc.Check(context.Background(), plainTool{name: "ReadFile"}, "ReadFile", `{"path":"main.go"}`)
	if got != loop.EffectAsk {
		t.Errorf("Check() = %v, want EffectAsk (in-repo approvals must be ignored)", got)
	}
}

// TestCheckMissingHomeFailsSecure proves that if the home dir cannot be resolved
// the persisted stage contributes nothing (falls through to Ask), never approves.
func TestCheckMissingHomeFailsSecure(t *testing.T) {
	t.Parallel()
	ws := newWS(t)
	if err := os.WriteFile(filepath.Join(ws, "main.go"), []byte("package main"), 0o600); err != nil {
		t.Fatalf("write main.go: %v", err)
	}
	pc := NewPermissionChecker(PermissionPolicy{WorkspaceRoot: ws, HardDeny: DefaultHardDeny()})
	pc.SetHomeDir(func() (string, error) { return "", os.ErrNotExist })
	got := pc.Check(context.Background(), plainTool{name: "ReadFile"}, "ReadFile", `{"path":"main.go"}`)
	if got != loop.EffectAsk {
		t.Errorf("Check() = %v, want EffectAsk when home dir unresolvable", got)
	}
}

// TestCheckUnknownToolWithPathFailsSecure proves an unclassifiable tool name that
// carries a path-shaped arg still runs containment + hard-deny (so it can DENY a
// secret/escape), and otherwise falls to Ask (never auto-approve).
func TestCheckUnknownToolWithPathFailsSecure(t *testing.T) {
	t.Parallel()
	ws := newWS(t)
	if err := os.WriteFile(filepath.Join(ws, ".env"), []byte("S=1"), 0o600); err != nil {
		t.Fatalf("write .env: %v", err)
	}

	tests := []struct {
		name string
		args string
		want loop.Effect
	}{
		{name: "unknown tool reading .env denied", args: `{"path":".env"}`, want: loop.EffectDeny},
		{name: "unknown tool escaping denied", args: `{"path":"../../etc/passwd"}`, want: loop.EffectDeny},
		{name: "unknown tool benign path falls to ask", args: `{"path":"a/b.go"}`, want: loop.EffectAsk},
		{name: "unknown tool no path falls to ask", args: `{"foo":"bar"}`, want: loop.EffectAsk},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			pc := NewPermissionChecker(PermissionPolicy{WorkspaceRoot: ws, HardDeny: DefaultHardDeny()})
			pc.SetHomeDir(func() (string, error) { return t.TempDir(), nil })
			got := pc.Check(context.Background(), plainTool{name: "MysteryTool"}, "MysteryTool", tt.args)
			if got != tt.want {
				t.Errorf("Check() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestCheckMalformedArgsFailSecure proves that unparseable args for a known tool
// never auto-approve. A path tool with bad JSON cannot pass containment → Deny;
// other tools fall through to Ask.
func TestCheckMalformedArgsFailSecure(t *testing.T) {
	t.Parallel()
	ws := newWS(t)
	tests := []struct {
		name     string
		toolName string
		args     string
		want     loop.Effect
	}{
		{name: "readfile bad json denied", toolName: "ReadFile", args: `not json`, want: loop.EffectDeny},
		{name: "readfile empty path denied", toolName: "ReadFile", args: `{}`, want: loop.EffectAsk},
		{name: "bash bad json falls to ask", toolName: "Bash", args: `not json`, want: loop.EffectAsk},
		{name: "fetch bad json falls to ask", toolName: "Fetch", args: `not json`, want: loop.EffectAsk},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			pc := NewPermissionChecker(PermissionPolicy{
				WorkspaceRoot: ws,
				HardDeny:      DefaultHardDeny(),
				HardApprove:   HardApproveRules{Tools: []string{wildcardTool}},
			})
			pc.SetHomeDir(func() (string, error) { return t.TempDir(), nil })
			got := pc.Check(context.Background(), plainTool{name: tt.toolName}, tt.toolName, tt.args)
			if got != tt.want {
				t.Errorf("Check() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestReadGuard proves DeniedRead matches secret globs via the absolute matcher
// and MaxReadBytes returns the policy value.
func TestReadGuard(t *testing.T) {
	t.Parallel()
	home := "/home/tester"
	pc := NewPermissionChecker(PermissionPolicy{
		WorkspaceRoot: "/ws",
		HardDeny: HardDenyRules{
			DeniedReadPaths: []string{"~/.ssh/**", "**/.env", "**/*.pem", "**/id_rsa", "~/.urvi/**"},
			MaxReadBytes:    4096,
		},
	})
	pc.SetHomeDir(func() (string, error) { return home, nil })

	tests := []struct {
		name    string
		absPath string
		want    bool
	}{
		{name: "deep env denied", absPath: "/ws/a/b/.env", want: true},
		{name: "root env denied", absPath: "/ws/.env", want: true},
		{name: "pem denied", absPath: "/ws/certs/server.pem", want: true},
		{name: "id_rsa denied", absPath: "/ws/keys/id_rsa", want: true},
		{name: "home ssh denied", absPath: "/home/tester/.ssh/id_ed25519", want: true},
		{name: "home ssh nested denied", absPath: "/home/tester/.ssh/config", want: true},
		{name: "home urvi denied", absPath: "/home/tester/.urvi/approvals.json", want: true},
		{name: "ordinary go file allowed", absPath: "/ws/main.go", want: false},
		{name: "envrc not env", absPath: "/ws/.envrc", want: false},
		{name: "other home ssh-like not matched", absPath: "/home/other/.ssh/id_rsa", want: true}, // **/id_rsa matches anywhere
		{name: "pem-ish not pem", absPath: "/ws/notes.pemx", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := pc.DeniedRead(tt.absPath); got != tt.want {
				t.Errorf("DeniedRead(%q) = %v, want %v", tt.absPath, got, tt.want)
			}
		})
	}

	if got := pc.MaxReadBytes(); got != 4096 {
		t.Errorf("MaxReadBytes() = %d, want 4096", got)
	}
}

// TestReadGuardInterfaces is a compile-time assertion that *PermissionChecker
// satisfies loop.ReadGuard AND loop.PermissionGate (the latter now that Grant
// exists; the package-level assertion lives in grant.go, re-stated here so the
// gate contract is exercised alongside the ReadGuard one).
func TestReadGuardInterfaces(t *testing.T) {
	t.Parallel()
	var _ loop.ReadGuard = (*PermissionChecker)(nil)
	var _ loop.PermissionGate = (*PermissionChecker)(nil)
}

// TestMatchHardDenyAbs unit-tests the absolute hard-deny matcher directly,
// including the ~/ expansion and the "** matches leading segments" requirement.
func TestMatchHardDenyAbs(t *testing.T) {
	t.Parallel()
	home := "/home/tester"
	tests := []struct {
		name    string
		pattern string
		abs     string
		want    bool
	}{
		{name: "doublestar env leading segments", pattern: "**/.env", abs: "/ws/a/b/.env", want: true},
		{name: "doublestar env at root", pattern: "**/.env", abs: "/ws/.env", want: true},
		{name: "tilde ssh expands to home", pattern: "~/.ssh/**", abs: "/home/tester/.ssh/id_rsa", want: true},
		{name: "tilde ssh exact dir not under not matched", pattern: "~/.ssh/**", abs: "/home/tester/.config/id_rsa", want: false},
		{name: "tilde urvi nested", pattern: "~/.urvi/**", abs: "/home/tester/.urvi/workspaces/x/approvals.json", want: true},
		{name: "pem glob within segment", pattern: "**/*.pem", abs: "/ws/a/cert.pem", want: true},
		{name: "non-match", pattern: "**/.env", abs: "/ws/.environment", want: false},
		{name: "id_rsa anywhere", pattern: "**/id_rsa", abs: "/deep/nested/path/id_rsa", want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := matchHardDenyAbs(tt.pattern, tt.abs, home); got != tt.want {
				t.Errorf("matchHardDenyAbs(%q, %q) = %v, want %v", tt.pattern, tt.abs, got, tt.want)
			}
		})
	}
}

// TestCheckConcurrent runs Check from many goroutines while Policies is appended
// (under the checker's lock via the session-grant path), proving -race cleanliness.
func TestCheckConcurrent(t *testing.T) {
	t.Parallel()
	ws := newWS(t)
	if err := os.WriteFile(filepath.Join(ws, "main.go"), []byte("package main"), 0o600); err != nil {
		t.Fatalf("write main.go: %v", err)
	}
	pc := NewPermissionChecker(PermissionPolicy{WorkspaceRoot: ws, HardDeny: DefaultHardDeny()})
	_, homeFn := fakeHome(t, ws, nil, nil)
	pc.SetHomeDir(homeFn)

	var wg sync.WaitGroup
	// Readers: concurrent Check calls.
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				_ = pc.Check(context.Background(), plainTool{name: "ReadFile"}, "ReadFile", `{"path":"main.go"}`)
			}
		}()
	}
	// Writers: append session policies under the checker's lock.
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				pc.appendSessionPolicy(loop.ToolPolicy{Tool: "ReadFile", Effect: loop.EffectAutoApprove, Match: []string{"main.go"}})
			}
		}()
	}
	wg.Wait()
}
