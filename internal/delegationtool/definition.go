package delegationtool

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/tool"
	inferencemodel "github.com/looprig/inference/model"
)

// Definition binds the harness-owned delegation control tool to one parent Loop.
func Definition(style loop.DelegationStyle, catalog []SubagentCatalogEntry, runtimeCatalog ...loop.RuntimeCatalog) tool.Definition {
	catalog = cloneSubagentCatalog(catalog)
	var snapshot loop.RuntimeCatalog
	hasSnapshot := len(runtimeCatalog) > 0
	if hasSnapshot {
		snapshot = runtimeCatalog[0]
	}
	return tool.NewDefinition(subagentToolName, tool.RequiresDelegateController, func(_ context.Context, bindings tool.Bindings) ([]tool.InvokableTool, error) {
		if !hasSnapshot {
			return []tool.InvokableTool{NewSubagent(bindings.Delegate, style, catalog)}, nil
		}
		return []tool.InvokableTool{NewSubagentWithRuntimeCatalog(bindings.Delegate, style, catalog, snapshot)}, nil
	})
}

// maxAvailableSubagentRows bounds the non-default rows rendered in the model-facing
// capability matrix. One deterministic default row per role is always retained;
// additional combinations are explicitly elided after this budget.
const maxAvailableSubagentRows = 64

func buildSubagentSchema(style loop.DelegationStyle, catalog []SubagentCatalogEntry, runtimeCatalog loop.RuntimeCatalog) string {
	fieldOrder := []string{"action", "description", "prompt", "subagent_type", "mode", "agent_harness", "agent_source", "model", "effort", "run_in_background", "delegate_id", "request_id", "timeout_seconds"}
	properties := map[string]any{
		"action":            map[string]any{"type": "string", "enum": []string{"start", "send", "wait", "interrupt", "status"}},
		"description":       map[string]any{"type": "string"},
		"prompt":            map[string]any{"type": "string"},
		"subagent_type":     map[string]any{"type": "string"},
		"mode":              map[string]any{"type": "string"},
		"run_in_background": map[string]any{"type": "boolean", "default": true},
		"delegate_id":       map[string]any{"type": "string"},
		"request_id":        map[string]any{"type": "string"},
		"timeout_seconds":   map[string]any{"type": "integer", "minimum": 0},
	}
	selectors := availableRuntimeSelectors(catalog, runtimeCatalog)
	if selectors.Harness {
		properties["agent_harness"] = map[string]any{"type": "string"}
	}
	if selectors.Source {
		properties["agent_source"] = map[string]any{"type": "string"}
	}
	if selectors.Model {
		properties["model"] = map[string]any{"type": "string"}
	}
	if selectors.Effort {
		properties["effort"] = map[string]any{"type": "string"}
	}
	startThen := map[string]any{
		"not":      map[string]any{"anyOf": requiredProperties([]string{"delegate_id", "request_id"})},
		"required": []string{"description", "prompt", "subagent_type"},
		"oneOf":    startRoleVariants(catalog, runtimeCatalog),
	}
	actionBranch := func(action string, required, allowed []string) map[string]any {
		allowedSet := map[string]struct{}{"action": {}}
		for _, name := range allowed {
			allowedSet[name] = struct{}{}
		}
		forbidden := make([]string, 0, len(fieldOrder))
		for _, name := range fieldOrder {
			if _, ok := allowedSet[name]; !ok {
				forbidden = append(forbidden, name)
			}
		}
		then := map[string]any{"not": map[string]any{"anyOf": requiredProperties(forbidden)}}
		if len(required) > 0 {
			then["required"] = required
		}
		return map[string]any{
			"if":   map[string]any{"required": []string{"action"}, "properties": map[string]any{"action": map[string]any{"const": action}}},
			"then": then,
		}
	}
	startAllowed := []string{"description", "prompt", "subagent_type", "mode", "run_in_background", "timeout_seconds"}
	if selectors.Harness {
		startAllowed = append(startAllowed, "agent_harness")
	}
	if selectors.Source {
		startAllowed = append(startAllowed, "agent_source")
	}
	if selectors.Model {
		startAllowed = append(startAllowed, "model")
	}
	if selectors.Effort {
		startAllowed = append(startAllowed, "effort")
	}
	startBranch := actionBranch("start", nil, startAllowed)
	startBranch["then"] = startThen
	defaultStartBranch := map[string]any{
		"if":   map[string]any{"not": map[string]any{"required": []string{"action"}}},
		"then": startThen,
	}
	branches := []any{
		startBranch,
		defaultStartBranch,
		actionBranch("send", []string{"delegate_id", "prompt"}, []string{"delegate_id", "prompt", "run_in_background", "timeout_seconds"}),
		actionBranch("wait", []string{"delegate_id", "request_id"}, []string{"delegate_id", "request_id", "timeout_seconds"}),
		actionBranch("interrupt", []string{"delegate_id"}, []string{"delegate_id"}),
		actionBranch("status", nil, []string{"delegate_id"}),
	}
	if style == loop.DelegationSyncOnly {
		properties["action"] = map[string]any{"type": "string", "enum": []string{"start"}}
		properties["run_in_background"] = map[string]any{"const": false}
		branches = branches[:2]
	}
	schema := map[string]any{"type": "object", "additionalProperties": false, "properties": properties, "allOf": branches}
	encoded, _ := json.Marshal(schema)
	return string(encoded)
}

func requiredProperties(names []string) []any {
	result := make([]any, len(names))
	for i, name := range names {
		result[i] = map[string]any{"required": []string{name}}
	}
	return result
}

func startRoleVariants(catalog []SubagentCatalogEntry, runtimeCatalog loop.RuntimeCatalog) []any {
	catalog = orderedSubagentCatalog(catalog)
	variants := make([]any, 0, len(catalog))
	for _, role := range catalog {
		entries := runtimeCatalog.EntriesFor(role.Name)
		if len(entries) == 0 {
			if runtimeCatalog.HasEntries() {
				continue
			}
			variants = append(variants, startRoleVariant(role, nil, false, false))
			continue
		}
		if !runtimeHarnessSelectable(entries) {
			defaultEntry := runtimeDefaultEntry(entries)
			variants = append(variants, startRoleChoiceForHarness(role, runtimeEntriesForHarness(entries, defaultEntry.AgentHarness), false, true))
			continue
		}
		defaultEntry := runtimeDefaultEntry(entries)
		harnesses := runtimeHarnessNames(entries)
		harnessBranches := []any{startRoleChoiceForHarness(role, runtimeEntriesForHarness(entries, defaultEntry.AgentHarness), false, true)}
		for _, harness := range harnesses {
			harnessEntries := runtimeEntriesForHarness(entries, harness)
			harnessBranches = append(harnessBranches, startRoleChoiceForHarness(role, harnessEntries, true, harness == defaultEntry.AgentHarness))
		}
		variants = append(variants, map[string]any{
			"type": "object",
			"properties": map[string]any{
				"agent_harness": map[string]any{"type": "string", "enum": harnesses},
			},
			"oneOf": harnessBranches,
		})
	}
	return variants
}

func startRoleVariant(role SubagentCatalogEntry, entry *loop.RuntimeCatalogEntry, explicitHarness, defaultBranch bool) map[string]any {
	return startRoleVariantWithSelectors(role, entry, explicitHarness, defaultBranch, false, false, false)
}

func startRoleChoiceForHarness(role SubagentCatalogEntry, entries []loop.RuntimeCatalogEntry, explicitHarness, defaultSource bool) any {
	if len(entries) == 0 {
		return startRoleVariant(role, nil, explicitHarness, !explicitHarness && defaultSource)
	}
	if runtimeSourceSelectableForEntries(entries) {
		return startRoleSourceVariants(role, entries, explicitHarness, defaultSource)
	}
	entry := runtimeDefaultEntry(entries)
	return startRoleVariantWithSelectors(role, &entry, explicitHarness, !explicitHarness && defaultSource, false, false, false)
}

func startRoleSourceVariants(role SubagentCatalogEntry, entries []loop.RuntimeCatalogEntry, explicitHarness, defaultSource bool) map[string]any {
	branches := make([]any, 0, len(entries)+1)
	if defaultSource {
		entry := runtimeDefaultEntry(entries)
		if filtered, ok := runtimeEntryForSource([]loop.RuntimeCatalogEntry{entry}, entry.Source); ok {
			entry = filtered
		}
		branches = append(branches, startRoleVariantWithSelectors(role, &entry, explicitHarness, !explicitHarness, false, true, true))
	}
	for _, source := range runtimeSourcesForEntries(entries) {
		entry, ok := runtimeEntryForSource(entries, source)
		if !ok {
			continue
		}
		branches = append(branches, startRoleVariantWithSelectors(role, &entry, explicitHarness, false, true, true, false))
	}
	return map[string]any{"oneOf": branches}
}

func startRoleVariantWithSelectors(role SubagentCatalogEntry, entry *loop.RuntimeCatalogEntry, explicitHarness, defaultHarness, explicitSource, sourceSelectable, defaultSource bool) map[string]any {
	properties := map[string]any{
		"action":            map[string]any{"const": "start"},
		"description":       map[string]any{"type": "string"},
		"prompt":            map[string]any{"type": "string"},
		"subagent_type":     map[string]any{"const": string(role.Name)},
		"run_in_background": map[string]any{"type": "boolean"},
		"timeout_seconds":   map[string]any{"type": "integer", "minimum": 0},
	}
	modes := make([]string, len(role.Modes))
	for i, mode := range role.Modes {
		modes[i] = string(mode)
	}
	if len(modes) > 0 {
		properties["mode"] = map[string]any{"type": "string", "enum": modes}
	}
	if entry != nil {
		if explicitHarness {
			properties["agent_harness"] = map[string]any{"const": string(entry.AgentHarness)}
		}
		if explicitSource {
			properties["agent_source"] = map[string]any{"const": string(entry.Source)}
		}
	}
	branch := map[string]any{"type": "object", "additionalProperties": false, "properties": properties}
	required := make([]string, 0, 2)
	notRequired := make([]string, 0, 2)
	if entry != nil {
		if runtimeModelEffortsVary(entry.Models) {
			properties["model"] = map[string]any{"type": "string"}
			properties["effort"] = map[string]any{"type": "string"}
			branch["oneOf"] = modelEffortVariants(entry.Models, entry.DefaultModel)
		} else {
			addModelAndEffort(properties, *entry)
		}
	}
	if explicitHarness {
		required = append(required, "agent_harness")
	} else if defaultHarness {
		notRequired = append(notRequired, "agent_harness")
	}
	if explicitSource {
		required = append(required, "agent_source")
	} else if sourceSelectable && defaultSource {
		notRequired = append(notRequired, "agent_source")
	}
	if len(notRequired) > 0 {
		branch["not"] = map[string]any{"anyOf": requiredProperties(notRequired)}
	}
	if len(required) > 0 {
		branch["required"] = required
	}
	return branch
}

func runtimeModelEffortsVary(models []loop.RuntimeModelOption) bool {
	if len(models) < 2 {
		return false
	}
	want := admittedEfforts(models[:1])
	for _, model := range models[1:] {
		if !equalStringSlices(want, admittedEfforts([]loop.RuntimeModelOption{model})) {
			return true
		}
	}
	return false
}

func equalStringSlices(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}

func modelEffortVariants(models []loop.RuntimeModelOption, defaultModel loop.ModelAlias) []any {
	defaultEfforts := admittedEfforts(models[:1])
	for _, model := range models {
		if model.Alias == defaultModel {
			defaultEfforts = admittedEfforts([]loop.RuntimeModelOption{model})
			break
		}
	}
	variants := []any{map[string]any{
		"not": map[string]any{"required": []string{"model"}},
		"properties": map[string]any{
			"effort": map[string]any{"type": "string", "enum": defaultEfforts},
		},
	}}
	for _, model := range models {
		variants = append(variants, map[string]any{
			"properties": map[string]any{
				"model":  map[string]any{"const": string(model.Alias)},
				"effort": map[string]any{"type": "string", "enum": admittedEfforts([]loop.RuntimeModelOption{model})},
			},
			"required": []string{"model"},
		})
	}
	return variants
}

func anyNonDefaultHarness(entries []loop.RuntimeCatalogEntry) bool {
	for _, entry := range entries {
		if !entry.Default {
			return true
		}
	}
	return false
}

type runtimeSelectorAvailability struct {
	Harness bool
	Source  bool
	Model   bool
	Effort  bool
}

func runtimeHarnessSelectable(entries []loop.RuntimeCatalogEntry) bool {
	return len(runtimeHarnessNames(entries)) > 1
}

func runtimeHarnessNames(entries []loop.RuntimeCatalogEntry) []loop.AgentHarnessName {
	seen := make(map[loop.AgentHarnessName]struct{}, len(entries))
	for _, entry := range entries {
		seen[entry.AgentHarness] = struct{}{}
	}
	result := make([]loop.AgentHarnessName, 0, len(seen))
	for harness := range seen {
		result = append(result, harness)
	}
	sort.Slice(result, func(i, j int) bool { return result[i] < result[j] })
	return result
}

func runtimeEntriesForHarness(entries []loop.RuntimeCatalogEntry, harness loop.AgentHarnessName) []loop.RuntimeCatalogEntry {
	result := make([]loop.RuntimeCatalogEntry, 0, len(entries))
	for _, entry := range entries {
		if entry.AgentHarness == harness {
			result = append(result, entry)
		}
	}
	return result
}

func runtimeEntrySources(entry loop.RuntimeCatalogEntry) []loop.RuntimeSourceName {
	seen := map[loop.RuntimeSourceName]struct{}{entry.Source: {}}
	for _, option := range entry.Models {
		source := option.Source
		if source == "" {
			source = entry.Source
		}
		seen[source] = struct{}{}
	}
	result := make([]loop.RuntimeSourceName, 0, len(seen))
	for source := range seen {
		if source != "" {
			result = append(result, source)
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i] < result[j] })
	return result
}

func runtimeSourcesForEntries(entries []loop.RuntimeCatalogEntry) []loop.RuntimeSourceName {
	seen := make(map[loop.RuntimeSourceName]struct{})
	for _, entry := range entries {
		for _, source := range runtimeEntrySources(entry) {
			seen[source] = struct{}{}
		}
	}
	result := make([]loop.RuntimeSourceName, 0, len(seen))
	for source := range seen {
		result = append(result, source)
	}
	sort.Slice(result, func(i, j int) bool { return result[i] < result[j] })
	return result
}

func runtimeSourceSelectableForEntries(entries []loop.RuntimeCatalogEntry) bool {
	return len(runtimeSourcesForEntries(entries)) > 1
}

func runtimeSourceSelectable(entries []loop.RuntimeCatalogEntry) bool {
	for _, harness := range runtimeHarnessNames(entries) {
		if runtimeSourceSelectableForEntries(runtimeEntriesForHarness(entries, harness)) {
			return true
		}
	}
	return false
}

func runtimeEntryForSource(entries []loop.RuntimeCatalogEntry, source loop.RuntimeSourceName) (loop.RuntimeCatalogEntry, bool) {
	for _, entry := range entries {
		if entry.Source == source {
			if filtered, ok := runtimeEntryViewForSource(entry, source); ok {
				return filtered, true
			}
		}
	}
	for _, entry := range entries {
		for _, candidate := range runtimeEntrySources(entry) {
			if candidate == source {
				if filtered, ok := runtimeEntryViewForSource(entry, source); ok {
					return filtered, true
				}
			}
		}
	}
	return loop.RuntimeCatalogEntry{}, false
}

// runtimeEntryViewForSource returns the catalog entry as seen by one source
// branch. A model option's source override is authoritative, so a mixed entry
// must not leak options from another source into this branch. The returned
// default model is also made source-local when the entry-level default belongs
// to a different source.
func runtimeEntryViewForSource(entry loop.RuntimeCatalogEntry, source loop.RuntimeSourceName) (loop.RuntimeCatalogEntry, bool) {
	if source == "" {
		return loop.RuntimeCatalogEntry{}, false
	}
	if len(entry.Models) == 0 {
		if entry.Source != source {
			return loop.RuntimeCatalogEntry{}, false
		}
		entry.Source = source
		return entry, true
	}

	filtered := make([]loop.RuntimeModelOption, 0, len(entry.Models))
	for _, option := range entry.Models {
		optionSource := option.Source
		if optionSource == "" {
			optionSource = entry.Source
		}
		if optionSource == source {
			filtered = append(filtered, option)
		}
	}
	if len(filtered) == 0 {
		return loop.RuntimeCatalogEntry{}, false
	}

	entry.Source = source
	entry.Models = filtered
	defaultBelongsToSource := false
	for _, option := range filtered {
		if option.Alias == entry.DefaultModel {
			defaultBelongsToSource = true
			break
		}
	}
	if !defaultBelongsToSource {
		entry.DefaultModel = filtered[0].Alias
	}
	return entry, true
}

func runtimeModelSelectable(entry loop.RuntimeCatalogEntry) bool {
	return len(entry.Models) > 1
}

func runtimeEffortSelectable(entry loop.RuntimeCatalogEntry) bool {
	return len(admittedEfforts(entry.Models)) > 1
}

func runtimeDefaultEntry(entries []loop.RuntimeCatalogEntry) loop.RuntimeCatalogEntry {
	if len(entries) == 0 {
		return loop.RuntimeCatalogEntry{}
	}
	for _, entry := range entries {
		if entry.Default {
			return entry
		}
	}
	return entries[0]
}

func runtimeAdvertisedSelectors(entries []loop.RuntimeCatalogEntry, selected loop.RuntimeCatalogEntry) runtimeSelectorAvailability {
	return runtimeSelectorAvailability{
		Harness: runtimeHarnessSelectable(entries),
		Source:  runtimeSourceSelectableForEntries(runtimeEntriesForHarness(entries, selected.AgentHarness)),
		Model:   runtimeModelSelectable(selected),
		Effort:  runtimeEffortSelectable(selected),
	}
}

func availableRuntimeSelectors(catalog []SubagentCatalogEntry, runtimeCatalog loop.RuntimeCatalog) runtimeSelectorAvailability {
	available := runtimeSelectorAvailability{}
	for _, role := range catalog {
		entries := runtimeCatalog.EntriesFor(role.Name)
		if len(entries) == 0 {
			continue
		}
		if runtimeHarnessSelectable(entries) {
			available.Harness = true
		}
		if runtimeSourceSelectable(entries) {
			available.Source = true
		}
		for _, entry := range entries {
			if runtimeModelSelectable(entry) {
				available.Model = true
			}
			if runtimeEffortSelectable(entry) {
				available.Effort = true
			}
		}
	}
	return available
}

func addModelAndEffort(properties map[string]any, entry loop.RuntimeCatalogEntry) {
	if len(entry.Models) > 1 {
		models := make([]string, len(entry.Models))
		for i, model := range entry.Models {
			models[i] = string(model.Alias)
		}
		properties["model"] = map[string]any{"type": "string", "enum": models}
	}
	efforts := admittedEfforts(entry.Models)
	if len(efforts) > 1 {
		properties["effort"] = map[string]any{"type": "string", "enum": efforts}
	}
}

func admittedEfforts(models []loop.RuntimeModelOption) []string {
	seen := make(map[string]struct{})
	efforts := make([]string, 0)
	for _, option := range models {
		for _, effort := range option.Efforts {
			value := string(effort)
			if effort == "" {
				value = "none"
			}
			if _, ok := seen[value]; ok {
				continue
			}
			seen[value] = struct{}{}
			efforts = append(efforts, value)
		}
	}
	sort.SliceStable(efforts, func(i, j int) bool { return effortOrder(efforts[i]) < effortOrder(efforts[j]) })
	return efforts
}

func effortOrder(effort string) int {
	switch effort {
	case "none":
		return 0
	case "low":
		return 1
	case "medium":
		return 2
	case "high":
		return 3
	case "max":
		return 4
	default:
		return 5
	}
}

type availableSubagentRow struct {
	role, description, harness, source, model, effort string
}

func buildSubagentDescription(catalog []SubagentCatalogEntry, runtimeCatalog loop.RuntimeCatalog) string {
	if len(catalog) == 0 {
		return subagentDescPrefix
	}
	catalog = orderedSubagentCatalog(catalog)
	rows := make([]availableSubagentRow, 0, len(catalog))
	nonDefault := make([]availableSubagentRow, 0)
	for _, role := range catalog {
		entries := runtimeCatalog.EntriesFor(role.Name)
		if len(entries) == 0 {
			if runtimeCatalog.HasEntries() {
				continue
			}
			rows = append(rows, availableSubagentRow{role: string(role.Name), description: role.Description, harness: "native", model: "default", effort: "default"})
			continue
		}
		defaultEntry := entries[0]
		defaultIndex := 0
		for index, entry := range entries {
			if entry.Default {
				defaultEntry = entry
				defaultIndex = index
				break
			}
		}
		if defaultEntry.SelectionKind == loop.RuntimeSelectionHarnessManaged {
			rows = append(rows, availableSubagentRow{
				role: string(role.Name), description: role.Description,
				harness: string(defaultEntry.AgentHarness), source: string(defaultEntry.Source),
			})
		} else {
			defaultModel := defaultEntry.DefaultModel
			defaultEffort := "none"
			for _, model := range defaultEntry.Models {
				if model.Alias == defaultModel {
					defaultEffort = string(model.DefaultEffort)
					if defaultEffort == "" {
						defaultEffort = "none"
					}
					break
				}
			}
			rows = append(rows, availableSubagentRow{role: string(role.Name), description: role.Description, harness: string(defaultEntry.AgentHarness), model: string(defaultModel), effort: defaultEffort})
		}
		for entryIndex, entry := range entries {
			if entry.SelectionKind == loop.RuntimeSelectionHarnessManaged {
				if entryIndex == defaultIndex {
					continue
				}
				nonDefault = append(nonDefault, availableSubagentRow{
					role: string(role.Name), description: role.Description,
					harness: string(entry.AgentHarness), source: string(entry.Source),
				})
				continue
			}
			for _, model := range entry.Models {
				efforts := model.Efforts
				if len(efforts) == 0 {
					efforts = []inferencemodel.Effort{model.DefaultEffort}
				}
				for _, effort := range efforts {
					value := string(effort)
					if value == "" {
						value = "none"
					}
					candidate := availableSubagentRow{role: string(role.Name), description: role.Description, harness: string(entry.AgentHarness), model: string(model.Alias), effort: value}
					if entryIndex == defaultIndex && candidate == rows[len(rows)-1] {
						continue
					}
					nonDefault = append(nonDefault, candidate)
				}
			}
		}
	}
	elided := len(nonDefault) > maxAvailableSubagentRows
	if elided {
		nonDefault = nonDefault[:maxAvailableSubagentRows]
	}
	var b strings.Builder
	b.WriteString(subagentDescPrefix)
	b.WriteString("\n<available_subagents>\n")
	for _, row := range append(rows, nonDefault...) {
		b.WriteString(fmt.Sprintf("- role=%s harness=%s", row.role, row.harness))
		if row.source != "" {
			b.WriteString(" source=")
			b.WriteString(row.source)
		}
		if row.model != "" {
			b.WriteString(" model=")
			b.WriteString(row.model)
		}
		if row.effort != "" {
			b.WriteString(" effort=")
			b.WriteString(row.effort)
		}
		if strings.TrimSpace(row.description) != "" {
			b.WriteString(": ")
			b.WriteString(row.description)
		}
		b.WriteString("\n")
	}
	if elided {
		b.WriteString("- <elided non-default runtime combinations>\n")
	}
	b.WriteString("</available_subagents>")
	return b.String()
}

func orderedSubagentCatalog(catalog []SubagentCatalogEntry) []SubagentCatalogEntry {
	ordered := cloneSubagentCatalog(catalog)
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].Name < ordered[j].Name })
	return ordered
}
