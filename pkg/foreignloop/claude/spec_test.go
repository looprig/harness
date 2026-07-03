package claude

import (
	"errors"
	"reflect"
	"testing"

	"github.com/looprig/harness/pkg/foreignloop"
)

// TestNewSpec proves the turn-key resolver builds a foreignloop.Spec backed by the
// claude adapter with a child env that is the EXPLICIT whitelist of the parent (never
// os.Environ() wholesale) plus the single credential, and fails closed (typed
// *SpecConfigError) when a required field is empty.
func TestNewSpec(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name         string
		parentEnv    []string
		cfg          SpecConfig
		wantErr      bool
		wantErrField string
		wantEnv      []string
	}{
		{
			name:      "happy path: allow-listed keys + credential present, stray secret excluded",
			parentEnv: []string{"PATH=/usr/bin", "HOME=/home/u", "SECRET_TOKEN=shh"},
			cfg: SpecConfig{
				ExecPath:   "/usr/local/bin/claude",
				Home:       "/home/u",
				Model:      "claude-opus-4-8",
				Cwd:        "/work/repo",
				Posture:    foreignloop.PostureAcceptEdits,
				EnvAllow:   []string{"PATH", "HOME"},
				Credential: map[string]string{"ANTHROPIC_API_KEY": "sk-test"},
			},
			wantEnv: []string{"PATH=/usr/bin", "HOME=/home/u", "ANTHROPIC_API_KEY=sk-test"},
		},
		{
			name:      "credential only, no allow-list keeps only the secret",
			parentEnv: []string{"PATH=/usr/bin", "SECRET=shh"},
			cfg: SpecConfig{
				ExecPath:   "/bin/claude",
				Model:      "m",
				Cwd:        "/w",
				Posture:    foreignloop.PostureDefault,
				Credential: map[string]string{"ANTHROPIC_API_KEY": "k"},
			},
			wantEnv: []string{"ANTHROPIC_API_KEY=k"},
		},
		{
			name:      "empty parent env yields only the credential",
			parentEnv: nil,
			cfg: SpecConfig{
				ExecPath:   "/bin/claude",
				Model:      "m",
				EnvAllow:   []string{"PATH"},
				Credential: map[string]string{"ANTHROPIC_API_KEY": "k"},
			},
			wantEnv: []string{"ANTHROPIC_API_KEY=k"},
		},
		{
			name:      "empty ExecPath returns SpecConfigError",
			parentEnv: []string{"PATH=/usr/bin"},
			cfg: SpecConfig{
				ExecPath: "",
				Model:    "m",
			},
			wantErr:      true,
			wantErrField: "ExecPath",
		},
		{
			name:      "empty Model returns SpecConfigError",
			parentEnv: []string{"PATH=/usr/bin"},
			cfg: SpecConfig{
				ExecPath: "/bin/claude",
				Model:    "",
			},
			wantErr:      true,
			wantErrField: "Model",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			spec, err := NewSpec(tt.parentEnv, tt.cfg)
			if tt.wantErr {
				var sce *SpecConfigError
				if !errors.As(err, &sce) {
					t.Fatalf("NewSpec() error = %v, want *SpecConfigError", err)
				}
				if sce.Field != tt.wantErrField {
					t.Errorf("SpecConfigError.Field = %q, want %q", sce.Field, tt.wantErrField)
				}
				if sce.Reason != "required" {
					t.Errorf("SpecConfigError.Reason = %q, want %q", sce.Reason, "required")
				}
				return
			}
			if err != nil {
				t.Fatalf("NewSpec() unexpected error = %v", err)
			}
			// Env is the whitelist of the parent (stray secret excluded) plus the credential.
			if !reflect.DeepEqual(spec.Env, tt.wantEnv) {
				t.Errorf("spec.Env = %v, want %v", spec.Env, tt.wantEnv)
			}
			// Agent is a non-nil *Agent carrying the resolved fields verbatim.
			agent, ok := spec.Agent.(*Agent)
			if !ok || agent == nil {
				t.Fatalf("spec.Agent = %T (%v), want non-nil *Agent", spec.Agent, spec.Agent)
			}
			if agent.ExecPath != tt.cfg.ExecPath {
				t.Errorf("agent.ExecPath = %q, want %q", agent.ExecPath, tt.cfg.ExecPath)
			}
			if agent.Home != tt.cfg.Home {
				t.Errorf("agent.Home = %q, want %q", agent.Home, tt.cfg.Home)
			}
			if agent.Model != tt.cfg.Model {
				t.Errorf("agent.Model = %q, want %q", agent.Model, tt.cfg.Model)
			}
			if !reflect.DeepEqual(agent.Env, tt.wantEnv) {
				t.Errorf("agent.Env = %v, want %v", agent.Env, tt.wantEnv)
			}
			// Spec scalar fields mirror the config so the loop and fingerprint see them.
			if spec.ExecPath != tt.cfg.ExecPath {
				t.Errorf("spec.ExecPath = %q, want %q", spec.ExecPath, tt.cfg.ExecPath)
			}
			if spec.Cwd != tt.cfg.Cwd {
				t.Errorf("spec.Cwd = %q, want %q", spec.Cwd, tt.cfg.Cwd)
			}
			if spec.Posture != tt.cfg.Posture {
				t.Errorf("spec.Posture = %v, want %v", spec.Posture, tt.cfg.Posture)
			}
			// Sanity: the resolved spec plugs into the composition-root builder seam.
			if foreignloop.BuildWith(spec) == nil {
				t.Error("foreignloop.BuildWith(spec) = nil, want non-nil Builder")
			}
		})
	}
}
