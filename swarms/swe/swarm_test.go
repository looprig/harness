package swe

import (
	"context"
	"sort"
	"strings"
	"testing"

	"github.com/inventivepotter/urvi/agents/orchestrator"
	"github.com/inventivepotter/urvi/internal/agent/loop"
	"github.com/inventivepotter/urvi/tui"
)

// TestNewWithClientHappy proves swe.New (via the fake-client seam) builds a usable
// tui.Agent that is releasable via Close.
func TestNewWithClientHappy(t *testing.T) {
	t.Parallel()

	agent, err := newWithClient(context.Background(), &fakeLLM{}, newModelFactory("test-key"))
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

	cfg := orchestratorConfig(&fakeLLM{}, newModelFactory("test-key"), "/tmp/workspace-root")

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

// TestOrchestratorToolSetIsExactlyTheFive proves the orchestrator's toolset in 4A
// is EXACTLY ReadFile, Glob, Grep, Todo, AskUser — and that Subagent is deliberately
// ABSENT (it is wired in 4B after the tool-arg flip).
func TestOrchestratorToolSetIsExactlyTheFive(t *testing.T) {
	t.Parallel()

	ts := orchestratorToolSet("/tmp/workspace-root")
	if ts.Permission == nil {
		t.Fatal("orchestratorToolSet() Permission = nil, want non-nil PermissionChecker")
	}

	want := []string{"AskUser", "Glob", "Grep", "ReadFile", "Todo"}
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
	for _, n := range got {
		if n == "Subagent" {
			t.Errorf("Subagent is wired, want absent in 4A (added in 4B)")
		}
	}
}

// TestOrchestratorAutoApproveSetIsTheFive proves the orchestrator's hard-approve
// set is exactly the five toolset members — every tool the orchestrator has is
// auto-approved (it only reads/searches/plans/asks; never mutates or networks) and
// Subagent is absent.
func TestOrchestratorAutoApproveSetIsTheFive(t *testing.T) {
	t.Parallel()

	want := []string{"AskUser", "Glob", "Grep", "ReadFile", "Todo"}
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
	for _, n := range got {
		if n == "Subagent" {
			t.Errorf("Subagent is auto-approved, want absent in 4A (added in 4B)")
		}
	}
}

// TestOrchestratorToolSetAllToolsAutoApproved proves the PermissionChecker
// auto-approves every tool the orchestrator carries with a valid in-workspace call
// — none gate. This is the 4A contract: a read/search/plan/ask-only orchestrator
// never prompts. Each tool is driven with args that clear the Stage-1 containment
// boundary (an in-root path), so the assertion exercises the HardApprove stage, not
// a malformed-args fail-secure ask.
func TestOrchestratorToolSetAllToolsAutoApproved(t *testing.T) {
	t.Parallel()

	// A real, existing root so the Stage-1 containment check (which EvalSymlinks the
	// root) clears for the read/search tools.
	root := t.TempDir()
	// Per-tool valid args: the read/search tools carry an in-root path/root field so
	// containment clears; Todo/AskUser are path-free.
	args := map[string]string{
		"ReadFile": `{"path":"file.txt"}`,
		"Glob":     `{"pattern":"*.go","root":"."}`,
		"Grep":     `{"pattern":"foo","path":"."}`,
		"Todo":     `{}`,
		"AskUser":  `{}`,
	}

	ts := orchestratorToolSet(root)
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
