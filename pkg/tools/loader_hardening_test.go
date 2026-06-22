package tools

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ciram-co/looprig/pkg/loop"
)

// loader_hardening_test.go exercises the READ-side store hardening (design §3c):
// the loader rejects an approvals file that is not a regular file or is
// group/world-writable, and rejects a symlinked component anywhere in the policy
// path — treating each as EMPTY (+ a path-only warning), never auto-approving.

// TestLoaderRejectsWorldWritableFile proves a group- or world-writable approvals
// file is treated as empty (its allow contributes nothing → Ask).
func TestLoaderRejectsWorldWritableFile(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		perm os.FileMode
		want loop.Effect
	}{
		{name: "0600 honored", perm: 0o600, want: loop.EffectAutoApprove},
		{name: "group-writable rejected", perm: 0o660, want: loop.EffectAsk},
		{name: "world-writable rejected", perm: 0o606, want: loop.EffectAsk},
		{name: "all-writable rejected", perm: 0o666, want: loop.EffectAsk},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ws := newWS(t)
			if err := os.WriteFile(filepath.Join(ws, "main.go"), []byte("package main"), 0o600); err != nil {
				t.Fatalf("write main.go: %v", err)
			}
			home := t.TempDir()
			// Write the workspace approvals file by hand with the test perms.
			hash, err := workspaceHash(ws)
			if err != nil {
				t.Fatalf("workspaceHash: %v", err)
			}
			wsFile := workspaceApprovalsPath(home, hash)
			if err := os.MkdirAll(filepath.Dir(wsFile), 0o700); err != nil {
				t.Fatalf("mkdir ws store: %v", err)
			}
			recs := writeApprovals(t, ApprovalRecord{Tool: "ReadFile", Match: "main.go", Effect: loop.EffectAutoApprove})
			if err := os.WriteFile(wsFile, recs, 0o600); err != nil {
				t.Fatalf("write ws approvals: %v", err)
			}
			// Chmod separately so the umask cannot strip the test perms.
			if err := os.Chmod(wsFile, tt.perm); err != nil {
				t.Fatalf("chmod: %v", err)
			}

			pc := NewPermissionChecker(PermissionPolicy{WorkspaceRoot: ws, HardDeny: DefaultHardDeny()})
			pc.SetHomeDir(func() (string, error) { return home, nil })
			got := pc.Check(context.Background(), plainTool{name: "ReadFile"}, "ReadFile", `{"path":"main.go"}`)
			if got != tt.want {
				t.Errorf("Check() = %v, want %v (perm %o)", got, tt.want, tt.perm)
			}
		})
	}
}

// TestLoaderRejectsSymlinkedFile proves an approvals file that is itself a symlink
// (not a regular file) is treated as empty even when its target is valid.
func TestLoaderRejectsSymlinkedFile(t *testing.T) {
	t.Parallel()
	ws := newWS(t)
	if err := os.WriteFile(filepath.Join(ws, "main.go"), []byte("package main"), 0o600); err != nil {
		t.Fatalf("write main.go: %v", err)
	}
	home := t.TempDir()
	hash, err := workspaceHash(ws)
	if err != nil {
		t.Fatalf("workspaceHash: %v", err)
	}
	wsFile := workspaceApprovalsPath(home, hash)
	if err := os.MkdirAll(filepath.Dir(wsFile), 0o700); err != nil {
		t.Fatalf("mkdir ws store: %v", err)
	}
	// The real, valid approvals payload lives elsewhere; wsFile is a symlink to it.
	target := filepath.Join(home, "real-approvals.json")
	recs := writeApprovals(t, ApprovalRecord{Tool: "ReadFile", Match: "main.go", Effect: loop.EffectAutoApprove})
	if err := os.WriteFile(target, recs, 0o600); err != nil {
		t.Fatalf("write target approvals: %v", err)
	}
	if err := os.Symlink(target, wsFile); err != nil {
		t.Fatalf("symlink approvals file: %v", err)
	}

	pc := NewPermissionChecker(PermissionPolicy{WorkspaceRoot: ws, HardDeny: DefaultHardDeny()})
	pc.SetHomeDir(func() (string, error) { return home, nil })
	got := pc.Check(context.Background(), plainTool{name: "ReadFile"}, "ReadFile", `{"path":"main.go"}`)
	if got != loop.EffectAsk {
		t.Errorf("Check() = %v, want EffectAsk (symlinked approvals file must be treated empty)", got)
	}
}

// TestLoaderRejectsSymlinkedComponent proves a symlinked PARENT directory anywhere
// in the policy path (here ~/.looprig itself) makes the loader treat the store as
// empty (don't read through a symlinked policy dir).
func TestLoaderRejectsSymlinkedComponent(t *testing.T) {
	t.Parallel()
	ws := newWS(t)
	if err := os.WriteFile(filepath.Join(ws, "main.go"), []byte("package main"), 0o600); err != nil {
		t.Fatalf("write main.go: %v", err)
	}
	home := t.TempDir()
	// Build the full valid store under a DECOY dir, then symlink ~/.looprig -> decoy.
	decoy := filepath.Join(home, "decoy-looprig")
	hash, err := workspaceHash(ws)
	if err != nil {
		t.Fatalf("workspaceHash: %v", err)
	}
	wsFileUnderDecoy := filepath.Join(decoy, workspacesDirName, hash, workspaceApprovalsName)
	if err := os.MkdirAll(filepath.Dir(wsFileUnderDecoy), 0o700); err != nil {
		t.Fatalf("mkdir decoy ws store: %v", err)
	}
	recs := writeApprovals(t, ApprovalRecord{Tool: "ReadFile", Match: "main.go", Effect: loop.EffectAutoApprove})
	if err := os.WriteFile(wsFileUnderDecoy, recs, 0o600); err != nil {
		t.Fatalf("write decoy approvals: %v", err)
	}
	// ~/.looprig is a symlink to the decoy directory.
	if err := os.Symlink(decoy, filepath.Join(home, looprigDirName)); err != nil {
		t.Fatalf("symlink ~/.looprig -> decoy: %v", err)
	}

	pc := NewPermissionChecker(PermissionPolicy{WorkspaceRoot: ws, HardDeny: DefaultHardDeny()})
	pc.SetHomeDir(func() (string, error) { return home, nil })
	got := pc.Check(context.Background(), plainTool{name: "ReadFile"}, "ReadFile", `{"path":"main.go"}`)
	if got != loop.EffectAsk {
		t.Errorf("Check() = %v, want EffectAsk (symlinked ~/.looprig must be treated empty)", got)
	}
}

// TestLoaderRejectsWorldWritableAncestorDir proves the loader rejects the store
// (→ Ask, never AutoApprove) when ANY store DIRECTORY component under <home> is
// group- or world-writable, even though the file itself is a valid 0600 regular
// file. A pre-existing world-writable ~/.looprig/workspaces (the ancestor) lets a
// non-owner plant an attacker-owned approvals.json that would otherwise pass
// every file-level check — this is the store-poisoning vector being closed.
func TestLoaderRejectsWorldWritableAncestorDir(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		// looseRel is the store-relative dir (under <home>) to make world-writable
		// AFTER a valid store is written; empty means "leave all dirs 0700".
		looseRel string
		mode     os.FileMode
		want     loop.Effect
	}{
		{name: "control: all dirs 0700", looseRel: "", want: loop.EffectAutoApprove},
		{name: "world-writable ~/.looprig rejected", looseRel: looprigDirName, mode: 0o777, want: loop.EffectAsk},
		{name: "world-writable ~/.looprig/workspaces rejected", looseRel: filepath.Join(looprigDirName, workspacesDirName), mode: 0o777, want: loop.EffectAsk},
		{name: "group-writable ~/.looprig rejected", looseRel: looprigDirName, mode: 0o770, want: loop.EffectAsk},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ws := newWS(t)
			if err := os.WriteFile(filepath.Join(ws, "main.go"), []byte("package main"), 0o600); err != nil {
				t.Fatalf("write main.go: %v", err)
			}
			home := t.TempDir()
			hash, err := workspaceHash(ws)
			if err != nil {
				t.Fatalf("workspaceHash: %v", err)
			}
			wsFile := workspaceApprovalsPath(home, hash)
			if err := os.MkdirAll(filepath.Dir(wsFile), 0o700); err != nil {
				t.Fatalf("mkdir ws store: %v", err)
			}
			recs := writeApprovals(t, ApprovalRecord{Tool: "ReadFile", Match: "main.go", Effect: loop.EffectAutoApprove})
			if err := os.WriteFile(wsFile, recs, 0o600); err != nil {
				t.Fatalf("write ws approvals: %v", err)
			}

			// Loosen an ANCESTOR dir (the file stays a valid 0600 regular file).
			if tt.looseRel != "" {
				if err := os.Chmod(filepath.Join(home, tt.looseRel), tt.mode); err != nil {
					t.Fatalf("chmod ancestor %q: %v", tt.looseRel, err)
				}
			}

			pc := NewPermissionChecker(PermissionPolicy{WorkspaceRoot: ws, HardDeny: DefaultHardDeny()})
			pc.SetHomeDir(func() (string, error) { return home, nil })
			got := pc.Check(context.Background(), plainTool{name: "ReadFile"}, "ReadFile", `{"path":"main.go"}`)
			if got != tt.want {
				t.Errorf("Check() = %v, want %v (ancestor %q mode %o)", got, tt.want, tt.looseRel, tt.mode)
			}
		})
	}
}

// TestLoaderRejectsLooseAncestorWarnsPathOnly proves rejecting the store for a
// world-writable ANCESTOR dir emits a path-only WARN and never leaks the file
// CONTENTS. Not parallel: swaps the global slog default.
func TestLoaderRejectsLooseAncestorWarnsPathOnly(t *testing.T) {
	ws := newWS(t)
	if err := os.WriteFile(filepath.Join(ws, "main.go"), []byte("package main"), 0o600); err != nil {
		t.Fatalf("write main.go: %v", err)
	}
	home := t.TempDir()
	hash, err := workspaceHash(ws)
	if err != nil {
		t.Fatalf("workspaceHash: %v", err)
	}
	wsFile := workspaceApprovalsPath(home, hash)
	if err := os.MkdirAll(filepath.Dir(wsFile), 0o700); err != nil {
		t.Fatalf("mkdir ws store: %v", err)
	}
	const secretToken = "TOPSECRET_ANCESTOR_DO_NOT_LOG"
	recs := writeApprovals(t, ApprovalRecord{Tool: "ReadFile", Match: secretToken, Effect: loop.EffectAutoApprove})
	if err := os.WriteFile(wsFile, recs, 0o600); err != nil {
		t.Fatalf("write ws approvals: %v", err)
	}
	// Loosen ~/.looprig/workspaces (an ancestor) to world-writable.
	if err := os.Chmod(filepath.Join(home, looprigDirName, workspacesDirName), 0o777); err != nil {
		t.Fatalf("chmod ancestor: %v", err)
	}

	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	pc := NewPermissionChecker(PermissionPolicy{WorkspaceRoot: ws, HardDeny: DefaultHardDeny()})
	pc.SetHomeDir(func() (string, error) { return home, nil })
	if got := pc.Check(context.Background(), plainTool{name: "ReadFile"}, "ReadFile", `{"path":"main.go"}`); got != loop.EffectAsk {
		t.Fatalf("Check() = %v, want EffectAsk (loose ancestor must reject store)", got)
	}

	logged := buf.String()
	if !strings.Contains(logged, "approvals.json") {
		t.Errorf("warning should name the file path; log was:\n%s", logged)
	}
	if strings.Contains(logged, secretToken) {
		t.Errorf("warning leaked file CONTENTS (%q); log was:\n%s", secretToken, logged)
	}
}

// TestLoaderRejectionWarns proves the loader emits a path-only WARN when it
// rejects a hardening-violating file (here world-writable) and never leaks the
// file CONTENTS. Not parallel: swaps the global slog default.
func TestLoaderRejectionWarns(t *testing.T) {
	ws := newWS(t)
	if err := os.WriteFile(filepath.Join(ws, "main.go"), []byte("package main"), 0o600); err != nil {
		t.Fatalf("write main.go: %v", err)
	}
	home := t.TempDir()
	hash, err := workspaceHash(ws)
	if err != nil {
		t.Fatalf("workspaceHash: %v", err)
	}
	wsFile := workspaceApprovalsPath(home, hash)
	if err := os.MkdirAll(filepath.Dir(wsFile), 0o700); err != nil {
		t.Fatalf("mkdir ws store: %v", err)
	}
	const secretToken = "TOPSECRET_LOADER_DO_NOT_LOG"
	recs := writeApprovals(t, ApprovalRecord{Tool: "ReadFile", Match: secretToken, Effect: loop.EffectAutoApprove})
	if err := os.WriteFile(wsFile, recs, 0o600); err != nil {
		t.Fatalf("write ws approvals: %v", err)
	}
	if err := os.Chmod(wsFile, 0o666); err != nil {
		t.Fatalf("chmod: %v", err)
	}

	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	pc := NewPermissionChecker(PermissionPolicy{WorkspaceRoot: ws, HardDeny: DefaultHardDeny()})
	pc.SetHomeDir(func() (string, error) { return home, nil })
	if got := pc.Check(context.Background(), plainTool{name: "ReadFile"}, "ReadFile", `{"path":"main.go"}`); got != loop.EffectAsk {
		t.Fatalf("Check() = %v, want EffectAsk", got)
	}

	logged := buf.String()
	if !strings.Contains(logged, "approvals.json") {
		t.Errorf("warning should name the file path; log was:\n%s", logged)
	}
	if strings.Contains(logged, secretToken) {
		t.Errorf("warning leaked file CONTENTS (%q); log was:\n%s", secretToken, logged)
	}
}
