package swe

import (
	"net/http"
	"sort"
	"testing"

	"github.com/inventivepotter/urvi/agents/explorer"
	"github.com/inventivepotter/urvi/agents/operator"
	"github.com/inventivepotter/urvi/agents/orchestrator"
	"github.com/inventivepotter/urvi/agents/researcher"
	"github.com/inventivepotter/urvi/agents/reviewer"
	"github.com/inventivepotter/urvi/internal/agent/loop/identity"
)

// testLeafDeps is a minimal LeafToolDeps for registry-shape tests: a throwaway
// root and a fresh http.Client. The tools are never invoked, only built.
func testLeafDeps() LeafToolDeps {
	return LeafToolDeps{Root: "/tmp/workspace-root", HTTPCl: &http.Client{}}
}

// equalStringSlice reports element-wise equality, treating nil and empty as equal
// (a skill-less agent's Skills is nil; the expectation is also nil).
func equalStringSlice(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestLeafRegistryHasExactlyTheFourLeaves proves leafRegistry registers EXACTLY
// the four spawnable leaf agents — operator, researcher, explorer, reviewer — in
// that order, and that the orchestrator is deliberately absent (it is the primary,
// not a spawnable leaf).
func TestLeafRegistryHasExactlyTheFourLeaves(t *testing.T) {
	t.Parallel()

	reg, _, err := leafRegistry(testLeafDeps(), Config{})
	if err != nil {
		t.Fatalf("leafRegistry() error = %v", err)
	}

	catalog := reg.Catalog()
	got := make([]identity.AgentName, 0, len(catalog))
	for _, e := range catalog {
		got = append(got, e.Name)
	}
	want := []identity.AgentName{operator.Name, researcher.Name, explorer.Name, reviewer.Name}
	if len(got) != len(want) {
		t.Fatalf("Catalog() names = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Catalog() names = %v, want %v (order matters)", got, want)
		}
	}
}

// TestLeafRegistryOrchestratorAbsent proves the orchestrator is NOT in the leaf
// registry — it is the primary loop, never spawned as a leaf.
func TestLeafRegistryOrchestratorAbsent(t *testing.T) {
	t.Parallel()

	reg, _, err := leafRegistry(testLeafDeps(), Config{})
	if err != nil {
		t.Fatalf("leafRegistry() error = %v", err)
	}
	if _, ok := reg.Lookup(orchestrator.Name); ok {
		t.Errorf("Lookup(%q) = found, want absent (orchestrator is the primary, not a leaf)", orchestrator.Name)
	}
}

// TestLeafRegistryLookupCarriesLeafData proves each registered leaf carries its
// own package's Name/Description/Role verbatim and a non-nil BuildTools that
// produces a tool set with a non-nil PermissionChecker.
func TestLeafRegistryLookupCarriesLeafData(t *testing.T) {
	t.Parallel()

	deps := testLeafDeps()
	reg, _, err := leafRegistry(deps, Config{})
	if err != nil {
		t.Fatalf("leafRegistry() error = %v", err)
	}

	tests := []struct {
		name              string
		agent             identity.AgentName
		wantDesc          string
		wantRole          string
		wantTools         []string
		wantSkills        []string // the agent's allowed-skill names (nil = none)
		wantRuntimeSkills bool     // §7a eligibility — true for the read-only explorer + researcher
	}{
		{
			name:       "operator",
			agent:      operator.Name,
			wantDesc:   operator.Description,
			wantRole:   operator.Role,
			wantTools:  []string{"AskUser", "Bash", "EditFile", "Glob", "Grep", "ReadFile", "Skill", "Todo", "WriteFile"},
			wantSkills: []string{"code-style"},
		},
		{
			name:              "researcher",
			agent:             researcher.Name,
			wantDesc:          researcher.Description,
			wantRole:          researcher.Role,
			wantTools:         []string{"AskUser", "Fetch", "Glob", "Grep", "ReadFile", "WebSearch"},
			wantRuntimeSkills: true,
		},
		{
			name:              "explorer",
			agent:             explorer.Name,
			wantDesc:          explorer.Description,
			wantRole:          explorer.Role,
			wantTools:         []string{"AskUser", "Glob", "Grep", "ReadFile"},
			wantRuntimeSkills: true,
		},
		{
			name:      "reviewer",
			agent:     reviewer.Name,
			wantDesc:  reviewer.Description,
			wantRole:  reviewer.Role,
			wantTools: []string{"AskUser", "Bash", "Glob", "Grep", "ReadFile", "Todo"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			a, ok := reg.Lookup(tt.agent)
			if !ok {
				t.Fatalf("Lookup(%q) not found", tt.agent)
			}
			if a.Description != tt.wantDesc {
				t.Errorf("Description = %q, want %q", a.Description, tt.wantDesc)
			}
			if a.Role != tt.wantRole {
				t.Errorf("Role = %q, want %q", a.Role, tt.wantRole)
			}
			if a.AllowsRuntimeSkills != tt.wantRuntimeSkills {
				t.Errorf("AllowsRuntimeSkills = %v, want %v (§7a: read-only agents only)", a.AllowsRuntimeSkills, tt.wantRuntimeSkills)
			}
			if !equalStringSlice(a.Skills, tt.wantSkills) {
				t.Errorf("Skills = %v, want %v", a.Skills, tt.wantSkills)
			}
			if a.BuildTools == nil {
				t.Fatal("BuildTools = nil, want non-nil")
			}
			ts := a.BuildTools(deps)
			if ts.Permission == nil {
				t.Error("BuildTools().Permission = nil, want non-nil PermissionChecker")
			}
			got := make([]string, 0, len(ts.Registry))
			for _, tl := range ts.Registry {
				info, err := tl.Info(t.Context())
				if err != nil {
					t.Fatalf("Info() error = %v", err)
				}
				got = append(got, info.Name)
			}
			sort.Strings(got)
			if len(got) != len(tt.wantTools) {
				t.Fatalf("tool names = %v, want %v", got, tt.wantTools)
			}
			for i := range tt.wantTools {
				if got[i] != tt.wantTools[i] {
					t.Fatalf("tool names = %v, want %v", got, tt.wantTools)
				}
			}
		})
	}
}
