// Package swe holds the SWE-Swarm's typed agent catalog. It is pure data and
// lookup: the single source of truth for which agents exist, what each one
// exposes (role prompt + its own toolset builder), and the deterministic order
// the catalog is presented in. Tool validation, the prompt catalog, and the
// greeting all read the Registry; nothing here drives a loop or owns identity,
// the model, or the full loop.Config — the swarm assembles those.
package swe

import (
	"net/http"
	"strconv"

	"github.com/ciram-co/looprig/pkg/identity"
	"github.com/ciram-co/looprig/pkg/llm"
	"github.com/ciram-co/looprig/pkg/loop"
)

// Agent is what an agent PACKAGE exposes. It owns its role prompt + toolset
// builder; it does NOT own identity, the model, or the full loop.Config (the
// swarm assembles those).
type Agent struct {
	Name        identity.AgentName
	Description string // shown in the Subagent catalog + greeting
	Role        string // role prompt; the swarm prepends identity

	// Skills is the agent's closed set of allowed embedded-skill names — part of
	// its boundary. An agent with ≥1 skill is wired with the Skill tool and an
	// <available_skills> catalog in its system prompt; an empty set gets neither.
	// The swarm builds the loader's per-agent allow-map from these names.
	Skills []string

	// BuildTools constructs the agent's OWN tool allowlist. The swarm calls it
	// per spawn so each invocation gets a fresh PermissionChecker (no shared
	// mutable permission state across loops). The optional Skill tool is threaded
	// in by the leaf adapter when the agent has ≥1 skill.
	BuildTools func(LeafToolDeps) loop.ToolSet

	AllowsRuntimeSkills bool // P2b; false in P1
}

// LeafToolDeps are the construction deps a leaf agent's toolset needs. There is
// deliberately NO Spawner here — a leaf cannot spawn (least privilege). The
// orchestrator's spawn-capable toolset is assembled separately.
type LeafToolDeps struct {
	Root   string
	HTTPCl *http.Client
}

// Config is the swarm's human-set construction config — the knobs a launch flag /
// operator decision sets, never the model (§2, §15). Today it carries only the
// runtime-skills enablement mode; it is a struct (not a bare bool) so future opt-in
// modes extend it without churning every construction signature. The zero value is
// the fail-secure default (every mode off).
type Config struct {
	// RuntimeSkills enables the untrusted, human-gated workspace skill source
	// (<workspaceRoot>/.skills/<name>/SKILL.md) for the agents whose definition sets
	// AllowsRuntimeSkills (the read-only explorer + researcher per §7a). Off by
	// default: embedded-only. The model can never set it — only a launch flag does
	// (cmd/swe's --runtime-skills). When off, no leaf gains a workspace skill source.
	RuntimeSkills bool

	// Greeting enables the OPTIONAL, UI-only startup greeting (§5a): a deterministic,
	// LLM-free opening transcript entry listing the swarm's agents (+ embedded skills),
	// rendered by the TUI before any turn. Off by default (fail-secure): off → no
	// greeting, behavior identical to today. It is purely a rendered opening entry — NOT
	// a turn, NOT a command, never in the model's context. The model can never set it;
	// only a launch flag does (cmd/swe's --greeting). See Greeting() and greeting.go.
	Greeting bool
}

// ModelFactory turns a finished system prompt into an llm.ModelSpec. The swarm
// owns the provider/model/sampling; agents never see it.
type ModelFactory func(systemPrompt string) llm.ModelSpec

// AgentCatalogEntry is the public, lookup-free view of an agent: just the name
// and one-line description used to render the Subagent catalog and greeting.
type AgentCatalogEntry struct {
	Name        identity.AgentName
	Description string
}

// DuplicateAgentError is returned by NewRegistry when two agents share a Name.
// A duplicate is a programming error at the composition root, so registration
// fails secure (no registry is built) rather than silently picking a winner.
// It is errors.As-recoverable so the caller can report which name collided.
type DuplicateAgentError struct {
	Name identity.AgentName
}

func (e *DuplicateAgentError) Error() string {
	return "swe: duplicate agent name " + strconv.Quote(string(e.Name))
}

// Registry is the single source of truth for agent lookup + the catalog. It is
// immutable after construction: built once at the composition root and only
// read thereafter, so the zero-copy maps/slices are safe to share.
type Registry struct {
	byName map[identity.AgentName]Agent
	order  []identity.AgentName // deterministic catalog order (insertion order)
}

// NewRegistry builds a Registry from agents in the given order, preserving
// insertion order for the catalog. A duplicate Name is rejected with a
// *DuplicateAgentError (fail secure: no partial registry is returned).
func NewRegistry(agents ...Agent) (*Registry, error) {
	r := &Registry{
		byName: make(map[identity.AgentName]Agent, len(agents)),
		order:  make([]identity.AgentName, 0, len(agents)),
	}
	for _, a := range agents {
		if _, exists := r.byName[a.Name]; exists {
			return nil, &DuplicateAgentError{Name: a.Name}
		}
		r.byName[a.Name] = a
		r.order = append(r.order, a.Name)
	}
	return r, nil
}

// Lookup returns the agent registered under n and true, or the zero Agent and
// false if no agent is registered under that name.
func (r *Registry) Lookup(n identity.AgentName) (Agent, bool) {
	a, ok := r.byName[n]
	return a, ok
}

// Catalog returns the name+description of every agent in deterministic
// insertion order. The returned slice is a fresh copy: callers may mutate it
// without affecting the registry.
func (r *Registry) Catalog() []AgentCatalogEntry {
	out := make([]AgentCatalogEntry, 0, len(r.order))
	for _, name := range r.order {
		a := r.byName[name]
		out = append(out, AgentCatalogEntry{Name: a.Name, Description: a.Description})
	}
	return out
}
