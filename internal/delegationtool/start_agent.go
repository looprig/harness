package delegationtool

import (
	"context"
	"encoding/json"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/tool"
)

const startAgentToolName = "StartAgent"

type StartAgentTool struct {
	controller tool.DelegateController
	config     *agentToolConfig
}

func newStartAgent(controller tool.DelegateController, config *agentToolConfig) *StartAgentTool {
	return &StartAgentTool{controller: controller, config: config}
}

func NewStartAgent(controller tool.DelegateController, style loop.DelegationStyle, catalog []AgentCatalogEntry, runtimeCatalog ...loop.RuntimeCatalog) *StartAgentTool {
	return newStartAgent(controller, newAgentToolConfig(style, catalog, runtimeCatalog...))
}

func (s *StartAgentTool) Info(context.Context) (*tool.ToolInfo, error) {
	return &tool.ToolInfo{Name: startAgentToolName, Desc: buildStartAgentDescription(s.config.catalog, s.config.runtimeCatalog), Schema: json.RawMessage(s.config.schema())}, nil
}

func (*StartAgentTool) AuditSummary(string) string { return startAgentToolName }

func (s *StartAgentTool) PrepareCall(ctx context.Context, _ uuid.UUID, argsJSON string) (tool.Request, tool.PreparedArtifact, error) {
	envelope, err := prepareEnvelope(argsJSON)
	if err != nil || envelope.Action != actionStart {
		if err == nil {
			err = preparationFailure(errCategoryInvalidValue)
		}
		return tool.Request{}, nil, err
	}
	if s.config.style == loop.DelegationSyncOnly && envelope.RunInBackground {
		return tool.Request{}, nil, preparationFailure(errCategoryInvalidValue)
	}
	runtime, err := s.config.resolveDelegateRuntime(envelope)
	if err != nil {
		return tool.Request{}, nil, err
	}
	request := tool.DelegateRequest{Operation: tool.DelegateStart, Agent: envelope.SubagentType, Mode: envelope.Mode, Message: envelope.Prompt, Wait: !envelope.RunInBackground, TimeoutSeconds: envelope.TimeoutSeconds, Runtime: runtime}
	return tool.Request{}, tool.DelegateArtifact{Request: request, Runtime: runtime}, nil
}

func (s *StartAgentTool) InvokableRun(ctx context.Context, _ string) (*tool.ToolResult, error) {
	return executeAgentCall(ctx, s.controller, formatStartAgentResult)
}

func formatStartAgentResult(req tool.DelegateRequest, result tool.DelegateResult) string {
	if req.Wait {
		return formatWaited(result)
	}
	return formatQueued(result, req.Runtime)
}

var _ tool.InvokableTool = (*StartAgentTool)(nil)
var _ tool.CallPreparer = (*StartAgentTool)(nil)
var _ tool.Auditable = (*StartAgentTool)(nil)
