package codex

import (
	"reflect"
	"testing"

	"github.com/looprig/harness/pkg/foreignloop"
)

func TestBuildStartArgs(t *testing.T) {
	t.Parallel()
	turn := foreignloop.ForeignTurn{Cwd: "/turn/ignored"}
	cfg := runConfig{
		cwd:              "/work/repo",
		model:            "gpt-5",
		profile:          "looprig",
		additionalDirs:   []string{"/deps/one", "/deps/two"},
		sandbox:          SandboxWorkspaceWrite,
		approval:         ApprovalOnRequest,
		ignoreUserConfig: true,
		ignoreRules:      true,
		skipGitRepoCheck: true,
	}

	got := buildStartArgs(turn, cfg, "write code")
	want := []string{
		"exec",
		"--json",
		"--cd", "/work/repo",
		"--model", "gpt-5",
		"--profile", "looprig",
		"--sandbox", "workspace-write",
		"--ask-for-approval", "on-request",
		"--add-dir", "/deps/one",
		"--add-dir", "/deps/two",
		"--ignore-user-config",
		"--ignore-rules",
		"--skip-git-repo-check",
		"write code",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("buildStartArgs() = %#v, want %#v", got, want)
	}
	if got[len(got)-1] != "write code" {
		t.Fatalf("prompt is not last: %v", got)
	}
}

func TestBuildResumeArgs(t *testing.T) {
	t.Parallel()
	turn := foreignloop.ForeignTurn{ForeignSID: "codex-session-123"}
	cfg := runConfig{
		cwd:              "/work/repo",
		model:            "gpt-5",
		profile:          "looprig",
		additionalDirs:   []string{"/deps"},
		sandbox:          SandboxDangerFullAccess,
		approval:         ApprovalNever,
		ignoreUserConfig: true,
		ignoreRules:      true,
		skipGitRepoCheck: true,
	}

	got := buildResumeArgs(turn, cfg, "continue")
	want := []string{
		"exec",
		"resume",
		"--json",
		"codex-session-123",
		"--model", "gpt-5",
		"--ignore-user-config",
		"--ignore-rules",
		"--skip-git-repo-check",
		"continue",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("buildResumeArgs() = %#v, want %#v", got, want)
	}
	if got[len(got)-1] != "continue" {
		t.Fatalf("prompt is not last: %v", got)
	}
	for _, absent := range []string{"--cd", "--profile", "--sandbox", "--ask-for-approval", "--add-dir"} {
		if containsArg(got, absent) {
			t.Fatalf("%s present in conservative resume argv: %v", absent, got)
		}
	}
}

func TestEnumMappingsFailClosed(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name         string
		cfg          runConfig
		wantSandbox  string
		wantApproval string
	}{
		{
			name:         "known least privilege",
			cfg:          runConfig{cwd: "/w", sandbox: SandboxReadOnly, approval: ApprovalUntrusted},
			wantSandbox:  "read-only",
			wantApproval: "untrusted",
		},
		{
			name:         "unknown values fail closed",
			cfg:          runConfig{cwd: "/w", sandbox: SandboxMode(99), approval: ApprovalPolicy(99)},
			wantSandbox:  "read-only",
			wantApproval: "on-request",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			argv := buildStartArgs(foreignloop.ForeignTurn{}, tt.cfg, "prompt")
			if got := nextArg(argv, "--sandbox"); got != tt.wantSandbox {
				t.Fatalf("--sandbox = %q, want %q in %v", got, tt.wantSandbox, argv)
			}
			if got := nextArg(argv, "--ask-for-approval"); got != tt.wantApproval {
				t.Fatalf("--ask-for-approval = %q, want %q in %v", got, tt.wantApproval, argv)
			}
		})
	}
}

func nextArg(argv []string, flag string) string {
	for i, arg := range argv {
		if arg == flag && i+1 < len(argv) {
			return argv[i+1]
		}
	}
	return ""
}

func containsArg(argv []string, want string) bool {
	for _, arg := range argv {
		if arg == want {
			return true
		}
	}
	return false
}
