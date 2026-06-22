package swe

import (
	"context"
	"strconv"

	"github.com/ciram-co/looprig/pkg/content"
	"github.com/ciram-co/looprig/pkg/identity"
	"github.com/ciram-co/looprig/pkg/llm"
	"github.com/ciram-co/looprig/pkg/loop"
	"github.com/ciram-co/looprig/pkg/session"
	"github.com/ciram-co/looprig/pkg/tools"
)

// subagentRunner is the ONE session method the spawner needs (interface segregation /
// DIP): run a subagent loop under parent, on blocks, with cfg, returning its final
// text. *session.Session satisfies it. Depending on this narrow interface — rather
// than the concrete session — lets the spawner's agent-resolution + cfg-assembly logic
// be unit-tested with a fake that captures the cfg, with no real session or network.
type subagentRunner interface {
	RunSubagent(ctx context.Context, parent loop.Provenance, cfg loop.Config, blocks []content.Block) (string, error)
}

// spawner.go wires the concrete tools.Spawner the orchestrator's Subagent tool needs.
// It is the composition-root adapter that lets the agent-aware Subagent TOOL — which
// depends only on the narrow tools.Spawner interface + the typed identity.AgentName —
// resolve a named leaf agent against the swarm's Registry and run it as an IN-SESSION
// subagent loop via session.Session.RunSubagent, WITHOUT the tools/ package ever
// importing session or swarms/swe (keeping tools → session a one-way dependency that
// lives only here).
//
// AGENT-AWARE. Each Spawn looks the requested agent up in the leaf Registry; an
// unknown name fails closed with a typed *UnknownAgentError (errors.As-recoverable).
// A resolved agent's loop.Config is built FRESH per call: its model spec is the swarm
// Identity prepended to the leaf's Role, its toolset is the leaf's own BuildTools
// allowlist (each call gets a fresh PermissionChecker — per-loop approval isolation),
// and its AgentName attributes the sub-loop to that leaf.
//
// LEAST PRIVILEGE. A leaf's toolset has NO Subagent tool (LeafToolDeps carries no
// Spawner), so a spawned leaf cannot itself spawn — only the orchestrator (the
// primary) holds the spawn capability. The recursion the old coding spawner allowed
// is deliberately NOT reproduced for leaves.
//
// LATE-BIND. The session field is set ONCE, synchronously, by the swarm's
// construction seam right after the session is created/restored and BEFORE any turn
// runs — the orchestrator's tools are built before the session exists (the Subagent
// tool needs this spawner, the spawner needs the session), so the cycle is resolved
// by a single post-construction assignment (bind). No goroutine reads session until a
// turn invokes the Subagent tool, which cannot happen until after Submit, which
// cannot happen until after the session is built and bound; so the unsynchronized
// write/read pair never races.

// UnknownAgentError is returned by swarmSpawner.Spawn when the Subagent tool requests
// an agent name that is not in the leaf Registry. It is the fail-secure boundary: an
// unrecognized name never silently runs a default agent. It is errors.As-recoverable
// so a caller (or the tool's error-string render) can report which name was unknown.
type UnknownAgentError struct {
	Name identity.AgentName
}

func (e *UnknownAgentError) Error() string {
	return "swe: unknown subagent " + strconv.Quote(string(e.Name))
}

// swarmSpawner adapts the real session engine to tools.Spawner, agent-aware. It owns
// the leaf Registry (the authoritative spawnable set), the per-spawn tool-construction
// deps (workspace root + shared HTTP client), the shared provider client, the model
// factory, the SkillDescriber it reads each leaf's <available_skills> catalog through,
// and a late-bound reference to the live session that runs each sub-loop.
type swarmSpawner struct {
	session    subagentRunner              // late-bound after the session is built (see bind)
	registry   *Registry                   // authoritative spawnable leaf set
	deps       LeafToolDeps                // per-spawn tool-construction deps (root + HTTP client)
	client     llm.LLM                     // provider client shared with the parent (no per-loop client)
	factory    ModelFactory                // builds each leaf's model spec from its finished system prompt
	describer  tools.SkillDescriber        // reads a leaf's allowed-skill metadata for the catalog
	runtimeCtx loop.RuntimeContextProvider // volatile per-turn context (date/cwd/git) every leaf gets; shared with the orchestrator
}

// newSwarmSpawner builds an UNBOUND swarmSpawner (its session is nil until bind is
// called). The caller (the swarm's construction seam) wires the orchestrator's
// Subagent tool with this spawner, builds the session, then calls bind exactly once.
// describer is the same per-agent-scoped loader the registry's Skill tools are built
// over: a leaf's <available_skills> catalog can only list a skill it is allowed to
// load. runtimeCtx is the swarm-wide RuntimeContextProvider (shared with the
// orchestrator) every spawned leaf's loop.Config carries, so leaves get the same
// volatile per-turn context as the primary; nil leaves a leaf with runtime context OFF.
func newSwarmSpawner(registry *Registry, deps LeafToolDeps, client llm.LLM, factory ModelFactory, describer tools.SkillDescriber, runtimeCtx loop.RuntimeContextProvider) *swarmSpawner {
	return &swarmSpawner{registry: registry, deps: deps, client: client, factory: factory, describer: describer, runtimeCtx: runtimeCtx}
}

// bind late-binds the live session onto the spawner. It is called exactly once,
// synchronously, after the session is created/restored and before any turn runs (see
// the LATE-BIND note). Mirrors the old coding spawner's post-construction assignment.
func (sp *swarmSpawner) bind(sess *session.Session) { sp.session = sess }

// Spawn resolves agent against the leaf Registry and runs it as an in-session loop
// spawned under parent, on message, returning its final assistant text. An unknown
// agent fails closed with a *UnknownAgentError. It builds a FRESH loop.Config per call
// so each sub-loop gets its own session-scope PermissionChecker (per-loop approval
// isolation): a sub-loop's approval grants never leak into the parent's policy or a
// sibling's, and vice versa.
//
// ctx is the CALLING TURN's context (the parent step's): a ctx-cancel interrupts the
// sub-loop's in-flight TURN (RunSubagent translates it into a loop-targeted Interrupt
// and drains to TurnInterrupted) — it does NOT tear the persistent sub-loop down. The
// sub-loop survives idle under the session root and routes follow-ups back.
func (sp *swarmSpawner) Spawn(ctx context.Context, parent loop.Provenance, agent identity.AgentName, message string) (string, error) {
	a, ok := sp.registry.Lookup(agent)
	if !ok {
		return "", &UnknownAgentError{Name: agent}
	}
	// The leaf's system prompt is Identity + Role + its <available_skills> catalog
	// (empty for a skill-less leaf, so its prompt is unchanged). The catalog is read
	// through the per-agent-scoped describer, so it lists only authorized skills.
	system := Identity + a.Role + availableSkillsCatalog(ctx, sp.describer, a.Name, a.Skills)
	cfg := loop.Config{
		Client:         sp.client,
		Model:          sp.factory(system),
		Tools:          a.BuildTools(sp.deps),
		AgentName:      a.Name,
		RuntimeContext: sp.runtimeCtx, // swarm-wide volatile per-turn context, shared with the orchestrator
	}
	blocks := []content.Block{&content.TextBlock{Text: message}}
	return sp.session.RunSubagent(ctx, parent, cfg, blocks)
}

// compile-time assertion: swarmSpawner satisfies the tool's narrow Spawner interface.
var _ tools.Spawner = (*swarmSpawner)(nil)
