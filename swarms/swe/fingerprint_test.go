package swe

import (
	"path/filepath"
	"testing"

	"github.com/ciram-co/looprig/agents/orchestrator"
	"github.com/ciram-co/looprig/pkg/session"
)

// TestOrchestratorFingerprintFields asserts the swarm-level config-fingerprint fields the
// composition root injects: AgentKind is the swarm+primary identity ("swe:orchestrator"),
// RuntimeSkills passes the human-set mode through verbatim, and WorkspaceRoot is the
// canonical absolute root. These are what a restore compares so a session cannot silently
// resume under a different agent identity, skill-trust mode, or repo.
func TestOrchestratorFingerprintFields(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	wantRoot := canonicalWorkspaceRoot(root)

	tests := []struct {
		name string
		cfg  Config
		want session.ConfigFingerprintFields
	}{
		{
			name: "runtime skills off",
			cfg:  Config{RuntimeSkills: false},
			want: session.ConfigFingerprintFields{AgentKind: "swe:orchestrator", RuntimeSkills: false, WorkspaceRoot: wantRoot},
		},
		{
			name: "runtime skills on",
			cfg:  Config{RuntimeSkills: true},
			want: session.ConfigFingerprintFields{AgentKind: "swe:orchestrator", RuntimeSkills: true, WorkspaceRoot: wantRoot},
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := orchestratorFingerprintFields(root, tt.cfg)
			if got != tt.want {
				t.Errorf("orchestratorFingerprintFields = %+v, want %+v", got, tt.want)
			}
		})
	}
}

// TestOrchestratorAgentKindFormat pins the AgentKind to "<swarm>:<primary agent>" so a
// rename of the orchestrator's attribution name is reflected in the fingerprint (and a
// prior coding/other session, with a different or empty AgentKind, cannot resume as SWE).
func TestOrchestratorAgentKindFormat(t *testing.T) {
	t.Parallel()
	want := "swe:" + string(orchestrator.Name)
	if orchestratorAgentKind != want {
		t.Errorf("orchestratorAgentKind = %q, want %q", orchestratorAgentKind, want)
	}
	if orchestratorAgentKind != "swe:orchestrator" {
		t.Errorf("orchestratorAgentKind = %q, want %q", orchestratorAgentKind, "swe:orchestrator")
	}
}

// TestCanonicalWorkspaceRoot asserts the root id is absolute + cleaned: an absolute path is
// returned cleaned (redundant separators/dot segments collapsed), so two runs against the
// same repo produce the SAME id and two repos produce DIFFERENT ids. A relative input is
// made absolute against the cwd.
func TestCanonicalWorkspaceRoot(t *testing.T) {
	t.Parallel()

	abs := t.TempDir()
	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "absolute clean is identity", in: abs, want: abs},
		{name: "redundant segments collapse", in: abs + "/./sub/../", want: filepath.Clean(abs)},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := canonicalWorkspaceRoot(tt.in); got != tt.want {
				t.Errorf("canonicalWorkspaceRoot(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}

	// A relative input is anchored to an absolute path (begins with the separator) — never
	// returned relative, so the id is stable regardless of how the caller spelled the root.
	rel := canonicalWorkspaceRoot("some/rel/path")
	if !filepath.IsAbs(rel) {
		t.Errorf("canonicalWorkspaceRoot(relative) = %q, want an absolute path", rel)
	}
}
