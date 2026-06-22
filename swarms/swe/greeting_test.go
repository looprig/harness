package swe

import (
	"strings"
	"testing"

	"github.com/ciram-co/looprig/agents/orchestrator"
	"github.com/ciram-co/looprig/pkg/identity"
)

// TestBuildGreeting pins the deterministic, LLM-free greeting builder: it is derived
// purely from the agent catalog (+ embedded skill names), lists every agent in catalog
// order, lists skills when present, and yields the empty string for an empty catalog.
func TestBuildGreeting(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		catalog     []AgentCatalogEntry
		skills      []string
		wantEmpty   bool
		wantAgents  []identity.AgentName // every name must appear, in this order
		wantSkills  []string             // every skill must appear, in this order
		wantNoSkill bool                 // assert no "Skills:" section
	}{
		{
			name:      "empty catalog yields empty greeting",
			catalog:   nil,
			wantEmpty: true,
		},
		{
			name: "single agent, no skills",
			catalog: []AgentCatalogEntry{
				{Name: "operator", Description: "edits code"},
			},
			wantAgents:  []identity.AgentName{"operator"},
			wantNoSkill: true,
		},
		{
			name: "agents render in catalog order",
			catalog: []AgentCatalogEntry{
				{Name: "orchestrator", Description: "delegates"},
				{Name: "researcher", Description: "reads the web"},
				{Name: "explorer", Description: "reads the repo"},
			},
			wantAgents:  []identity.AgentName{"orchestrator", "researcher", "explorer"},
			wantNoSkill: true,
		},
		{
			name: "skills section listed when present",
			catalog: []AgentCatalogEntry{
				{Name: "operator", Description: "edits code"},
			},
			skills:     []string{"code-style"},
			wantAgents: []identity.AgentName{"operator"},
			wantSkills: []string{"code-style"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := buildGreeting(tt.catalog, tt.skills)

			if tt.wantEmpty {
				if got != "" {
					t.Fatalf("buildGreeting() = %q, want empty", got)
				}
				return
			}
			if got == "" {
				t.Fatalf("buildGreeting() = empty, want non-empty greeting")
			}

			// Every agent name + description must appear.
			lastIdx := -1
			for _, name := range tt.wantAgents {
				idx := strings.Index(got, string(name))
				if idx < 0 {
					t.Errorf("greeting missing agent %q:\n%s", name, got)
				}
				if idx <= lastIdx {
					t.Errorf("agent %q out of catalog order in greeting:\n%s", name, got)
				}
				lastIdx = idx
			}
			for _, e := range tt.catalog {
				if e.Description != "" && !strings.Contains(got, e.Description) {
					t.Errorf("greeting missing description %q for %q:\n%s", e.Description, e.Name, got)
				}
			}

			for _, s := range tt.wantSkills {
				if !strings.Contains(got, s) {
					t.Errorf("greeting missing skill %q:\n%s", s, got)
				}
			}
			if tt.wantNoSkill && strings.Contains(got, "Skills:") {
				t.Errorf("greeting has a Skills: section but none expected:\n%s", got)
			}
		})
	}
}

// TestBuildGreetingDeterministic proves the builder is a pure function of its inputs:
// repeated calls with the same catalog produce byte-identical output (no map iteration,
// no clock, no randomness — and no LLM call).
func TestBuildGreetingDeterministic(t *testing.T) {
	t.Parallel()

	catalog := []AgentCatalogEntry{
		{Name: "orchestrator", Description: "delegates"},
		{Name: "operator", Description: "edits code"},
		{Name: "researcher", Description: "reads the web"},
	}
	skills := []string{"code-style"}

	first := buildGreeting(catalog, skills)
	for i := 0; i < 16; i++ {
		if got := buildGreeting(catalog, skills); got != first {
			t.Fatalf("buildGreeting() non-deterministic on call %d:\nfirst=%q\ngot  =%q", i, first, got)
		}
	}
}

// TestGreetingFromRegistry pins the registry-sourced greeting: when on, it lists the
// real wired leaves derived from the live registry catalog; when off (toggle false) it
// is empty. It also asserts the skill source is the registry's embedded skill names, so
// the greeting can never drift from the actual wiring.
func TestGreetingFromRegistry(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		enabled   bool
		wantEmpty bool
	}{
		{name: "toggle off yields empty greeting", enabled: false, wantEmpty: true},
		{name: "toggle on yields registry-derived greeting", enabled: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := Greeting(Config{Greeting: tt.enabled})

			if tt.wantEmpty {
				if got != "" {
					t.Fatalf("Greeting(off) = %q, want empty", got)
				}
				return
			}
			if got == "" {
				t.Fatal("Greeting(on) = empty, want a greeting derived from the registry")
			}
			// Every wired leaf must be named (derived from the live registry).
			for _, b := range leafBuiltins() {
				if !strings.Contains(got, string(b.name)) {
					t.Errorf("greeting missing wired agent %q:\n%s", b.name, got)
				}
			}
			// The operator's embedded skill is listed (skills are sourced from the registry).
			for _, s := range operatorSkills {
				if !strings.Contains(got, s) {
					t.Errorf("greeting missing embedded skill %q:\n%s", s, got)
				}
			}
		})
	}
}

// TestGreetingNotInModelContext is the "not in model context" gate at the config-assembly
// seam (§5a): even with the greeting toggle ON, the orchestrator's assembled system prompt
// is EXACTLY Identity+Role and contains NONE of the greeting text — the greeting flows only
// to the TUI banner, never into any loop.Config.Model.System. This holds because the
// greeting and the system prompt are built on entirely separate paths; the assertion makes
// that structural fact a regression test.
func TestGreetingNotInModelContext(t *testing.T) {
	t.Parallel()

	spawner, catalog := testWiring(t)
	cfg := orchestratorConfig(&fakeLLM{}, newModelFactory("test-key"), "/tmp/workspace-root", spawner, catalog, NewRuntimeContextProvider())

	if want := Identity + orchestrator.Role; cfg.Model.System != want {
		t.Fatalf("orchestrator system prompt drifted from Identity+Role; greeting must not touch it")
	}
	greeting := Greeting(Config{Greeting: true})
	if greeting == "" {
		t.Fatal("Greeting(on) empty; cannot assert it is absent from the model context")
	}
	if strings.Contains(cfg.Model.System, greetingLead) {
		t.Errorf("greeting lead leaked into the orchestrator system prompt (model context):\n%s", cfg.Model.System)
	}
	// No agent's catalog line from the greeting body may appear in the system prompt.
	for _, b := range leafBuiltins() {
		if b.description != "" && strings.Contains(cfg.Model.System, "  - "+string(b.name)+": "+b.description) {
			t.Errorf("greeting agent line for %q leaked into the model context", b.name)
		}
	}
}

// TestGreetingNeedsNoSecrets proves the greeting path is LLM-free and side-effect-free:
// it builds the full greeting with NO LLM_API_KEY set, so it cannot be touching
// buildClient / the provider / the model. (buildClient fails loud on a missing key; if
// Greeting reached it, this would panic or return empty.) It is the lifecycle-neutral
// proof at the swarm seam: no session, no client, no model call.
func TestGreetingNeedsNoSecrets(t *testing.T) {
	// Not parallel: it mutates the process environment (LLM_API_KEY).
	t.Setenv("LLM_API_KEY", "")

	got := Greeting(Config{Greeting: true})
	if got == "" {
		t.Fatal("Greeting(on) = empty with no API key; it must build deterministically without any provider/model")
	}
	for _, b := range leafBuiltins() {
		if !strings.Contains(got, string(b.name)) {
			t.Errorf("greeting missing wired agent %q:\n%s", b.name, got)
		}
	}
}
