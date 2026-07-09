package codex

import "github.com/looprig/harness/pkg/foreignloop"

// SandboxMode is the typed Codex CLI sandbox mode.
type SandboxMode uint8

const (
	SandboxReadOnly SandboxMode = iota
	SandboxWorkspaceWrite
	SandboxDangerFullAccess
)

// ApprovalPolicy is the typed Codex CLI approval policy.
type ApprovalPolicy uint8

const (
	ApprovalUntrusted ApprovalPolicy = iota
	ApprovalOnRequest
	ApprovalNever
)

// SpecConfigError is the fail-closed result of resolving an invalid SpecConfig.
type SpecConfigError struct{ Field, Reason string }

func (e *SpecConfigError) Error() string {
	return "codex: spec config: " + e.Field + ": " + e.Reason
}

// SpecConfig is the operator-supplied input resolved into a foreignloop.Spec.
type SpecConfig struct {
	ExecPath         string
	Model            string
	Profile          string
	Cwd              string
	AdditionalDirs   []string
	Sandbox          SandboxMode
	Approval         ApprovalPolicy
	EnvAllow         []string
	Credential       map[string]string
	IgnoreUserConfig bool
	IgnoreRules      bool
	SkipGitRepoCheck bool
}

// Agent is the Codex CLI adapter configuration.
type Agent struct {
	ExecPath         string
	Model            string
	Profile          string
	AdditionalDirs   []string
	Sandbox          SandboxMode
	Approval         ApprovalPolicy
	Env              []string
	IgnoreUserConfig bool
	IgnoreRules      bool
	SkipGitRepoCheck bool
}

// NewSpec resolves a SpecConfig + parent environment into a late-bound Codex spec.
func NewSpec(parentEnv []string, cfg SpecConfig) (foreignloop.Spec, error) {
	if cfg.ExecPath == "" {
		return foreignloop.Spec{}, &SpecConfigError{Field: "ExecPath", Reason: "required"}
	}
	if cfg.Cwd == "" {
		return foreignloop.Spec{}, &SpecConfigError{Field: "Cwd", Reason: "required"}
	}
	env := whitelistEnv(parentEnv, cfg.EnvAllow, cfg.Credential)
	return foreignloop.Spec{
		Agent: &Agent{
			ExecPath:         cfg.ExecPath,
			Model:            cfg.Model,
			Profile:          cfg.Profile,
			AdditionalDirs:   append([]string(nil), cfg.AdditionalDirs...),
			Sandbox:          cfg.Sandbox,
			Approval:         cfg.Approval,
			Env:              env,
			IgnoreUserConfig: cfg.IgnoreUserConfig,
			IgnoreRules:      cfg.IgnoreRules,
			SkipGitRepoCheck: cfg.SkipGitRepoCheck,
		},
		ExecPath: cfg.ExecPath,
		Cwd:      cfg.Cwd,
		Env:      env,
		SIDMode:  foreignloop.SIDLateBound,
	}, nil
}
