package swe

import (
	"context"
	"net/http"
	"sort"
	"strings"
	"testing"

	"github.com/ciram-co/looprig/agents/orchestrator"
	"github.com/ciram-co/looprig/pkg/loop"
	"github.com/ciram-co/looprig/pkg/tools"
	"github.com/ciram-co/looprig/pkg/tui"
)

// testWiring builds the orchestrator's spawner + Subagent catalog from the real leaf
// registry for the toolset/config tests. The spawner is UNBOUND (its session is nil);
// these tests only assemble + inspect the cfg/toolset and never run a turn, so no bind
// is needed.
func testWiring(t *testing.T) (*swarmSpawner, []tools.SubagentCatalogEntry) {
	t.Helper()
	deps := LeafToolDeps{Root: "/tmp/workspace-root", HTTPCl: &http.Client{}}
	reg, loader, err := leafRegistry(deps, Config{})
	if err != nil {
		t.Fatalf("leafRegistry() error = %v", err)
	}
	return newSwarmSpawner(reg, deps, &fakeLLM{}, newModelFactory("test-key"), loader, NewRuntimeContextProvider()), toolCatalog(reg)
}

// TestNewWithClientHappy proves swe.New (via the fake-client seam) builds a usable
// tui.Agent that is releasable via Close.
func TestNewWithClientHappy(t *testing.T) {
	t.Parallel()

	agent, err := newWithClient(context.Background(), &fakeLLM{}, newModelFactory("test-key"), Config{})
	if err != nil {
		t.Fatalf("newWithClient() error = %v", err)
	}
	if agent == nil {
		t.Fatal("newWithClient() returned nil agent")
	}
	t.Cleanup(func() { _ = agent.Close(context.Background()) })

	// The returned agent must satisfy the TUI surface.
	var _ tui.Agent = agent
}

// TestOrchestratorConfigIsPrimaryWithIdentityAndRole proves the orchestrator
// config: its AgentName is the orchestrator's name (so it runs AS the primary),
// and its system prompt is the shared Identity followed by the orchestrator's Role
// (the swarm prepends identity to each agent's role).
func TestOrchestratorConfigIsPrimaryWithIdentityAndRole(t *testing.T) {
	t.Parallel()

	spawner, catalog := testWiring(t)
	cfg := orchestratorConfig(&fakeLLM{}, newModelFactory("test-key"), "/tmp/workspace-root", spawner, catalog, NewRuntimeContextProvider())

	if cfg.AgentName != orchestrator.Name {
		t.Errorf("cfg.AgentName = %q, want %q", cfg.AgentName, orchestrator.Name)
	}
	if cfg.Client == nil {
		t.Error("cfg.Client = nil, want the supplied client")
	}
	want := Identity + orchestrator.Role
	if cfg.Model.System != want {
		t.Errorf("cfg.Model.System = %q, want Identity+orchestrator.Role", cfg.Model.System)
	}
	if !strings.Contains(cfg.Model.System, "<identity product=\"SWE\">") {
		t.Error("system prompt missing the shared identity block")
	}
	if !strings.Contains(cfg.Model.System, "<role name=\"orchestrator\">") {
		t.Error("system prompt missing the orchestrator role block")
	}
}

// TestOrchestratorConfigCarriesRuntimeContext proves the orchestrator's primary
// loop.Config has a non-nil RuntimeContext when one is wired (so the loop injects the
// volatile date/cwd/git tail every turn), and that a nil provider leaves it OFF.
func TestOrchestratorConfigCarriesRuntimeContext(t *testing.T) {
	t.Parallel()

	t.Run("provider wired -> non-nil RuntimeContext", func(t *testing.T) {
		t.Parallel()
		spawner, catalog := testWiring(t)
		rc := NewRuntimeContextProvider()
		cfg := orchestratorConfig(&fakeLLM{}, newModelFactory("test-key"), "/tmp/workspace-root", spawner, catalog, rc)
		if cfg.RuntimeContext == nil {
			t.Error("orchestrator cfg.RuntimeContext = nil, want the wired provider")
		}
	})

	t.Run("nil provider -> RuntimeContext stays nil (OFF)", func(t *testing.T) {
		t.Parallel()
		spawner, catalog := testWiring(t)
		cfg := orchestratorConfig(&fakeLLM{}, newModelFactory("test-key"), "/tmp/workspace-root", spawner, catalog, nil)
		if cfg.RuntimeContext != nil {
			t.Error("orchestrator cfg.RuntimeContext != nil with a nil provider, want OFF")
		}
	})
}

// TestBuildOrchestratorWiringEnablesRuntimeContext proves the SHARED construction
// seam (used by New, openNew, openResume) wires a non-nil RuntimeContext onto the
// orchestrator's primary cfg, so every construction path inherits runtime-context
// injection.
func TestBuildOrchestratorWiringEnablesRuntimeContext(t *testing.T) {
	t.Parallel()
	wiring, err := buildOrchestratorWiring(&fakeLLM{}, newModelFactory("test-key"), "/tmp/workspace-root", Config{})
	if err != nil {
		t.Fatalf("buildOrchestratorWiring() error = %v", err)
	}
	if wiring.cfg.RuntimeContext == nil {
		t.Error("wiring.cfg.RuntimeContext = nil, want runtime context enabled for the orchestrator")
	}
}

// TestOrchestratorToolSetIsExactlyTheSix proves the orchestrator's toolset is EXACTLY
// ReadFile, Glob, Grep, Todo, AskUser, Subagent — and in particular that the agent-aware
// Subagent IS now wired (the orchestrator can spawn leaf agents by name).
func TestOrchestratorToolSetIsExactlyTheSix(t *testing.T) {
	t.Parallel()

	spawner, catalog := testWiring(t)
	ts := orchestratorToolSet("/tmp/workspace-root", spawner, catalog)
	if ts.Permission == nil {
		t.Fatal("orchestratorToolSet() Permission = nil, want non-nil PermissionChecker")
	}

	want := []string{"AskUser", "Glob", "Grep", "ReadFile", "Subagent", "Todo"}
	got := make([]string, 0, len(ts.Registry))
	for _, tl := range ts.Registry {
		info, err := tl.Info(t.Context())
		if err != nil {
			t.Fatalf("Info() error = %v", err)
		}
		got = append(got, info.Name)
	}
	sort.Strings(got)

	if len(got) != len(want) {
		t.Fatalf("orchestrator tool names = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("orchestrator tool names = %v, want %v", got, want)
		}
	}

	var hasSubagent bool
	for _, n := range got {
		if n == "Subagent" {
			hasSubagent = true
		}
	}
	if !hasSubagent {
		t.Error("Subagent is absent, want it wired (the orchestrator must be able to spawn leaves)")
	}
}

// TestOrchestratorAutoApproveSetIsTheSix proves the orchestrator's hard-approve set is
// exactly the six toolset members — every tool the orchestrator has is auto-approved
// (it reads/searches/plans/asks/spawns; never directly mutates or networks) and Subagent
// IS in the set (it has no path/command boundary, so it auto-approves only by being
// named here).
func TestOrchestratorAutoApproveSetIsTheSix(t *testing.T) {
	t.Parallel()

	want := []string{"AskUser", "Glob", "Grep", "ReadFile", "Subagent", "Todo"}
	got := append([]string(nil), orchestratorAutoApprovedTools...)
	sort.Strings(got)
	if len(got) != len(want) {
		t.Fatalf("orchestratorAutoApprovedTools = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("orchestratorAutoApprovedTools = %v, want %v", got, want)
		}
	}

	var hasSubagent bool
	for _, n := range got {
		if n == "Subagent" {
			hasSubagent = true
		}
	}
	if !hasSubagent {
		t.Error("Subagent is not auto-approved, want it in the hard-approve set")
	}
}

// TestOrchestratorToolSetAllToolsAutoApproved proves the PermissionChecker
// auto-approves every tool the orchestrator carries with a valid in-workspace call
// — none gate. This is the contract: a read/search/plan/ask/spawn orchestrator never
// prompts. Each tool is driven with args that clear the Stage-1 containment boundary
// (an in-root path); Subagent has no path boundary and reaches AutoApprove via the
// hard-approve list — so the assertion exercises the HardApprove stage, not a
// malformed-args fail-secure ask.
func TestOrchestratorToolSetAllToolsAutoApproved(t *testing.T) {
	t.Parallel()

	// A real, existing root so the Stage-1 containment check (which EvalSymlinks the
	// root) clears for the read/search tools.
	root := t.TempDir()
	// Per-tool valid args: the read/search tools carry an in-root path/root field so
	// containment clears; Todo/AskUser are path-free; Subagent carries a name+message.
	args := map[string]string{
		"ReadFile": `{"path":"file.txt"}`,
		"Glob":     `{"pattern":"*.go","root":"."}`,
		"Grep":     `{"pattern":"foo","path":"."}`,
		"Todo":     `{}`,
		"AskUser":  `{}`,
		"Subagent": `{"agent":"operator","message":"do it"}`,
	}

	spawner, catalog := testWiring(t)
	ts := orchestratorToolSet(root, spawner, catalog)
	for _, tl := range ts.Registry {
		info, err := tl.Info(t.Context())
		if err != nil {
			t.Fatalf("Info() error = %v", err)
		}
		callArgs, ok := args[info.Name]
		if !ok {
			t.Fatalf("no test args for tool %q", info.Name)
		}
		eff := ts.Permission.Check(t.Context(), tl, info.Name, callArgs)
		if eff != loop.EffectAutoApprove {
			t.Errorf("Check(%q, %s) = %v, want EffectAutoApprove (auto-approved)", info.Name, callArgs, eff)
		}
	}
}
