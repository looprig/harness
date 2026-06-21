package tools

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/inventivepotter/urvi/internal/agent/loop"
	"github.com/inventivepotter/urvi/internal/tool"
)

// subagent.go implements the Subagent tool (design §2/§3). Subagent spawns an
// IN-SESSION subagent loop to handle a sub-task: it asks the injected Spawner to
// run a subagent — as a sibling loop under the current step's provenance — to
// completion on the supplied message, and returns the subagent's final assistant
// text as the tool result.
//
// PROVENANCE. The tool learns its OWN provenance from the ctx the loop injects at
// the tool-batch boundary (loop.ProvenanceFrom; design Task 12) and passes it as
// the `parent` to Spawn, so the spawned loop is recorded as a child of the step
// that requested it. Absent provenance (e.g. a run outside a turn) is the zero
// (root) Provenance — fail-safe, not an error.
//
// DIP / LEAST PRIVILEGE. The tool depends on ONE narrow interface it defines
// itself — Spawner — NOT on the concrete session.Session. The coding manifest
// wires a concrete Spawner that adapts session.Session.RunSubagent. Keeping the
// tool behind this interface makes it unit-testable with a fake and keeps
// tools/ → session a one-way (acyclic) dependency that lives only at the
// composition root.
//
// AUTO-APPROVE. Subagent is AutoApprove — it has no path/command boundary, so
// classifyTool puts it in classUnknown and it reaches AutoApprove only via the
// manifest's HardApprove list (defaulting to Ask otherwise). It deliberately does
// NOT implement tool.PermissionPrompter. It DOES implement tool.Auditable, whose
// summary is the constant "Subagent": the message may contain sensitive context
// and must never reach the audit event.
//
// FAILURE MODEL. Every failure — unparsable args, a missing/empty message, or a
// Spawn error — is a tool-result error STRING. InvokableRun never returns a Go
// error (CLAUDE.md: tool failures → tool-result strings).

// subagentToolName is the EXACT tool name. It is an UNKNOWN class to classifyTool
// (no path/command boundary), so Check skips Stages 1–2 and the call reaches
// AutoApprove only via the manifest's HardApprove list (which names "Subagent").
const subagentToolName = "Subagent"

// Spawner runs a subagent as an in-session loop spawned under `parent`, on the
// given task message, and returns the subagent's final assistant text. The
// concrete impl (wired by the coding manifest) adapts session.Session.RunSubagent.
type Spawner interface {
	Spawn(ctx context.Context, parent loop.Provenance, message string) (string, error)
}

// subagentArgs is the typed decode of Subagent's untrusted argsJSON. The JSON
// field contract is {message string}.
type subagentArgs struct {
	Message string `json:"message"`
}

const subagentSchema = `{
  "type": "object",
  "properties": {
    "message": {"type": "string", "description": "The task for the subagent to perform; it runs to completion and its final response is returned."}
  },
  "required": ["message"]
}`

const subagentDesc = "Spawn an in-session subagent to handle a sub-task, run it to completion, and return its final response."

// Subagent spawns in-session subagent loops. It depends only on the narrow
// Spawner (DIP), so it never imports the concrete session.
type Subagent struct {
	spawner Spawner
}

// NewSubagent constructs a Subagent from a Spawner (wired by the coding manifest
// at the composition root).
func NewSubagent(spawner Spawner) *Subagent {
	return &Subagent{spawner: spawner}
}

// Info returns Subagent's self-description. Name MUST equal "Subagent".
func (s *Subagent) Info(context.Context) (*tool.ToolInfo, error) {
	return &tool.ToolInfo{
		Name:   subagentToolName,
		Desc:   subagentDesc,
		Schema: json.RawMessage(subagentSchema),
	}, nil
}

// AuditSummary returns the constant "Subagent". The message may carry sensitive
// context, so it must NEVER reach the audit event — there is no skill or other
// non-sensitive field to surface, so the summary is a fixed label.
func (s *Subagent) AuditSummary(string) string {
	return "Subagent"
}

// InvokableRun reads the tool's own provenance from ctx, asks the Spawner to run
// a subagent to completion on the message, and returns its final text. Every
// failure is a tool-result error STRING; it never returns a Go error.
func (s *Subagent) InvokableRun(ctx context.Context, argsJSON string) (*tool.ToolResult, error) {
	var args subagentArgs
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return tool.TextResult("error: invalid arguments: not a JSON object"), nil
	}
	if strings.TrimSpace(args.Message) == "" {
		return tool.TextResult("error: a non-empty 'message' is required"), nil
	}

	// The discarded bool is PRESENCE, not an error: an absent key (a run outside a
	// turn) yields the zero (root) Provenance, which is the correct parent there —
	// so this is not a CLAUDE.md error-swallow violation.
	parent, _ := loop.ProvenanceFrom(ctx)

	finalText, err := s.spawner.Spawn(ctx, parent, args.Message)
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
