package delegationtool

import (
	"context"
	"encoding/json"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/tool"
)

const listAgentsToolName = "ListAgents"

type ListAgentsTool struct {
	controller tool.DelegateController
	config     *agentToolConfig
}

func newListAgents(controller tool.DelegateController, config *agentToolConfig) *ListAgentsTool {
	return &ListAgentsTool{controller: controller, config: config}
}

func NewListAgents(controller tool.DelegateController, style loop.DelegationStyle, catalog []AgentCatalogEntry, runtimeCatalog ...loop.RuntimeCatalog) *ListAgentsTool {
	return newListAgents(controller, newAgentToolConfig(style, catalog, runtimeCatalog...))
}

func (s *ListAgentsTool) Info(context.Context) (*tool.ToolInfo, error) {
	return &tool.ToolInfo{Name: listAgentsToolName, Desc: "List child agents directly owned by this loop.", Schema: json.RawMessage(buildListAgentsSchema())}, nil
}

func (*ListAgentsTool) AuditSummary(string) string { return listAgentsToolName }

func (s *ListAgentsTool) PrepareCall(_ context.Context, _ uuid.UUID, argsJSON string) (tool.Request, tool.PreparedArtifact, error) {
	if s.config.style != loop.DelegationManaged {
		return tool.Request{}, nil, preparationFailure(errCategoryInvalidValue)
	}
	prepared, err := prepareListAgents(argsJSON)
	if err != nil {
		return tool.Request{}, nil, err
	}
	request := tool.DelegateRequest{Operation: tool.DelegateStatus}
	if prepared.AgentID != nil {
		request.DelegateID = *prepared.AgentID
	}
	return tool.Request{}, tool.DelegateArtifact{Request: request}, nil
}

func (s *ListAgentsTool) InvokableRun(ctx context.Context, _ string) (*tool.ToolResult, error) {
	return executeAgentCall(ctx, s.controller, tool.DelegateStatus, formatListAgentsResult)
}

func formatListAgentsResult(_ tool.DelegateRequest, result tool.DelegateResult) string {
	if result.Children != nil {
		children := make([]statusChildResult, len(result.Children))
		for i, child := range result.Children {
			children[i] = statusChildResult{DelegateID: child.DelegateID.String(), Status: statusLabel(child.Status), PendingRequests: child.PendingRequests}
		}
		return marshalResult(statusListResult{Children: children, Truncated: result.ChildrenTruncated})
	}
	return marshalResult(statusResult{DelegateID: result.DelegateID.String(), Status: statusLabel(result.Status), PendingRequests: result.PendingRequests})
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

var _ tool.InvokableTool = (*ListAgentsTool)(nil)
var _ tool.CallPreparer = (*ListAgentsTool)(nil)
var _ tool.Auditable = (*ListAgentsTool)(nil)
