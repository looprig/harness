// Package explorer is the SWE-Swarm's read-only codebase-mapping leaf agent. It
// exposes its boundary as pure data (Name, Description, Role) and a raw-signature
// BuildTools so the swarm composition root can adapt it into a swe.Agent WITHOUT
// this package importing swarms/swe (which would be an import cycle). It is the
// narrowest leaf: it only ever reads the workspace — no network, no shell, no
// mutation, no spawning — so every tool it has is auto-approved (it never gates).
package explorer

import (
	"github.com/ciram-co/looprig/pkg/identity"
	"github.com/ciram-co/looprig/pkg/loop"
	"github.com/ciram-co/looprig/pkg/tool"
	"github.com/ciram-co/looprig/pkg/tools"
)

// Name is the explorer's immutable attribution name.
const Name = identity.AgentName("explorer")

// Description is the one-line summary the Subagent catalog and greeting render.
const Description = "Read-only navigator: maps the codebase's structure and where things live."

// Role is the explorer's role prompt: a single well-formed
// <role name="explorer"> element, identity-free (the swarm prepends the shared
// identity). It pins read-only codebase mapping/navigation with no network.
const Role = `<role name="explorer">
  <mission>You map and navigate the codebase read-only: you locate where things live, how files and packages relate, and how a change would ripple. You answer "where" and "how is this structured", never "change this".</mission>
  <method>
    <item>Use Glob to discover files, Grep to find symbols and call-sites, and ReadFile to confirm details. Report concrete paths, symbols, and line ranges.</item>
    <item>You have no network access — stay entirely within the workspace. If a question needs the web, say so and defer to the researcher.</item>
  </method>
</role>`

// autoApprovedTools is explorer's hard-approve set: ALL of its tools. Because
// explorer only ever reads the workspace and asks the user, nothing it can do is
// dangerous — so it never prompts. Names match each tool's Info().Name exactly.
var autoApprovedTools = []string{"ReadFile", "Glob", "Grep", "AskUser"}

// BuildTools assembles explorer's exact allowlist (Glob, Grep, ReadFile,
// AskUser) behind a FRESH fail-secure PermissionChecker. A fresh checker per
// call gives every spawned loop independent approval state. Least privilege: the
// read tools get the workspace root + the checker as their ReadGuard; AskUser is
// self-contained. There is deliberately NO Subagent (a leaf cannot spawn), NO
// network tool, NO shell, and NO write/edit tool — explorer is read-only.
//
// skill is the OPTIONAL per-agent Skill tool the swarm wires when explorer has
// ≥1 allowed skill (nil otherwise). When non-nil it is added to the registry and
// "Skill" is appended to the hard-approve set so it auto-approves — a scoped,
// side-effect-free read of trusted in-repo content, the same class as ReadFile.
func BuildTools(root string, skill tool.InvokableTool) loop.ToolSet {
	approved := autoApprovedTools
	if skill != nil {
		approved = append(append([]string(nil), autoApprovedTools...), "Skill")
	}
	policy := tools.PermissionPolicy{
		WorkspaceRoot: root,
		HardDeny:      tools.DefaultHardDeny(),
		HardApprove:   tools.HardApproveRules{Tools: approved},
	}
	pc := tools.NewPermissionChecker(policy)

	registry := []tool.InvokableTool{
		tools.NewReadFile(root, pc),
		tools.NewGlob(root, pc),
		tools.NewGrep(root, pc),
		tools.NewAskUser(),
	}
	if skill != nil {
		registry = append(registry, skill)
	}
	return loop.ToolSet{Permission: pc, Registry: registry}
}
