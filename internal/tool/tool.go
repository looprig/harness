// Package tool defines the dependency-free contract surface for the tools
// subsystem: the BaseTool/InvokableTool interfaces every tool implements, the
// ToolResult value tools return, and the optional capability interfaces the
// runner probes for via type assertion. It imports only internal/content (plus
// stdlib); it must never import internal/agent/loop or tools/, so both can
// depend on it without a cycle.
package tool

import (
	"context"
	"encoding/json"

	"github.com/inventivepotter/urvi/internal/content"
)

// ToolInfo is a tool's self-description. Schema is a JSON Schema describing the
// tool's argument object; it maps 1:1 to llm.Tool.Schema.
type ToolInfo struct {
	Name   string
	Desc   string
	Schema json.RawMessage
}

// BaseTool is the minimal contract: every tool can describe itself. It is never
// widened — new behavior is added via separate optional capability interfaces,
// never folded into BaseTool (design Rule 1 / Open-Closed).
type BaseTool interface {
	Info(ctx context.Context) (*ToolInfo, error)
}

// InvokableTool is a BaseTool that can be executed with JSON-encoded arguments.
// argsJSON is the untrusted, model-supplied argument object; the implementation
// is responsible for parsing and validating it.
type InvokableTool interface {
	BaseTool
	InvokableRun(ctx context.Context, argsJSON string) (*ToolResult, error)
}

// ToolResult is the value an InvokableTool returns. Content must hold at least
// one block; the runner injects an "error: empty result" block when it is nil
// or empty. There is intentionally no Terminate field in v1 — a turn ends only
// via the model emitting no more tool calls or an abort.
type ToolResult struct {
	Content []content.Block
}

// TextResult is the convenience constructor for the common single-text-block
// result. It always returns a non-nil *ToolResult holding exactly one
// *content.TextBlock, even for the empty string.
func TextResult(s string) *ToolResult {
	return &ToolResult{Content: []content.Block{&content.TextBlock{Text: s}}}
}

// Optional capability interfaces. Each is a separate, focused interface (never
// folded into BaseTool — design Rule 1 / Open-Closed + Interface Segregation).
// The runner probes for each via type assertion; a tool implementing none of
// them is still a fully valid InvokableTool.

// Sequential is implemented by tools that must not run concurrently with other
// tool calls in the same batch. Sequential() reports whether this call must be
// serialized.
type Sequential interface {
	Sequential() bool
}

// PermissionPrompter is implemented by tools whose execution may require user
// approval. BuildRequest derives a sealed PermissionRequest from the
// (untrusted) argsJSON for the approval prompt; it returns an error when the
// args cannot be parsed into a request.
type PermissionPrompter interface {
	BuildRequest(argsJSON string) (PermissionRequest, error)
}

// Auditable is implemented by tools that can emit a redacted, length-capped
// one-line summary of a call for the ToolCallStarted audit event. The summary
// must never contain secrets, full file contents, headers, or request bodies.
type Auditable interface {
	AuditSummary(argsJSON string) string
}

// WriteTarget lets the runner group same-path mutations without importing the
// tools package: a write tool returns its resolved write path as key with
// ok=true, and the runner serializes calls sharing a key. ok=false means the
// call is not a write (no serialization). A non-nil err (e.g. unparseable args)
// is treated like invalid args: tool-result error, not executed, not grouped.
type WriteTarget interface {
	WriteTarget(argsJSON string) (key string, ok bool, err error)
}

// ToolExecuteFunc is the terminal/next step in a middleware chain: it runs the
// tool against argsJSON and returns its result.
type ToolExecuteFunc func(ctx context.Context, argsJSON string) (*ToolResult, error)

// ToolMiddleware wraps tool execution. It receives the tool, the (untrusted)
// argsJSON, and the next step in the chain; it may run logic before/after and
// must call next to proceed (or short-circuit by not calling it).
type ToolMiddleware func(ctx context.Context, t InvokableTool, argsJSON string, next ToolExecuteFunc) (*ToolResult, error)

// PermissionRequest (the sealed approval-prompt contract returned by
// PermissionPrompter.BuildRequest) and ApprovalScope are declared in
// permission_request.go alongside their concrete implementers.
