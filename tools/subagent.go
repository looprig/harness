package tools

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"

	"github.com/inventivepotter/urvi/internal/tool"
)

// subagent.go implements the Subagent tool (design §4b, row Subagent; §4a
// construction; impl plan row 6.11). Subagent spawns a SYNCHRONOUS child agent
// session to handle a sub-task: it picks a persona by skill name, runs that child
// to completion on the supplied message, and returns the child's final assistant
// text as the tool result.
//
// SCOPE (v1). Only synchronous Invoke-to-completion + a hard recursion-depth cap.
// The full Subagent design — streaming, per-child budget, a skill catalog — is
// deferred (design "Out of scope").
//
// DIP / LEAST PRIVILEGE. The tool depends on TWO narrow interfaces it defines
// itself — SubagentFactory (build a child by skill) and Subsession (run one child
// to completion and read its final text) — NOT on the concrete
// session.AgentSession. The manifest wires a concrete SubagentFactory that adapts
// the real session: it uses internal/registry to map a skill name to a persona /
// loop.Config, constructs a session.NewAgent, drives session.Invoke to a terminal
// event, and projects the terminal TurnDone.Message text into the string this
// tool's Subsession.Invoke returns. Keeping the tool behind these interfaces makes
// it unit-testable with fakes and keeps tools/ → session a one-way (acyclic)
// dependency that lives only at the composition root.
//
// SECURITY — recursion depth cap (fail-secure). A runaway agent that spawns
// children which spawn children is a resource-exhaustion / cost vector. A
// package-private context key carries the current spawn depth (absent == 0, the
// top level). InvokableRun reads that depth and, if it is already at the cap
// (maxSubagentDepth), returns a tool-result error and creates NO child — the
// check runs BEFORE the factory is ever called. When below the cap, the tool
// injects depth+1 into the ctx it hands the child's Invoke, so a Subagent invoked
// BY that child observes the incremented depth and the cap genuinely bounds the
// recursion. The default cap is 2.
//
// AUTO-APPROVE. Subagent is AutoApprove — it has no path/command boundary, so
// classifyTool puts it in classUnknown and it reaches AutoApprove only via the
// manifest's HardApprove list (defaulting to Ask otherwise). It deliberately does
// NOT implement tool.PermissionPrompter. It DOES implement tool.Auditable, whose
// summary carries ONLY the skill name: the message may contain sensitive context
// and must never reach the audit event.
//
// FAILURE MODEL. Every failure — unparsable args, a missing/empty skill or
// message, the depth cap, a factory error (e.g. unknown skill), or a child Invoke
// error — is a tool-result error STRING. InvokableRun never returns a Go error
// (CLAUDE.md: tool failures → tool-result strings).

// subagentToolName is the EXACT tool name. It is an UNKNOWN class to classifyTool
// (no path/command boundary), so Check skips Stages 1–2 and the call reaches
// AutoApprove only via the manifest's HardApprove list (which names "Subagent").
const subagentToolName = "Subagent"

// maxSubagentDepth is the hard recursion-depth cap (default 2). At the top level
// the depth carried in ctx is 0; each spawn injects depth+1 into the child ctx.
// A spawn requested at depth >= maxSubagentDepth is refused before any child is
// created. Bounding factor: with a cap of N, at most N levels of nesting exist.
const maxSubagentDepth = 2

// subagentDepthKey is the unexported context key carrying the current spawn
// depth (an int). It is a distinct zero-size type so it can never collide with
// any other package's context key.
type subagentDepthKey struct{}

// withSubagentDepth returns a child ctx carrying depth as the current spawn
// depth. The manifest/loop may use it to seed an explicit top-level depth; absent
// the key, subagentDepth treats the depth as 0 (the top level), so seeding is
// optional.
func withSubagentDepth(ctx context.Context, depth int) context.Context {
	return context.WithValue(ctx, subagentDepthKey{}, depth)
}

// subagentDepth reads the current spawn depth from ctx. An absent key (the normal
// top-level case) or any value of an unexpected type is treated as depth 0 —
// fail-secure: an unreadable depth is the most restrictive interpretation (start
// counting from zero so the cap still applies), never an implicit bypass.
func subagentDepth(ctx context.Context) int {
	d, ok := ctx.Value(subagentDepthKey{}).(int)
	if !ok {
		return 0
	}
	return d
}

// Subsession is the narrow interface the tool needs from a child agent: run the
// child to completion on message and return its final assistant text. The
// concrete implementation (wired by the manifest) adapts session.AgentSession —
// whose Invoke returns a terminal event.Event — by extracting the text blocks of
// the terminal event.TurnDone.Message. A failed/interrupted turn is surfaced as a
// non-nil error.
type Subsession interface {
	Invoke(ctx context.Context, message string) (string, error)
}

// SubagentFactory builds a child Subsession for the named skill. The concrete
// implementation (wired by the manifest) resolves skill → persona via
// internal/registry and constructs a session.NewAgent. An unknown skill (or any
// construction failure) is returned as an error, which the tool turns into a
// tool-result error string.
type SubagentFactory interface {
	New(ctx context.Context, skill string) (Subsession, error)
}

// subagentArgs is the typed decode of Subagent's untrusted argsJSON. The JSON
// field contract is {skill string, message string}.
type subagentArgs struct {
	Skill   string `json:"skill"`
	Message string `json:"message"`
}

const subagentSchema = `{
  "type": "object",
  "properties": {
    "skill": {"type": "string", "description": "The persona/skill that selects which child agent to spawn."},
    "message": {"type": "string", "description": "The task for the child agent to perform. It runs to completion and its final response is returned."}
  },
  "required": ["skill", "message"]
}`

const subagentDesc = "Spawn a synchronous child agent (selected by skill) to handle a sub-task, run it to completion, and return its final response. Nested spawning is capped at a hard recursion depth to prevent runaway agents."

// Subagent spawns synchronous child agent sessions. It depends only on the narrow
// SubagentFactory; rootCtx is the session-root context captured at construction
// (per design §4a) — the base lifetime for any child the factory creates. v1's
// synchronous Invoke uses the per-call ctx (which carries the depth and the
// turn's cancellation), so rootCtx is retained for the factory/child lifetime
// contract rather than threaded into each call.
type Subagent struct {
	factory SubagentFactory
	rootCtx context.Context
}

// NewSubagent constructs a Subagent from a SubagentFactory and the session-root
// context (design §4a — the manifest's rootCtx, already in hand when it builds
// tools before session.NewAgent).
func NewSubagent(factory SubagentFactory, rootCtx context.Context) *Subagent {
	return &Subagent{factory: factory, rootCtx: rootCtx}
}

// Info returns Subagent's self-description. Name MUST equal "Subagent".
func (s *Subagent) Info(context.Context) (*tool.ToolInfo, error) {
	return &tool.ToolInfo{
		Name:   subagentToolName,
		Desc:   subagentDesc,
		Schema: json.RawMessage(subagentSchema),
	}, nil
}

// AuditSummary returns "Subagent: <skill>". ONLY the skill name is surfaced — the
// message may carry sensitive context and must never reach the audit event.
// Unparsable args / a missing skill yield a generic summary.
func (s *Subagent) AuditSummary(argsJSON string) string {
	var args subagentArgs
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil || strings.TrimSpace(args.Skill) == "" {
		return "Subagent (unparsable args)"
	}
	return "Subagent: " + args.Skill
}

// InvokableRun spawns a child agent for the requested skill, runs it to
// completion on the message, and returns its final text. The recursion-depth cap
// is enforced FIRST — before the factory is ever consulted — so an over-deep
// spawn creates nothing. Every failure is a tool-result error STRING; it never
// returns a Go error.
func (s *Subagent) InvokableRun(ctx context.Context, argsJSON string) (*tool.ToolResult, error) {
	var args subagentArgs
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return tool.TextResult("error: invalid arguments: not a JSON object"), nil
	}
	skill := strings.TrimSpace(args.Skill)
	if skill == "" {
		return tool.TextResult("error: a non-empty 'skill' is required"), nil
	}
	if strings.TrimSpace(args.Message) == "" {
		return tool.TextResult("error: a non-empty 'message' is required"), nil
	}

	// SECURITY (fail-secure): enforce the depth cap BEFORE creating any child.
	depth := subagentDepth(ctx)
	if depth >= maxSubagentDepth {
		return tool.TextResult("error: subagent depth limit reached (max " + strconv.Itoa(maxSubagentDepth) + ")"), nil
	}

	// Inject depth+1 once, then run the WHOLE child lifetime — construction and
	// invocation — under it. A Subagent invoked BY this child (whether during the
	// factory's construction or the child's own turn) therefore sees the higher
	// depth, so the cap bounds the recursion across both phases.
	childCtx := withSubagentDepth(ctx, depth+1)

	child, err := s.factory.New(childCtx, skill)
	if err != nil {
		// Unknown skill or any construction failure. The error may name internal
		// registry state, but it is not a secret (the skill list is operator-known).
		return tool.TextResult("error: could not start subagent: " + err.Error()), nil
	}

	finalText, err := child.Invoke(childCtx, args.Message)
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
