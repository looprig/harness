package delegationtool

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/identity"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/tool"
)

// subagent.go implements the model-facing Subagent tool (design §"Subagent tool mode
// selection"/§"Synchronous and managed delegation"). It is the ONE parent-to-child
// communication surface: a flat, strictly validated action envelope driving the
// parent-scoped tool.DelegateController.Execute — the tool's only runtime binding.
//
// SCHEMA IS NOT A SECURITY BOUNDARY. The exposed JSON schema is DERIVED from the
// active delegation style (SyncOnly ⇒ only `start` with `wait` fixed true; Managed ⇒
// all five actions) purely to guide the model. The parent-scoped controller
// re-enforces the same action set, ownership, mode, and permission ceiling regardless
// of crafted JSON, so the tool forwards a well-formed envelope faithfully and lets the
// controller deny.
//
// FAILURE MODEL. Every failure — unparsable args, a boundary-validation rejection, or
// a controller error — is a tool-result error STRING. InvokableRun never returns a Go
// error (CLAUDE.md: tool failures → tool-result strings).
//
// AUDIT. AuditSummary is the constant "Subagent": the agent name and message may carry
// sensitive context and must never reach the audit event.

// subagentToolName is the EXACT tool name. It is an UNKNOWN class to classifyTool
// (no path/command boundary), so it reaches AutoApprove only via the manifest's
// HardApprove list (which names "Subagent").
const subagentToolName = "Subagent"

// SubagentAction is the model-facing delegation verb carried by the envelope.
type SubagentAction string

const (
	actionStart     SubagentAction = "start"
	actionSend      SubagentAction = "send"
	actionWait      SubagentAction = "wait"
	actionInterrupt SubagentAction = "interrupt"
	actionStatus    SubagentAction = "status"
)

// SubagentCatalogEntry is one delegate the tool advertises in its Info().Desc: the
// name the model passes as {agent} and a one-line description. The rig projects the
// parent definition's delegate set onto this at the composition root.
type SubagentCatalogEntry struct {
	Name        identity.AgentName
	Description string
	Modes       []loop.ModeName
}

// SubagentArgs is the typed decode of the untrusted argsJSON. Absent typed pointers
// distinguish "not supplied" from a supplied zero value: an absent request_id is nil,
// while a supplied zero UUID is present-but-invalid.
type SubagentArgs struct {
	Action         SubagentAction     `json:"action,omitempty"`
	Agent          identity.AgentName `json:"agent,omitempty"`
	Mode           loop.ModeName      `json:"mode,omitempty"`
	DelegateID     *uuid.UUID         `json:"delegate_id,omitempty"`
	RequestID      *uuid.UUID         `json:"request_id,omitempty"`
	Message        string             `json:"message,omitempty"`
	Wait           *bool              `json:"wait,omitempty"`
	TimeoutSeconds *int               `json:"timeout_seconds,omitempty"`
}

const subagentDescPrefix = "Delegate a sub-task to an in-session child agent by name via one action envelope, and optionally wait for its response."

// SubagentTool drives parent-to-child delegation through one action envelope. It
// depends only on the narrow tool.DelegateController (DIP); the style and catalog are
// static construction config used to derive the model-facing schema and description.
type SubagentTool struct {
	controller tool.DelegateController
	style      loop.DelegationStyle
	catalog    []SubagentCatalogEntry
}

// NewSubagent constructs a SubagentTool bound to the parent-scoped controller, with the
// delegation style and delegate catalog derived from the parent definition at the
// composition root.
func NewSubagent(controller tool.DelegateController, style loop.DelegationStyle, catalog []SubagentCatalogEntry) *SubagentTool {
	return &SubagentTool{controller: controller, style: style, catalog: cloneSubagentCatalog(catalog)}
}

func (s *SubagentTool) schema() string {
	fieldOrder := []string{"action", "agent", "mode", "delegate_id", "request_id", "message", "wait", "timeout_seconds"}
	properties := map[string]any{
		"action": map[string]any{"type": "string", "enum": []string{"start", "send", "wait", "interrupt", "status"}},
		"agent":  map[string]any{"type": "string"}, "mode": map[string]any{"type": "string"},
		"delegate_id": map[string]any{"type": "string"}, "request_id": map[string]any{"type": "string"},
		"message": map[string]any{"type": "string"}, "wait": map[string]any{"type": "boolean"},
		"timeout_seconds": map[string]any{"type": "integer", "minimum": 0},
	}
	startVariants := make([]any, 0, len(s.catalog))
	for _, entry := range s.catalog {
		modes := make([]string, len(entry.Modes))
		for i, mode := range entry.Modes {
			modes[i] = string(mode)
		}
		variantProps := map[string]any{"agent": map[string]any{"const": string(entry.Name)}}
		if len(modes) > 0 {
			variantProps["mode"] = map[string]any{"type": "string", "enum": modes}
		}
		startVariants = append(startVariants, map[string]any{"type": "object", "properties": variantProps})
	}
	actionBranch := func(action string, required, allowed []string) map[string]any {
		allowedSet := map[string]struct{}{"action": {}}
		for _, name := range allowed {
			allowedSet[name] = struct{}{}
		}
		forbidden := make([]string, 0)
		for _, name := range fieldOrder {
			if _, ok := allowedSet[name]; !ok {
				forbidden = append(forbidden, name)
			}
		}
		then := map[string]any{"not": map[string]any{"anyOf": requiredProperties(forbidden)}}
		if len(required) > 0 {
			then["required"] = required
		}
		return map[string]any{
			"if":   map[string]any{"required": []string{"action"}, "properties": map[string]any{"action": map[string]any{"const": action}}},
			"then": then,
		}
	}
	startAllowed := []string{"agent", "mode", "message", "wait", "timeout_seconds"}
	startBranch := actionBranch("start", []string{"agent", "message"}, startAllowed)
	if len(startVariants) > 0 {
		startBranch["then"].(map[string]any)["oneOf"] = startVariants
	}
	defaultStartBranch := map[string]any{
		"if":   map[string]any{"not": map[string]any{"required": []string{"action"}}},
		"then": startBranch["then"],
	}
	branches := []any{
		startBranch,
		defaultStartBranch,
		actionBranch("send", []string{"delegate_id", "message"}, []string{"delegate_id", "message", "wait", "timeout_seconds"}),
		actionBranch("wait", []string{"delegate_id", "request_id"}, []string{"delegate_id", "request_id", "timeout_seconds"}),
		actionBranch("interrupt", []string{"delegate_id"}, []string{"delegate_id"}),
		actionBranch("status", nil, []string{"delegate_id"}),
	}
	if s.style == loop.DelegationSyncOnly {
		properties["action"] = map[string]any{"type": "string", "enum": []string{"start"}}
		properties["wait"] = map[string]any{"const": true}
		branches = branches[:2]
	}
	schema := map[string]any{"type": "object", "additionalProperties": false, "properties": properties, "allOf": branches}
	encoded, _ := json.Marshal(schema)
	return string(encoded)
}

func requiredProperties(names []string) []any {
	result := make([]any, len(names))
	for i, name := range names {
		result[i] = map[string]any{"required": []string{name}}
	}
	return result
}

func cloneSubagentCatalog(catalog []SubagentCatalogEntry) []SubagentCatalogEntry {
	result := append([]SubagentCatalogEntry(nil), catalog...)
	for i := range result {
		result[i].Modes = append([]loop.ModeName(nil), result[i].Modes...)
	}
	return result
}

// subagentDesc renders the static prefix followed by an <available_agents> block
// listing each catalog entry. An empty catalog renders just the prefix.
func (s *SubagentTool) subagentDesc() string {
	if len(s.catalog) == 0 {
		return subagentDescPrefix
	}
	var b strings.Builder
	b.WriteString(subagentDescPrefix)
	b.WriteString("\n<available_agents>\n")
	for _, e := range s.catalog {
		b.WriteString("- ")
		b.WriteString(string(e.Name))
		if strings.TrimSpace(e.Description) != "" {
			b.WriteString(": ")
			b.WriteString(e.Description)
		}
		if len(e.Modes) > 0 {
			b.WriteString(" (modes: ")
			for i, mode := range e.Modes {
				if i > 0 {
					b.WriteString(", ")
				}
				if mode == "" {
					b.WriteString("default")
				} else {
					b.WriteString(string(mode))
				}
			}
			b.WriteString(")")
		}
		b.WriteString("\n")
	}
	b.WriteString("</available_agents>")
	return b.String()
}

// Info returns the self-description. Name MUST equal "Subagent"; the schema is derived
// from the delegation style and the description carries the delegate catalog.
func (s *SubagentTool) Info(context.Context) (*tool.ToolInfo, error) {
	return &tool.ToolInfo{
		Name:   subagentToolName,
		Desc:   s.subagentDesc(),
		Schema: json.RawMessage(s.schema()),
	}, nil
}

// AuditSummary returns the constant "Subagent"; the agent name and message may carry
// sensitive context and never reach the audit event.
func (s *SubagentTool) AuditSummary(string) string { return "Subagent" }

// PrepareCall implements the mandatory tool preparation capability with a pure
// empty request: delegation needs no OS capability, resource grant, or durable
// rule, so the combined gate auto-allows it (the tool's historical AutoApprove
// posture). Argument validation stays in InvokableRun, whose failures are
// model-visible tool-result strings.
func (s *SubagentTool) PrepareCall(context.Context, uuid.UUID, string) (tool.Request, tool.PreparedArtifact, error) {
	return tool.Request{}, nil, nil
}

// InvokableRun parses the untrusted envelope, validates it at the boundary, translates
// it into a tool.DelegateRequest, and forwards it to the parent-scoped controller.
// Every failure is a tool-result error STRING; it never returns a Go error.
func (s *SubagentTool) InvokableRun(ctx context.Context, argsJSON string) (*tool.ToolResult, error) {
	args, present, err := decodeSubagentArgs(argsJSON)
	if err != nil {
		return tool.TextResult("error: invalid arguments: not a valid Subagent envelope"), nil
	}
	action := args.Action
	if action == "" {
		action = actionStart
	}
	if errText := validateEnvelopeFields(action, present); errText != "" {
		return tool.TextResult(errText), nil
	}
	if s.style == loop.DelegationSyncOnly && (action != actionStart || (args.Wait != nil && !*args.Wait)) {
		return tool.TextResult("error: subagent action is unavailable for sync-only delegation"), nil
	}
	req, errText := buildDelegateRequest(action, args)
	if errText != "" {
		return tool.TextResult(errText), nil
	}
	// The tool learns its own provider tool-use id from ctx so a tool-spawned child can
	// be correlated to the Subagent call. The discarded bool is PRESENCE (an absent id
	// yields ""), not a swallowed error.
	tuid, _ := loop.ToolUseIDFrom(ctx)
	req.ParentToolUseID = tuid

	result, err := s.controller.Execute(ctx, req)
	if err != nil {
		return tool.TextResult("error: subagent failed: " + err.Error()), nil
	}
	return tool.TextResult(formatResult(req, result)), nil
}

func decodeSubagentArgs(input string) (SubagentArgs, map[string]json.RawMessage, error) {
	var args SubagentArgs
	decoder := json.NewDecoder(bytes.NewBufferString(input))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&args); err != nil {
		return SubagentArgs{}, nil, err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return SubagentArgs{}, nil, fmt.Errorf("trailing JSON")
	}
	var present map[string]json.RawMessage
	if err := json.Unmarshal([]byte(input), &present); err != nil {
		return SubagentArgs{}, nil, err
	}
	return args, present, nil
}

func validateEnvelopeFields(action SubagentAction, present map[string]json.RawMessage) string {
	allowed := map[SubagentAction]map[string]struct{}{
		actionStart:     fields("action", "agent", "mode", "message", "wait", "timeout_seconds"),
		actionSend:      fields("action", "delegate_id", "message", "wait", "timeout_seconds"),
		actionWait:      fields("action", "delegate_id", "request_id", "timeout_seconds"),
		actionInterrupt: fields("action", "delegate_id"),
		actionStatus:    fields("action", "delegate_id"),
	}
	set, ok := allowed[action]
	if !ok {
		return ""
	}
	for name := range present {
		if _, ok := set[name]; !ok {
			return "error: field " + strconv.Quote(name) + " is forbidden for action " + strconv.Quote(string(action))
		}
	}
	return ""
}

func fields(names ...string) map[string]struct{} {
	result := make(map[string]struct{}, len(names))
	for _, name := range names {
		result[name] = struct{}{}
	}
	return result
}

// buildDelegateRequest validates the envelope for the selected action and returns the
// typed request, or a non-empty error string on a boundary rejection (fail-secure: a
// rejected envelope never reaches the controller).
func buildDelegateRequest(action SubagentAction, args SubagentArgs) (tool.DelegateRequest, string) {
	if args.TimeoutSeconds != nil && *args.TimeoutSeconds < 0 {
		return tool.DelegateRequest{}, "error: 'timeout_seconds' must be non-negative"
	}
	req := tool.DelegateRequest{
		Agent:          string(args.Agent),
		Mode:           string(args.Mode),
		Message:        args.Message,
		RequestID:      args.RequestID,
		TimeoutSeconds: args.TimeoutSeconds,
	}
	if args.DelegateID != nil {
		req.DelegateID = *args.DelegateID
	}
	switch action {
	case actionStart:
		if strings.TrimSpace(string(args.Agent)) == "" {
			return tool.DelegateRequest{}, "error: a non-empty 'agent' is required for start"
		}
		if strings.TrimSpace(args.Message) == "" {
			return tool.DelegateRequest{}, "error: a non-empty 'message' is required for start"
		}
		req.Operation = tool.DelegateStart
		req.Wait = waitOrDefault(args.Wait, true)
	case actionSend:
		if req.DelegateID.IsZero() {
			return tool.DelegateRequest{}, "error: a 'delegate_id' is required for send"
		}
		if strings.TrimSpace(args.Message) == "" {
			return tool.DelegateRequest{}, "error: a non-empty 'message' is required for send"
		}
		req.Operation = tool.DelegateSend
		req.Wait = waitOrDefault(args.Wait, true)
	case actionWait:
		if req.DelegateID.IsZero() {
			return tool.DelegateRequest{}, "error: a 'delegate_id' is required for wait"
		}
		if args.RequestID == nil || args.RequestID.IsZero() {
			return tool.DelegateRequest{}, "error: a non-zero 'request_id' is required for wait"
		}
		req.Operation = tool.DelegateWait
		req.Wait = true
	case actionInterrupt:
		if req.DelegateID.IsZero() {
			return tool.DelegateRequest{}, "error: a 'delegate_id' is required for interrupt"
		}
		req.Operation = tool.DelegateInterrupt
	case actionStatus:
		req.Operation = tool.DelegateStatus
	default:
		return tool.DelegateRequest{}, "error: unknown action " + strconv.Quote(string(action))
	}
	return req, ""
}

func waitOrDefault(wait *bool, def bool) bool {
	if wait == nil {
		return def
	}
	return *wait
}

// formatResult renders the controller's typed result as the model-facing tool string.
func formatResult(req tool.DelegateRequest, result tool.DelegateResult) string {
	switch req.Operation {
	case tool.DelegateStart, tool.DelegateSend:
		if req.Wait {
			return formatWaited(result)
		}
		return formatQueued(result)
	case tool.DelegateWait:
		return formatWaited(result)
	case tool.DelegateInterrupt:
		return `{"delegate_id":` + strconv.Quote(result.DelegateID.String()) + `,"status":` + strconv.Quote(statusLabel(result.Status)) + `}`
	case tool.DelegateStatus:
		return formatStatus(result)
	default:
		return "error: subagent returned an unexpected operation"
	}
}

// formatWaited maps a resolved terminal status onto the answer text or a typed error.
func formatWaited(result tool.DelegateResult) string {
	switch result.Status {
	case tool.DelegateStatusCompleted:
		return result.Output
	case tool.DelegateStatusFailed:
		return "error: delegate failed"
	case tool.DelegateStatusInterrupted:
		return "error: delegate interrupted"
	case tool.DelegateStatusTimedOut:
		return "error: delegate timed out"
	default:
		return "error: delegate returned invalid status: " + statusLabel(result.Status)
	}
}

func formatQueued(result tool.DelegateResult) string {
	return `{"delegate_id":` + strconv.Quote(result.DelegateID.String()) +
		`,"request_id":` + strconv.Quote(result.RequestID.String()) +
		`,"status":"queued"}`
}

// formatStatus renders bounded mechanical status only (state + pending counts) — never
// a raw event cursor or child transcript. A per-child list (delegate_id omitted) is
// rendered when Children is populated.
func formatStatus(result tool.DelegateResult) string {
	if result.Children != nil {
		var b strings.Builder
		b.WriteString(`{"children":[`)
		for i, child := range result.Children {
			if i > 0 {
				b.WriteString(",")
			}
			b.WriteString(`{"delegate_id":` + strconv.Quote(child.DelegateID.String()) +
				`,"status":` + strconv.Quote(statusLabel(child.Status)) +
				`,"pending_requests":` + strconv.Itoa(child.PendingRequests) + `}`)
		}
		b.WriteString(`]}`)
		return b.String()
	}
	return `{"delegate_id":` + strconv.Quote(result.DelegateID.String()) +
		`,"status":` + strconv.Quote(statusLabel(result.Status)) +
		`,"pending_requests":` + strconv.Itoa(result.PendingRequests) + `}`
}

func statusLabel(status tool.DelegateStatusValue) string {
	switch status {
	case tool.DelegateStatusRunning:
		return "running"
	case tool.DelegateStatusIdle:
		return "idle"
	case tool.DelegateStatusCompleted:
		return "completed"
	case tool.DelegateStatusInterrupted:
		return "interrupted"
	case tool.DelegateStatusFailed:
		return "faulted"
	case tool.DelegateStatusTimedOut:
		return "timed_out"
	case tool.DelegateStatusQueued:
		return "queued"
	default:
		return "unknown"
	}
}

// compile-time assertions: SubagentTool is an InvokableTool and Auditable. It is
// deliberately NOT a WriteTarget, and its preparation yields an empty request
// (delegation is auto-approved; the child's own gate governs its tools).
var (
	_ tool.InvokableTool = (*SubagentTool)(nil)
	_ tool.CallPreparer  = (*SubagentTool)(nil)
	_ tool.Auditable     = (*SubagentTool)(nil)
)
