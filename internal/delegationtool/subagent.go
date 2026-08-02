package delegationtool

import (
	"context"
	"encoding/json"
	"errors"

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
// active delegation style (SyncOnly ⇒ only `start` with `run_in_background` fixed false; Managed ⇒
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

// subagentToolName is the EXACT tool name. The tool's PrepareCall returns an
// empty typed request (no requirements), so the combined access gate allows it
// without a prompt; there is no path or command boundary to classify.
const subagentToolName = "Subagent"

const maxSubagentResultBytes = 256 << 10

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
// name the model passes as {subagent_type} and a one-line description. The rig projects the
// parent definition's delegate set onto this at the composition root.
type SubagentCatalogEntry struct {
	Name        identity.AgentName
	Description string
	Modes       []loop.ModeName
}

const subagentDescPrefix = "Delegate a sub-task to an in-session child agent by name via one action envelope, and optionally wait for its response."

// SubagentTool drives parent-to-child delegation through one action envelope. It
// depends only on the narrow tool.DelegateController (DIP); the style and catalog are
// static construction config used to derive the model-facing schema and description.
type SubagentTool struct {
	controller tool.DelegateController
	style      loop.DelegationStyle
	catalog    []SubagentCatalogEntry

	runtimeCatalog    loop.RuntimeCatalog
	hasRuntimeCatalog bool
}

// NewSubagent constructs a SubagentTool bound to the parent-scoped controller, with the
// delegation style and delegate catalog derived from the parent definition at the
// composition root.
func NewSubagent(controller tool.DelegateController, style loop.DelegationStyle, catalog []SubagentCatalogEntry, runtimeCatalog ...loop.RuntimeCatalog) *SubagentTool {
	// A missing variadic argument is an explicit empty/native catalog, not a
	// legacy escape hatch. This keeps preparation fail-closed for runtime
	// selectors even when a product has no optional adapter profiles.
	s := &SubagentTool{controller: controller, style: style, catalog: cloneSubagentCatalog(catalog), hasRuntimeCatalog: true}
	if len(runtimeCatalog) > 0 {
		s.runtimeCatalog = runtimeCatalog[0]
	}
	return s
}

// NewSubagentWithRuntimeCatalog is the explicit construction path for the
// parent-scoped preparation boundary. NewSubagent remains source-compatible
// for native/legacy callers that do not provide a catalog.
func NewSubagentWithRuntimeCatalog(controller tool.DelegateController, style loop.DelegationStyle, catalog []SubagentCatalogEntry, runtimeCatalog loop.RuntimeCatalog) *SubagentTool {
	return NewSubagent(controller, style, catalog, runtimeCatalog)
}

func (s *SubagentTool) schema() string {
	return buildSubagentSchema(s.style, s.catalog, s.runtimeCatalog)
}

func cloneSubagentCatalog(catalog []SubagentCatalogEntry) []SubagentCatalogEntry {
	result := append([]SubagentCatalogEntry(nil), catalog...)
	for i := range result {
		result[i].Modes = append([]loop.ModeName(nil), result[i].Modes...)
	}
	return result
}

// subagentDesc renders the static prefix followed by an <available_subagents> block
// listing each catalog entry. An empty catalog renders just the prefix.
func (s *SubagentTool) subagentDesc() string {
	return buildSubagentDescription(s.catalog, s.runtimeCatalog)
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

// PrepareCall owns envelope validation and produces the typed delegation artifact.
// Delegation needs no OS capability, resource grant, or durable rule, so the
// combined gate still auto-allows it; execution only consumes the artifact.
func (s *SubagentTool) PrepareCall(ctx context.Context, _ uuid.UUID, argsJSON string) (tool.Request, tool.PreparedArtifact, error) {
	envelope, err := prepareEnvelope(argsJSON)
	if err != nil {
		return tool.Request{}, nil, err
	}
	if s.style == loop.DelegationSyncOnly && (envelope.Action != actionStart || envelope.RunInBackground) {
		return tool.Request{}, nil, preparationFailure(errCategoryInvalidValue)
	}
	delegateRequest, runtime, err := s.prepareDelegateCall(ctx, envelope)
	if err != nil {
		return tool.Request{}, nil, err
	}
	delegateRequest.Runtime = runtime
	return tool.Request{}, tool.DelegateArtifact{Request: delegateRequest, Runtime: runtime}, nil
}

// InvokableRun consumes only the runner-installed prepared artifact. Raw argsJSON is
// intentionally ignored: decoding at execution would create a second, untrusted
// interpretation of the call after the permission gate.
func (s *SubagentTool) InvokableRun(ctx context.Context, _ string) (*tool.ToolResult, error) {
	prepared, ok := loop.PreparedCallFromContext(ctx)
	if !ok {
		return tool.TextResult("error: subagent call unavailable"), nil
	}
	artifact, ok := prepared.Artifact.(tool.DelegateArtifact)
	if !ok {
		return tool.TextResult("error: subagent call unavailable"), nil
	}
	req := artifact.Request
	req.Runtime = artifact.Runtime
	// ParentToolUseID is an execution-context fact, not part of the prepared
	// model-facing envelope. Clear any stale value before adding the trusted
	// runner context value.
	req.ParentToolUseID = ""
	if toolUseID, present := loop.ToolUseIDFrom(ctx); present {
		req.ParentToolUseID = toolUseID
	}
	result, err := s.controller.Execute(ctx, req)
	if err != nil {
		var modelFacing interface{ ModelFacingError() string }
		if errors.As(err, &modelFacing) {
			if message := modelFacing.ModelFacingError(); message != "" {
				return tool.TextResult("error: " + message), nil
			}
		}
		return tool.TextResult("error: subagent request failed"), nil
	}
	return tool.TextResult(formatResult(req, result)), nil
}

// formatResult renders the controller's typed result as the model-facing tool string.
func formatResult(req tool.DelegateRequest, result tool.DelegateResult) string {
	switch req.Operation {
	case tool.DelegateStart, tool.DelegateSend:
		if req.Wait {
			return formatWaited(result)
		}
		return formatQueued(result, req.Runtime)
	case tool.DelegateWait:
		return formatWaited(result)
	case tool.DelegateInterrupt:
		return marshalResult(interruptResult{DelegateID: result.DelegateID.String(), Status: statusLabel(result.Status)})
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
		return boundSubagentOutput(result.Output)
	case tool.DelegateStatusFailed:
		return "error: delegate failed"
	case tool.DelegateStatusInterrupted:
		return "error: delegate interrupted"
	case tool.DelegateStatusTimedOut:
		return "error: delegate timed out"
	default:
		return "error: delegate returned invalid status"
	}
}

func boundSubagentOutput(value string) string {
	if len(value) <= maxSubagentResultBytes {
		return value
	}
	return value[:maxSubagentResultBytes]
}

func formatQueued(result tool.DelegateResult, runtime *tool.DelegateRuntime) string {
	var advertised *runtimeResult
	if runtime != nil && runtime.Advertised.Any() {
		advertised = &runtimeResult{}
		if runtime.Advertised.Harness {
			advertised.AgentHarness = runtime.Harness
		}
		if runtime.Advertised.Model {
			advertised.Model = runtime.Model
		}
		if runtime.Advertised.Effort {
			advertised.Effort = runtime.Effort
		}
	}
	return marshalResult(queuedResult{DelegateID: result.DelegateID.String(), RequestID: result.RequestID.String(), Status: "queued", Runtime: advertised})
}

// formatStatus renders bounded mechanical status only (state + pending counts) — never
// a raw event cursor or child transcript. A per-child list (delegate_id omitted) is
// rendered when Children is populated.
func formatStatus(result tool.DelegateResult) string {
	if result.Children != nil {
		children := make([]statusChildResult, len(result.Children))
		for i, child := range result.Children {
			children[i] = statusChildResult{DelegateID: child.DelegateID.String(), Status: statusLabel(child.Status), PendingRequests: child.PendingRequests}
		}
		return marshalResult(statusListResult{Children: children, Truncated: result.ChildrenTruncated})
	}
	return marshalResult(statusResult{DelegateID: result.DelegateID.String(), Status: statusLabel(result.Status), PendingRequests: result.PendingRequests})
}

type runtimeResult struct {
	AgentHarness string `json:"agent_harness,omitempty"`
	Model        string `json:"model,omitempty"`
	Effort       string `json:"effort,omitempty"`
}

type queuedResult struct {
	DelegateID string         `json:"delegate_id"`
	RequestID  string         `json:"request_id"`
	Status     string         `json:"status"`
	Runtime    *runtimeResult `json:"runtime,omitempty"`
}

type interruptResult struct {
	DelegateID string `json:"delegate_id"`
	Status     string `json:"status"`
}

type statusResult struct {
	DelegateID      string `json:"delegate_id"`
	Status          string `json:"status"`
	PendingRequests int    `json:"pending_requests"`
}

type statusChildResult struct {
	DelegateID      string `json:"delegate_id"`
	Status          string `json:"status"`
	PendingRequests int    `json:"pending_requests"`
}

type statusListResult struct {
	Children  []statusChildResult `json:"children"`
	Truncated bool                `json:"truncated,omitempty"`
}

func marshalResult(value any) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		return "error: subagent result unavailable"
	}
	return string(encoded)
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
