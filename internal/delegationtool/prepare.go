package delegationtool

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/identity"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/tool"
	inferencemodel "github.com/looprig/inference/model"
)

const (
	maxAgentArgsBytes        = 256 << 10
	maxAgentNameBytes        = 256
	maxAgentMessageBytes     = 192 << 10
	maxAgentTypeBytes        = 128
	maxAgentModeBytes        = 128
	maxTimeoutSeconds        = 24 * 60 * 60
	maxPreparationErrorBytes = 512
)

const (
	errCategoryOversized       = "oversized"
	errCategoryMalformed       = "malformed"
	errCategoryUnknownField    = "unknown_field"
	errCategoryFieldNotAllowed = "field_not_allowed"
	errCategoryMissingField    = "missing_field"
	errCategoryInvalidValue    = "invalid_value"
	errCategoryUnknownRuntime  = "unknown_runtime"
)

var errPreparationSentinel = errors.New("agent preparation rejected")

type preparationError struct {
	category string
	detail   string
}

func (e *preparationError) Error() string {
	detail := e.detail
	if detail == "" {
		detail = e.category
	}
	prefix := errPreparationSentinel.Error() + ": "
	return prefix + boundPreparationDiagnostic(detail, maxPreparationErrorBytes-len(prefix))
}
func (e *preparationError) Unwrap() error { return errPreparationSentinel }

func preparationFailure(category string) error {
	return &preparationError{category: category}
}

func missingFieldFailure(name string) error {
	return &preparationError{category: errCategoryMissingField, detail: "missing field " + strconv.Quote(name)}
}

func invalidFieldFailure(name string, value any) error {
	return &preparationError{category: errCategoryInvalidValue, detail: "invalid field " + strconv.Quote(name) + ": " + preparationValue(value)}
}

func invalidFieldWithoutValueFailure(name string) error {
	return &preparationError{category: errCategoryInvalidValue, detail: "invalid field " + strconv.Quote(name)}
}

func forbiddenFieldFailure(name string) error {
	return &preparationError{category: errCategoryFieldNotAllowed, detail: "field " + strconv.Quote(name) + " is not allowed"}
}

func unknownFieldFailure(name string) error {
	return &preparationError{category: errCategoryUnknownField, detail: "unknown field " + strconv.Quote(name)}
}

func malformedJSONFailure(err error) error {
	detail := "malformed JSON"
	if err != nil {
		detail += ": " + err.Error()
	}
	return &preparationError{category: errCategoryMalformed, detail: detail}
}

func unavailableSelectorFailure(detail string) error {
	return &preparationError{category: errCategoryUnknownRuntime, detail: detail}
}

func unavailableAgentTypeFailure(agentType string) error {
	return unavailableSelectorFailure("agent type " + strconv.Quote(agentType) + " is unavailable")
}

func unselectableAgentModeFailure(agentType string) error {
	return &preparationError{
		category: errCategoryFieldNotAllowed,
		detail:   "field \"agent_mode\" is not selectable for agent type " + strconv.Quote(agentType),
	}
}

func unavailableAgentModeFailure(mode, agentType string) error {
	return &preparationError{
		category: errCategoryInvalidValue,
		detail:   "agent mode " + strconv.Quote(mode) + " is unavailable for agent type " + strconv.Quote(agentType),
	}
}

func unavailableAgentHarnessFailure(harness, agentType string) error {
	return unavailableSelectorFailure("agent harness " + strconv.Quote(harness) + " is unavailable for agent type " + strconv.Quote(agentType))
}

func unavailableAgentSourceFailure(source, agentType, harness string) error {
	return unavailableSelectorFailure("agent source " + strconv.Quote(source) + " is unavailable for agent type " + strconv.Quote(agentType) + " and harness " + strconv.Quote(harness))
}

func unavailableModelFailure(model, agentType, harness, source string) error {
	return unavailableSelectorFailure("model " + strconv.Quote(model) + " is unavailable for agent type " + strconv.Quote(agentType) + ", harness " + strconv.Quote(harness) + ", and source " + strconv.Quote(source))
}

func unavailableEffortFailure(effort, model string) error {
	return unavailableSelectorFailure("effort " + strconv.Quote(effort) + " is unavailable for model " + strconv.Quote(model))
}

func preparationValue(value any) string {
	if value == nil {
		return "null"
	}
	if text, ok := value.(string); ok {
		return strconv.Quote(text)
	}
	return fmt.Sprint(value)
}

func boundPreparationDiagnostic(text string, maxBytes int) string {
	if maxBytes <= 0 {
		return ""
	}
	text = strings.ToValidUTF8(text, "\uFFFD")
	if len(text) <= maxBytes {
		return text
	}
	const marker = "..."
	limit := maxBytes - len(marker)
	if limit <= 0 {
		return marker[:maxBytes]
	}
	text = text[:limit]
	for !utf8.ValidString(text) {
		text = text[:len(text)-1]
	}
	return text + marker
}

type startAgentWire struct {
	AgentType       string  `json:"agent_type"`
	Name            string  `json:"name,omitempty"`
	Instructions    string  `json:"instructions"`
	WaitForResponse *bool   `json:"wait_for_response,omitempty"`
	TimeoutSeconds  *int    `json:"timeout_seconds,omitempty"`
	AgentHarness    string  `json:"agent_harness,omitempty"`
	AgentSource     string  `json:"agent_source,omitempty"`
	Model           string  `json:"model,omitempty"`
	Effort          *string `json:"effort,omitempty"`
	AgentMode       string  `json:"agent_mode,omitempty"`
}

type messageAgentWire struct {
	AgentID         *string `json:"agent_id"`
	Message         string  `json:"message"`
	WaitForResponse *bool   `json:"wait_for_response,omitempty"`
	TimeoutSeconds  *int    `json:"timeout_seconds,omitempty"`
}

type listAgentsWire struct {
	AgentID *string `json:"agent_id,omitempty"`
}

type stopAgentWire struct {
	AgentID *string `json:"agent_id"`
}

// PreparedStartAgent contains validated StartAgent arguments ready for delegation.
type PreparedStartAgent struct {
	AgentType       string
	Name            string
	Instructions    string
	WaitForResponse bool
	TimeoutSeconds  *int
	AgentHarness    string
	AgentSource     string
	Model           string
	Effort          *string
	AgentMode       string
	Runtime         *tool.DelegateRuntime

	agentHarnessSet bool
	agentSourceSet  bool
	modelSet        bool
	effortSet       bool
	agentModeSet    bool
}

// PreparedMessageAgent contains validated MessageAgent arguments ready for delegation.
type PreparedMessageAgent struct {
	AgentID         uuid.UUID
	Message         string
	WaitForResponse bool
	TimeoutSeconds  *int
}

// PreparedListAgents contains validated ListAgents arguments ready for delegation.
type PreparedListAgents struct{ AgentID *uuid.UUID }

// PreparedStopAgent contains validated StopAgent arguments ready for delegation.
type PreparedStopAgent struct{ AgentID uuid.UUID }

func prepareStartAgent(argsJSON string) (PreparedStartAgent, error) {
	var wire startAgentWire
	present, err := decodeStrictAgentJSON(argsJSON, &wire)
	if err != nil {
		return PreparedStartAgent{}, err
	}
	if err := requireText("agent_type", wire.AgentType, present, maxAgentTypeBytes); err != nil {
		return PreparedStartAgent{}, err
	}
	if err := requireText("instructions", wire.Instructions, present, maxAgentMessageBytes); err != nil {
		return PreparedStartAgent{}, err
	}
	if err := validateOptionalText("name", wire.Name, present, maxAgentNameBytes); err != nil {
		return PreparedStartAgent{}, err
	}
	if err := validateOptionalText("agent_harness", wire.AgentHarness, present, maxAgentTypeBytes); err != nil {
		return PreparedStartAgent{}, err
	}
	if err := validateOptionalText("agent_source", wire.AgentSource, present, maxAgentTypeBytes); err != nil {
		return PreparedStartAgent{}, err
	}
	if err := validateOptionalText("model", wire.Model, present, maxAgentTypeBytes); err != nil {
		return PreparedStartAgent{}, err
	}
	if err := validateOptionalText("agent_mode", wire.AgentMode, present, maxAgentModeBytes); err != nil {
		return PreparedStartAgent{}, err
	}
	if err := validateWaitAndTimeout(wire.WaitForResponse, wire.TimeoutSeconds, present); err != nil {
		return PreparedStartAgent{}, err
	}
	if wire.Effort != nil {
		switch *wire.Effort {
		case "none", "low", "medium", "high", "max":
		default:
			return PreparedStartAgent{}, invalidFieldFailure("effort", *wire.Effort)
		}
	} else if _, supplied := present["effort"]; supplied {
		return PreparedStartAgent{}, invalidFieldFailure("effort", nil)
	}
	return PreparedStartAgent{
		AgentType: wire.AgentType, Name: wire.Name, Instructions: wire.Instructions,
		WaitForResponse: waitForResponse(wire.WaitForResponse), TimeoutSeconds: wire.TimeoutSeconds,
		AgentHarness: wire.AgentHarness, AgentSource: wire.AgentSource, Model: wire.Model,
		Effort: wire.Effort, AgentMode: wire.AgentMode,
		agentHarnessSet: hasField(present, "agent_harness"), agentSourceSet: hasField(present, "agent_source"),
		modelSet: hasField(present, "model"), effortSet: hasField(present, "effort"), agentModeSet: hasField(present, "agent_mode"),
	}, nil
}

func prepareMessageAgent(argsJSON string) (PreparedMessageAgent, error) {
	var wire messageAgentWire
	present, err := decodeStrictAgentJSON(argsJSON, &wire)
	if err != nil {
		return PreparedMessageAgent{}, err
	}
	id, err := requireAgentID(wire.AgentID, present)
	if err != nil {
		return PreparedMessageAgent{}, err
	}
	if err := requireText("message", wire.Message, present, maxAgentMessageBytes); err != nil {
		return PreparedMessageAgent{}, err
	}
	if err := validateWaitAndTimeout(wire.WaitForResponse, wire.TimeoutSeconds, present); err != nil {
		return PreparedMessageAgent{}, err
	}
	return PreparedMessageAgent{AgentID: id, Message: wire.Message, WaitForResponse: waitForResponse(wire.WaitForResponse), TimeoutSeconds: wire.TimeoutSeconds}, nil
}

func prepareListAgents(argsJSON string) (PreparedListAgents, error) {
	var wire listAgentsWire
	present, err := decodeStrictAgentJSON(argsJSON, &wire)
	if err != nil {
		return PreparedListAgents{}, err
	}
	if !hasField(present, "agent_id") {
		return PreparedListAgents{}, nil
	}
	id, err := parseAgentID(wire.AgentID)
	if err != nil {
		return PreparedListAgents{}, err
	}
	return PreparedListAgents{AgentID: &id}, nil
}

func prepareStopAgent(argsJSON string) (PreparedStopAgent, error) {
	var wire stopAgentWire
	present, err := decodeStrictAgentJSON(argsJSON, &wire)
	if err != nil {
		return PreparedStopAgent{}, err
	}
	id, err := requireAgentID(wire.AgentID, present)
	if err != nil {
		return PreparedStopAgent{}, err
	}
	return PreparedStopAgent{AgentID: id}, nil
}

func decodeStrictAgentJSON(argsJSON string, destination any) (map[string]json.RawMessage, error) {
	if len(argsJSON) > maxAgentArgsBytes {
		return nil, preparationFailure(errCategoryOversized)
	}
	if !utf8.ValidString(argsJSON) {
		return nil, malformedJSONFailure(errors.New("input is not valid UTF-8"))
	}
	raw, err := oneJSONValue([]byte(argsJSON))
	if err != nil {
		return nil, malformedJSONFailure(err)
	}
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return nil, malformedJSONFailure(errors.New("expected JSON object"))
	}
	present := make(map[string]json.RawMessage)
	if err := json.Unmarshal(trimmed, &present); err != nil || present == nil {
		return nil, malformedJSONFailure(err)
	}
	decoder := json.NewDecoder(bytes.NewReader(trimmed))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		const unknownFieldPrefix = "json: unknown field "
		if encodedName, found := strings.CutPrefix(err.Error(), unknownFieldPrefix); found {
			if name, unquoteErr := strconv.Unquote(encodedName); unquoteErr == nil {
				return nil, unknownFieldFailure(name)
			}
		}
		return nil, malformedJSONFailure(err)
	}
	return present, nil
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

func waitForResponse(value *bool) bool { return value == nil || *value }
func hasField(present map[string]json.RawMessage, name string) bool {
	_, ok := present[name]
	return ok
}

func validateWaitAndTimeout(wait *bool, timeout *int, present map[string]json.RawMessage) error {
	if wait == nil && hasField(present, "wait_for_response") {
		return invalidFieldFailure("wait_for_response", nil)
	}
	if timeout == nil && hasField(present, "timeout_seconds") {
		return invalidFieldFailure("timeout_seconds", nil)
	}
	if timeout != nil && (*timeout < 0 || *timeout > maxTimeoutSeconds) {
		return invalidFieldFailure("timeout_seconds", *timeout)
	}
	return nil
}

func requireText(name, value string, present map[string]json.RawMessage, limit int) error {
	if !hasField(present, name) {
		return missingFieldFailure(name)
	}
	if strings.TrimSpace(value) == "" {
		if name == "instructions" || name == "message" {
			return invalidFieldWithoutValueFailure(name)
		}
		return invalidFieldFailure(name, value)
	}
	if len(value) > limit || !utf8.ValidString(value) {
		return invalidFieldWithoutValueFailure(name)
	}
	return nil
}

func validateOptionalText(name, value string, present map[string]json.RawMessage, limit int) error {
	if !hasField(present, name) {
		return nil
	}
	if strings.TrimSpace(value) == "" {
		return invalidFieldFailure(name, value)
	}
	if len(value) > limit || !utf8.ValidString(value) {
		return invalidFieldWithoutValueFailure(name)
	}
	return nil
}

func requireAgentID(value *string, present map[string]json.RawMessage) (uuid.UUID, error) {
	if !hasField(present, "agent_id") {
		return uuid.UUID{}, missingFieldFailure("agent_id")
	}
	return parseAgentID(value)
}

func parseAgentID(value *string) (uuid.UUID, error) {
	if value == nil || len(*value) > 36 || !utf8.ValidString(*value) {
		if value == nil {
			return uuid.UUID{}, invalidFieldFailure("agent_id", nil)
		}
		return uuid.UUID{}, invalidFieldFailure("agent_id", *value)
	}
	id, err := uuid.Parse(*value)
	if err != nil || id.IsZero() {
		return uuid.UUID{}, invalidFieldFailure("agent_id", *value)
	}
	return id, nil
}

func (s *agentToolConfig) prepareStartAgent(argsJSON string) (PreparedStartAgent, error) {
	prepared, err := prepareStartAgent(argsJSON)
	if err != nil {
		return PreparedStartAgent{}, err
	}
	if !s.hasAgentType(prepared.AgentType) {
		return PreparedStartAgent{}, unavailableAgentTypeFailure(prepared.AgentType)
	}
	if err := s.validateAgentMode(prepared); err != nil {
		return PreparedStartAgent{}, err
	}
	runtime, err := s.resolveDelegateRuntime(prepared)
	if err != nil {
		return PreparedStartAgent{}, err
	}
	prepared.Runtime = runtime
	return prepared, nil
}

func (s *agentToolConfig) validateAgentMode(prepared PreparedStartAgent) error {
	for _, role := range s.catalog {
		if string(role.Name) != prepared.AgentType {
			continue
		}
		if !prepared.agentModeSet {
			return nil
		}
		modes := selectableAgentModes(role.Modes)
		if len(modes) < 2 {
			return unselectableAgentModeFailure(prepared.AgentType)
		}
		for _, mode := range modes {
			if mode == prepared.AgentMode {
				return nil
			}
		}
		return unavailableAgentModeFailure(prepared.AgentMode, prepared.AgentType)
	}
	return unavailableAgentTypeFailure(prepared.AgentType)
}

func (s *agentToolConfig) resolveDelegateRuntime(prepared PreparedStartAgent) (*tool.DelegateRuntime, error) {
	if !s.hasRuntimeCatalog {
		if field := firstExplicitRuntimeField(prepared); field != "" {
			return nil, forbiddenFieldFailure(field)
		}
		return nil, nil
	}
	entries := s.runtimeCatalog.EntriesFor(identity.AgentName(prepared.AgentType))
	if len(entries) == 0 {
		if !s.runtimeCatalog.HasEntries() {
			if field := firstExplicitRuntimeField(prepared); field != "" {
				return nil, forbiddenFieldFailure(field)
			}
			return nil, nil
		}
		return nil, unavailableAgentTypeFailure(prepared.AgentType)
	}
	selected := runtimeDefaultEntry(entries)
	selectedHarness := selected.AgentHarness
	if prepared.agentHarnessSet {
		if !runtimeHarnessSelectable(entries) {
			return nil, forbiddenFieldFailure("agent_harness")
		}
		selectedHarness = loop.AgentHarnessName(prepared.AgentHarness)
		harnessEntries := runtimeEntriesForHarness(entries, selectedHarness)
		if len(harnessEntries) == 0 {
			return nil, unavailableAgentHarnessFailure(prepared.AgentHarness, prepared.AgentType)
		}
		selected = runtimeDefaultEntry(harnessEntries)
	}
	harnessEntries := runtimeEntriesForHarness(entries, selectedHarness)
	if prepared.agentSourceSet {
		if !runtimeSourceSelectableForEntries(harnessEntries) {
			return nil, forbiddenFieldFailure("agent_source")
		}
		var found bool
		selected, found = runtimeEntryForSource(harnessEntries, loop.RuntimeSourceName(prepared.AgentSource))
		if !found {
			return nil, unavailableAgentSourceFailure(prepared.AgentSource, prepared.AgentType, string(selectedHarness))
		}
	}
	if !prepared.agentSourceSet && runtimeSourceSelectableForEntries(harnessEntries) {
		if filtered, ok := runtimeEntryForSource([]loop.RuntimeCatalogEntry{selected}, selected.Source); ok {
			selected = filtered
		}
	}
	advertised := runtimeAdvertisedSelectors(entries, selected)
	if prepared.agentSourceSet && !advertised.Source {
		return nil, forbiddenFieldFailure("agent_source")
	}
	if prepared.modelSet {
		if !advertised.Model {
			return nil, forbiddenFieldFailure("model")
		}
		found := false
		for _, option := range selected.Models {
			if string(option.Alias) == prepared.Model {
				found = true
				break
			}
		}
		if !found {
			return nil, unavailableModelFailure(prepared.Model, prepared.AgentType, string(selectedHarness), string(selected.Source))
		}
	}
	var effort inferencemodel.Effort
	if prepared.effortSet {
		if !advertised.Effort {
			return nil, forbiddenFieldFailure("effort")
		}
		effort = parseDelegateEffort(*prepared.Effort)
		modelAlias := selected.DefaultModel
		if prepared.modelSet {
			modelAlias = loop.ModelAlias(prepared.Model)
		}
		for _, option := range selected.Models {
			if option.Alias == modelAlias && !runtimeOptionAllowsEffort(option, effort) {
				return nil, unavailableEffortFailure(*prepared.Effort, string(modelAlias))
			}
		}
	}
	selectedSource := loop.RuntimeSourceName("")
	if prepared.agentSourceSet {
		selectedSource = loop.RuntimeSourceName(prepared.AgentSource)
	}
	resolved, err := s.runtimeCatalog.ResolveWithExplicitSource(identity.AgentName(prepared.AgentType), selectedHarness, selectedSource, loop.ModelAlias(prepared.Model), effort, prepared.effortSet)
	if err != nil {
		return nil, unavailableSelectorFailure(err.Error())
	}
	runtimeModel := string(resolved.ModelAlias)
	runtimeEffort := delegateEffortString(resolved.Effort)
	if resolved.SelectionKind == loop.RuntimeSelectionHarnessManaged {
		runtimeModel, runtimeEffort = "", ""
	}
	return &tool.DelegateRuntime{
		Harness: string(resolved.AgentHarness), Profile: string(resolved.Profile), Source: string(resolved.Source), SelectionKind: string(resolved.SelectionKind),
		Model: runtimeModel, SmallModel: string(resolved.SmallModel), Effort: runtimeEffort,
		Explicit: tool.DelegateRuntimeExplicit{Harness: prepared.agentHarnessSet, Source: prepared.agentSourceSet, Model: prepared.modelSet, Effort: prepared.effortSet},
	}, nil
}

func firstExplicitRuntimeField(prepared PreparedStartAgent) string {
	switch {
	case prepared.agentHarnessSet:
		return "agent_harness"
	case prepared.agentSourceSet:
		return "agent_source"
	case prepared.modelSet:
		return "model"
	case prepared.effortSet:
		return "effort"
	default:
		return ""
	}
}

func runtimeOptionAllowsEffort(option loop.RuntimeModelOption, effort inferencemodel.Effort) bool {
	for _, available := range option.Efforts {
		if available == effort {
			return true
		}
	}
	return false
}

func (s *agentToolConfig) hasAgentType(agent string) bool {
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
