package tools

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/ciram-co/looprig/pkg/identity"
	"github.com/ciram-co/looprig/pkg/loop"
	"github.com/ciram-co/looprig/pkg/tool"
)

// subagent.go implements the Subagent tool (design §2/§3). Subagent spawns an
// IN-SESSION subagent loop to handle a sub-task: it asks the injected Spawner to
// run a NAMED agent — as a sibling loop under the current step's provenance — to
// completion on the supplied message, and returns the subagent's final assistant
// text as the tool result.
//
// AGENT-AWARE. The model picks WHICH agent to spawn by name (the {agent} arg) from
// the catalog rendered into Info().Desc. The tool itself does NOT know the agent
// set — it forwards the name to the Spawner, which resolves it against the swarm's
// registry (an unknown name comes back as a Spawn error → tool-result string). The
// tool stays a thin, registry-agnostic forwarder (DIP / least privilege): it depends
// only on the narrow Spawner + the typed identity.AgentName, never on swarms/swe.
//
// PROVENANCE. The tool learns its OWN provenance from the ctx the loop injects at
// the tool-batch boundary (loop.ProvenanceFrom; design Task 12) and passes it as
// the `parent` to Spawn, so the spawned loop is recorded as a child of the step
// that requested it. Absent provenance (e.g. a run outside a turn) is the zero
// (root) Provenance — fail-safe, not an error.
//
// DIP / LEAST PRIVILEGE. The tool depends on ONE narrow interface it defines
// itself — Spawner — NOT on the concrete session.Session. The swarm wires a
// concrete Spawner that adapts session.Session.RunSubagent. Keeping the tool behind
// this interface makes it unit-testable with a fake and keeps tools/ → session a
// one-way (acyclic) dependency that lives only at the composition root.
//
// AUTO-APPROVE. Subagent is AutoApprove — it has no path/command boundary, so
// classifyTool puts it in classUnknown and it reaches AutoApprove only via the
// manifest's HardApprove list (defaulting to Ask otherwise). It deliberately does
// NOT implement tool.PermissionPrompter. It DOES implement tool.Auditable, whose
// summary is the constant "Subagent": the message may contain sensitive context
// and must never reach the audit event.
//
// FAILURE MODEL. Every failure — unparsable args, a missing/empty agent or message,
// or a Spawn error — is a tool-result error STRING. InvokableRun never returns a Go
// error (CLAUDE.md: tool failures → tool-result strings).

// subagentToolName is the EXACT tool name. It is an UNKNOWN class to classifyTool
// (no path/command boundary), so Check skips Stages 1–2 and the call reaches
// AutoApprove only via the manifest's HardApprove list (which names "Subagent").
const subagentToolName = "Subagent"

// Spawner runs a named subagent as an in-session loop spawned under `parent`, on the
// given task message, and returns the subagent's final assistant text. The concrete
// impl (wired by the swarm) resolves `agent` against the leaf registry and adapts
// session.Session.RunSubagent; an unknown agent is returned as an error.
type Spawner interface {
	Spawn(ctx context.Context, parent loop.Provenance, agent identity.AgentName, message string, parentToolUseID string) (string, error)
}

// SubagentCatalogEntry is one spawnable agent the tool advertises in its Info().Desc
// listing: the name the model passes as {agent} and a one-line description. It is a
// tools-level type (the tool may import identity but NOT swarms/swe), so the swarm
// projects its registry catalog onto a []SubagentCatalogEntry at the composition
// root.
type SubagentCatalogEntry struct {
	Name        identity.AgentName
	Description string
}

// subagentArgs is the typed decode of Subagent's untrusted argsJSON. The JSON field
// contract is {agent string, message string} — both required.
type subagentArgs struct {
	Agent   string `json:"agent"`
	Message string `json:"message"`
}

const subagentSchema = `{
  "type": "object",
  "properties": {
    "agent": {"type": "string", "description": "The name of the subagent to spawn (see the available subagents listed in the tool description)."},
    "message": {"type": "string", "description": "The task for the subagent to perform; it runs to completion and its final response is returned."}
  },
  "required": ["agent", "message"]
}`

// subagentDescPrefix is the static lead of Info().Desc; the available-subagents
// catalog is rendered after it so the model knows which agents it may spawn.
const subagentDescPrefix = "Spawn an in-session subagent by name to handle a sub-task, run it to completion, and return its final response."

// Subagent spawns in-session subagent loops by name. It depends only on the narrow
// Spawner (DIP), so it never imports the concrete session; it carries a static
// catalog of spawnable agents purely to render its self-description.
type Subagent struct {
	spawner Spawner
	catalog []SubagentCatalogEntry
}

// NewSubagent constructs a Subagent from a Spawner and the catalog of spawnable
// agents (both wired by the swarm at the composition root). The catalog is rendered
// into Info().Desc as an <available_subagents> listing; it is descriptive only — the
// authoritative agent set is the Spawner's registry, so an agent absent from the
// catalog still fails closed at Spawn (unknown agent → tool-result error string).
func NewSubagent(spawner Spawner, catalog []SubagentCatalogEntry) *Subagent {
	return &Subagent{spawner: spawner, catalog: catalog}
}

// subagentDesc renders the tool description: the static prefix followed by an
// <available_subagents> block listing each catalog entry (name + description) so the
// model can pick a valid {agent}. An empty catalog renders just the prefix.
func (s *Subagent) subagentDesc() string {
	if len(s.catalog) == 0 {
		return subagentDescPrefix
	}
	var b strings.Builder
	b.WriteString(subagentDescPrefix)
	b.WriteString("\n<available_subagents>\n")
	for _, e := range s.catalog {
		b.WriteString("- ")
		b.WriteString(string(e.Name))
		if strings.TrimSpace(e.Description) != "" {
			b.WriteString(": ")
			b.WriteString(e.Description)
		}
		b.WriteString("\n")
	}
	b.WriteString("</available_subagents>")
	return b.String()
}

// Info returns Subagent's self-description. Name MUST equal "Subagent"; Desc carries
// the available-subagents catalog.
func (s *Subagent) Info(context.Context) (*tool.ToolInfo, error) {
	return &tool.ToolInfo{
		Name:   subagentToolName,
		Desc:   s.subagentDesc(),
		Schema: json.RawMessage(subagentSchema),
	}, nil
}

// AuditSummary returns the constant "Subagent". The agent name and message may carry
// sensitive context, so neither reaches the audit event — there is no non-sensitive
// field to surface, so the summary is a fixed label.
func (s *Subagent) AuditSummary(string) string {
	return "Subagent"
}

// InvokableRun reads the tool's own provenance from ctx, asks the Spawner to run the
// named subagent to completion on the message, and returns its final text. Every
// failure is a tool-result error STRING; it never returns a Go error.
func (s *Subagent) InvokableRun(ctx context.Context, argsJSON string) (*tool.ToolResult, error) {
	var args subagentArgs
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return tool.TextResult("error: invalid arguments: not a JSON object"), nil
	}
	if strings.TrimSpace(args.Agent) == "" {
		return tool.TextResult("error: a non-empty 'agent' is required"), nil
	}
	if strings.TrimSpace(args.Message) == "" {
		return tool.TextResult("error: a non-empty 'message' is required"), nil
	}

	// The discarded bool is PRESENCE, not an error: an absent key (a run outside a
	// turn) yields the zero (root) Provenance, which is the correct parent there —
	// so this is not a CLAUDE.md error-swallow violation.
	parent, _ := loop.ProvenanceFrom(ctx)

	// The discarded bool is PRESENCE: an absent tool-use id (e.g. a run outside a
	// turn) yields "" — the correct graceful default: no provider tool-use id to
	// correlate the spawned loop against downstream — so this is not a CLAUDE.md
	// error-swallow violation.
	tuid, _ := loop.ToolUseIDFrom(ctx)

	finalText, err := s.spawner.Spawn(ctx, parent, identity.AgentName(args.Agent), args.Message, tuid)
	if err != nil {
		return tool.TextResult("error: subagent failed: " + err.Error()), nil
	}
	return tool.TextResult(finalText), nil
}

// compile-time assertions: Subagent is an InvokableTool and Auditable. It is
// deliberately NOT a PermissionPrompter (AutoApprove) and NOT a WriteTarget.
var (
	_ tool.InvokableTool = (*Subagent)(nil)
	_ tool.Auditable     = (*Subagent)(nil)
)
