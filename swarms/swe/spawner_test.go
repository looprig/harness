package swe

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/inventivepotter/urvi/agents/operator"
	"github.com/inventivepotter/urvi/internal/agent/loop"
	"github.com/inventivepotter/urvi/internal/agent/loop/identity"
	"github.com/inventivepotter/urvi/internal/content"
)

// spawner_test.go exercises swarmSpawner against a FAKE subagentRunner (DIP: the
// spawner never touches the real session.Session). The fake captures the cfg + parent
// + blocks RunSubagent is invoked with, so the tests assert the agent-resolution +
// fresh-cfg-assembly invariants:
//
//   - a permitted agent resolves to a FRESH loop.Config whose AgentName is the leaf's
//     name, whose system prompt is the swarm Identity + the leaf's Role, and whose
//     toolset is the leaf's OWN allowlist (NOT the orchestrator's, and with no Subagent
//     tool — a leaf cannot spawn);
//   - an unknown agent fails closed with a *UnknownAgentError (errors.As-recoverable),
//     and RunSubagent is never reached.

// fakeRunner is a fake subagentRunner. It records the parent, cfg, and blocks it was
// asked to run and returns reply (or runErr). called reports whether RunSubagent ran.
type fakeRunner struct {
	reply  string
	runErr error

	called    bool
	gotParent loop.Provenance
	gotCfg    loop.Config
	gotBlocks []content.Block
}

func (f *fakeRunner) RunSubagent(_ context.Context, parent loop.Provenance, cfg loop.Config, blocks []content.Block) (string, error) {
	f.called = true
	f.gotParent = parent
	f.gotCfg = cfg
	f.gotBlocks = blocks
	if f.runErr != nil {
		return "", f.runErr
	}
	return f.reply, nil
}

// newTestSwarmSpawner builds a swarmSpawner over the real leaf registry and a fake
// runner late-bound as its session — mirroring the production wiring (build spawner,
// then bind the live session). The fake runner is returned so a test can inspect the
// captured cfg after Spawn.
func newTestSwarmSpawner(t *testing.T) (*swarmSpawner, *fakeRunner) {
	t.Helper()
	deps := LeafToolDeps{Root: "/tmp/workspace-root", HTTPCl: &http.Client{}}
	reg, err := leafRegistry(deps)
	if err != nil {
		t.Fatalf("leafRegistry() error = %v", err)
	}
	sp := newSwarmSpawner(reg, deps, &fakeLLM{}, newModelFactory("test-key"))
	runner := &fakeRunner{reply: "subagent done"}
	sp.session = runner // late-bind a fake, exactly where bind sets the live session
	return sp, runner
}

// toolNames flattens a toolset's tool names so a test can assert which tools a leaf
// got. It fails the test on an Info error.
func toolNames(t *testing.T, ts loop.ToolSet) []string {
	t.Helper()
	out := make([]string, 0, len(ts.Registry))
	for _, tl := range ts.Registry {
		info, err := tl.Info(context.Background())
		if err != nil {
			t.Fatalf("Info() error = %v", err)
		}
		out = append(out, info.Name)
	}
	return out
}

// TestSpawnResolvesPermittedAgent proves Spawn resolves each registered leaf to a
// FRESH loop.Config: AgentName == the leaf's name, system prompt == Identity + Role,
// the message is delivered as a single TextBlock, the parent provenance is forwarded,
// and the toolset is the leaf's OWN allowlist (no Subagent — a leaf cannot spawn).
func TestSpawnResolvesPermittedAgent(t *testing.T) {
	t.Parallel()

	parent := loop.Provenance{LoopID: mustUUID(t), TurnID: mustUUID(t), StepID: mustUUID(t)}

	tests := []struct {
		name  identity.AgentName
		role  string
		tools []string // a subset the leaf's toolset MUST contain
	}{
		{name: operator.Name, role: operator.Role, tools: []string{"Bash", "WriteFile"}},
		{name: "researcher", tools: []string{"WebSearch", "Fetch"}},
		{name: "explorer", tools: []string{"Glob", "Grep"}},
		{name: "reviewer", tools: []string{"ReadFile"}},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(string(tt.name), func(t *testing.T) {
			t.Parallel()
			sp, runner := newTestSwarmSpawner(t)

			got, err := sp.Spawn(context.Background(), parent, tt.name, "do the thing")
			if err != nil {
				t.Fatalf("Spawn(%q) error = %v", tt.name, err)
			}
			if got != "subagent done" {
				t.Errorf("Spawn(%q) = %q, want %q", tt.name, got, "subagent done")
			}
			if !runner.called {
				t.Fatal("RunSubagent was never called")
			}
			if runner.gotParent != parent {
				t.Errorf("RunSubagent parent = %+v, want %+v", runner.gotParent, parent)
			}
			if runner.gotCfg.AgentName != tt.name {
				t.Errorf("cfg.AgentName = %q, want %q", runner.gotCfg.AgentName, tt.name)
			}
			if !strings.HasPrefix(runner.gotCfg.Model.System, Identity) {
				t.Errorf("cfg.Model.System = %q, want it to begin with the swarm Identity", runner.gotCfg.Model.System)
			}
			if tt.role != "" && !strings.Contains(runner.gotCfg.Model.System, tt.role) {
				t.Errorf("cfg.Model.System = %q, want it to contain the leaf Role", runner.gotCfg.Model.System)
			}
			if runner.gotCfg.Client == nil {
				t.Error("cfg.Client = nil, want the shared client")
			}

			// The message is delivered as a single TextBlock carrying the task.
			if len(runner.gotBlocks) != 1 {
				t.Fatalf("RunSubagent blocks len = %d, want 1", len(runner.gotBlocks))
			}
			tb, ok := runner.gotBlocks[0].(*content.TextBlock)
			if !ok {
				t.Fatalf("RunSubagent block[0] type = %T, want *content.TextBlock", runner.gotBlocks[0])
			}
			if tb.Text != "do the thing" {
				t.Errorf("RunSubagent block text = %q, want %q", tb.Text, "do the thing")
			}

			// The leaf gets its OWN toolset (contains the expected tools) and crucially
			// NO Subagent (least privilege — only the primary orchestrator may spawn).
			names := toolNames(t, runner.gotCfg.Tools)
			for _, want := range tt.tools {
				if !containsName(names, want) {
					t.Errorf("leaf %q toolset = %v, want it to contain %q", tt.name, names, want)
				}
			}
			if containsName(names, "Subagent") {
				t.Errorf("leaf %q toolset = %v, must NOT contain Subagent (a leaf cannot spawn)", tt.name, names)
			}
		})
	}
}

// TestSpawnFreshToolSetPerCall proves two Spawns of the same agent build INDEPENDENT
// PermissionCheckers, so a sub-loop's session-scope approval state can never leak into
// a sibling's — the per-loop approval-isolation guarantee.
func TestSpawnFreshToolSetPerCall(t *testing.T) {
	t.Parallel()
	parent := loop.Provenance{LoopID: mustUUID(t)}

	sp, first := newTestSwarmSpawner(t)
	if _, err := sp.Spawn(context.Background(), parent, operator.Name, "a"); err != nil {
		t.Fatalf("Spawn #1 error = %v", err)
	}
	firstChecker := first.gotCfg.Tools.Permission

	// Re-bind a fresh runner so the second call's cfg is captured independently.
	second := &fakeRunner{reply: "x"}
	sp.session = second
	if _, err := sp.Spawn(context.Background(), parent, operator.Name, "b"); err != nil {
		t.Fatalf("Spawn #2 error = %v", err)
	}
	secondChecker := second.gotCfg.Tools.Permission

	if firstChecker == nil || secondChecker == nil {
		t.Fatal("Spawn built a nil PermissionChecker")
	}
	if firstChecker == secondChecker {
		t.Error("Spawn reused the SAME PermissionChecker across calls; want a fresh one per call (per-loop approval isolation)")
	}
}

// TestSpawnUnknownAgent proves an unknown agent name fails closed with a typed
// *UnknownAgentError (errors.As-recoverable, naming the bad agent) and that
// RunSubagent is NEVER reached — an unrecognized name never silently runs anything.
func TestSpawnUnknownAgent(t *testing.T) {
	t.Parallel()
	parent := loop.Provenance{LoopID: mustUUID(t)}

	tests := []struct {
		name  string
		agent identity.AgentName
	}{
		{name: "nonexistent agent", agent: "nope"},
		{name: "empty agent", agent: ""},
		{name: "the orchestrator is not a spawnable leaf", agent: "orchestrator"},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			sp, runner := newTestSwarmSpawner(t)

			_, err := sp.Spawn(context.Background(), parent, tt.agent, "do it")
			if err == nil {
				t.Fatalf("Spawn(%q) error = nil, want *UnknownAgentError", tt.agent)
			}
			var ua *UnknownAgentError
			if !errors.As(err, &ua) {
				t.Fatalf("Spawn(%q) error = %T (%v), want *UnknownAgentError", tt.agent, err, err)
			}
			if ua.Name != tt.agent {
				t.Errorf("UnknownAgentError.Name = %q, want %q", ua.Name, tt.agent)
			}
			if runner.called {
				t.Error("RunSubagent was called for an unknown agent (want fail-closed, no spawn)")
			}
		})
	}
}

// containsName reports whether names contains want.
func containsName(names []string, want string) bool {
	for _, n := range names {
		if n == want {
			return true
		}
	}
	return false
}
