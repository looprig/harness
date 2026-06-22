package swe

import (
	"errors"
	"net/http"
	"testing"

	"github.com/ciram-co/looprig/pkg/identity"
	"github.com/ciram-co/looprig/pkg/llm"
	"github.com/ciram-co/looprig/pkg/loop"
)

// stubAgent builds a minimal Agent with a no-op BuildTools so the table rows are terse.
func stubAgent(name identity.AgentName, desc string) Agent {
	return Agent{
		Name:        name,
		Description: desc,
		Role:        "role for " + string(name),
		BuildTools:  func(LeafToolDeps) loop.ToolSet { return loop.ToolSet{} },
	}
}

func TestNewRegistry(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		agents    []Agent
		wantErr   bool
		wantDup   identity.AgentName // when wantErr, the name the DuplicateAgentError should carry
		wantOrder []identity.AgentName
	}{
		{
			name:      "empty registry is valid",
			agents:    nil,
			wantOrder: nil,
		},
		{
			name:      "single agent",
			agents:    []Agent{stubAgent("operator", "the boss")},
			wantOrder: []identity.AgentName{"operator"},
		},
		{
			name: "insertion order preserved",
			agents: []Agent{
				stubAgent("operator", "o"),
				stubAgent("coding", "c"),
				stubAgent("reviewer", "r"),
			},
			wantOrder: []identity.AgentName{"operator", "coding", "reviewer"},
		},
		{
			name: "duplicate name rejected",
			agents: []Agent{
				stubAgent("operator", "first"),
				stubAgent("coding", "c"),
				stubAgent("operator", "second"),
			},
			wantErr: true,
			wantDup: "operator",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			r, err := NewRegistry(tt.agents...)
			if (err != nil) != tt.wantErr {
				t.Fatalf("NewRegistry() error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				var dup *DuplicateAgentError
				if !errors.As(err, &dup) {
					t.Fatalf("NewRegistry() error = %v, want *DuplicateAgentError", err)
				}
				if dup.Name != tt.wantDup {
					t.Errorf("DuplicateAgentError.Name = %q, want %q", dup.Name, tt.wantDup)
				}
				if r != nil {
					t.Errorf("NewRegistry() returned non-nil registry on error: %v", r)
				}
				return
			}
			cat := r.Catalog()
			if len(cat) != len(tt.wantOrder) {
				t.Fatalf("Catalog() len = %d, want %d", len(cat), len(tt.wantOrder))
			}
			for i, e := range cat {
				if e.Name != tt.wantOrder[i] {
					t.Errorf("Catalog()[%d].Name = %q, want %q", i, e.Name, tt.wantOrder[i])
				}
			}
		})
	}
}

func TestRegistryLookup(t *testing.T) {
	t.Parallel()

	r, err := NewRegistry(
		stubAgent("operator", "the boss"),
		stubAgent("coding", "writes code"),
	)
	if err != nil {
		t.Fatalf("NewRegistry() unexpected error: %v", err)
	}

	tests := []struct {
		name     string
		lookup   identity.AgentName
		wantOK   bool
		wantDesc string
	}{
		{name: "hit returns agent and true", lookup: "operator", wantOK: true, wantDesc: "the boss"},
		{name: "second hit", lookup: "coding", wantOK: true, wantDesc: "writes code"},
		{name: "miss returns zero and false", lookup: "ghost", wantOK: false},
		{name: "empty name miss", lookup: "", wantOK: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, ok := r.Lookup(tt.lookup)
			if ok != tt.wantOK {
				t.Fatalf("Lookup(%q) ok = %v, want %v", tt.lookup, ok, tt.wantOK)
			}
			if !tt.wantOK {
				if got.Name != "" {
					t.Errorf("Lookup(%q) miss returned non-zero agent: %+v", tt.lookup, got)
				}
				return
			}
			if got.Name != tt.lookup {
				t.Errorf("Lookup(%q).Name = %q, want %q", tt.lookup, got.Name, tt.lookup)
			}
			if got.Description != tt.wantDesc {
				t.Errorf("Lookup(%q).Description = %q, want %q", tt.lookup, got.Description, tt.wantDesc)
			}
		})
	}
}

func TestRegistryCatalogOrder(t *testing.T) {
	t.Parallel()

	r, err := NewRegistry(
		stubAgent("operator", "o"),
		stubAgent("planner", "p"),
		stubAgent("coding", "c"),
		stubAgent("reviewer", "r"),
		stubAgent("explorer", "e"),
	)
	if err != nil {
		t.Fatalf("NewRegistry() unexpected error: %v", err)
	}

	want := []AgentCatalogEntry{
		{Name: "operator", Description: "o"},
		{Name: "planner", Description: "p"},
		{Name: "coding", Description: "c"},
		{Name: "reviewer", Description: "r"},
		{Name: "explorer", Description: "e"},
	}

	// Repeated calls must return the same deterministic order.
	for call := 0; call < 3; call++ {
		got := r.Catalog()
		if len(got) != len(want) {
			t.Fatalf("call %d: Catalog() len = %d, want %d", call, len(got), len(want))
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("call %d: Catalog()[%d] = %+v, want %+v", call, i, got[i], want[i])
			}
		}
	}
}

// TestAgentBuildTools verifies a registered agent's BuildTools is invokable and
// receives its LeafToolDeps (Root + HTTPCl) — documenting the leaf construction
// contract (no Spawner is reachable).
func TestAgentBuildTools(t *testing.T) {
	t.Parallel()

	var gotRoot string
	var gotClient *http.Client
	a := Agent{
		Name:        "coding",
		Description: "writes code",
		Role:        "code",
		BuildTools: func(d LeafToolDeps) loop.ToolSet {
			gotRoot = d.Root
			gotClient = d.HTTPCl
			return loop.ToolSet{MaxToolIterations: 7}
		},
	}
	r, err := NewRegistry(a)
	if err != nil {
		t.Fatalf("NewRegistry() unexpected error: %v", err)
	}
	got, ok := r.Lookup("coding")
	if !ok {
		t.Fatal("Lookup(coding) missed a registered agent")
	}

	client := &http.Client{}
	ts := got.BuildTools(LeafToolDeps{Root: "/repo", HTTPCl: client})
	if gotRoot != "/repo" {
		t.Errorf("BuildTools got Root = %q, want %q", gotRoot, "/repo")
	}
	if gotClient != client {
		t.Errorf("BuildTools got HTTPCl = %p, want %p", gotClient, client)
	}
	if ts.MaxToolIterations != 7 {
		t.Errorf("BuildTools returned ToolSet.MaxToolIterations = %d, want 7", ts.MaxToolIterations)
	}
}

// TestCatalogIsACopy guards that mutating a returned catalog slice does not corrupt
// the registry's internal order (the catalog is a defensive copy).
func TestCatalogIsACopy(t *testing.T) {
	t.Parallel()

	r, err := NewRegistry(stubAgent("operator", "o"), stubAgent("coding", "c"))
	if err != nil {
		t.Fatalf("NewRegistry() unexpected error: %v", err)
	}
	first := r.Catalog()
	first[0] = AgentCatalogEntry{Name: "tampered", Description: "x"}

	second := r.Catalog()
	if second[0].Name != "operator" {
		t.Errorf("Catalog() leaked internal state: second call head = %q, want %q", second[0].Name, "operator")
	}
}

// ModelFactory is a plain func type; this compile-time assertion documents its shape:
// it maps a finished system prompt to an llm.ModelSpec.
var _ ModelFactory = func(systemPrompt string) llm.ModelSpec {
	return llm.ModelSpec{System: systemPrompt}
}
