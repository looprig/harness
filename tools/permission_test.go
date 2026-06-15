package tools

import (
	"slices"
	"testing"
)

// TestDefaultHardDeny asserts the DefaultHardDeny() constructor returns the
// fail-secure defaults from design §3c: the secret read/write globs, the
// .urvi policy-store deny-write entries (so the tool system can never mutate
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
		{name: "read denies user urvi store", set: d.DeniedReadPaths, want: "~/.urvi/**"},

		// Secret globs must ALSO be in the WRITE deny set (write set ⊇ read set).
		{name: "write denies ssh", set: d.DeniedWritePaths, want: "~/.ssh/**"},
		{name: "write denies dotenv", set: d.DeniedWritePaths, want: "**/.env"},
		{name: "write denies pem", set: d.DeniedWritePaths, want: "**/*.pem"},
		{name: "write denies id_rsa", set: d.DeniedWritePaths, want: "**/id_rsa"},
		// Write-only additions.
		{name: "write denies git config", set: d.DeniedWritePaths, want: "**/.git/config"},
		{name: "write denies go.sum", set: d.DeniedWritePaths, want: "**/go.sum"},
		// Policy-store deny-write entries — the security-critical requirement.
		{name: "write denies in-repo urvi store", set: d.DeniedWritePaths, want: "**/.urvi/**"},
		{name: "write denies user urvi store", set: d.DeniedWritePaths, want: "~/.urvi/**"},

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
