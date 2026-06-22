package tools

import (
	"slices"
	"testing"
)

// TestDefaultHardDeny asserts the DefaultHardDeny() constructor returns the
// fail-secure defaults from design §3c: the secret read/write globs, the
// .looprig policy-store deny-write entries (so the tool system can never mutate
// its own approvals), the dangerous Bash prefixes, and the 1 MiB read cap.
func TestDefaultHardDeny(t *testing.T) {
	t.Parallel()
	d := DefaultHardDeny()

	tests := []struct {
		name string
		set  []string
		want string
	}{
		// Secret globs in the READ deny set.
		{name: "read denies ssh", set: d.DeniedReadPaths, want: "~/.ssh/**"},
		{name: "read denies dotenv", set: d.DeniedReadPaths, want: "**/.env"},
		{name: "read denies pem", set: d.DeniedReadPaths, want: "**/*.pem"},
		{name: "read denies id_rsa", set: d.DeniedReadPaths, want: "**/id_rsa"},
		// Policy store (user-level) is read-denied.
		{name: "read denies user urvi store", set: d.DeniedReadPaths, want: "~/.looprig/**"},
		// Workspace skill source is read-denied for generic file tools (gate-bypass
		// prevention): only the gated Skill tool may reach it (via embed.FS).
		{name: "read denies workspace skills", set: d.DeniedReadPaths, want: "**/.skills/**"},

		// Secret globs must ALSO be in the WRITE deny set (write set ⊇ read set).
		{name: "write denies ssh", set: d.DeniedWritePaths, want: "~/.ssh/**"},
		{name: "write denies dotenv", set: d.DeniedWritePaths, want: "**/.env"},
		{name: "write denies pem", set: d.DeniedWritePaths, want: "**/*.pem"},
		{name: "write denies id_rsa", set: d.DeniedWritePaths, want: "**/id_rsa"},
		// Write-only additions.
		{name: "write denies git config", set: d.DeniedWritePaths, want: "**/.git/config"},
		{name: "write denies go.sum", set: d.DeniedWritePaths, want: "**/go.sum"},
		// Workspace skill source must ALSO be write-denied (write set ⊇ read set), so
		// no generic tool can write the .skills/ source either.
		{name: "write denies workspace skills", set: d.DeniedWritePaths, want: "**/.skills/**"},
		// Policy-store deny-write entries — the security-critical requirement.
		{name: "write denies in-repo urvi store", set: d.DeniedWritePaths, want: "**/.looprig/**"},
		{name: "write denies user urvi store", set: d.DeniedWritePaths, want: "~/.looprig/**"},

		// Dangerous Bash prefixes.
		{name: "bash denies rm -rf root", set: d.DeniedBashPrefixes, want: "rm -rf /"},
		{name: "bash denies sudo", set: d.DeniedBashPrefixes, want: "sudo"},
		{name: "bash denies curl pipe bash", set: d.DeniedBashPrefixes, want: "curl | bash"},
		{name: "bash denies dd if", set: d.DeniedBashPrefixes, want: "dd if="},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if !slices.Contains(tt.set, tt.want) {
				t.Errorf("DefaultHardDeny: %q missing from set %v", tt.want, tt.set)
			}
		})
	}
}

// TestDefaultHardDenyMaxReadBytes asserts the per-file read cap default is
// exactly 1 MiB (1<<20), the boundary value the ReadGuard enforces.
func TestDefaultHardDenyMaxReadBytes(t *testing.T) {
	t.Parallel()
	const wantMiB = int64(1 << 20)
	tests := []struct {
		name string
		got  int64
		want int64
	}{
		{name: "default is 1 MiB", got: DefaultHardDeny().MaxReadBytes, want: wantMiB},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if tt.got != tt.want {
				t.Errorf("DefaultHardDeny().MaxReadBytes = %d, want %d", tt.got, tt.want)
			}
		})
	}
}

// TestDefaultHardDenyWriteSupersetsRead asserts the invariant that every read
// deny glob is also a write deny glob (you may never write what you may not
// read), per §3c "same + …".
func TestDefaultHardDenyWriteSupersetsRead(t *testing.T) {
	t.Parallel()
	d := DefaultHardDeny()
	for _, r := range d.DeniedReadPaths {
		if !slices.Contains(d.DeniedWritePaths, r) {
			t.Errorf("read-deny glob %q is not in the write-deny set (write must superset read)", r)
		}
	}
}

// TestDeniedReadWorkspaceSkills proves, through the DeniedRead surface the read
// tools call, that the workspace .skills/ source tree is hard-denied for generic
// file tools (P2b §7a/§10 gate-bypass prevention) while NORMAL workspace paths
// that merely resemble it are NOT — pinning the "**/.skills/**" glob's precision.
// It uses the real DefaultHardDeny() so it exercises the shipped policy, and a
// fixed home so the ~/ globs cannot accidentally match the /ws candidates.
func TestDeniedReadWorkspaceSkills(t *testing.T) {
	t.Parallel()
	pc := NewPermissionChecker(PermissionPolicy{
		WorkspaceRoot: "/ws",
		HardDeny:      DefaultHardDeny(),
	})
	pc.SetHomeDir(func() (string, error) { return "/home/tester", nil })

	tests := []struct {
		name    string
		absPath string
		want    bool
	}{
		// Denied: the .skills dir itself, its contents, and a nested .skills tree.
		{name: "skills dir itself denied", absPath: "/ws/.skills", want: true},
		{name: "skills subdir denied", absPath: "/ws/.skills/foo", want: true},
		{name: "skills file denied", absPath: "/ws/.skills/foo/SKILL.md", want: true},
		{name: "nested skills tree denied", absPath: "/ws/pkg/.skills/bar/SKILL.md", want: true},
		// Allowed: precision — a same-named Go file, a non-dotted skills dir, and
		// near-miss segment names must NOT be denied (no over-broad match).
		{name: "skills.go source allowed", absPath: "/ws/skills.go", want: false},
		{name: "non-dot skills dir allowed", absPath: "/ws/src/skills/x", want: false},
		{name: "skills-prefixed name allowed", absPath: "/ws/.skillsx/foo", want: false},
		{name: "dotted suffix name allowed", absPath: "/ws/a.skills/b", want: false},
		{name: "ordinary go file allowed", absPath: "/ws/main.go", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := pc.DeniedRead(tt.absPath); got != tt.want {
				t.Errorf("DeniedRead(%q) = %v, want %v", tt.absPath, got, tt.want)
			}
		})
	}
}
