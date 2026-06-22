package swe

import (
	"github.com/inventivepotter/urvi/agents/explorer"
	"github.com/inventivepotter/urvi/agents/operator"
	"github.com/inventivepotter/urvi/agents/researcher"
	"github.com/inventivepotter/urvi/agents/reviewer"
	"github.com/inventivepotter/urvi/internal/agent/loop"
	"github.com/inventivepotter/urvi/internal/agent/loop/identity"
	"github.com/inventivepotter/urvi/internal/tool"
	"github.com/inventivepotter/urvi/tools"
)

// operatorSkills is the operator leaf's closed set of allowed embedded skills. The
// implementer gets the coding-style checklist; the other leaves start with none in
// this cut. This is the single source of truth the loader's allow-map AND the
// agent's <available_skills> catalog are both derived from.
var operatorSkills = []string{"code-style"}

// leafBuiltins describes each spawnable leaf the swarm wires: its package-exported
// boundary (Name/Description/Role + allowed Skills) and a raw binder that adapts
// the leaf package's BuildTools(root[, http], skill) into the swe.Agent shape. It
// is the ONE place the per-agent skill set and the leaf wiring are declared, so the
// loader's allow-map, the per-agent Skill tool, and the catalog all stay in sync.
type leafBuiltin struct {
	name        identity.AgentName
	description string
	role        string
	skills      []string
	// build adapts the leaf's raw BuildTools, threading the OPTIONAL per-agent Skill
	// tool (nil when the agent has no skills) into the leaf's allowlist.
	build func(d LeafToolDeps, skill tool.InvokableTool) loop.ToolSet
}

// leafBuiltins is the fixed roster of spawnable leaves, in deterministic catalog
// order. The orchestrator is deliberately absent: it is the primary loop, not a
// spawnable leaf.
func leafBuiltins() []leafBuiltin {
	return []leafBuiltin{
		{
			name:        operator.Name,
			description: operator.Description,
			role:        operator.Role,
			skills:      operatorSkills,
			build:       func(d LeafToolDeps, s tool.InvokableTool) loop.ToolSet { return operator.BuildTools(d.Root, s) },
		},
		{
			name:        researcher.Name,
			description: researcher.Description,
			role:        researcher.Role,
			build: func(d LeafToolDeps, s tool.InvokableTool) loop.ToolSet {
				return researcher.BuildTools(d.Root, d.HTTPCl, s)
			},
		},
		{
			name:        explorer.Name,
			description: explorer.Description,
			role:        explorer.Role,
			build:       func(d LeafToolDeps, s tool.InvokableTool) loop.ToolSet { return explorer.BuildTools(d.Root, s) },
		},
		{
			name:        reviewer.Name,
			description: reviewer.Description,
			role:        reviewer.Role,
			build:       func(d LeafToolDeps, s tool.InvokableTool) loop.ToolSet { return reviewer.BuildTools(d.Root, s) },
		},
	}
}

// leafRegistry builds the SWE-Swarm's registry of spawnable LEAF agents from the
// four leaf packages, adapting each leaf's raw-signature BuildTools into the
// swe.Agent shape (func(LeafToolDeps) loop.ToolSet) at the composition root — so
// the leaf packages never import swarms/swe (no import cycle). The orchestrator is
// deliberately absent: it is the primary loop, not a spawnable leaf.
// AllowsRuntimeSkills is left false in P1. A duplicate name fails secure with a
// *DuplicateAgentError.
//
// It also returns the ONE per-agent-scoped skill loader (built over the embedded
// SkillsFS + the allow-map derived from every leaf's Skills), TYPED as the narrow
// tools.SkillDescriber the spawner needs (interface segregation: the spawner only
// reads catalog metadata). The loader is wired two ways: a per-agent tools.Skill
// tool — built here from the loader's Load capability — is captured in each skilled
// leaf's BuildTools closure (so the leaf gets the Skill tool, which auto-approves by
// being named in HardApprove), and the spawner uses the returned SkillDescriber to
// append the <available_skills> catalog to a skilled leaf's system prompt. A leaf
// with no skills gets neither the tool nor the catalog (its closure passes a nil
// skill).
//
// The deps parameter is deliberately NOT captured by the adapters: each adapter
// re-invokes the leaf's BuildTools with the deps the swarm passes PER SPAWN (the
// registry stores the closure, not its result), so every spawn gets a FRESH
// PermissionChecker — the per-loop approval-isolation guarantee. The Skill tool is
// the one captured value: it is immutable + side-effect-free (loader + agent name),
// so sharing one instance across a leaf's spawns is safe.
func leafRegistry(_ LeafToolDeps) (*Registry, tools.SkillDescriber, error) {
	builtins := leafBuiltins()

	scopes := make([]skillScope, 0, len(builtins))
	for _, b := range builtins {
		scopes = append(scopes, skillScope{name: b.name, skills: b.skills})
	}
	loader := tools.NewEmbeddedSkillLoader(SkillsFS, buildSkillAllow(scopes))

	agents := make([]Agent, 0, len(builtins))
	for _, b := range builtins {
		b := b
		// Capture a per-agent Skill tool ONLY when the leaf has ≥1 allowed skill;
		// nil otherwise so the leaf gets neither the tool nor a HardApprove entry.
		var skill tool.InvokableTool
		if len(b.skills) > 0 {
			skill = tools.NewSkill(loader, b.name)
		}
		agents = append(agents, Agent{
			Name:        b.name,
			Description: b.description,
			Role:        b.role,
			Skills:      b.skills,
			BuildTools:  func(d LeafToolDeps) loop.ToolSet { return b.build(d, skill) },
		})
	}

	reg, err := NewRegistry(agents...)
	if err != nil {
		return nil, nil, err
	}
	return reg, loader, nil
}
