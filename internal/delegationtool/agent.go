package delegationtool

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/looprig/harness/pkg/identity"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/tool"
)

const maxAgentResultBytes = 256 << 10

// AgentCatalogEntry is one child capability advertised to StartAgent.
type AgentCatalogEntry struct {
	Name        identity.AgentName
	Description string
	Modes       []loop.ModeName
}

// agentToolConfig is an immutable, parent-scoped catalogue snapshot shared by
// the four tools built as one atomic bundle.
type agentToolConfig struct {
	style             loop.DelegationStyle
	catalog           []AgentCatalogEntry
	runtimeCatalog    loop.RuntimeCatalog
	hasRuntimeCatalog bool
}

func newAgentToolConfig(style loop.DelegationStyle, catalog []AgentCatalogEntry, runtimeCatalog ...loop.RuntimeCatalog) *agentToolConfig {
	config := &agentToolConfig{style: style, catalog: cloneAgentCatalog(catalog), hasRuntimeCatalog: true}
	if len(runtimeCatalog) > 0 {
		config.runtimeCatalog = runtimeCatalog[0]
	}
	return config
}

func cloneAgentCatalog(catalog []AgentCatalogEntry) []AgentCatalogEntry {
	result := append([]AgentCatalogEntry(nil), catalog...)
	for i := range result {
		result[i].Modes = append([]loop.ModeName(nil), result[i].Modes...)
	}
	return result
}

func (c *agentToolConfig) schema() string {
	return buildAgentTransitionSchema(c.style, c.catalog, c.runtimeCatalog)
}

func executeAgentCall(ctx context.Context, controller tool.DelegateController, format func(tool.DelegateRequest, tool.DelegateResult) string) (*tool.ToolResult, error) {
	prepared, ok := loop.PreparedCallFromContext(ctx)
	if !ok {
		return tool.TextResult("error: agent call unavailable"), nil
	}
	artifact, ok := prepared.Artifact.(tool.DelegateArtifact)
	if !ok {
		return tool.TextResult("error: agent call unavailable"), nil
	}
	req := artifact.Request
	req.Runtime = artifact.Runtime
	req.ParentToolUseID = ""
	if toolUseID, present := loop.ToolUseIDFrom(ctx); present {
		req.ParentToolUseID = toolUseID
	}
	result, err := controller.Execute(ctx, req)
	if err != nil {
		var modelFacing interface{ ModelFacingError() string }
		if errors.As(err, &modelFacing) {
			if message := modelFacing.ModelFacingError(); message != "" {
				return tool.TextResult("error: " + message), nil
			}
		}
		return tool.TextResult("error: agent request failed"), nil
	}
	return tool.TextResult(format(req, result)), nil
}

func formatWaited(result tool.DelegateResult) string {
	switch result.Status {
	case tool.DelegateStatusCompleted:
		return boundAgentOutput(result.Output)
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

func boundAgentOutput(value string) string {
	if len(value) <= maxAgentResultBytes {
		return value
	}
	return value[:maxAgentResultBytes]
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

func marshalResult(value any) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		return "error: agent result unavailable"
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
