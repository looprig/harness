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
