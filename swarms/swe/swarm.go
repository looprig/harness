// Package swe assembles the SWE-Swarm: it owns the model/provider, the leaf-agent
// registry, and the construction of the orchestrator as the swarm's PRIMARY loop.
// New is the composition root the TUI/CLI calls to obtain a tui.Agent.
package swe

import (
	"context"
	"os"

	"github.com/inventivepotter/urvi/agents/orchestrator"
	"github.com/inventivepotter/urvi/internal/agent/loop"
	"github.com/inventivepotter/urvi/internal/agent/session"
	"github.com/inventivepotter/urvi/internal/llm"
	"github.com/inventivepotter/urvi/internal/tool"
	"github.com/inventivepotter/urvi/tools"
	"github.com/inventivepotter/urvi/tui"
)

// Subagent-spawn safety caps applied to the orchestrator's session. They are the
// two independent backstops against a runaway agent tree: orchestratorSpawnDepth
// bounds spawn-chain nesting, orchestratorSpawnQuota bounds the total sub-loops the
// session may ever spawn. They take effect once the Subagent tool is wired (4B);
// they are passed now so the cap is in force the moment spawning is enabled.
const (
	orchestratorSpawnDepth = 3
	orchestratorSpawnQuota = 64
)

// orchestratorAutoApprovedTools is the orchestrator's hard-approve set in 4A:
// EVERY tool it carries. The orchestrator only reads/searches the workspace, plans
// (Todo), and asks the user (AskUser) — nothing it can do is side-effecting — so
// every tool runs without prompting. Subagent is deliberately ABSENT: it is not
// wired until 4B (the tool-arg flip), so it appears in neither the registry nor the
// approve set here. Names match each tool's Info().Name exactly.
var orchestratorAutoApprovedTools = []string{"ReadFile", "Glob", "Grep", "Todo", "AskUser"}

// orchestratorToolSet assembles the orchestrator's 4A toolset behind a FRESH
// fail-secure PermissionChecker: ReadFile, Glob, Grep, Todo, AskUser — all five
// auto-approved. Least privilege: the read/search tools get the workspace root +
// the checker as their ReadGuard; Todo/AskUser are self-contained. There is
// deliberately NO Subagent (added in 4B), NO write/edit tool, NO shell, and NO
// network tool — the 4A orchestrator only reads, plans, and asks.
func orchestratorToolSet(root string) loop.ToolSet {
	policy := tools.PermissionPolicy{
		WorkspaceRoot: root,
		HardDeny:      tools.DefaultHardDeny(),
		HardApprove:   tools.HardApproveRules{Tools: orchestratorAutoApprovedTools},
	}
	pc := tools.NewPermissionChecker(policy)

	registry := []tool.InvokableTool{
		tools.NewReadFile(root, pc),
		tools.NewGlob(root, pc),
		tools.NewGrep(root, pc),
		tools.NewTodo(),
		tools.NewAskUser(),
	}
	return loop.ToolSet{Permission: pc, Registry: registry}
}

// orchestratorConfig assembles the orchestrator's primary loop.Config: the shared
// client, a model spec whose system prompt is the swarm Identity prepended to the
// orchestrator's Role (the swarm owns identity; the agent owns its role), its 4A
// toolset, and its attribution name. It is the single place the orchestrator's
// primary config is built so New and the test seam cannot drift.
func orchestratorConfig(client llm.LLM, factory ModelFactory, root string) loop.Config {
	return loop.Config{
		Client:    client,
		Model:     factory(Identity + orchestrator.Role),
		Tools:     orchestratorToolSet(root),
		AgentName: orchestrator.Name,
	}
}

// New constructs the SWE-Swarm and returns it as a tui.Agent driven by the
// orchestrator running as the PRIMARY loop. It reads LLM_API_KEY (the only
// env-sourced value; fail-loud via *MissingEnvError if a required key is missing),
// builds the shared provider client + ModelFactory, resolves the workspace root,
// and starts the orchestrator's session under the spawn caps. The session runs
// under an agent-owned root context, so ctx bounds only construction — Close, not
// ctx, controls the session's lifetime. The caller owns the agent and must Close it.
//
// 4A scope: the orchestrator's toolset is read/search + Todo + AskUser only. The
// Subagent tool is NOT wired this phase (it needs the tool-arg flip in 4B), so the
// orchestrator cannot yet spawn the leaf registry's agents.
func New(ctx context.Context) (tui.Agent, error) {
	client, factory, err := buildClient()
	if err != nil {
		return nil, err
	}
	return newWithClient(ctx, client, factory)
}

// newWithClient is the construction seam shared by New and tests; tests inject a
// fake llm.LLM + a key-bound ModelFactory here, avoiding real environment reads and
// network calls. It resolves the workspace root (fail-fast on os.Getwd error),
// builds the orchestrator's primary config, and starts the session under the spawn
// caps via newSessionAgent (which owns the agent-rooted lifetime). ctx bounds only
// this construction call.
func newWithClient(ctx context.Context, client llm.LLM, factory ModelFactory) (*sessionAgent, error) {
	// The workspace root is the process working directory: file tools are confined
	// to it and the PermissionChecker uses it for containment + path relativisation.
	root, err := os.Getwd()
	if err != nil {
		return nil, &WorkspaceRootError{Cause: err}
	}

	cfg := orchestratorConfig(client, factory, root)
	return newSessionAgent(ctx, cfg, session.WithLimits(session.Limits{
		Depth: orchestratorSpawnDepth,
		Quota: orchestratorSpawnQuota,
	}))
}
