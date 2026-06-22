package swe

import (
	"context"
	"strings"
	"testing"

	"github.com/ciram-co/looprig/agents/explorer"
	"github.com/ciram-co/looprig/agents/operator"
	"github.com/ciram-co/looprig/pkg/identity"
	"github.com/ciram-co/looprig/pkg/loop"
	"github.com/ciram-co/looprig/pkg/tool"
)

// skills_wiring_test.go proves the Task-3 composition: a SKILLED leaf (operator)
// gets the Skill tool (auto-approved) AND an <available_skills> catalog in its
// system prompt, while a SKILL-LESS leaf (explorer) gets neither — assembled
// through the SAME spawner seam production uses (Spawn builds the leaf's
// loop.Config), so the assertions exercise the wired behaviour end-to-end.

// spawnCfg drives the spawner to resolve an agent and captures the loop.Config it
// assembled (system prompt + toolset), via the fake runner. It reuses the spawner
// + fakeRunner idiom from spawner_test.go.
func spawnCfg(t *testing.T, agent identity.AgentName) loop.Config {
	t.Helper()
	sp, runner := newTestSwarmSpawner(t)
	if _, err := sp.Spawn(context.Background(), loop.Provenance{}, agent, "do the thing"); err != nil {
		t.Fatalf("Spawn(%q) error = %v", agent, err)
	}
	if !runner.called {
		t.Fatalf("Spawn(%q) never ran the runner", agent)
	}
	return runner.gotCfg
}

// TestSkilledAgentGetsToolAndCatalog proves operator's assembled config carries the
// Skill tool (auto-approved through its wired PermissionChecker) and an
// <available_skills> block listing the code-style skill (name + description).
func TestSkilledAgentGetsToolAndCatalog(t *testing.T) {
	t.Parallel()

	cfg := spawnCfg(t, operator.Name)

	// The Skill tool is wired into operator's toolset.
	names := toolNames(t, cfg.Tools)
	if !containsName(names, "Skill") {
		t.Errorf("operator toolset = %v, want it to contain the Skill tool", names)
	}

	// The Skill tool auto-approves (classUnknown + named in HardApprove).
	skillTool := mustTool(t, cfg.Tools, "Skill")
	if eff := cfg.Tools.Permission.Check(context.Background(), skillTool, "Skill", `{"name":"code-style"}`); eff != loop.EffectAutoApprove {
		t.Errorf("Check(Skill) effect = %v, want %v (Skill must auto-approve)", eff, loop.EffectAutoApprove)
	}

	// The system prompt carries the <available_skills> catalog naming code-style and
	// its description (read from the trusted embedded SKILL.md), AFTER Identity+Role.
	sys := cfg.Model.System
	if !strings.HasPrefix(sys, Identity+operator.Role) {
		t.Error("operator system prompt does not begin with Identity+Role")
	}
	if !strings.Contains(sys, "<available_skills>") || !strings.Contains(sys, "</available_skills>") {
		t.Errorf("operator system prompt missing <available_skills> block:\n%s", sys)
	}
	if !strings.Contains(sys, "code-style") {
		t.Error("operator <available_skills> does not list the code-style skill")
	}
	// The description is read from the embedded SKILL.md frontmatter.
	if !strings.Contains(sys, "coding-style checklist") {
		t.Errorf("operator <available_skills> missing the skill description:\n%s", sys)
	}
}

// TestSkillLessAgentGetsNeither proves a leaf with no skills (explorer) gets neither
// the Skill tool nor an <available_skills> catalog — its config is unchanged.
func TestSkillLessAgentGetsNeither(t *testing.T) {
	t.Parallel()

	cfg := spawnCfg(t, explorer.Name)

	names := toolNames(t, cfg.Tools)
	if containsName(names, "Skill") {
		t.Errorf("explorer toolset = %v, must NOT contain a Skill tool (no skills)", names)
	}
	sys := cfg.Model.System
	if strings.Contains(sys, "available_skills") {
		t.Errorf("explorer system prompt must NOT carry an <available_skills> block:\n%s", sys)
	}
	// A skill-less leaf's prompt is exactly Identity + Role.
	if sys != Identity+explorer.Role {
		t.Errorf("explorer system prompt = %q, want exactly Identity+Role", sys)
	}
}

// mustTool returns the InvokableTool whose Info().Name == name from a toolset, or
// fails the test if it is absent.
func mustTool(t *testing.T, ts loop.ToolSet, name string) tool.InvokableTool {
	t.Helper()
	for _, tl := range ts.Registry {
		info, err := tl.Info(context.Background())
		if err != nil {
			t.Fatalf("Info() error = %v", err)
		}
		if info.Name == name {
			return tl
		}
	}
	t.Fatalf("tool %q not in toolset", name)
	return nil
}
