package swe

import (
	"net/http"
	"testing"

	"github.com/inventivepotter/urvi/agents/explorer"
	"github.com/inventivepotter/urvi/agents/operator"
	"github.com/inventivepotter/urvi/agents/researcher"
	"github.com/inventivepotter/urvi/agents/reviewer"
	"github.com/inventivepotter/urvi/internal/agent/loop"
	"github.com/inventivepotter/urvi/internal/agent/loop/identity"
)

// runtime_skills_test.go pins P2b Phase 3c: the --runtime-skills enablement gate
// and the per-agent AllowsRuntimeSkills eligibility. When the mode is OFF (the
// default) NO leaf gains a workspace-capable Skill tool — explorer/researcher get
// no Skill tool at all, and operator keeps its embedded-only (auto-approve) one.
// When the mode is ON, ONLY the read-only eligible agents (explorer, researcher)
// gain a WORKSPACE-enabled Skill tool (its CheckEffect Asks for a non-embedded
// name); operator stays embedded-only and the orchestrator never gets one.

// runtimeDeps is a minimal LeafToolDeps whose Root is a real, distinct path so the
// workspace-enabled Skill tool has a non-empty workspace root.
func runtimeDeps(root string) LeafToolDeps {
	return LeafToolDeps{Root: root, HTTPCl: &http.Client{}}
}

// skillToolFromRegistry resolves agent in reg, builds its toolset, and returns the
// Skill tool (or nil if the agent has none). The deps Root must be the same root the
// registry was built with so the workspace-enabled tool's root matches.
func skillToolFromRegistry(t *testing.T, reg *Registry, deps LeafToolDeps, agent identity.AgentName) loop.ToolSet {
	t.Helper()
	a, ok := reg.Lookup(agent)
	if !ok {
		t.Fatalf("Lookup(%q) not found", agent)
	}
	return a.BuildTools(deps)
}

// TestAllowsRuntimeSkillsEligibility proves the per-agent eligibility flag is set on
// EXACTLY the two read-only agents (explorer, researcher) and is false for the
// write/exec-capable operator + reviewer — the §7a boundary, regardless of mode.
func TestAllowsRuntimeSkillsEligibility(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name identity.AgentName
		want bool
	}{
		{name: operator.Name, want: false},
		{name: researcher.Name, want: true},
		{name: explorer.Name, want: true},
		{name: reviewer.Name, want: false},
	}
	// Eligibility is independent of the RuntimeSkills mode: assert it under BOTH.
	for _, mode := range []bool{false, true} {
		mode := mode
		reg, _, err := leafRegistry(runtimeDeps(t.TempDir()), Config{RuntimeSkills: mode})
		if err != nil {
			t.Fatalf("leafRegistry(RuntimeSkills=%v) error = %v", mode, err)
		}
		for _, tt := range tests {
			tt := tt
			t.Run(string(tt.name), func(t *testing.T) {
				t.Parallel()
				a, ok := reg.Lookup(tt.name)
				if !ok {
					t.Fatalf("Lookup(%q) not found", tt.name)
				}
				if a.AllowsRuntimeSkills != tt.want {
					t.Errorf("AllowsRuntimeSkills = %v, want %v (mode=%v)", a.AllowsRuntimeSkills, tt.want, mode)
				}
			})
		}
	}
}

// TestRuntimeSkillsOffNoWorkspaceTool proves the default (mode OFF): explorer and
// researcher get NO Skill tool, and operator keeps its embedded Skill tool which
// AUTO-APPROVES (embedded-only). This is the HEAD behaviour, unchanged.
func TestRuntimeSkillsOffNoWorkspaceTool(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	deps := runtimeDeps(root)
	reg, _, err := leafRegistry(deps, Config{RuntimeSkills: false})
	if err != nil {
		t.Fatalf("leafRegistry() error = %v", err)
	}

	// Eligible read-only agents get NO Skill tool when the mode is off.
	for _, agent := range []identity.AgentName{explorer.Name, researcher.Name} {
		ts := skillToolFromRegistry(t, reg, deps, agent)
		if containsName(toolNames(t, ts), "Skill") {
			t.Errorf("%q toolset has a Skill tool with RuntimeSkills OFF, want none", agent)
		}
	}

	// operator keeps its embedded Skill tool, and it AUTO-APPROVES (embedded-only):
	// the mode flag never touches an embedded-only agent.
	opTS := skillToolFromRegistry(t, reg, deps, operator.Name)
	skillTool := mustTool(t, opTS, "Skill")
	eff := opTS.Permission.Check(t.Context(), skillTool, "Skill", `{"name":"code-style"}`)
	if eff != loop.EffectAutoApprove {
		t.Errorf("operator Skill effect = %v, want EffectAutoApprove (embedded-only auto-approves)", eff)
	}
}

// TestRuntimeSkillsOnWorkspaceTool proves the mode ON: explorer and researcher each
// gain a WORKSPACE-enabled Skill tool whose CheckEffect Asks (gate) for a
// non-embedded name; operator's Skill stays embedded-only (its code-style name still
// auto-approves), proving the mode never workspace-enables an ineligible agent.
func TestRuntimeSkillsOnWorkspaceTool(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	deps := runtimeDeps(root)
	reg, _, err := leafRegistry(deps, Config{RuntimeSkills: true})
	if err != nil {
		t.Fatalf("leafRegistry() error = %v", err)
	}

	// explorer + researcher gain a workspace-enabled Skill tool: a NON-embedded name
	// returns (EffectAsk, handled=true) from CheckEffect — the workspace gate.
	for _, agent := range []identity.AgentName{explorer.Name, researcher.Name} {
		ts := skillToolFromRegistry(t, reg, deps, agent)
		skillTool := mustTool(t, ts, "Skill")
		ec, ok := skillTool.(interface {
			CheckEffect(string) (loop.Effect, bool)
		})
		if !ok {
			t.Fatalf("%q Skill tool does not implement EffectChecker", agent)
		}
		eff, handled := ec.CheckEffect(`{"name":"project-local"}`)
		if !handled || eff != loop.EffectAsk {
			t.Errorf("%q Skill CheckEffect(non-embedded) = (%v, %v), want (EffectAsk, true) — workspace gate", agent, eff, handled)
		}
	}

	// operator's Skill stays embedded-only even with the mode on (operator is not
	// AllowsRuntimeSkills): its code-style name still auto-approves.
	opTS := skillToolFromRegistry(t, reg, deps, operator.Name)
	skillTool := mustTool(t, opTS, "Skill")
	eff := opTS.Permission.Check(t.Context(), skillTool, "Skill", `{"name":"code-style"}`)
	if eff != loop.EffectAutoApprove {
		t.Errorf("operator Skill effect = %v, want EffectAutoApprove (operator stays embedded-only even when mode on)", eff)
	}
}

// TestRuntimeSkillsOrchestratorNeverGetsTool proves the orchestrator's toolset never
// carries a Skill tool under either mode — it is delegate-only (§7a), regardless of
// the runtime-skills flag.
func TestRuntimeSkillsOrchestratorNeverGetsTool(t *testing.T) {
	t.Parallel()

	for _, mode := range []bool{false, true} {
		mode := mode
		t.Run(map[bool]string{false: "off", true: "on"}[mode], func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			deps := runtimeDeps(root)
			reg, loader, err := leafRegistry(deps, Config{RuntimeSkills: mode})
			if err != nil {
				t.Fatalf("leafRegistry() error = %v", err)
			}
			sp := newSwarmSpawner(reg, deps, &fakeLLM{}, newModelFactory("k"), loader)
			ts := orchestratorToolSet(deps.Root, sp, toolCatalog(reg))
			if containsName(toolNames(t, ts), "Skill") {
				t.Errorf("orchestrator toolset has a Skill tool (mode=%v), want none (delegate-only)", mode)
			}
		})
	}
}
