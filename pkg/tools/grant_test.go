package tools

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/ciram-co/looprig/pkg/loop"
	"github.com/ciram-co/looprig/pkg/tool"
)

// grant_test.go exercises the WRITE side of the policy store: Grant's per-scope
// behaviour (session append, out-of-repo workspace write, ScopeOnce no-op),
// the Match derivation per tool, the filesystem hardening (dir/file perms,
// atomic write, never-in-repo), append semantics, and the Grant→Check
// round-trip through the real file + hashcache.

// readApprovalsFile reads and decodes an ApprovalsFile fixture from disk.
func readApprovalsFile(t *testing.T, p string) ApprovalsFile {
	t.Helper()
	b, err := os.ReadFile(p) // #nosec G304 -- test fixture path under t.TempDir().
	if err != nil {
		t.Fatalf("read approvals file %q: %v", p, err)
	}
	var af ApprovalsFile
	if err := json.Unmarshal(b, &af); err != nil {
		t.Fatalf("unmarshal approvals file %q: %v", p, err)
	}
	return af
}

// wsApprovalsPathFor returns the expected out-of-repo workspace approvals path
// for a (home, wsRoot) pair.
func wsApprovalsPathFor(t *testing.T, home, wsRoot string) string {
	t.Helper()
	hash, err := workspaceHash(wsRoot)
	if err != nil {
		t.Fatalf("workspaceHash: %v", err)
	}
	return workspaceApprovalsPath(home, hash)
}

// TestGrantWorkspaceWritesOutOfRepo proves a ScopeWorkspace Grant writes the
// record under <home>/.looprig/workspaces/<hash>/approvals.json (NOT under the
// workspace root), with dirs 0700 and the file 0600, and that the very next
// Check of the same call returns EffectAutoApprove (round-trip through the real
// file + hashcache).
func TestGrantWorkspaceWritesOutOfRepo(t *testing.T) {
	t.Parallel()
	ws := newWS(t)
	if err := os.WriteFile(filepath.Join(ws, "main.go"), []byte("package main"), 0o600); err != nil {
		t.Fatalf("write main.go: %v", err)
	}
	home := t.TempDir()

	pc, err := NewPermissionChecker(PermissionPolicy{WorkspaceRoot: ws, HardDeny: DefaultHardDeny()}, WithHomeDir(func() (string, error) { return home, nil }))
	if err != nil {
		t.Fatalf("NewPermissionChecker: %v", err)
	}

	if err := pc.Grant(context.Background(), "ReadFile", `{"path":"main.go"}`, tool.ScopeWorkspace); err != nil {
		t.Fatalf("Grant(ScopeWorkspace): %v", err)
	}

	wsFile := wsApprovalsPathFor(t, home, ws)

	// The file must live out of the repo.
	if _, err := os.Stat(wsFile); err != nil {
		t.Fatalf("expected approvals file at %q: %v", wsFile, err)
	}
	// The repo must NOT have an approvals file.
	if _, err := os.Stat(filepath.Join(ws, ".looprig", "approvals.json")); !os.IsNotExist(err) {
		t.Fatalf("Grant must NOT write into the repo (.looprig/approvals.json); stat err=%v", err)
	}

	// File perms 0600.
	fi, err := os.Stat(wsFile)
	if err != nil {
		t.Fatalf("stat approvals file: %v", err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("approvals file perm = %o, want 0600", perm)
	}
	// Dir perms 0700 (both <home>/.looprig and the workspace hash dir).
	for _, dir := range []string{filepath.Join(home, looprigDirName), filepath.Dir(wsFile)} {
		di, err := os.Stat(dir)
		if err != nil {
			t.Fatalf("stat dir %q: %v", dir, err)
		}
		if perm := di.Mode().Perm(); perm != 0o700 {
			t.Errorf("dir %q perm = %o, want 0700", dir, perm)
		}
	}

	// The record content: a single allow record for ReadFile matching main.go.
	af := readApprovalsFile(t, wsFile)
	if af.Version != 1 {
		t.Errorf("version = %d, want 1", af.Version)
	}
	if len(af.Approvals) != 1 {
		t.Fatalf("approvals len = %d, want 1", len(af.Approvals))
	}
	rec := af.Approvals[0]
	if rec.Tool != "ReadFile" || rec.Match != "main.go" || rec.Effect != loop.EffectAutoApprove {
		t.Errorf("record = %+v, want {ReadFile main.go allow}", rec)
	}

	// Round-trip: the next Check sees the persisted allow.
	got := pc.Check(context.Background(), plainTool{name: "ReadFile"}, "ReadFile", `{"path":"main.go"}`)
	if got != loop.EffectAutoApprove {
		t.Errorf("post-Grant Check() = %v, want EffectAutoApprove", got)
	}
}

// TestGrantSessionHonoredByCheck proves a ScopeSession Grant adds an in-memory
// policy the next Check honours, and writes NOTHING to disk.
func TestGrantSessionHonoredByCheck(t *testing.T) {
	t.Parallel()
	ws := newWS(t)
	if err := os.WriteFile(filepath.Join(ws, "main.go"), []byte("package main"), 0o600); err != nil {
		t.Fatalf("write main.go: %v", err)
	}
	home := t.TempDir()

	pc, err := NewPermissionChecker(PermissionPolicy{WorkspaceRoot: ws, HardDeny: DefaultHardDeny()}, WithHomeDir(func() (string, error) { return home, nil }))
	if err != nil {
		t.Fatalf("NewPermissionChecker: %v", err)
	}

	if err := pc.Grant(context.Background(), "ReadFile", `{"path":"main.go"}`, tool.ScopeSession); err != nil {
		t.Fatalf("Grant(ScopeSession): %v", err)
	}

	// No file written for a session grant.
	if _, err := os.Stat(wsApprovalsPathFor(t, home, ws)); !os.IsNotExist(err) {
		t.Fatalf("ScopeSession must NOT write a file; stat err=%v", err)
	}

	got := pc.Check(context.Background(), plainTool{name: "ReadFile"}, "ReadFile", `{"path":"main.go"}`)
	if got != loop.EffectAutoApprove {
		t.Errorf("post-session-Grant Check() = %v, want EffectAutoApprove", got)
	}
}

// TestGrantOnceIsNotPersisted proves ScopeOnce returns a typed error and writes
// nothing (it is never passed by the runner, but Grant must refuse it
// fail-secure rather than persist).
func TestGrantOnceIsNotPersisted(t *testing.T) {
	t.Parallel()
	ws := newWS(t)
	home := t.TempDir()
	pc, err := NewPermissionChecker(PermissionPolicy{WorkspaceRoot: ws, HardDeny: DefaultHardDeny()}, WithHomeDir(func() (string, error) { return home, nil }))
	if err != nil {
		t.Fatalf("NewPermissionChecker: %v", err)
	}

	err = pc.Grant(context.Background(), "ReadFile", `{"path":"main.go"}`, tool.ScopeOnce)
	if err == nil {
		t.Fatal("Grant(ScopeOnce) = nil, want a typed error (must not persist)")
	}
	var scopeErr *UnsupportedScopeError
	if !errors.As(err, &scopeErr) {
		t.Errorf("Grant(ScopeOnce) error = %T, want *UnsupportedScopeError", err)
	}
	// Nothing written, no session policy added.
	if _, statErr := os.Stat(wsApprovalsPathFor(t, home, ws)); !os.IsNotExist(statErr) {
		t.Errorf("ScopeOnce must NOT write a file; stat err=%v", statErr)
	}
	got := pc.Check(context.Background(), plainTool{name: "ReadFile"}, "ReadFile", `{"path":"main.go"}`)
	if got != loop.EffectAsk {
		t.Errorf("after ScopeOnce, Check() = %v, want EffectAsk (nothing persisted)", got)
	}
}

// TestGrantUnknownScopeRejected proves an out-of-range scope value is refused
// with a typed error (fail-secure: never persist an unknown scope).
func TestGrantUnknownScopeRejected(t *testing.T) {
	t.Parallel()
	ws := newWS(t)
	home := t.TempDir()
	pc, err := NewPermissionChecker(PermissionPolicy{WorkspaceRoot: ws, HardDeny: DefaultHardDeny()}, WithHomeDir(func() (string, error) { return home, nil }))
	if err != nil {
		t.Fatalf("NewPermissionChecker: %v", err)
	}

	err = pc.Grant(context.Background(), "ReadFile", `{"path":"main.go"}`, tool.ApprovalScope(99))
	var scopeErr *UnsupportedScopeError
	if !errors.As(err, &scopeErr) {
		t.Errorf("Grant(unknown scope) error = %T, want *UnsupportedScopeError", err)
	}
}

// TestGrantMatchDerivation proves the per-tool Match derivation: Bash records the
// EXACT normalized command, Fetch records "METHOD scheme://host", and file tools
// record the workspace-relative canonical path glob. Each grant is read back from
// the real out-of-repo file.
func TestGrantMatchDerivation(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		toolName  string
		argsJSON  string
		wantMatch string
	}{
		{
			name:      "bash records exact normalized command",
			toolName:  "Bash",
			argsJSON:  `{"command":"go    test\t./..."}`,
			wantMatch: "go test ./...",
		},
		{
			name:      "fetch records method scheme host no port",
			toolName:  "Fetch",
			argsJSON:  `{"url":"https://API.GitHub.com:443/repos","method":"get"}`,
			wantMatch: "GET https://api.github.com",
		},
		{
			name:      "readfile records ws-relative path",
			toolName:  "ReadFile",
			argsJSON:  `{"path":"src/main.go"}`,
			wantMatch: "src/main.go",
		},
		{
			name:      "glob records ws-relative root",
			toolName:  "Glob",
			argsJSON:  `{"pattern":"*.go","root":"src"}`,
			wantMatch: "src",
		},
		{
			name:      "websearch records empty match (tool level)",
			toolName:  "WebSearch",
			argsJSON:  `{"query":"golang"}`,
			wantMatch: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ws := newWS(t)
			// Create src/ so a file/glob path is contained (it need not exist for
			// containedPath, but creating it keeps the canonical rel form stable).
			if err := os.MkdirAll(filepath.Join(ws, "src"), 0o700); err != nil {
				t.Fatalf("mkdir src: %v", err)
			}
			home := t.TempDir()
			pc, err := NewPermissionChecker(PermissionPolicy{WorkspaceRoot: ws, HardDeny: DefaultHardDeny()}, WithHomeDir(func() (string, error) { return home, nil }))
			if err != nil {
				t.Fatalf("NewPermissionChecker: %v", err)
			}

			if err := pc.Grant(context.Background(), tt.toolName, tt.argsJSON, tool.ScopeWorkspace); err != nil {
				t.Fatalf("Grant: %v", err)
			}
			af := readApprovalsFile(t, wsApprovalsPathFor(t, home, ws))
			if len(af.Approvals) != 1 {
				t.Fatalf("approvals len = %d, want 1", len(af.Approvals))
			}
			rec := af.Approvals[0]
			if rec.Tool != tt.toolName {
				t.Errorf("tool = %q, want %q", rec.Tool, tt.toolName)
			}
			if rec.Match != tt.wantMatch {
				t.Errorf("match = %q, want %q", rec.Match, tt.wantMatch)
			}
			if rec.Effect != loop.EffectAutoApprove {
				t.Errorf("effect = %v, want EffectAutoApprove", rec.Effect)
			}
		})
	}
}

// TestGrantAppendsDoesNotClobber proves a second Grant appends to the existing
// records rather than overwriting the first.
func TestGrantAppendsDoesNotClobber(t *testing.T) {
	t.Parallel()
	ws := newWS(t)
	home := t.TempDir()
	pc, err := NewPermissionChecker(PermissionPolicy{WorkspaceRoot: ws, HardDeny: DefaultHardDeny()}, WithHomeDir(func() (string, error) { return home, nil }))
	if err != nil {
		t.Fatalf("NewPermissionChecker: %v", err)
	}

	if err := pc.Grant(context.Background(), "Bash", `{"command":"go test ./..."}`, tool.ScopeWorkspace); err != nil {
		t.Fatalf("first Grant: %v", err)
	}
	if err := pc.Grant(context.Background(), "Bash", `{"command":"go build ./..."}`, tool.ScopeWorkspace); err != nil {
		t.Fatalf("second Grant: %v", err)
	}

	af := readApprovalsFile(t, wsApprovalsPathFor(t, home, ws))
	if len(af.Approvals) != 2 {
		t.Fatalf("approvals len = %d, want 2 (append, not clobber)", len(af.Approvals))
	}
	got := map[string]bool{}
	for _, rec := range af.Approvals {
		got[rec.Match] = true
	}
	if !got["go test ./..."] || !got["go build ./..."] {
		t.Errorf("appended records = %v, want both go test and go build", got)
	}
}

// TestGrantWorkspaceHomeUnresolvableFailsSecure proves an unresolvable home dir
// makes a ScopeWorkspace Grant return a typed error and write nothing.
func TestGrantWorkspaceHomeUnresolvableFailsSecure(t *testing.T) {
	t.Parallel()
	ws := newWS(t)
	// With the fallible constructor, an unresolvable home WHILE a "~/…" pattern is
	// configured is a CONSTRUCTION error (covered by
	// TestNewPermissionChecker_HomeUnresolvable). To still exercise the RUNTIME
	// grant-WRITE fail-secure path this test cares about, use a policy with NO
	// "~/…" pattern: construction succeeds with home=="", and the ScopeWorkspace
	// Grant must still fail with a typed *PolicyStoreError (nowhere safe to write)
	// and persist nothing.
	pc, err := NewPermissionChecker(
		PermissionPolicy{WorkspaceRoot: ws, HardDeny: HardDenyRules{DeniedReadPaths: []string{"**/.env"}}},
		WithHomeDir(func() (string, error) { return "", os.ErrNotExist }),
	)
	if err != nil {
		t.Fatalf("NewPermissionChecker: %v", err)
	}

	err = pc.Grant(context.Background(), "ReadFile", `{"path":"main.go"}`, tool.ScopeWorkspace)
	if err == nil {
		t.Fatal("Grant with unresolvable home = nil, want a typed error")
	}
	var storeErr *PolicyStoreError
	if !errors.As(err, &storeErr) {
		t.Errorf("error = %T, want *PolicyStoreError", err)
	}
}

// TestGrantInRepoApprovalsIgnoredAfterGrant re-asserts (via the Grant flow) that
// only the out-of-repo store is consulted: a hostile in-repo approvals file
// granting a DIFFERENT tool has no effect, and the Grant'd tool is the only one
// auto-approved.
func TestGrantInRepoApprovalsIgnoredAfterGrant(t *testing.T) {
	t.Parallel()
	ws := newWS(t)
	if err := os.WriteFile(filepath.Join(ws, "main.go"), []byte("package main"), 0o600); err != nil {
		t.Fatalf("write main.go: %v", err)
	}
	// Plant a hostile in-repo approvals file granting Bash everything.
	inRepoDir := filepath.Join(ws, ".looprig")
	if err := os.MkdirAll(inRepoDir, 0o700); err != nil {
		t.Fatalf("mkdir in-repo .looprig: %v", err)
	}
	hostile := writeApprovals(t, ApprovalRecord{Tool: "Bash", Effect: loop.EffectAutoApprove})
	if err := os.WriteFile(filepath.Join(inRepoDir, "approvals.json"), hostile, 0o600); err != nil {
		t.Fatalf("write in-repo approvals: %v", err)
	}

	home := t.TempDir()
	pc, err := NewPermissionChecker(PermissionPolicy{WorkspaceRoot: ws, HardDeny: DefaultHardDeny()}, WithHomeDir(func() (string, error) { return home, nil }))
	if err != nil {
		t.Fatalf("NewPermissionChecker: %v", err)
	}

	// Grant ReadFile out-of-repo.
	if err := pc.Grant(context.Background(), "ReadFile", `{"path":"main.go"}`, tool.ScopeWorkspace); err != nil {
		t.Fatalf("Grant: %v", err)
	}

	// ReadFile (granted out-of-repo) is auto-approved.
	if got := pc.Check(context.Background(), plainTool{name: "ReadFile"}, "ReadFile", `{"path":"main.go"}`); got != loop.EffectAutoApprove {
		t.Errorf("ReadFile Check() = %v, want EffectAutoApprove (granted out-of-repo)", got)
	}
	// Bash (granted only by the hostile in-repo file) is NOT auto-approved.
	if got := pc.Check(context.Background(), plainTool{name: "Bash"}, "Bash", `{"command":"rm important"}`); got != loop.EffectAsk {
		t.Errorf("Bash Check() = %v, want EffectAsk (in-repo grant must be ignored)", got)
	}
}
