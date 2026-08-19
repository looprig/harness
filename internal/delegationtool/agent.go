package delegationtool

import (
	"context"
	"encoding/json"
	"strings"
	"unicode/utf8"

	"github.com/looprig/core/content"
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

func (c *agentToolConfig) startSchema() string {
	return buildStartAgentSchema(c.style, c.catalog, c.runtimeCatalog)
}

func executeAgentCall(ctx context.Context, controller tool.DelegateController, operation tool.DelegateOperation, format func(tool.DelegateRequest, tool.DelegateResult) string) (*tool.ToolResult, error) {
	prepared, ok := loop.PreparedCallFromContext(ctx)
	if !ok {
		return agentFailureResult(operation, "agent call unavailable"), nil
	}
	artifact, ok := prepared.Artifact.(tool.DelegateArtifact)
	if !ok || artifact.Request.Operation != operation {
		return agentFailureResult(operation, "agent call unavailable"), nil
	}
	req := artifact.Request
	req.Runtime = artifact.Runtime
	req.ParentToolUseID = ""
	if toolUseID, present := loop.ToolUseIDFrom(ctx); present {
		req.ParentToolUseID = toolUseID
	}
	result, err := controller.Execute(ctx, req)
	if err != nil {
		return agentFailureResult(operation, err.Error()), nil
	}
	if detail, failed := terminalAgentFailure(req, result); failed {
		return agentFailureResult(operation, detail), nil
	}
	return tool.TextResult(format(req, result)), nil
}

func agentFailureResult(operation tool.DelegateOperation, detail string) *tool.ToolResult {
	message := agentOperationName(operation) + " failed"
	if detail != "" {
		message += ": " + detail
	}
	message = boundAgentOutput(strings.ToValidUTF8(message, "\uFFFD"))
	return &tool.ToolResult{Content: []content.Block{&content.ToolResultBlock{
		IsError: true,
		Content: []content.Block{&content.TextBlock{Text: message}},
	}}}
}

func agentOperationName(operation tool.DelegateOperation) string {
	switch operation {
	case tool.DelegateStart:
		return startAgentToolName
	case tool.DelegateSend:
		return messageAgentToolName
	case tool.DelegateStatus:
		return listAgentsToolName
	case tool.DelegateInterrupt:
		return stopAgentToolName
	default:
		return "AgentTool"
	}
}

func terminalAgentFailure(req tool.DelegateRequest, result tool.DelegateResult) (string, bool) {
	if !req.WaitForResponse || (req.Operation != tool.DelegateStart && req.Operation != tool.DelegateSend) {
		return "", false
	}
	if result.DeliveryStatus != "" {
		return "", false
	}
	switch result.ResponseStatus {
	case tool.DelegateResponseCompleted:
		return "", false
	case tool.DelegateResponseFailed:
		if result.Response != "" {
			return result.Response, true
		}
		return "agent failed", true
	case tool.DelegateResponseInterrupted:
		return "agent interrupted", true
	case tool.DelegateResponseTimedOut:
		return "agent timed out", true
	default:
		return "agent returned invalid response status", true
	}
}

func formatForeground(result tool.DelegateResult) string {
	if result.DeliveryStatus != "" {
		responseStatus := wireResponseStatus(result.ResponseStatus)
		var response *string
		if result.DeliveryStatus == tool.DelegateDeliveryUnknown || result.DeliveryStatus == tool.DelegateDeliveryUntrackable {
			responseStatus = ""
		} else if result.ResponseStatus == tool.DelegateResponseCompleted || (responseStatus != "" && result.Response != "") {
			response = &result.Response
		}
		return marshalForegroundResult(foregroundResult{
			AgentID:        result.AgentID.String(),
			Name:           result.Name,
			State:          result.State,
			DeliveryStatus: result.DeliveryStatus,
			ResponseStatus: responseStatus,
			Response:       response,
		})
	}
	switch result.ResponseStatus {
	case tool.DelegateResponseCompleted:
		return marshalForegroundResult(foregroundResult{AgentID: result.AgentID.String(), Name: result.Name, State: result.State, Response: &result.Response})
	case tool.DelegateResponseFailed:
		return "error: agent failed"
	case tool.DelegateResponseInterrupted:
		return "error: agent interrupted"
	case tool.DelegateResponseTimedOut:
		return "error: agent timed out"
	default:
		return "error: agent returned invalid response status"
	}
}

func boundAgentOutput(value string) string {
	if len(value) <= maxAgentResultBytes {
		return value
	}
	end := maxAgentResultBytes
	for end > 0 && !utf8.RuneStart(value[end]) {
		end--
	}
	return value[:end]
}

type foregroundResult struct {
	AgentID        string                      `json:"agent_id"`
	Name           string                      `json:"name"`
	State          tool.AgentState             `json:"state"`
	DeliveryStatus tool.DelegateDeliveryStatus `json:"delivery_status,omitempty"`
	ResponseStatus string                      `json:"response_status,omitempty"`
	Response       *string                     `json:"response,omitempty"`
}

type backgroundResult struct {
	AgentID        string                      `json:"agent_id"`
	Name           string                      `json:"name"`
	State          tool.AgentState             `json:"state"`
	DeliveryStatus tool.DelegateDeliveryStatus `json:"delivery_status,omitempty"`
}

func marshalForegroundResult(result foregroundResult) string {
	includeResponse := result.Response != nil
	response := ""
	if includeResponse {
		response = boundAgentOutput(*result.Response)
	}
	prefixEnds := make([]int, 1, len(response)+1)
	for end := 0; end < len(response); {
		_, size := utf8.DecodeRuneInString(response[end:])
		end += size
		prefixEnds = append(prefixEnds, end)
	}

	var best []byte
	low, high := 0, len(prefixEnds)
	for low < high {
		mid := low + (high-low)/2
		if includeResponse {
			candidate := response[:prefixEnds[mid]]
			result.Response = &candidate
		}
		encoded, err := json.Marshal(result)
		if err != nil {
			return "error: agent result unavailable"
		}
		if len(encoded) <= maxAgentResultBytes {
			best = encoded
			low = mid + 1
		} else {
			high = mid
		}
	}
	if best == nil {
		return "error: agent result unavailable"
	}
	return string(best)
}

func formatBackground(result tool.DelegateResult) string {
	return marshalResult(backgroundResult{AgentID: result.AgentID.String(), Name: result.Name, State: result.State, DeliveryStatus: result.DeliveryStatus})
}

func wireResponseStatus(status tool.DelegateResponseStatus) string {
	switch status {
	case tool.DelegateResponseCompleted:
		return "completed"
	case tool.DelegateResponseFailed:
		return "failed"
	case tool.DelegateResponseInterrupted:
		return "interrupted"
	case tool.DelegateResponseTimedOut:
		return "timed_out"
	default:
		return ""
	}
}

func marshalResult(value any) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		return "error: agent result unavailable"
	}
	return string(encoded)
}
