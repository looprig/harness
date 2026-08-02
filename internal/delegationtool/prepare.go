package delegationtool

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/identity"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/tool"
	inferencemodel "github.com/looprig/inference/model"
)

const (
	maxSubagentArgsBytes = 256 << 10
	maxDescriptionBytes  = 256
	maxPromptBytes       = 192 << 10
	maxTimeoutSeconds    = 24 * 60 * 60
)

// Preparation error categories are deliberately short and closed. Their strings
// are the complete model-facing error; decoder details and untrusted values never
// escape this package.
const (
	errCategoryOversized       = "oversized"
	errCategoryMalformed       = "malformed"
	errCategoryUnknownField    = "unknown_field"
	errCategoryFieldNotAllowed = "field_not_allowed"
	errCategoryMissingField    = "missing_field"
	errCategoryInvalidValue    = "invalid_value"
	errCategoryUnknownRuntime  = "unknown_runtime"
)

var errPreparationSentinel = errors.New("subagent preparation rejected")

type preparationError struct{ category string }

func (e *preparationError) Error() string {
	return errPreparationSentinel.Error() + ": " + e.category
}

func (e *preparationError) Unwrap() error { return errPreparationSentinel }

func preparationFailure(category string) error { return &preparationError{category: category} }

// SubagentEnvelope is the normalized, strictly validated model-facing call.
// Runtime selectors remain opaque strings here; Task 21 resolves them against a
// parent-scoped catalog. A nil Effort means that effort was omitted, while a
// non-nil pointer preserves an explicit "none" selection.
type SubagentEnvelope struct {
	Action          SubagentAction
	Description     string
	Prompt          string
	SubagentType    string
	Mode            string
	AgentHarness    string
	Model           string
	Effort          *string
	RunInBackground bool
	DelegateID      *uuid.UUID
	RequestID       *uuid.UUID
	TimeoutSeconds  *int

	// Selector presence is kept separately from the normalized string values so
	// an explicit selector can never be confused with an omitted selector.
	agentHarnessSet bool
	modelSet        bool
	effortSet       bool
}

// wireEnvelope is intentionally the complete and only accepted JSON surface.
// Do not add compatibility aliases here: the old agent/message/wait envelope is
// being hard-replaced in a later task.
type wireEnvelope struct {
	Action          SubagentAction `json:"action,omitempty"`
	Description     string         `json:"description,omitempty"`
	Prompt          string         `json:"prompt,omitempty"`
	SubagentType    string         `json:"subagent_type,omitempty"`
	Mode            string         `json:"mode,omitempty"`
	AgentHarness    string         `json:"agent_harness,omitempty"`
	Model           string         `json:"model,omitempty"`
	Effort          *string        `json:"effort,omitempty"`
	RunInBackground *bool          `json:"run_in_background,omitempty"`
	DelegateID      *string        `json:"delegate_id,omitempty"`
	RequestID       *string        `json:"request_id,omitempty"`
	TimeoutSeconds  *int           `json:"timeout_seconds,omitempty"`
}

func fields(names ...string) map[string]struct{} {
	result := make(map[string]struct{}, len(names))
	for _, name := range names {
		result[name] = struct{}{}
	}
	return result
}

// prepareEnvelope performs the untrusted preparation boundary for a Subagent
// call. It is deliberately independent of the controller and runtime catalog so
// the same normalized artifact can be extended by Task 21 without re-decoding.
func prepareEnvelope(argsJSON string) (SubagentEnvelope, error) {
	if len(argsJSON) > maxSubagentArgsBytes {
		return SubagentEnvelope{}, preparationFailure(errCategoryOversized)
	}

	raw, err := oneJSONValue([]byte(argsJSON))
	if err != nil {
		return SubagentEnvelope{}, preparationFailure(errCategoryMalformed)
	}
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return SubagentEnvelope{}, preparationFailure(errCategoryMalformed)
	}

	present := make(map[string]json.RawMessage)
	if err := json.Unmarshal(trimmed, &present); err != nil || present == nil {
		return SubagentEnvelope{}, preparationFailure(errCategoryMalformed)
	}

	var wire wireEnvelope
	decoder := json.NewDecoder(bytes.NewReader(trimmed))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&wire); err != nil {
		if strings.Contains(err.Error(), "unknown field") {
			return SubagentEnvelope{}, preparationFailure(errCategoryUnknownField)
		}
		if strings.Contains(err.Error(), "delegate_id") || strings.Contains(err.Error(), "request_id") {
			return SubagentEnvelope{}, preparationFailure(errCategoryInvalidValue)
		}
		return SubagentEnvelope{}, preparationFailure(errCategoryMalformed)
	}

	action := wire.Action
	if _, supplied := present["action"]; !supplied {
		action = actionStart
	} else if action == "" {
		return SubagentEnvelope{}, preparationFailure(errCategoryInvalidValue)
	}
	if !knownSubagentAction(action) {
		return SubagentEnvelope{}, preparationFailure(errCategoryInvalidValue)
	}
	delegateID, err := parseWireUUID(wire.DelegateID)
	if err != nil {
		return SubagentEnvelope{}, preparationFailure(errCategoryInvalidValue)
	}
	requestID, err := parseWireUUID(wire.RequestID)
	if err != nil {
		return SubagentEnvelope{}, preparationFailure(errCategoryInvalidValue)
	}

	if err := validateAllowedFields(action, present); err != nil {
		return SubagentEnvelope{}, err
	}
	if err := validateRequiredFields(action, wire, delegateID, requestID, present); err != nil {
		return SubagentEnvelope{}, err
	}
	if err := validateBounds(action, wire, delegateID, requestID, present); err != nil {
		return SubagentEnvelope{}, err
	}

	background := true
	if wire.RunInBackground != nil {
		background = *wire.RunInBackground
	}
	_, agentHarnessSet := present["agent_harness"]
	_, modelSet := present["model"]
	_, effortSet := present["effort"]

	return SubagentEnvelope{
		Action:          action,
		Description:     wire.Description,
		Prompt:          wire.Prompt,
		SubagentType:    wire.SubagentType,
		Mode:            wire.Mode,
		AgentHarness:    wire.AgentHarness,
		Model:           wire.Model,
		Effort:          wire.Effort,
		RunInBackground: background,
		DelegateID:      delegateID,
		RequestID:       requestID,
		TimeoutSeconds:  wire.TimeoutSeconds,
		agentHarnessSet: agentHarnessSet,
		modelSet:        modelSet,
		effortSet:       effortSet,
	}, nil
}

func oneJSONValue(input []byte) (json.RawMessage, error) {
	decoder := json.NewDecoder(bytes.NewReader(input))
	var raw json.RawMessage
	if err := decoder.Decode(&raw); err != nil {
		return nil, err
	}
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); err != io.EOF {
		return nil, errors.New("trailing JSON")
	}
	return raw, nil
}

func knownSubagentAction(action SubagentAction) bool {
	switch action {
	case actionStart, actionSend, actionWait, actionInterrupt, actionStatus:
		return true
	default:
		return false
	}
}

func validateAllowedFields(action SubagentAction, present map[string]json.RawMessage) error {
	allowed := map[SubagentAction]map[string]struct{}{
		actionStart:     fields("action", "description", "prompt", "subagent_type", "mode", "agent_harness", "model", "effort", "run_in_background", "timeout_seconds"),
		actionSend:      fields("action", "prompt", "run_in_background", "timeout_seconds", "delegate_id"),
		actionWait:      fields("action", "delegate_id", "request_id", "timeout_seconds"),
		actionInterrupt: fields("action", "delegate_id"),
		actionStatus:    fields("action", "delegate_id"),
	}
	for name := range present {
		if _, ok := allowed[action][name]; !ok {
			return preparationFailure(errCategoryFieldNotAllowed)
		}
	}
	return nil
}

func validateRequiredFields(action SubagentAction, wire wireEnvelope, delegateID, requestID *uuid.UUID, present map[string]json.RawMessage) error {
	switch action {
	case actionStart:
		if err := requireNonBlank("description", wire.Description, present); err != nil {
			return err
		}
		if err := requireNonBlank("prompt", wire.Prompt, present); err != nil {
			return err
		}
		return requireNonBlank("subagent_type", wire.SubagentType, present)
	case actionSend:
		if err := requireUUID("delegate_id", delegateID, present); err != nil {
			return err
		}
		return requireNonBlank("prompt", wire.Prompt, present)
	case actionWait:
		if err := requireUUID("delegate_id", delegateID, present); err != nil {
			return err
		}
		return requireUUID("request_id", requestID, present)
	case actionInterrupt:
		return requireUUID("delegate_id", delegateID, present)
	}
	return validateOptionalUUID("delegate_id", delegateID, present)
}

func validateBounds(action SubagentAction, wire wireEnvelope, delegateID, requestID *uuid.UUID, present map[string]json.RawMessage) error {
	if action == actionStart || action == actionSend {
		if len(wire.Description) > maxDescriptionBytes || len(wire.Prompt) > maxPromptBytes {
			return preparationFailure(errCategoryInvalidValue)
		}
	} else if len(wire.Prompt) > maxPromptBytes || len(wire.Description) > maxDescriptionBytes {
		return preparationFailure(errCategoryInvalidValue)
	}

	if wire.Effort != nil {
		switch *wire.Effort {
		case "none", "low", "medium", "high", "max":
		default:
			return preparationFailure(errCategoryInvalidValue)
		}
	} else if _, supplied := present["effort"]; supplied {
		return preparationFailure(errCategoryInvalidValue)
	}

	if wire.TimeoutSeconds != nil {
		if *wire.TimeoutSeconds < 0 || *wire.TimeoutSeconds > maxTimeoutSeconds {
			return preparationFailure(errCategoryInvalidValue)
		}
	} else if _, supplied := present["timeout_seconds"]; supplied {
		return preparationFailure(errCategoryInvalidValue)
	}
	if action == actionStart || action == actionSend {
		background := true
		if wire.RunInBackground != nil {
			background = *wire.RunInBackground
		} else if _, supplied := present["run_in_background"]; supplied {
			return preparationFailure(errCategoryInvalidValue)
		}
		if background && wire.TimeoutSeconds != nil {
			return preparationFailure(errCategoryFieldNotAllowed)
		}
	}

	if err := validateOptionalUUID("delegate_id", delegateID, present); err != nil {
		return err
	}
	return validateOptionalUUID("request_id", requestID, present)
}

func requireNonBlank(name, value string, present map[string]json.RawMessage) error {
	if _, supplied := present[name]; !supplied {
		return preparationFailure(errCategoryMissingField)
	}
	if strings.TrimSpace(value) == "" {
		return preparationFailure(errCategoryInvalidValue)
	}
	return nil
}

func requireUUID(name string, value *uuid.UUID, present map[string]json.RawMessage) error {
	if _, supplied := present[name]; !supplied {
		return preparationFailure(errCategoryMissingField)
	}
	return validateOptionalUUID(name, value, present)
}

func validateOptionalUUID(name string, value *uuid.UUID, present map[string]json.RawMessage) error {
	if _, supplied := present[name]; !supplied {
		return nil
	}
	if value == nil || value.IsZero() {
		return preparationFailure(errCategoryInvalidValue)
	}
	return nil
}

func parseWireUUID(value *string) (*uuid.UUID, error) {
	if value == nil {
		return nil, nil
	}
	u, err := uuid.Parse(*value)
	if err != nil {
		return nil, err
	}
	return &u, nil
}

// prepareDelegateCall translates the validated envelope exactly once into the
// controller request and, for starts, resolves the optional runtime tuple from
// the immutable parent catalog.
func (s *SubagentTool) prepareDelegateCall(ctx context.Context, envelope SubagentEnvelope) (tool.DelegateRequest, *tool.DelegateRuntime, error) {
	request := tool.DelegateRequest{
		Agent:          envelope.SubagentType,
		Mode:           envelope.Mode,
		Message:        envelope.Prompt,
		RequestID:      envelope.RequestID,
		TimeoutSeconds: envelope.TimeoutSeconds,
	}
	if envelope.DelegateID != nil {
		request.DelegateID = *envelope.DelegateID
	}
	var runtime *tool.DelegateRuntime
	switch envelope.Action {
	case actionStart:
		request.Operation = tool.DelegateStart
		request.Wait = !envelope.RunInBackground
		var err error
		runtime, err = s.resolveDelegateRuntime(envelope)
		if err != nil {
			return tool.DelegateRequest{}, nil, err
		}
	case actionSend:
		request.Operation = tool.DelegateSend
		request.Wait = !envelope.RunInBackground
	case actionWait:
		request.Operation = tool.DelegateWait
		request.Wait = true
	case actionInterrupt:
		request.Operation = tool.DelegateInterrupt
	case actionStatus:
		request.Operation = tool.DelegateStatus
	default:
		return tool.DelegateRequest{}, nil, preparationFailure(errCategoryInvalidValue)
	}
	return request, runtime, nil
}

func (s *SubagentTool) resolveDelegateRuntime(envelope SubagentEnvelope) (*tool.DelegateRuntime, error) {
	if !s.hasRuntimeCatalog {
		// A zero catalog is the native/no-choice catalog. An explicitly supplied
		// selector must never be silently ignored when a caller bypasses the
		// composition seam.
		if envelope.agentHarnessSet || envelope.modelSet || envelope.effortSet {
			return nil, preparationFailure(errCategoryFieldNotAllowed)
		}
		return nil, nil
	}

	entries := s.runtimeCatalog.EntriesFor(identity.AgentName(envelope.SubagentType))
	if len(entries) == 0 {
		if !s.runtimeCatalog.HasEntries() {
			if envelope.agentHarnessSet || envelope.modelSet || envelope.effortSet {
				return nil, preparationFailure(errCategoryFieldNotAllowed)
			}
			return nil, nil
		}
		if !s.hasSubagentType(envelope.SubagentType) {
			return nil, preparationFailure(errCategoryUnknownRuntime)
		}
		if envelope.agentHarnessSet || envelope.modelSet || envelope.effortSet {
			return nil, preparationFailure(errCategoryFieldNotAllowed)
		}
		// A parent may expose runtime choices for other roles while this role is
		// native-only. Unknown entries must never leak across the role boundary.
		return nil, nil
	}

	if envelope.agentHarnessSet && !runtimeHarnessSelectable(entries) {
		return nil, preparationFailure(errCategoryFieldNotAllowed)
	}
	selected := entries[0]
	if !envelope.agentHarnessSet {
		for _, entry := range entries {
			if entry.Default {
				selected = entry
				break
			}
		}
	} else {
		found := false
		for _, entry := range entries {
			if string(entry.AgentHarness) == envelope.AgentHarness {
				selected = entry
				found = true
				break
			}
		}
		if !found {
			return nil, preparationFailure(errCategoryUnknownRuntime)
		}
	}
	advertised := runtimeAdvertisedSelectors(entries, selected)

	if envelope.modelSet {
		if !advertised.Model {
			return nil, preparationFailure(errCategoryFieldNotAllowed)
		}
		found := false
		for _, option := range selected.Models {
			if string(option.Alias) == envelope.Model {
				found = true
				break
			}
		}
		if !found {
			return nil, preparationFailure(errCategoryUnknownRuntime)
		}
	}

	var effort inferencemodel.Effort
	if envelope.effortSet {
		if !advertised.Effort {
			return nil, preparationFailure(errCategoryFieldNotAllowed)
		}
		effort = parseDelegateEffort(*envelope.Effort)
	}
	resolved, err := s.runtimeCatalog.ResolveWithExplicitEffort(
		identity.AgentName(envelope.SubagentType),
		loop.AgentHarnessName(envelope.AgentHarness),
		loop.ModelAlias(envelope.Model),
		effort,
		envelope.effortSet,
	)
	if err != nil {
		var catalogErr *loop.RuntimeCatalogError
		if errors.As(err, &catalogErr) && catalogErr.Kind != loop.RuntimeCatalogIncompatibleEffort {
			return nil, preparationFailure(errCategoryUnknownRuntime)
		}
		return nil, preparationFailure(errCategoryUnknownRuntime)
	}

	return &tool.DelegateRuntime{
		Harness:    string(resolved.AgentHarness),
		Profile:    string(resolved.Profile),
		Model:      string(resolved.ModelAlias),
		SmallModel: string(resolved.SmallModel),
		Effort:     delegateEffortString(resolved.Effort),
		Explicit: tool.DelegateRuntimeExplicit{
			Harness: envelope.agentHarnessSet,
			Model:   envelope.modelSet,
			Effort:  envelope.effortSet,
		},
		Advertised: tool.DelegateRuntimeAdvertised{
			Harness: advertised.Harness,
			Model:   advertised.Model,
			Effort:  advertised.Effort,
		},
	}, nil
}

func (s *SubagentTool) hasSubagentType(agent string) bool {
	for _, entry := range s.catalog {
		if string(entry.Name) == agent {
			return true
		}
	}
	return false
}

func delegateEffortString(effort inferencemodel.Effort) string {
	if effort == inferencemodel.EffortNone {
		return "none"
	}
	return string(effort)
}

func parseDelegateEffort(value string) inferencemodel.Effort {
	if value == "none" {
		return inferencemodel.EffortNone
	}
	return inferencemodel.Effort(value)
}
