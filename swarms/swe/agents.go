package swe

import (
	"github.com/ciram-co/looprig/agents/explorer"
	"github.com/ciram-co/looprig/agents/operator"
	"github.com/ciram-co/looprig/agents/researcher"
	"github.com/ciram-co/looprig/agents/reviewer"
	"github.com/ciram-co/looprig/pkg/identity"
	"github.com/ciram-co/looprig/pkg/loop"
	"github.com/ciram-co/looprig/pkg/tool"
	"github.com/ciram-co/looprig/pkg/tools"
)

// operatorSkills is the operator leaf's closed set of allowed embedded skills. The
// implementer gets the coding-style checklist; the other leaves start with none in
// this cut. This is the single source of truth the loader's allow-map AND the
// agent's <available_skills> catalog are both derived from.
var operatorSkills = []string{"code-style"}

// leafBuiltin describes each spawnable leaf the swarm wires: its package-exported
// boundary (Name/Description/Role + allowed Skills + runtime-skills eligibility) and
// a raw binder that adapts the leaf package's BuildTools(root[, http], skill) into
// the swe.Agent shape. It is the ONE place the per-agent skill set, the runtime-skills
// eligibility, and the leaf wiring are declared, so the loader's allow-map, the
// per-agent Skill tool, and the catalog all stay in sync.
type leafBuiltin struct {
	name        identity.AgentName
	description string
	role        string
	skills      []string
	// allowsRuntimeSkills marks a leaf eligible for the untrusted, human-gated
	// workspace skill source (§7a). True ONLY for the read-only agents (explorer +
	// researcher); a write/exec-capable leaf (operator, reviewer) stays false — it
	// already inspects files through its human Bash gate, so it needs no auto path to
	// untrusted skills. It is the per-agent half of the gate; the swarm-wide
	// RuntimeSkills mode is the other half — BOTH must be true to wire the source.
	allowsRuntimeSkills bool
	// build adapts the leaf's raw BuildTools, threading the OPTIONAL per-agent Skill
	// tool (nil when the agent has neither embedded skills nor a wired workspace
	// source) into the leaf's allowlist.
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
			name:                researcher.Name,
			description:         researcher.Description,
			role:                researcher.Role,
			allowsRuntimeSkills: true, // §7a: read-only agent — eligible for the workspace skill source
			build: func(d LeafToolDeps, s tool.InvokableTool) loop.ToolSet {
				return researcher.BuildTools(d.Root, d.HTTPCl, s)
			},
		},
		{
			name:                explorer.Name,
			description:         explorer.Description,
			role:                explorer.Role,
			allowsRuntimeSkills: true, // §7a: read-only agent — eligible for the workspace skill source
			build:               func(d LeafToolDeps, s tool.InvokableTool) loop.ToolSet { return explorer.BuildTools(d.Root, s) },
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
// deliberately absent: it is the primary loop, not a spawnable leaf. Each leaf's
// AllowsRuntimeSkills is carried through from its builtin definition (§7a: true for
// the read-only explorer + researcher). A duplicate name fails secure with a
// *DuplicateAgentError.
//
// It also returns the ONE per-agent-scoped skill loader (built over the embedded
// SkillsFS + the allow-map derived from every leaf's Skills), TYPED as the narrow
// tools.SkillDescriber the spawner needs (interface segregation: the spawner only
// reads catalog metadata). The loader is wired two ways: a per-agent tools.Skill
// tool — built here from the loader's Load capability — is captured in each skilled
// leaf's BuildTools closure (so the leaf gets the Skill tool, which auto-approves by
// being named in HardApprove for an embedded name), and the spawner uses the returned
// SkillDescriber to append the <available_skills> catalog to a skilled leaf's system
// prompt.
//
// RUNTIME (WORKSPACE) SKILLS — §7a. A leaf gets a Skill tool when it has ≥1 embedded
// skill OR it is workspace-eligible AND the cfg.RuntimeSkills mode is on (BOTH gates).
// When the leaf is workspace-eligible and the mode is on, its Skill tool is built
// WORKSPACE-ENABLED (tools.WithWorkspaceRoot(deps.Root), the same root the file tools
// use): an embedded name still auto-approves (embedded-wins), a non-embedded name is a
// human-gated workspace load. The eligible agents (explorer, researcher) have no
// embedded skills, so their workspace Skill tool gets NO <available_skills> catalog —
// workspace skill descriptions are untrusted (§7a) and are never injected into the
// system prompt; the model loads a workspace skill by a name it already knows. A leaf
// that is neither skilled nor (eligible+mode-on) gets a nil Skill tool — neither the
// tool nor a HardApprove entry.
//
// The deps parameter IS now read to source the workspace root for an enabled leaf's
// Skill tool, but it is still NOT captured by the build adapters: each adapter
// re-invokes the leaf's BuildTools with the deps the swarm passes PER SPAWN (the
// registry stores the closure, not its result), so every spawn gets a FRESH
// PermissionChecker — the per-loop approval-isolation guarantee. The Skill tool is
// the one captured value: it is immutable + side-effect-free (loader + agent name +
// the fixed workspace root), so sharing one instance across a leaf's spawns is safe.
func leafRegistry(deps LeafToolDeps, cfg Config) (*Registry, tools.SkillDescriber, error) {
	builtins := leafBuiltins()

	scopes := make([]skillScope, 0, len(builtins))
	for _, b := range builtins {
		scopes = append(scopes, skillScope{name: b.name, skills: b.skills})
	}
	loader := tools.NewEmbeddedSkillLoader(SkillsFS, buildSkillAllow(scopes))

	agents := make([]Agent, 0, len(builtins))
	for _, b := range builtins {
		b := b
		skill := buildLeafSkill(loader, b, deps, cfg)
		agents = append(agents, Agent{
			Name:                b.name,
			Description:         b.description,
			Role:                b.role,
			Skills:              b.skills,
			AllowsRuntimeSkills: b.allowsRuntimeSkills,
			BuildTools:          func(d LeafToolDeps) loop.ToolSet { return b.build(d, skill) },
		})
	}

	reg, err := NewRegistry(agents...)
	if err != nil {
		return nil, nil, err
	}
	return reg, loader, nil
}

// buildLeafSkill constructs the per-agent Skill tool for one leaf, honoring BOTH
// halves of the §7a gate. It returns nil — the leaf gets no Skill tool — unless the
// leaf either has ≥1 embedded skill or is workspace-eligible with the RuntimeSkills
// mode on. When the leaf is workspace-eligible and the mode is on, the tool is
// WORKSPACE-ENABLED at deps.Root (embedded-wins; a non-embedded name is Ask-gated);
// otherwise it is the embedded-only tool (auto-approve). Returning a typed nil
// tool.InvokableTool (not a typed-nil *tools.Skill) keeps the caller's nil check
// correct — the build closure passes nil straight through to the leaf.
func buildLeafSkill(loader tools.SkillLoader, b leafBuiltin, deps LeafToolDeps, cfg Config) tool.InvokableTool {
	workspaceEnabled := cfg.RuntimeSkills && b.allowsRuntimeSkills
	if len(b.skills) == 0 && !workspaceEnabled {
		return nil // neither embedded skills nor a wired workspace source: no Skill tool.
	}
	if workspaceEnabled {
		return tools.NewSkill(loader, b.name, tools.WithWorkspaceRoot(deps.Root))
	}
	return tools.NewSkill(loader, b.name)
}
