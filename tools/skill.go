package tools

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/inventivepotter/urvi/internal/agent/loop/identity"
	"github.com/inventivepotter/urvi/internal/tool"
)

// skill.go implements the Skill tool: an on-demand reader of a single curated,
// embedded SKILL.md body, scoped to the ONE agent the tool is bound to (design
// §7). The model names a skill ({name}) from the <available_skills> catalog the
// swarm injects into the agent's system prompt; the tool asks its injected
// SkillLoader for that body and returns it as the tool result.
//
// PER-AGENT BINDING / LEAST PRIVILEGE. A Skill tool is constructed PER AGENT and
// carries that agent's identity.AgentName as immutable state. It forwards (agent,
// name) to the loader, which authorizes the name against THAT agent's closed
// allow-set — so an agent can never load a skill outside its own boundary, and a
// traversal/unknown name is denied at the loader's gate before any path is built.
// The tool itself holds no catalog and no allow-map (DIP): it depends only on the
// narrow SkillLoader interface, never on swarms/swe.
//
// AUTO-APPROVE. Skill is AutoApprove — it has no path/command boundary (its only
// arg is {name}), so classifyTool puts it in classUnknown and it reaches
// AutoApprove only via the manifest's HardApprove list (the wiring names "Skill"
// for each skilled agent). It deliberately does NOT implement
// tool.PermissionPrompter: it is a scoped, side-effect-free read of trusted
// in-repo content, the same class as ReadFile/Subagent.
//
// AUDIT. Skill implements tool.Auditable: the summary is "Skill <name>" — the
// requested skill NAME only, never the body. The body is curated markdown, but the
// audit event is a redacted one-liner by contract, so only the name is surfaced.
//
// FAILURE MODEL. Every failure — unparsable args, an empty name, or any loader
// error (unknown/unauthorized, missing, malformed) — is a tool-result error
// STRING. InvokableRun never returns a Go error (CLAUDE.md: tool failures →
// tool-result strings) and never echoes the body on an error path.

// skillToolName is the EXACT tool name. It is classUnknown to classifyTool (no
// path/command boundary), so Check skips Stages 1–2 and the call reaches
// AutoApprove only via the manifest's HardApprove list (which names "Skill").
const skillToolName = "Skill"

// skillSchema is the JSON Schema for Skill's argument object: a single required
// {name} string selecting a skill from the agent's <available_skills> catalog.
const skillSchema = `{
  "type": "object",
  "properties": {
    "name": {"type": "string", "description": "The name of the skill to load (see the available skills listed in the system prompt)."}
  },
  "required": ["name"]
}`

const skillDesc = "Load a named skill's instructions on demand and return them as the result. Pick a name from the <available_skills> catalog in your system prompt; the skill body is injected so you can apply it."

// skillArgs is the typed decode of Skill's untrusted argsJSON. The JSON field
// contract is {name string} — required.
type skillArgs struct {
	Name string `json:"name"`
}

// Skill loads a single named SKILL.md body for the ONE agent it is bound to. It
// depends only on the narrow SkillLoader (DIP) and its own immutable agent
// identity; it never imports swarms/swe and holds no allow-map of its own.
type Skill struct {
	loader SkillLoader
	agent  identity.AgentName
}

// NewSkill constructs a Skill bound to a loader and the agent identity it serves.
// The swarm wires one per skilled agent at the composition root; the agent name is
// fixed at construction so every Load is scoped to that agent's closed allow-set.
func NewSkill(loader SkillLoader, agent identity.AgentName) *Skill {
	return &Skill{loader: loader, agent: agent}
}

// Info returns Skill's self-description. Name MUST equal "Skill" (the HardApprove
// key). The catalog of which skills are available is rendered into the agent's
// SYSTEM PROMPT by the swarm, not here, so the description is a static lead.
func (s *Skill) Info(context.Context) (*tool.ToolInfo, error) {
	return &tool.ToolInfo{
		Name:   skillToolName,
		Desc:   skillDesc,
		Schema: json.RawMessage(skillSchema),
	}, nil
}

// AuditSummary returns a redacted, body-free one-line summary: the skill NAME
// only. An unparseable or empty-name args document yields a generic summary; the
// skill body is never included.
func (s *Skill) AuditSummary(argsJSON string) string {
	var a skillArgs
	if err := json.Unmarshal([]byte(argsJSON), &a); err != nil || strings.TrimSpace(a.Name) == "" {
		return "Skill (unparsable args)"
	}
	return "Skill " + a.Name
}

// InvokableRun decodes {name}, asks the bound loader for the body scoped to this
// tool's agent, and returns it as the tool result. Every failure mode (bad args,
// empty name, or any loader error) is a tool-result error STRING — never a Go
// error and never echoing the body.
func (s *Skill) InvokableRun(ctx context.Context, argsJSON string) (*tool.ToolResult, error) {
	var a skillArgs
	if err := json.Unmarshal([]byte(argsJSON), &a); err != nil {
		return tool.TextResult("error: invalid arguments: not a JSON object"), nil
	}
	name := strings.TrimSpace(a.Name)
	if name == "" {
		return tool.TextResult("error: a non-empty 'name' is required"), nil
	}

	body, err := s.loader.Load(ctx, s.agent, name)
	if err != nil {
		// The loader's typed errors carry a non-secret message (the skill name + a
		// reason); render it as a tool-result string so the model can recover.
		return tool.TextResult("error: " + err.Error()), nil
	}
	return tool.TextResult(body), nil
}

// compile-time assertions: Skill is an InvokableTool and Auditable. It is
// deliberately NOT a PermissionPrompter (AutoApprove) and NOT a WriteTarget.
var (
	_ tool.InvokableTool = (*Skill)(nil)
	_ tool.Auditable     = (*Skill)(nil)
)
