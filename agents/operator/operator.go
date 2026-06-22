// Package operator is the SWE-Swarm's write+exec implementer leaf agent. It
// exposes its boundary as pure data (Name, Description, Role) and a raw-signature
// BuildTools so the swarm composition root can adapt it into a swe.Agent WITHOUT
// this package importing swarms/swe (which would be an import cycle). It is a
// leaf: it cannot spawn. It is the only leaf that mutates the workspace — it
// reads/searches, writes and edits files, and runs commands via Bash — but every
// mutation is human-gated (Write/Edit/Bash default to Ask). It has no network.
package operator

import (
	"github.com/ciram-co/looprig/pkg/identity"
	"github.com/ciram-co/looprig/pkg/loop"
	"github.com/ciram-co/looprig/pkg/tool"
	"github.com/ciram-co/looprig/pkg/tools"
)

// Name is the operator's immutable attribution name.
const Name = identity.AgentName("operator")

// Description is the one-line summary the Subagent catalog and greeting render.
const Description = "Implements changes: reads, writes/edits files, and runs commands — every mutation human-gated."

// Role is the operator's role prompt: a single well-formed
// <role name="operator"> element, identity-free (the swarm prepends the shared
// identity). It pins the implementer's craft: fix at the root cause, match the
// existing style, read before editing, prefer editing to creating, state the plan
// before any gated mutation, validate the narrowest test first then broaden, and
// don't fix unrelated failures (mention them).
const Role = `<role name="operator">
  <mission>You implement changes: you read and search the workspace, write and edit files, and run commands to make the change real. You carry the work to a verified, working state — you do not just describe a fix, you apply it.</mission>
  <method>
    <item>Fix the problem at its root cause, not with a surface-level patch. Avoid unneeded complexity; keep the change focused on the task.</item>
    <item>Match the style and conventions of the existing codebase. Never guess a file's contents — read it first. Prefer editing an existing file to creating a new one.</item>
    <item>WriteFile, EditFile, and Bash require approval before they run: state your plan in one or two sentences first so the change can be followed and approved, then act.</item>
    <item>Validate your work with the project's tests or build. Start with the narrowest test that covers your change, then broaden as confidence grows.</item>
    <item>Do not fix unrelated test failures or pre-existing breakage — mention them instead and stay focused on the task at hand.</item>
  </method>
</role>`

// autoApprovedTools is operator's hard-approve set: the side-effect-free read/
// search/plan/ask tools that run without prompting. WriteFile, EditFile, and Bash
// are deliberately ABSENT — they mutate the filesystem or run a shell, so they
// stay Ask (a human reads and approves each call; the permission gate is the
// security boundary). Subagent is also absent — a leaf cannot spawn, so the tool
// is never wired at all. Names match each tool's Info().Name exactly; the
// PermissionChecker matches on them.
var autoApprovedTools = []string{"ReadFile", "Glob", "Grep", "Todo", "AskUser"}

// BuildTools assembles operator's exact allowlist (ReadFile, Glob, Grep,
// WriteFile, EditFile, Bash, Todo, AskUser) behind a FRESH fail-secure
// PermissionChecker. A fresh checker per call gives every spawned loop
// independent approval state. Least privilege: the read/search tools get the
// workspace root + the checker as their ReadGuard; WriteFile/EditFile/Bash get
// only the root (and stay human-gated); Todo/AskUser are self-contained. There is
// deliberately NO Subagent (a leaf cannot spawn) and NO network tool (Fetch/
// WebSearch) — operator works entirely within the workspace.
//
// skill is the OPTIONAL per-agent Skill tool the swarm wires when operator has
// ≥1 allowed skill (nil otherwise). When non-nil it is added to the registry and
// "Skill" is appended to the hard-approve set, so it auto-approves — a scoped,
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
		tools.NewWriteFile(root),
		tools.NewEditFile(root),
		tools.NewBash(root),
		tools.NewTodo(),
		tools.NewAskUser(),
	}
	if skill != nil {
		registry = append(registry, skill)
	}
	return loop.ToolSet{Permission: pc, Registry: registry}
}
