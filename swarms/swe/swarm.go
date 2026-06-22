// Package swe assembles the SWE-Swarm: it owns the model/provider, the leaf-agent
// registry, and the construction of the orchestrator as the swarm's PRIMARY loop.
// New is the composition root the TUI/CLI calls to obtain a tui.Agent.
package swe

import (
	"context"
	"crypto/tls"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/inventivepotter/urvi/agents/orchestrator"
	"github.com/inventivepotter/urvi/internal/agent/loop"
	"github.com/inventivepotter/urvi/internal/agent/session"
	"github.com/inventivepotter/urvi/internal/llm"
	"github.com/inventivepotter/urvi/internal/tool"
	"github.com/inventivepotter/urvi/tools"
	"github.com/inventivepotter/urvi/tui"
)

// orchestratorAgentKind is the swarm + primary agent identity stamped onto the session's
// config fingerprint (the AgentKind field). It binds a persisted session to the SWE swarm
// running the orchestrator as its primary, so a prior coding/other-swarm session (a
// different or empty AgentKind, and anyway a different system-prompt/tool-policy digest)
// can never silently resume as SWE. Format is "<swarm>:<primary agent>".
const orchestratorAgentKind = "swe:" + string(orchestrator.Name)

// canonicalWorkspaceRoot returns the canonical absolute id of the workspace root for the
// config fingerprint: filepath.Abs (os.Getwd already returns absolute, but a future caller
// may not) then filepath.Clean. Two runs against the same repo produce the same id; two
// repos produce different ids — so a session cannot silently resume under a different
// repo's .skills/ (the RuntimeSkills mode flag alone does not distinguish them).
func canonicalWorkspaceRoot(root string) string {
	abs, err := filepath.Abs(root)
	if err != nil {
		// Abs only fails when the cwd is unavailable; root from os.Getwd is already
		// absolute, so fall back to a Clean of the input rather than fail the fingerprint.
		return filepath.Clean(root)
	}
	return filepath.Clean(abs)
}

// orchestratorFingerprintFields assembles the swarm-level config-fingerprint inputs that
// do NOT live on loop.Config: the swarm+primary AgentKind, the human-set RuntimeSkills
// mode (so a session can't resume under a different skill-trust mode), and the canonical
// workspace-root id (so it can't resume against a different repo's workspace). The session
// merges these onto the loop-derived fingerprint at New and compares them at Restore.
func orchestratorFingerprintFields(root string, cfg Config) session.ConfigFingerprintFields {
	return session.ConfigFingerprintFields{
		AgentKind:     orchestratorAgentKind,
		RuntimeSkills: cfg.RuntimeSkills,
		WorkspaceRoot: canonicalWorkspaceRoot(root),
	}
}

// Subagent-spawn safety caps applied to the orchestrator's session. They are the
// two independent backstops against a runaway agent tree: orchestratorSpawnDepth
// bounds spawn-chain nesting, orchestratorSpawnQuota bounds the total sub-loops the
// session may ever spawn. They take effect once the Subagent tool is wired (4B);
// they are passed now so the cap is in force the moment spawning is enabled.
const (
	orchestratorSpawnDepth = 3
	orchestratorSpawnQuota = 64
)

// orchestratorLimits is the single source of the orchestrator session's subagent-spawn
// safety caps (depth + quota). Both the headless New path and the persisted Open path
// build the session under these caps via session.WithLimits, so the cap is identical
// however the session is opened (new, resumed, or reopened on /clear).
func orchestratorLimits() session.Limits {
	return session.Limits{Depth: orchestratorSpawnDepth, Quota: orchestratorSpawnQuota}
}

// orchestratorAutoApprovedTools is the orchestrator's hard-approve set: EVERY tool it
// carries. The orchestrator reads/searches the workspace, plans (Todo), asks the user
// (AskUser), and spawns named leaf agents (Subagent) — none of these is directly
// side-effecting (a spawned leaf's own side-effecting tools are gated by the leaf's
// OWN PermissionChecker, built fresh per spawn) — so every orchestrator tool runs
// without prompting. Subagent itself has no path/command boundary (classUnknown), so
// it reaches AutoApprove only by being named here. Names match each tool's Info().Name
// exactly.
var orchestratorAutoApprovedTools = []string{"ReadFile", "Glob", "Grep", "Todo", "AskUser", "Subagent"}

// orchestratorToolSet assembles the orchestrator's toolset behind a FRESH fail-secure
// PermissionChecker: ReadFile, Glob, Grep, Todo, AskUser, Subagent — all auto-approved.
// Least privilege: the read/search tools get the workspace root + the checker as their
// ReadGuard; Todo/AskUser are self-contained; Subagent depends only on the narrow
// swarmSpawner (which resolves named leaves against the registry) + the spawnable-agent
// catalog it renders into its description. There is deliberately NO write/edit tool, NO
// shell, and NO network tool on the orchestrator itself — it reads, plans, asks, and
// delegates side-effecting work to spawned leaves (each gated by the leaf's own checker).
func orchestratorToolSet(root string, spawner *swarmSpawner, catalog []tools.SubagentCatalogEntry) loop.ToolSet {
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
		tools.NewSubagent(spawner, catalog),
	}
	return loop.ToolSet{Permission: pc, Registry: registry}
}

// toolCatalog projects the swarm's registry catalog (swe.AgentCatalogEntry) onto the
// tools-level []tools.SubagentCatalogEntry the Subagent tool renders into its
// description. It is the composition-root seam that keeps tools/ from importing
// swarms/swe: the swarm owns the agent set; the tool only needs name+description.
func toolCatalog(reg *Registry) []tools.SubagentCatalogEntry {
	entries := reg.Catalog()
	out := make([]tools.SubagentCatalogEntry, 0, len(entries))
	for _, e := range entries {
		out = append(out, tools.SubagentCatalogEntry{Name: e.Name, Description: e.Description})
	}
	return out
}

// orchestratorConfig assembles the orchestrator's primary loop.Config: the shared
// client, a model spec whose system prompt is the swarm Identity prepended to the
// orchestrator's Role (the swarm owns identity; the agent owns its role), its toolset
// (read/search + Todo + AskUser + the agent-aware Subagent wired to spawner), and its
// attribution name. It is the single place the orchestrator's primary config is built
// so every construction path (New, openNew, openResume) cannot drift. spawner is the
// UNBOUND swarmSpawner the Subagent tool forwards to; the caller binds the live session
// onto it after the session is built.
func orchestratorConfig(client llm.LLM, factory ModelFactory, root string, spawner *swarmSpawner, catalog []tools.SubagentCatalogEntry) loop.Config {
	return loop.Config{
		Client:    client,
		Model:     factory(Identity + orchestrator.Role),
		Tools:     orchestratorToolSet(root, spawner, catalog),
		AgentName: orchestrator.Name,
	}
}

// httpClientTimeout bounds every web request a spawned leaf's Fetch/WebSearch tools
// make, so a hung endpoint can never block a tool call indefinitely (CLAUDE.md: no
// unbounded blocking).
const httpClientTimeout = 30 * time.Second

// newHTTPClient builds the single *http.Client shared by every spawned leaf's web
// tools (Fetch + the DuckDuckGo provider). It sets an explicit overall timeout and
// pins the TLS floor to 1.2 (never InsecureSkipVerify), per CLAUDE.md's TLS rules.
func newHTTPClient() *http.Client {
	return &http.Client{
		Timeout: httpClientTimeout,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12},
		},
	}
}

// orchestratorWiring is the assembled orchestrator construction: the primary cfg
// (Subagent wired) plus the UNBOUND swarmSpawner the Subagent tool forwards to. A
// construction path builds it, creates/restores the session from cfg, then binds the
// live session onto the spawner (see swarmSpawner's LATE-BIND note). The leaf Registry
// is the authoritative spawnable set; a build error (a duplicate leaf name) fails the
// whole construction (fail secure — no half-wired orchestrator).
type orchestratorWiring struct {
	cfg     loop.Config
	spawner *swarmSpawner
}

// buildOrchestratorWiring is the single seam that assembles the orchestratorWiring used
// by ALL THREE construction paths (New, openNew, openResume), so the spawner + Subagent
// wiring cannot drift across them. It builds the leaf Registry + shared HTTP client, the
// unbound spawner, and the primary cfg (with Subagent wired to the spawner). cfg carries
// the human-set construction modes (today: RuntimeSkills) down to leafRegistry, so a
// workspace-eligible leaf's Skill tool is workspace-enabled when the mode is on. The
// workspace root the Skill tool reads is the SAME root the file tools use (LeafToolDeps.
// Root). The caller builds the session from wiring.cfg and then calls
// wiring.spawner.bind(session) once.
func buildOrchestratorWiring(client llm.LLM, factory ModelFactory, root string, cfg Config) (orchestratorWiring, error) {
	deps := LeafToolDeps{Root: root, HTTPCl: newHTTPClient()}
	registry, loader, err := leafRegistry(deps, cfg)
	if err != nil {
		return orchestratorWiring{}, err
	}
	spawner := newSwarmSpawner(registry, deps, client, factory, loader)
	orchCfg := orchestratorConfig(client, factory, root, spawner, toolCatalog(registry))
	return orchestratorWiring{cfg: orchCfg, spawner: spawner}, nil
}

// New constructs the SWE-Swarm and returns it as a tui.Agent driven by the
// orchestrator running as the PRIMARY loop. It reads LLM_API_KEY (the only
// env-sourced value; fail-loud via *MissingEnvError if a required key is missing),
// builds the shared provider client + ModelFactory, resolves the workspace root,
// and starts the orchestrator's session under the spawn caps. cfg carries the
// human-set construction modes (RuntimeSkills) — the model never sets them. The
// session runs under an agent-owned root context, so ctx bounds only construction —
// Close, not ctx, controls the session's lifetime. The caller owns the agent and must
// Close it.
//
// The orchestrator's toolset is read/search + Todo + AskUser + the agent-aware Subagent,
// so the orchestrator can spawn the leaf registry's agents by name; a spawned leaf has no
// Subagent tool (least privilege — only the primary holds the spawn capability).
func New(ctx context.Context, cfg Config) (tui.Agent, error) {
	client, factory, err := buildClient()
	if err != nil {
		return nil, err
	}
	return newWithClient(ctx, client, factory, cfg)
}

// newWithClient is the construction seam shared by New and tests; tests inject a
// fake llm.LLM + a key-bound ModelFactory here, avoiding real environment reads and
// network calls. It resolves the workspace root (fail-fast on os.Getwd error), builds
// the orchestrator wiring (leaf registry + unbound spawner + primary cfg with Subagent
// wired) under cfg (the human-set modes), starts the session under the spawn caps via
// newSessionAgent (which owns the agent-rooted lifetime), then binds the live session
// onto the spawner BEFORE returning (no turn can run before bind, so the Subagent tool
// always sees a live session). ctx bounds only this construction call.
func newWithClient(ctx context.Context, client llm.LLM, factory ModelFactory, cfg Config) (*sessionAgent, error) {
	// The workspace root is the process working directory: file tools are confined
	// to it and the PermissionChecker uses it for containment + path relativisation.
	root, err := os.Getwd()
	if err != nil {
		return nil, &WorkspaceRootError{Cause: err}
	}

	wiring, err := buildOrchestratorWiring(client, factory, root, cfg)
	if err != nil {
		return nil, err
	}
	agent, err := newSessionAgent(ctx, wiring.cfg,
		session.WithLimits(orchestratorLimits()),
		session.WithConfigFingerprintFields(orchestratorFingerprintFields(root, cfg)),
	)
	if err != nil {
		return nil, err
	}
	wiring.spawner.bind(agent.session) // late-bind before any turn runs
	return agent, nil
}
