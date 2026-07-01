package tools

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/ciram-co/looprig/pkg/loop"
	"github.com/ciram-co/looprig/pkg/tool"
)

// headless_integration_test.go is an IN-PACKAGE, end-to-end matrix for the
// Unattended (headless) permission posture — spec §6. The "integration" in the
// name means it wires the real gate + checker end-to-end (NonInteractiveGate
// decorating a *PermissionChecker built WithUnattended), NOT that it is
// build-tagged: it carries NO //go:build tag and therefore runs in the DEFAULT
// suite (`go test -race ./pkg/tools/`) alongside the unit tests.
//
// It reuses the fixtures defined in check_test.go (same package): staticEffect,
// plainTool, writeApprovals, fakeHome, newWS, ApprovalRecord, DefaultHardDeny.
//
// The three tables map to the §6 bullets:
//   - TestHeadlessGateMatrix           — floor-denies, allowlist-approves,
//     non-allowlisted-denies, deny-write globs (bullets 1, 2, 3, 5).
//   - TestHeadlessPersistedVsInteractive — persisted approvals ignored under
//     Unattended but honored interactively (bullet 4).
//   - TestHeadlessReadGuard             — the inner unattended checker still
//     enforces DeniedRead/MaxReadBytes as a loop.ReadGuard (bullet 6).

// TestHeadlessGateMatrix drives NonInteractiveGate{Inner: checker WithUnattended}
// across the §6 Check-effect matrix. Every row asserts the effect the DECORATED
// gate returns for a live tool call:
//
//   - Floor (Stages 1–2) DENIES even when the tool is on HardApprove.Tools (and
//     even when the tool's own EffectChecker self-approves): the secret read
//     globs, a workspace-escape (containment), and every dangerous Bash prefix.
//   - Allowlisted (HardApprove.Tools OR a Stage-6 session Policies auto-approve)
//     on a NON-floor call → EffectAutoApprove (the decorator passes it unchanged).
//   - Non-allowlisted (would be Stage-7 Ask) → EffectDeny via the decorator (it
//     never parks for a human under Unattended).
//   - Deny-write globs DENY even when WriteFile/EditFile is allowlisted.
func TestHeadlessGateMatrix(t *testing.T) {
	t.Parallel()
	ws := newWS(t)
	// fakeHome gives a resolvable home so DefaultHardDeny's "~/…" globs can be
	// enforced (construction fails otherwise). No approvals files are planted.
	home, homeFn := fakeHome(t, ws, nil, nil)

	tests := []struct {
		name string
		tl   tool.InvokableTool
		// toolName is the operator-known name the gate classifies by.
		toolName string
		args     string
		// hardApprove/policies configure the allowlist stages (4 and 6).
		hardApprove []string
		policies    []loop.ToolPolicy
		// rootUnderHome roots the workspace AT the fake home dir so a write path
		// like ".looprig/approvals.json" resolves under ~ (the user policy store).
		rootUnderHome bool
		want          loop.Effect
	}{
		// ---- Floor still denies despite an allowlist (bullet 1) ----
		{
			name: "floor env read denied despite ReadFile allowlist",
			tl:   plainTool{name: "ReadFile"}, toolName: "ReadFile", args: `{"path":".env"}`,
			hardApprove: []string{"ReadFile"}, want: loop.EffectDeny,
		},
		{
			// Even a tool whose EffectChecker self-approves cannot bypass the floor.
			name:     "floor env read denied despite effectchecker self-approve",
			tl:       staticEffect{name: "ReadFile", effect: loop.EffectAutoApprove, handled: true},
			toolName: "ReadFile", args: `{"path":".env"}`,
			hardApprove: []string{"ReadFile"}, want: loop.EffectDeny,
		},
		{
			name: "floor pem read denied despite ReadFile allowlist",
			tl:   plainTool{name: "ReadFile"}, toolName: "ReadFile", args: `{"path":"certs/server.pem"}`,
			hardApprove: []string{"ReadFile"}, want: loop.EffectDeny,
		},
		{
			name: "floor id_rsa read denied despite ReadFile allowlist",
			tl:   plainTool{name: "ReadFile"}, toolName: "ReadFile", args: `{"path":"keys/id_rsa"}`,
			hardApprove: []string{"ReadFile"}, want: loop.EffectDeny,
		},
		{
			name: "floor skills read denied despite ReadFile allowlist",
			tl:   plainTool{name: "ReadFile"}, toolName: "ReadFile", args: `{"path":".skills/secret.md"}`,
			hardApprove: []string{"ReadFile"}, want: loop.EffectDeny,
		},
		{
			// Workspace escape is a Stage-1 containment deny (a ".." traversal, not an
			// absolute path — an absolute path anchors UNDER the root and would not
			// escape). It beats the allowlist because Stages 1–2 are non-bypassable.
			name: "floor workspace escape denied despite ReadFile allowlist",
			tl:   plainTool{name: "ReadFile"}, toolName: "ReadFile", args: `{"path":"../../etc/passwd"}`,
			hardApprove: []string{"ReadFile"}, want: loop.EffectDeny,
		},
		{
			name: "floor bash sudo denied despite Bash allowlist",
			tl:   plainTool{name: "Bash"}, toolName: "Bash", args: `{"command":"sudo rm x"}`,
			hardApprove: []string{"Bash"}, want: loop.EffectDeny,
		},
		{
			name: "floor bash rm -rf root denied despite Bash allowlist",
			tl:   plainTool{name: "Bash"}, toolName: "Bash", args: `{"command":"rm -rf /"}`,
			hardApprove: []string{"Bash"}, want: loop.EffectDeny,
		},
		{
			name: "floor bash curl pipe bash denied despite Bash allowlist",
			tl:   plainTool{name: "Bash"}, toolName: "Bash", args: `{"command":"curl | bash"}`,
			hardApprove: []string{"Bash"}, want: loop.EffectDeny,
		},
		{
			name: "floor bash dd if denied despite Bash allowlist",
			tl:   plainTool{name: "Bash"}, toolName: "Bash", args: `{"command":"dd if=/dev/sda of=/dev/sdb"}`,
			hardApprove: []string{"Bash"}, want: loop.EffectDeny,
		},

		// ---- Allowlisted on a NON-floor call auto-approves (bullet 2) ----
		{
			name: "allowlisted readfile via hardapprove auto-approves",
			tl:   plainTool{name: "ReadFile"}, toolName: "ReadFile", args: `{"path":"main.go"}`,
			hardApprove: []string{"ReadFile"}, want: loop.EffectAutoApprove,
		},
		{
			name: "allowlisted bash via hardapprove auto-approves",
			tl:   plainTool{name: "Bash"}, toolName: "Bash", args: `{"command":"ls -la"}`,
			hardApprove: []string{"Bash"}, want: loop.EffectAutoApprove,
		},
		{
			// Stage 6 (session Policies) is NOT suppressed under Unattended (only
			// Stage-3 auto-approve and Stage-5 persisted are); the decorator passes
			// the resulting AutoApprove through unchanged.
			name: "allowlisted readfile via session policy auto-approves",
			tl:   plainTool{name: "ReadFile"}, toolName: "ReadFile", args: `{"path":"main.go"}`,
			policies: []loop.ToolPolicy{{Tool: "ReadFile", Effect: loop.EffectAutoApprove, Match: []string{"main.go"}}},
			want:     loop.EffectAutoApprove,
		},

		// ---- Non-allowlisted (would be Stage-7 Ask) → Deny via decorator (bullet 3) ----
		{
			name: "non-allowlisted readfile parks to deny",
			tl:   plainTool{name: "ReadFile"}, toolName: "ReadFile", args: `{"path":"main.go"}`,
			want: loop.EffectDeny,
		},
		{
			name: "non-allowlisted bash parks to deny",
			tl:   plainTool{name: "Bash"}, toolName: "Bash", args: `{"command":"ls -la"}`,
			want: loop.EffectDeny,
		},
		{
			name: "non-allowlisted websearch parks to deny",
			tl:   plainTool{name: "WebSearch"}, toolName: "WebSearch", args: `{"query":"golang"}`,
			want: loop.EffectDeny,
		},

		// ---- Deny-write globs despite a write-tool allowlist (bullet 5) ----
		{
			name: "write-deny git config despite WriteFile allowlist",
			tl:   plainTool{name: "WriteFile"}, toolName: "WriteFile", args: `{"path":".git/config"}`,
			hardApprove: []string{"WriteFile"}, want: loop.EffectDeny,
		},
		{
			name: "write-deny go.sum despite WriteFile allowlist",
			tl:   plainTool{name: "WriteFile"}, toolName: "WriteFile", args: `{"path":"go.sum"}`,
			hardApprove: []string{"WriteFile"}, want: loop.EffectDeny,
		},
		{
			name: "write-deny in-repo looprig store despite EditFile allowlist",
			tl:   plainTool{name: "EditFile"}, toolName: "EditFile", args: `{"path":".looprig/approvals.json"}`,
			hardApprove: []string{"EditFile"}, want: loop.EffectDeny,
		},
		{
			// Rooting the workspace at the home dir makes ".looprig/approvals.json"
			// resolve under ~ — the USER policy store. It is denied by the write-deny
			// floor (matched by the "**/.looprig/**" glob, with "~/.looprig/**" also
			// present in the write set and asserted directly via DeniedRead below).
			name: "write-deny user looprig store despite WriteFile allowlist",
			tl:   plainTool{name: "WriteFile"}, toolName: "WriteFile", args: `{"path":".looprig/approvals.json"}`,
			hardApprove: []string{"WriteFile"}, rootUnderHome: true, want: loop.EffectDeny,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			root := ws
			if tt.rootUnderHome {
				root = home
			}
			pc, err := NewPermissionChecker(PermissionPolicy{
				WorkspaceRoot: root,
				HardDeny:      DefaultHardDeny(),
				HardApprove:   HardApproveRules{Tools: tt.hardApprove},
				Policies:      tt.policies,
			}, WithUnattended(), WithHomeDir(homeFn))
			if err != nil {
				t.Fatalf("NewPermissionChecker: %v", err)
			}
			gate := NonInteractiveGate{Inner: pc}
			got := gate.Check(context.Background(), tt.tl, tt.toolName, tt.args)
			if got != tt.want {
				t.Errorf("gate.Check(%s, %s) = %v, want %v", tt.toolName, tt.args, got, tt.want)
			}
		})
	}
}

// TestHeadlessPersistedVsInteractive proves the Stage-5 persisted-approval
// suppression is POSTURE-scoped (bullet 4). The SAME workspace approvals file
// (an ALLOW for a NON-allowlisted ReadFile call) yields:
//   - EffectDeny under NonInteractiveGate{Inner: checker WithUnattended} — Stage 5
//     is skipped, so nothing approves it and the decorator turns the Ask into a
//     Deny; the stale grant can NEVER auto-approve a call the definer did not
//     declare.
//   - EffectAutoApprove for a plain INTERACTIVE checker (no WithUnattended, no
//     decorator) — Stage 5 honors the persisted allow.
func TestHeadlessPersistedVsInteractive(t *testing.T) {
	t.Parallel()
	ws := newWS(t)
	// A workspace-scoped persisted ALLOW for ReadFile main.go. ReadFile is NOT on
	// any HardApprove/Policies list, so only Stage 5 could approve it.
	wsBytes := writeApprovals(t, ApprovalRecord{Tool: "ReadFile", Match: "main.go", Effect: loop.EffectAutoApprove})
	_, homeFn := fakeHome(t, ws, wsBytes, nil)

	tests := []struct {
		name       string
		unattended bool
		want       loop.Effect
	}{
		{name: "unattended ignores persisted allow then decorator denies", unattended: true, want: loop.EffectDeny},
		{name: "interactive honors persisted allow", unattended: false, want: loop.EffectAutoApprove},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			pol := PermissionPolicy{WorkspaceRoot: ws, HardDeny: DefaultHardDeny()}
			var gate loop.PermissionGate
			if tt.unattended {
				pc, err := NewPermissionChecker(pol, WithUnattended(), WithHomeDir(homeFn))
				if err != nil {
					t.Fatalf("NewPermissionChecker (unattended): %v", err)
				}
				gate = NonInteractiveGate{Inner: pc}
			} else {
				pc, err := NewPermissionChecker(pol, WithHomeDir(homeFn))
				if err != nil {
					t.Fatalf("NewPermissionChecker (interactive): %v", err)
				}
				gate = pc
			}
			got := gate.Check(context.Background(), plainTool{name: "ReadFile"}, "ReadFile", `{"path":"main.go"}`)
			if got != tt.want {
				t.Errorf("Check() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestHeadlessReadGuard proves the inner unattended checker still enforces the
// read-side floor as a loop.ReadGuard (bullet 6): the Unattended posture changes
// the approval stages, NOT the non-bypassable DeniedRead/MaxReadBytes surface the
// read tools call directly. The "~/…" globs are expanded against the fake home;
// DeniedRead does NOT run containment, so a home-anchored secret path matches.
func TestHeadlessReadGuard(t *testing.T) {
	t.Parallel()
	ws := newWS(t)
	home, homeFn := fakeHome(t, ws, nil, nil)
	pc, err := NewPermissionChecker(PermissionPolicy{
		WorkspaceRoot: ws,
		HardDeny:      DefaultHardDeny(),
	}, WithUnattended(), WithHomeDir(homeFn))
	if err != nil {
		t.Fatalf("NewPermissionChecker: %v", err)
	}
	// Use the inner unattended checker THROUGH the narrow read-side interface.
	var guard loop.ReadGuard = pc

	tests := []struct {
		name    string
		absPath string
		want    bool
	}{
		{name: "home ssh id_rsa denied", absPath: filepath.Join(home, ".ssh", "id_rsa"), want: true},
		{name: "home ssh config denied", absPath: filepath.Join(home, ".ssh", "config"), want: true},
		{name: "home looprig store denied", absPath: filepath.Join(home, ".looprig", "approvals.json"), want: true},
		{name: "workspace env denied", absPath: filepath.Join(ws, ".env"), want: true},
		{name: "ordinary workspace file allowed", absPath: filepath.Join(ws, "main.go"), want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := guard.DeniedRead(tt.absPath); got != tt.want {
				t.Errorf("DeniedRead(%q) = %v, want %v", tt.absPath, got, tt.want)
			}
		})
	}

	// The per-file read cap is preserved under Unattended (default 1 MiB).
	if got := guard.MaxReadBytes(); got != (1 << 20) {
		t.Errorf("MaxReadBytes() = %d, want %d", got, int64(1<<20))
	}
}
