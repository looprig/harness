package delegationtool

import (
	"context"

	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/tool"
)

// Definition binds the harness-owned delegation control tool to one parent Loop.
func Definition(style loop.DelegationStyle, catalog []SubagentCatalogEntry) tool.Definition {
	catalog = cloneSubagentCatalog(catalog)
	return tool.NewDefinition(subagentToolName, tool.RequiresDelegateController, func(_ context.Context, bindings tool.Bindings) ([]tool.InvokableTool, error) {
		return []tool.InvokableTool{NewSubagent(bindings.Delegate, style, catalog)}, nil
	})
}
