package swe

import (
	"sort"
	"strings"

	"github.com/ciram-co/looprig/agents/orchestrator"
)

// greeting.go builds the OPTIONAL, UI-only startup greeting (§5a): a deterministic,
// LLM-free description of what the swarm can do, sourced from the SAME agent set that
// feeds the Subagent catalog (§6b) so it can never drift from the actual wiring. It is
// pure string-building — it never builds a session, never calls the model, and is NOT a
// turn or a command. The TUI renders the returned string as its opening transcript
// entry; the primary loop's history stays empty until the first real user message.

// greetingLead is the opening line of the greeting. It is a plain capability statement,
// never an assistant turn — the model never sees it.
const greetingLead = "SWE is a software-engineering swarm. It plans a task and delegates to specialist agents."

// Greeting returns the deterministic startup-greeting string for cfg, or the EMPTY
// string when the greeting toggle is off (cfg.Greeting == false — the fail-secure
// default). It is built from the swarm's wired agent set (the orchestrator primary +
// the spawnable leaf roster) and the leaves' embedded skill names — the same source of
// truth the Subagent catalog reads — so it costs nothing and never drifts from the
// wiring. It performs NO I/O and NO model call: pure, side-effect-free string building,
// safe to call at the composition root before any session exists.
func Greeting(cfg Config) string {
	if !cfg.Greeting {
		return ""
	}
	return buildGreeting(greetingCatalog(), greetingSkills())
}

// greetingCatalog returns the agent entries the greeting lists, in display order: the
// orchestrator (the primary loop) first, then the spawnable leaves in their fixed
// catalog order. It is derived from the package-exported agent boundaries
// (orchestrator.Name/Description + leafBuiltins), the SAME source the leaf Registry and
// Subagent catalog read, so the greeting can never name an agent the swarm does not wire.
func greetingCatalog() []AgentCatalogEntry {
	builtins := leafBuiltins()
	out := make([]AgentCatalogEntry, 0, len(builtins)+1)
	out = append(out, AgentCatalogEntry{Name: orchestrator.Name, Description: orchestrator.Description})
	for _, b := range builtins {
		out = append(out, AgentCatalogEntry{Name: b.name, Description: b.description})
	}
	return out
}

// greetingSkills returns the de-duplicated, sorted set of embedded skill names wired
// across all leaves — the cheap, registry-sourced skill listing for the greeting. It
// reads only leafBuiltins' declared Skills (no loader, no SKILL.md parse), so it is
// deterministic and free; an empty result yields no Skills section in the greeting.
func greetingSkills() []string {
	seen := make(map[string]struct{})
	for _, b := range leafBuiltins() {
		for _, s := range b.skills {
			seen[s] = struct{}{}
		}
	}
	out := make([]string, 0, len(seen))
	for s := range seen {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

// buildGreeting renders the greeting string from an agent catalog (in the given order)
// and a list of skill names. It is the pure, unit-testable core: an empty catalog
// yields the empty string (nothing to greet); otherwise it emits the lead line, one
// "  - <name>: <description>" line per agent (description omitted when empty), and — only
// when skills are present — a Skills section listing each name. The output is a pure
// function of the inputs (no clock, no map iteration, no randomness), so it is byte-stable.
func buildGreeting(catalog []AgentCatalogEntry, skills []string) string {
	if len(catalog) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString(greetingLead)
	b.WriteString("\n\nAgents:")
	for _, e := range catalog {
		b.WriteString("\n  - ")
		b.WriteString(string(e.Name))
		if d := strings.TrimSpace(e.Description); d != "" {
			b.WriteString(": ")
			b.WriteString(d)
		}
	}
	if len(skills) > 0 {
		b.WriteString("\n\nSkills:")
		for _, s := range skills {
			b.WriteString("\n  - ")
			b.WriteString(s)
		}
	}
	return b.String()
}
