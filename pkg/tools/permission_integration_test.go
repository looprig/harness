//go:build integration

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

// permission_integration_test.go exercises the policy store against the REAL
// filesystem (under a t.TempDir() fake home) to prove the §3c hardening end to
// end: a symlinked policy dir is rejected by BOTH Grant (no write) and the loader
// (treated empty); a world-writable approvals file is rejected by the loader; and
// deny-beats-allow holds across two REAL on-disk files. Tagged `integration` so it
// is excluded from the default `go test ./...` run.

// intWS creates an EvalSymlinks-resolved temp workspace (the form containment and
// the workspace hash resolve to).
func intWS(t *testing.T) string {
	t.Helper()
	ws := t.TempDir()
	resolved, err := filepath.EvalSymlinks(ws)
	if err != nil {
		t.Fatalf("EvalSymlinks ws: %v", err)
	}
	return resolved
}

// intReadApprovals decodes the on-disk ApprovalsFile at p.
func intReadApprovals(t *testing.T, p string) ApprovalsFile {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read %q: %v", p, err)
	}
	var af ApprovalsFile
	if err := json.Unmarshal(b, &af); err != nil {
		t.Fatalf("unmarshal %q: %v", p, err)
	}
	return af
}

// intWriteApprovals serializes an ApprovalsFile for an on-disk fixture.
func intWriteApprovals(t *testing.T, recs ...ApprovalRecord) []byte {
	t.Helper()
	b, err := json.Marshal(ApprovalsFile{Version: 1, Approvals: recs})
	if err != nil {
		t.Fatalf("marshal approvals: %v", err)
	}
	return b
}

// TestIntegrationSymlinkedPolicyDirRejected proves a symlinked policy directory is
// refused on BOTH the write path (Grant returns a typed *PolicyStoreError and
// writes nothing through the symlink) and the read path (the loader treats the
// store as empty → Check Asks). Two sub-cases: ~/.looprig itself, and the deeper
// workspaces/<hash> dir, are each a symlink.
func TestIntegrationSymlinkedPolicyDirRejected(t *testing.T) {
	tests := []struct {
		name string
		// linkComponent builds the symlinked component for (home, wsRoot) and returns
		// the decoy directory the symlink targets (where a real store could live).
		linkComponent func(t *testing.T, home, wsRoot string) (decoy string)
	}{
		{
			name: "urvi dir is a symlink",
			linkComponent: func(t *testing.T, home, wsRoot string) string {
				decoy := filepath.Join(home, "decoy-urvi")
				if err := os.MkdirAll(decoy, 0o700); err != nil {
					t.Fatalf("mkdir decoy: %v", err)
				}
				if err := os.Symlink(decoy, filepath.Join(home, urviDirName)); err != nil {
					t.Fatalf("symlink ~/.looprig -> decoy: %v", err)
				}
				return decoy
			},
		},
		{
			name: "workspaces hash dir is a symlink",
			linkComponent: func(t *testing.T, home, wsRoot string) string {
				hash, err := workspaceHash(wsRoot)
				if err != nil {
					t.Fatalf("workspaceHash: %v", err)
				}
				// Real ~/.looprig/workspaces, but the <hash> entry is a symlink to a decoy.
				wsParent := filepath.Join(home, urviDirName, workspacesDirName)
				if err := os.MkdirAll(wsParent, 0o700); err != nil {
					t.Fatalf("mkdir workspaces: %v", err)
				}
				decoy := filepath.Join(home, "decoy-hash")
				if err := os.MkdirAll(decoy, 0o700); err != nil {
					t.Fatalf("mkdir decoy: %v", err)
				}
				if err := os.Symlink(decoy, filepath.Join(wsParent, hash)); err != nil {
					t.Fatalf("symlink hash dir -> decoy: %v", err)
				}
				return decoy
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ws := intWS(t)
			if err := os.WriteFile(filepath.Join(ws, "main.go"), []byte("package main"), 0o600); err != nil {
				t.Fatalf("write main.go: %v", err)
			}
			home := t.TempDir()
			decoy := tt.linkComponent(t, home, ws)

			pc := NewPermissionChecker(PermissionPolicy{WorkspaceRoot: ws, HardDeny: DefaultHardDeny()})
			pc.SetHomeDir(func() (string, error) { return home, nil })

			// WRITE PATH: Grant must refuse (typed error) and write nothing through
			// the symlink (the decoy must stay empty of an approvals.json).
			err := pc.Grant(context.Background(), "ReadFile", `{"path":"main.go"}`, tool.ScopeWorkspace)
			if err == nil {
				t.Fatal("Grant through a symlinked policy dir = nil, want a typed *PolicyStoreError")
			}
			var storeErr *PolicyStoreError
			if !errors.As(err, &storeErr) {
				t.Errorf("Grant error = %T, want *PolicyStoreError", err)
			}
			// Nothing should have been written under the decoy target.
			if _, statErr := os.Stat(filepath.Join(decoy, workspaceApprovalsName)); !os.IsNotExist(statErr) {
				t.Errorf("Grant wrote through the symlink into the decoy; stat err=%v", statErr)
			}

			// READ PATH: plant a VALID, hostile store under the decoy (reachable only
			// by following the symlink). The loader must NOT follow it → Check Asks.
			hash, err := workspaceHash(ws)
			if err != nil {
				t.Fatalf("workspaceHash: %v", err)
			}
			var plant string
			if tt.name == "urvi dir is a symlink" {
				plant = filepath.Join(decoy, workspacesDirName, hash, workspaceApprovalsName)
			} else {
				plant = filepath.Join(decoy, workspaceApprovalsName)
			}
			if err := os.MkdirAll(filepath.Dir(plant), 0o700); err != nil {
				t.Fatalf("mkdir plant dir: %v", err)
			}
			recs := intWriteApprovals(t, ApprovalRecord{Tool: "ReadFile", Match: "main.go", Effect: loop.EffectAutoApprove})
			if err := os.WriteFile(plant, recs, 0o600); err != nil {
				t.Fatalf("plant hostile store: %v", err)
			}

			got := pc.Check(context.Background(), plainTool{name: "ReadFile"}, "ReadFile", `{"path":"main.go"}`)
			if got != loop.EffectAsk {
				t.Errorf("Check through symlinked policy dir = %v, want EffectAsk (must not follow symlink)", got)
			}
		})
	}
}

// TestIntegrationWorldWritableFileRejected proves a world-writable on-disk
// approvals file is rejected by the loader (treated empty → Check Asks) even
// though its records are valid.
func TestIntegrationWorldWritableFileRejected(t *testing.T) {
	ws := intWS(t)
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
	recs := intWriteApprovals(t, ApprovalRecord{Tool: "ReadFile", Match: "main.go", Effect: loop.EffectAutoApprove})
	if err := os.WriteFile(wsFile, recs, 0o600); err != nil {
		t.Fatalf("write ws approvals: %v", err)
	}
	if err := os.Chmod(wsFile, 0o666); err != nil { // world-writable
		t.Fatalf("chmod: %v", err)
	}

	pc := NewPermissionChecker(PermissionPolicy{WorkspaceRoot: ws, HardDeny: DefaultHardDeny()})
	pc.SetHomeDir(func() (string, error) { return home, nil })

	got := pc.Check(context.Background(), plainTool{name: "ReadFile"}, "ReadFile", `{"path":"main.go"}`)
	if got != loop.EffectAsk {
		t.Errorf("Check with world-writable approvals = %v, want EffectAsk (file must be rejected)", got)
	}
}

// TestIntegrationDenyBeatsAllowTwoRealFiles proves deny-beats-allow holds across
// two REAL files on disk: a workspace-scope ALLOW and a user-scope DENY for the
// same call resolve to Deny. The workspace allow is written via Grant (exercising
// the real write path); the user deny is planted directly.
func TestIntegrationDenyBeatsAllowTwoRealFiles(t *testing.T) {
	ws := intWS(t)
	if err := os.WriteFile(filepath.Join(ws, "main.go"), []byte("package main"), 0o600); err != nil {
		t.Fatalf("write main.go: %v", err)
	}
	home := t.TempDir()

	pc := NewPermissionChecker(PermissionPolicy{WorkspaceRoot: ws, HardDeny: DefaultHardDeny()})
	pc.SetHomeDir(func() (string, error) { return home, nil })

	// Workspace ALLOW via the real Grant write path.
	if err := pc.Grant(context.Background(), "ReadFile", `{"path":"main.go"}`, tool.ScopeWorkspace); err != nil {
		t.Fatalf("Grant(ScopeWorkspace): %v", err)
	}
	// Confirm the workspace file really is an allow on disk.
	wsFile := workspaceApprovalsPath(home, mustHash(t, ws))
	af := intReadApprovals(t, wsFile)
	if len(af.Approvals) != 1 || af.Approvals[0].Effect != loop.EffectAutoApprove {
		t.Fatalf("workspace store = %+v, want a single allow", af.Approvals)
	}

	// User-scope DENY for the same call, planted directly as a real file.
	userFile := userApprovalsPath(home)
	if err := os.MkdirAll(filepath.Dir(userFile), 0o700); err != nil {
		t.Fatalf("mkdir user store: %v", err)
	}
	deny := intWriteApprovals(t, ApprovalRecord{Tool: "ReadFile", Match: "main.go", Effect: loop.EffectDeny})
	if err := os.WriteFile(userFile, deny, 0o600); err != nil {
		t.Fatalf("write user deny: %v", err)
	}

	got := pc.Check(context.Background(), plainTool{name: "ReadFile"}, "ReadFile", `{"path":"main.go"}`)
	if got != loop.EffectDeny {
		t.Errorf("Check with ws-allow + user-deny = %v, want EffectDeny (deny beats allow)", got)
	}
}

// mustHash is a test helper that returns the workspace hash or fails.
func mustHash(t *testing.T, wsRoot string) string {
	t.Helper()
	h, err := workspaceHash(wsRoot)
	if err != nil {
		t.Fatalf("workspaceHash: %v", err)
	}
	return h
}
