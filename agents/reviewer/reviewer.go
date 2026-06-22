// Package reviewer is the SWE-Swarm's critique leaf agent. It exposes its
// boundary as pure data (Name, Description, Role) and a raw-signature BuildTools
// so the swarm composition root can adapt it into a swe.Agent WITHOUT this
// package importing swarms/swe (which would be an import cycle). It is a leaf: it
// cannot spawn and it never mutates the filesystem — it reads, may run tests/
// build via Bash to verify claims, and reports findings. It does not fix.
package reviewer

import (
	"github.com/ciram-co/looprig/pkg/identity"
	"github.com/ciram-co/looprig/pkg/loop"
	"github.com/ciram-co/looprig/pkg/tool"
	"github.com/ciram-co/looprig/pkg/tools"
)

// Name is the reviewer's immutable attribution name.
const Name = identity.AgentName("reviewer")

// Description is the one-line summary the Subagent catalog and greeting render.
const Description = "Critiques code and verifies it with tests/build; reports findings, never fixes."

// Role is the reviewer's role prompt: a single well-formed
// <role name="reviewer"> element, identity-free (the swarm prepends the shared
// identity). It pins critique-don't-fix, the ability to run tests/build via
// Bash, and report-don't-mutate.
const Role = `<role name="reviewer">
  <mission>You critique code: correctness, security, design, and adherence to the project's standards. You assess and report — you do NOT fix. Fixing is the implementer's job; your job is to find what is wrong and say why.</mission>
  <method>
    <item>Read the change and its context, then verify your claims: you may run the test suite or build via Bash to confirm a failure rather than assert it from inspection alone.</item>
    <item>Report findings as a prioritized list — each with the file, line range, the problem, and why it matters. Distinguish blocking defects from nits.</item>
  </method>
  <boundary>Never edit, write, or otherwise mutate the workspace; you have no write tools. If a fix is needed, describe it precisely for the implementer instead of applying it.</boundary>
</role>`

// autoApprovedTools is reviewer's hard-approve set: everything EXCEPT Bash. Bash
// runs a shell, so it stays Ask — a human reads and approves each command before
// it runs (the permission gate is the security boundary). The read/todo/ask
// tools are side-effect-free and run without prompting. Names match each tool's
// Info().Name exactly.
var autoApprovedTools = []string{"ReadFile", "Glob", "Grep", "Todo", "AskUser"}

// BuildTools assembles reviewer's exact allowlist (Glob, Grep, ReadFile, Bash,
// Todo, AskUser) behind a FRESH fail-secure PermissionChecker. A fresh checker
// per call gives every spawned loop independent approval state. Least privilege:
// the read tools get the workspace root + the checker as their ReadGuard; Bash
// gets only the root (and stays human-gated); Todo/AskUser are self-contained.
// There is deliberately NO Subagent (a leaf cannot spawn) and NO write/edit tool
// (reviewer critiques, it never mutates).
//
// skill is the OPTIONAL per-agent Skill tool the swarm wires when reviewer has
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
		tools.NewBash(root),
		tools.NewTodo(),
		tools.NewAskUser(),
	}
	if skill != nil {
		registry = append(registry, skill)
	}
	return loop.ToolSet{Permission: pc, Registry: registry}
}
