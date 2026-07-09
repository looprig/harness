package codex

import (
	"errors"
	"reflect"
	"testing"

	"github.com/looprig/harness/pkg/foreignloop"
)

func TestNewSpecRejectsEmptyRequiredFields(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name         string
		cfg          SpecConfig
		wantErrField string
	}{
		{
			name: "empty ExecPath",
			cfg: SpecConfig{
				Cwd: "/work/repo",
			},
			wantErrField: "ExecPath",
		},
		{
			name: "empty Cwd",
			cfg: SpecConfig{
				ExecPath: "/usr/local/bin/codex",
			},
			wantErrField: "Cwd",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := NewSpec(nil, tt.cfg)
			var sce *SpecConfigError
			if !errors.As(err, &sce) {
				t.Fatalf("NewSpec() error = %v, want *SpecConfigError", err)
			}
			if sce.Field != tt.wantErrField {
				t.Fatalf("SpecConfigError.Field = %q, want %q", sce.Field, tt.wantErrField)
			}
			if sce.Reason != "required" {
				t.Fatalf("SpecConfigError.Reason = %q, want required", sce.Reason)
			}
		})
	}
}

func TestNewSpecBuildsLateBoundSpec(t *testing.T) {
	t.Parallel()
	cfg := SpecConfig{
		ExecPath: "/usr/local/bin/codex",
		Model:    "gpt-5",
		Profile:  "looprig",
		Cwd:      "/work/repo",
		EnvAllow: []string{"PATH"},
	}
	spec, err := NewSpec([]string{"PATH=/usr/bin", "SECRET_TOKEN=shh"}, cfg)
	if err != nil {
		t.Fatalf("NewSpec() error = %v", err)
	}
	if spec.ExecPath != cfg.ExecPath {
		t.Fatalf("spec.ExecPath = %q, want %q", spec.ExecPath, cfg.ExecPath)
	}
	if spec.Cwd != cfg.Cwd {
		t.Fatalf("spec.Cwd = %q, want %q", spec.Cwd, cfg.Cwd)
	}
	if spec.SIDMode != foreignloop.SIDLateBound {
		t.Fatalf("spec.SIDMode = %v, want SIDLateBound", spec.SIDMode)
	}
	if !reflect.DeepEqual(spec.Env, []string{"PATH=/usr/bin"}) {
		t.Fatalf("spec.Env = %v, want PATH only", spec.Env)
	}
	agent, ok := spec.Agent.(*Agent)
	if !ok || agent == nil {
		t.Fatalf("spec.Agent = %T, want *Agent", spec.Agent)
	}
	if agent.ExecPath != cfg.ExecPath {
		t.Fatalf("agent.ExecPath = %q, want %q", agent.ExecPath, cfg.ExecPath)
	}
	if agent.Model != cfg.Model {
		t.Fatalf("agent.Model = %q, want %q", agent.Model, cfg.Model)
	}
	if agent.Profile != cfg.Profile {
		t.Fatalf("agent.Profile = %q, want %q", agent.Profile, cfg.Profile)
	}
	if !reflect.DeepEqual(agent.Env, spec.Env) {
		t.Fatalf("agent.Env = %v, want %v", agent.Env, spec.Env)
	}
}

func TestNewSpecRetainsSpawnTimeConfigOnAgent(t *testing.T) {
	t.Parallel()
	cfg := SpecConfig{
		ExecPath:         "/usr/local/bin/codex",
		Model:            "gpt-5",
		Profile:          "looprig",
		Cwd:              "/work/repo",
		AdditionalDirs:   []string{"/deps/one", "/deps/two"},
		Sandbox:          SandboxDangerFullAccess,
		Approval:         ApprovalNever,
		IgnoreUserConfig: true,
		IgnoreRules:      true,
		SkipGitRepoCheck: true,
	}

	spec, err := NewSpec(nil, cfg)
	if err != nil {
		t.Fatalf("NewSpec() error = %v", err)
	}
	agent, ok := spec.Agent.(*Agent)
	if !ok || agent == nil {
		t.Fatalf("spec.Agent = %T, want *Agent", spec.Agent)
	}
	if !reflect.DeepEqual(agent.AdditionalDirs, cfg.AdditionalDirs) {
		t.Fatalf("agent.AdditionalDirs = %v, want %v", agent.AdditionalDirs, cfg.AdditionalDirs)
	}
	if agent.Sandbox != cfg.Sandbox {
		t.Fatalf("agent.Sandbox = %v, want %v", agent.Sandbox, cfg.Sandbox)
	}
	if agent.Approval != cfg.Approval {
		t.Fatalf("agent.Approval = %v, want %v", agent.Approval, cfg.Approval)
	}
	if agent.IgnoreUserConfig != cfg.IgnoreUserConfig {
		t.Fatalf("agent.IgnoreUserConfig = %v, want %v", agent.IgnoreUserConfig, cfg.IgnoreUserConfig)
	}
	if agent.IgnoreRules != cfg.IgnoreRules {
		t.Fatalf("agent.IgnoreRules = %v, want %v", agent.IgnoreRules, cfg.IgnoreRules)
	}
	if agent.SkipGitRepoCheck != cfg.SkipGitRepoCheck {
		t.Fatalf("agent.SkipGitRepoCheck = %v, want %v", agent.SkipGitRepoCheck, cfg.SkipGitRepoCheck)
	}
}
