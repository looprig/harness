package codex

import "github.com/looprig/harness/pkg/foreignloop"

type runConfig struct {
	cwd              string
	model            string
	profile          string
	additionalDirs   []string
	sandbox          SandboxMode
	approval         ApprovalPolicy
	ignoreUserConfig bool
	ignoreRules      bool
	skipGitRepoCheck bool
}

func buildStartArgs(_ foreignloop.ForeignTurn, c runConfig, prompt string) []string {
	args := []string{
		"exec",
		"--json",
		"--cd", c.cwd,
	}
	if c.model != "" {
		args = append(args, "--model", c.model)
	}
	if c.profile != "" {
		args = append(args, "--profile", c.profile)
	}
	args = append(args,
		"--sandbox", sandboxString(c.sandbox),
		"--ask-for-approval", approvalString(c.approval),
	)
	for _, dir := range c.additionalDirs {
		args = append(args, "--add-dir", dir)
	}
	if c.ignoreUserConfig {
		args = append(args, "--ignore-user-config")
	}
	if c.ignoreRules {
		args = append(args, "--ignore-rules")
	}
	if c.skipGitRepoCheck {
		args = append(args, "--skip-git-repo-check")
	}
	return append(args, prompt)
}

func buildResumeArgs(t foreignloop.ForeignTurn, c runConfig, prompt string) []string {
	args := []string{
		"exec",
		"resume",
		"--json",
	}
	if c.model != "" {
		args = append(args, "--model", c.model)
	}
	if c.ignoreUserConfig {
		args = append(args, "--ignore-user-config")
	}
	if c.ignoreRules {
		args = append(args, "--ignore-rules")
	}
	if c.skipGitRepoCheck {
		args = append(args, "--skip-git-repo-check")
	}
	return append(args, t.ForeignSID, prompt)
}

func sandboxString(mode SandboxMode) string {
	switch mode {
	case SandboxWorkspaceWrite:
		return "workspace-write"
	case SandboxDangerFullAccess:
		return "danger-full-access"
	default:
		return "read-only"
	}
}

func approvalString(policy ApprovalPolicy) string {
	switch policy {
	case ApprovalUntrusted:
		return "untrusted"
	case ApprovalNever:
		return "never"
	case ApprovalOnRequest:
		return "on-request"
	default:
		return "on-request"
	}
}
