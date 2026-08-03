package delegationtool

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/looprig/harness/pkg/identity"
	"github.com/looprig/harness/pkg/loop"
	inferencemodel "github.com/looprig/inference/model"
)

func TestNewEnvelopeSchemaUsesOnlyNewWireFields(t *testing.T) {
	info, err := NewSubagentWithRuntimeCatalog(&fakeController{}, loop.DelegationManaged, subagentCatalog(), emptyRuntimeCatalog(t)).Info(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var schema map[string]any
	if err := json.Unmarshal(info.Schema, &schema); err != nil {
		t.Fatal(err)
	}
	properties, ok := schema["properties"].(map[string]any)
	if !ok {
		t.Fatalf("schema properties = %T, want object", schema["properties"])
	}
	for _, old := range []string{"agent", "message", "wait"} {
		if _, present := properties[old]; present {
			t.Errorf("legacy property %q is present", old)
		}
	}
	for _, field := range []string{"action", "description", "prompt", "subagent_type", "mode", "run_in_background", "delegate_id", "request_id", "timeout_seconds"} {
		if _, present := properties[field]; !present {
			t.Errorf("new property %q is missing", field)
		}
	}
	for _, field := range []string{"agent_harness", "model", "effort"} {
		if _, present := properties[field]; present {
			t.Errorf("selector property %q is present without a runtime choice", field)
		}
	}
	if schema["additionalProperties"] != false {
		t.Fatal("schema must set additionalProperties:false")
	}
}

func TestSchemaRuntimeSelectorsFollowCapabilities(t *testing.T) {
	tests := []struct {
		name       string
		catalog    loop.RuntimeCatalog
		wantFields []string
		noFields   []string
		wantEnums  map[string][]string
	}{
		{
			name:     "single default harness and model",
			catalog:  schemaCatalog(t, schemaEntry("worker", "claude-code", true, []string{"sonnet"}, []inferencemodel.Effort{inferencemodel.EffortMedium})),
			noFields: []string{"agent_harness", "model", "effort"},
		},
		{
			name: "multiple harnesses only exposes harness",
			catalog: schemaCatalog(t,
				schemaEntry("worker", "claude-code", true, []string{"sonnet"}, []inferencemodel.Effort{inferencemodel.EffortMedium}),
				schemaEntry("worker", "codex", false, []string{"luna"}, []inferencemodel.Effort{inferencemodel.EffortHigh})),
			wantFields: []string{"agent_harness"},
			noFields:   []string{"model", "effort"},
			wantEnums:  map[string][]string{"agent_harness": {"claude-code", "codex"}},
		},
		{
			name:       "multiple models and efforts expose both selectors",
			catalog:    schemaCatalog(t, schemaEntryWithModels("worker", "claude-code", true, []schemaModel{{alias: "sonnet", efforts: []inferencemodel.Effort{inferencemodel.EffortLow, inferencemodel.EffortHigh}}, {alias: "opus", efforts: []inferencemodel.Effort{inferencemodel.EffortLow, inferencemodel.EffortHigh}}})),
			wantFields: []string{"model", "effort"},
			noFields:   []string{"agent_harness"},
			wantEnums:  map[string][]string{"model": {"opus", "sonnet"}, "effort": {"low", "high"}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			info, err := NewSubagentWithRuntimeCatalog(&fakeController{}, loop.DelegationManaged, []SubagentCatalogEntry{{Name: "worker", Description: "builds"}}, tt.catalog).Info(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			for field := range tt.wantEnums {
				if !strings.Contains(string(info.Schema), `"`+field+`"`) {
					t.Errorf("schema does not contain selector %q", field)
				}
			}
			assertSchemaFieldPresence(t, info.Schema, tt.wantFields, true)
			assertSchemaFieldPresence(t, info.Schema, tt.noFields, false)
			for field, want := range tt.wantEnums {
				if got := schemaEnumValues(t, info.Schema, field); !equalStrings(got, want) {
					t.Errorf("%s enum = %v, want %v", field, got, want)
				}
			}
		})
	}
}

func TestSchemaAndDescriptionOmitRolesMissingFromPopulatedCatalog(t *testing.T) {
	t.Parallel()
	roles := []SubagentCatalogEntry{{Name: "worker", Description: "builds"}, {Name: "reviewer", Description: "reviews"}}
	catalog := schemaCatalog(t, schemaEntry("worker", "claude-code", true, []string{"sonnet"}, []inferencemodel.Effort{inferencemodel.EffortMedium}))
	info, err := NewSubagentWithRuntimeCatalog(&fakeController{}, loop.DelegationManaged, roles, catalog).Info(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(info.Schema), "worker") || strings.Contains(string(info.Schema), "reviewer") {
		t.Fatalf("populated catalog schema = %s, want only catalogued role", info.Schema)
	}
	if !strings.Contains(info.Desc, "role=worker") || strings.Contains(info.Desc, "role=reviewer") {
		t.Fatalf("populated catalog description = %q, want only catalogued role", info.Desc)
	}

	empty := emptyRuntimeCatalog(t)
	nativeInfo, err := NewSubagentWithRuntimeCatalog(&fakeController{}, loop.DelegationManaged, roles, empty).Info(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(nativeInfo.Schema), "reviewer") || !strings.Contains(nativeInfo.Desc, "role=reviewer") {
		t.Fatalf("empty catalog native fallback omitted reviewer: schema=%s description=%q", nativeInfo.Schema, nativeInfo.Desc)
	}
}

func TestSchemaMixedSourcesAdvertisesAgentSourceWithoutManagedPlaceholders(t *testing.T) {
	gateway := schemaEntry("worker", "codex", true, []string{"luna"}, []inferencemodel.Effort{inferencemodel.EffortHigh})
	native := loop.RuntimeCatalogEntry{
		SubagentType: "worker", AgentHarness: "codex", Profile: "profile/codex-native",
		Credential: loop.CredentialNativeAuth, Source: loop.RuntimeSourceNative,
		SelectionKind: loop.RuntimeSelectionHarnessManaged,
	}
	catalog := schemaCatalog(t, gateway, native)
	info, err := NewSubagentWithRuntimeCatalog(&fakeController{}, loop.DelegationManaged, []SubagentCatalogEntry{{Name: "worker"}}, catalog).Info(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	assertSchemaFieldPresence(t, info.Schema, []string{"agent_source"}, true)
	if strings.Contains(info.Desc, "model=harness-managed") || strings.Contains(info.Desc, "effort=harness-managed") {
		t.Fatalf("managed description contains a placeholder: %q", info.Desc)
	}
	managedRow := "- role=worker harness=codex source=native"
	if !strings.Contains(info.Desc, managedRow) {
		t.Fatalf("description does not identify the managed native source %q: %s", managedRow, info.Desc)
	}
	for _, line := range strings.Split(info.Desc, "\n") {
		if !strings.HasPrefix(line, managedRow) {
			continue
		}
		if strings.Contains(line, " model=") || strings.Contains(line, " effort=") {
			t.Fatalf("managed description row contains a model/effort selector: %q", line)
		}
	}
}

func TestSchemaMixedSourceOverridesFilterEachAgentSourceBranch(t *testing.T) {
	info, err := NewSubagentWithRuntimeCatalog(
		&fakeController{},
		loop.DelegationManaged,
		[]SubagentCatalogEntry{{Name: "worker"}},
		singleEntryMixedSourcePreparationCatalog(t),
	).Info(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	var schema map[string]any
	if err := json.Unmarshal(info.Schema, &schema); err != nil {
		t.Fatal(err)
	}
	branches := schemaSourceModelAliases(schema)
	if got := branches["gateway"]; !equalStringSet(got, map[string]struct{}{"gateway": {}, "gateway-alt": {}}) {
		t.Fatalf("gateway source branch models = %v, want only gateway options", got)
	}
	if got := branches["native"]; !equalStringSet(got, map[string]struct{}{"native": {}, "native-alt": {}}) {
		t.Fatalf("native source branch models = %v, want only native options", got)
	}
}

func schemaContainsSourceModelPair(value any, source, model string) bool {
	return schemaContainsSourceModelPairWithContext(value, "", "", source, model)
}

func schemaSourceModelAliases(value any) map[string]map[string]struct{} {
	result := make(map[string]map[string]struct{})
	var walk func(any)
	walk = func(node any) {
		switch object := node.(type) {
		case map[string]any:
			properties, _ := object["properties"].(map[string]any)
			sourceProperty, _ := properties["agent_source"].(map[string]any)
			source, _ := sourceProperty["const"].(string)
			if source != "" {
				models := result[source]
				if models == nil {
					models = make(map[string]struct{})
					result[source] = models
				}
				collectSchemaModelAliases(object, models)
			}
			for _, child := range object {
				walk(child)
			}
		case []any:
			for _, child := range object {
				walk(child)
			}
		}
	}
	walk(value)
	return result
}

func collectSchemaModelAliases(value any, models map[string]struct{}) {
	switch node := value.(type) {
	case map[string]any:
		if properties, ok := node["properties"].(map[string]any); ok {
			if modelProperty, ok := properties["model"].(map[string]any); ok {
				if alias, ok := modelProperty["const"].(string); ok {
					models[alias] = struct{}{}
				}
				if aliases, ok := modelProperty["enum"].([]any); ok {
					for _, alias := range aliases {
						if value, ok := alias.(string); ok {
							models[value] = struct{}{}
						}
					}
				}
			}
		}
		for _, child := range node {
			collectSchemaModelAliases(child, models)
		}
	case []any:
		for _, child := range node {
			collectSchemaModelAliases(child, models)
		}
	}
}

func equalStringSet(got, want map[string]struct{}) bool {
	if len(got) != len(want) {
		return false
	}
	for value := range want {
		if _, ok := got[value]; !ok {
			return false
		}
	}
	return true
}

func schemaContainsSourceModelPairWithContext(value any, currentSource, currentModel, wantSource, wantModel string) bool {
	switch node := value.(type) {
	case map[string]any:
		if properties, ok := node["properties"].(map[string]any); ok {
			if property, ok := properties["agent_source"].(map[string]any); ok {
				if constant, ok := property["const"].(string); ok {
					currentSource = constant
				}
			}
			if property, ok := properties["model"].(map[string]any); ok {
				if constant, ok := property["const"].(string); ok {
					currentModel = constant
				}
				if aliases, ok := property["enum"].([]any); ok {
					for _, alias := range aliases {
						if value, ok := alias.(string); ok && value == wantModel {
							currentModel = value
						}
					}
				}
			}
		}
		if currentSource == wantSource && currentModel == wantModel {
			return true
		}
		for _, child := range node {
			if schemaContainsSourceModelPairWithContext(child, currentSource, currentModel, wantSource, wantModel) {
				return true
			}
		}
	case []any:
		for _, child := range node {
			if schemaContainsSourceModelPairWithContext(child, currentSource, currentModel, wantSource, wantModel) {
				return true
			}
		}
	}
	return false
}

func TestSchemaRuntimeSelectorsKeepModelEffortPairsResolvable(t *testing.T) {
	catalog := schemaCatalog(t, schemaEntryWithModels("worker", "claude-code", true, []schemaModel{
		{alias: "sonnet", efforts: []inferencemodel.Effort{inferencemodel.EffortLow}},
		{alias: "opus", efforts: []inferencemodel.Effort{inferencemodel.EffortHigh}},
	}))
	info, err := NewSubagentWithRuntimeCatalog(&fakeController{}, loop.DelegationManaged, []SubagentCatalogEntry{{Name: "worker"}}, catalog).Info(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var schema map[string]any
	if err := json.Unmarshal(info.Schema, &schema); err != nil {
		t.Fatal(err)
	}
	for model, effort := range map[string]string{"sonnet": "low", "opus": "high"} {
		if !schemaContainsModelEffortPair(schema, model, effort) {
			t.Errorf("schema missing resolvable model/effort pair %q/%q", model, effort)
		}
	}
	if schemaContainsModelEffortPair(schema, "sonnet", "high") || schemaContainsModelEffortPair(schema, "opus", "low") {
		t.Fatal("schema advertises an unresolved model/effort pair")
	}
}

func schemaContainsModelEffortPair(value any, model, effort string) bool {
	object, ok := value.(map[string]any)
	if !ok {
		if children, ok := value.([]any); ok {
			for _, child := range children {
				if schemaContainsModelEffortPair(child, model, effort) {
					return true
				}
			}
		}
		return false
	}
	properties, _ := object["properties"].(map[string]any)
	modelProperty, _ := properties["model"].(map[string]any)
	if modelProperty["const"] == model {
		effortProperty, _ := properties["effort"].(map[string]any)
		for _, value := range effortProperty["enum"].([]any) {
			if value == effort {
				return true
			}
		}
	}
	for _, child := range object {
		if schemaContainsModelEffortPair(child, model, effort) {
			return true
		}
	}
	return false
}

func TestSchemaDescriptionBoundsAvailableSubagentRows(t *testing.T) {
	entries := make([]loop.RuntimeCatalogEntry, 0, 2)
	entries = append(entries, schemaEntryWithModels("worker", "claude-code", true, []schemaModel{{alias: "default", efforts: []inferencemodel.Effort{inferencemodel.EffortMedium}}}))
	for i := 0; i < maxAvailableSubagentRows+3; i++ {
		entries = append(entries, schemaEntryWithModels("worker", loop.AgentHarnessName(fmt.Sprintf("harness-%02d", i)), false, []schemaModel{{alias: loop.ModelAlias(fmt.Sprintf("model-%02d", i)), efforts: []inferencemodel.Effort{inferencemodel.EffortMedium}}}))
	}
	info, err := NewSubagentWithRuntimeCatalog(&fakeController{}, loop.DelegationManaged, []SubagentCatalogEntry{{Name: "worker", Description: "builds"}}, schemaCatalog(t, entries...)).Info(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(info.Desc, "<available_subagents>") || !strings.Contains(info.Desc, "<elided") {
		t.Fatalf("description = %q, want bounded matrix with elision marker", info.Desc)
	}
	if got := strings.Count(info.Desc, "- role="); got != maxAvailableSubagentRows+1 {
		t.Fatalf("description rows = %d, want default row plus %d non-default rows", got, maxAvailableSubagentRows)
	}
}

func TestSyncOnlySchemaIsStartOnlyForeground(t *testing.T) {
	info, err := NewSubagentWithRuntimeCatalog(&fakeController{}, loop.DelegationSyncOnly, []SubagentCatalogEntry{{Name: "worker"}}, emptyRuntimeCatalog(t)).Info(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var schema map[string]any
	if err := json.Unmarshal(info.Schema, &schema); err != nil {
		t.Fatal(err)
	}
	properties := schema["properties"].(map[string]any)
	action := properties["action"].(map[string]any)
	if got := action["enum"].([]any); len(got) != 1 || got[0] != "start" {
		t.Fatalf("sync-only action enum = %v, want [start]", got)
	}
	background := properties["run_in_background"].(map[string]any)
	if background["const"] != false {
		t.Fatalf("sync-only run_in_background = %v, want const false", background)
	}
}

type schemaModel struct {
	alias   loop.ModelAlias
	efforts []inferencemodel.Effort
}

func schemaEntry(agent loopAgentName, harness loop.AgentHarnessName, defaultHarness bool, aliases []string, efforts []inferencemodel.Effort) loop.RuntimeCatalogEntry {
	models := make([]schemaModel, 0, len(aliases))
	for _, alias := range aliases {
		models = append(models, schemaModel{alias: loop.ModelAlias(alias), efforts: efforts})
	}
	return schemaEntryWithModels(agent, harness, defaultHarness, models)
}

type loopAgentName = identity.AgentName

func schemaEntryWithModels(agent loopAgentName, harness loop.AgentHarnessName, defaultHarness bool, models []schemaModel) loop.RuntimeCatalogEntry {
	options := make([]loop.RuntimeModelOption, 0, len(models))
	for _, model := range models {
		options = append(options, loop.RuntimeModelOption{Alias: model.alias, DefaultEffort: model.efforts[0], Efforts: append([]inferencemodel.Effort(nil), model.efforts...), Target: inferencemodel.Model{Provider: "provider", Name: string(model.alias), Sampling: inferencemodel.Sampling{Effort: model.efforts[0]}}})
	}
	return loop.RuntimeCatalogEntry{SubagentType: agent, AgentHarness: harness, Profile: loop.RuntimeProfileName("profile/" + string(harness)), Credential: loop.CredentialGatewayBacked, Default: defaultHarness, DefaultModel: models[0].alias, Models: options}
}

func schemaCatalog(t *testing.T, entries ...loop.RuntimeCatalogEntry) loop.RuntimeCatalog {
	t.Helper()
	catalog, err := loop.NewRuntimeCatalog(entries)
	if err != nil {
		t.Fatal(err)
	}
	return catalog
}

func emptyRuntimeCatalog(t *testing.T) loop.RuntimeCatalog { return schemaCatalog(t) }

func assertSchemaFieldPresence(t *testing.T, raw []byte, fields []string, want bool) {
	t.Helper()
	var schema map[string]any
	if err := json.Unmarshal(raw, &schema); err != nil {
		t.Fatal(err)
	}
	allOf := schema["allOf"].([]any)
	start := allOf[0].(map[string]any)["then"].(map[string]any)
	for _, field := range fields {
		got := schemaContainsProperty(start, field)
		if got != want {
			t.Errorf("start schema field %q present=%v, want %v", field, got, want)
		}
	}
}

func schemaContainsProperty(value any, field string) bool {
	switch node := value.(type) {
	case map[string]any:
		if properties, ok := node["properties"].(map[string]any); ok {
			if _, found := properties[field]; found {
				return true
			}
		}
		for _, child := range node {
			if schemaContainsProperty(child, field) {
				return true
			}
		}
	case []any:
		for _, child := range node {
			if schemaContainsProperty(child, field) {
				return true
			}
		}
	}
	return false
}

func schemaEnumValues(t *testing.T, raw []byte, field string) []string {
	t.Helper()
	var schema map[string]any
	if err := json.Unmarshal(raw, &schema); err != nil {
		t.Fatal(err)
	}
	allOf := schema["allOf"].([]any)
	start := allOf[0].(map[string]any)["then"].(map[string]any)
	values := findSchemaEnum(start, field)
	return values
}

func findSchemaEnum(value any, field string) []string {
	switch node := value.(type) {
	case map[string]any:
		if properties, ok := node["properties"].(map[string]any); ok {
			if candidate, ok := properties[field].(map[string]any); ok {
				if raw, ok := candidate["enum"].([]any); ok {
					values := make([]string, len(raw))
					for i, item := range raw {
						values[i] = item.(string)
					}
					return values
				}
			}
		}
		for _, child := range node {
			if values := findSchemaEnum(child, field); values != nil {
				return values
			}
		}
	case []any:
		for _, child := range node {
			if values := findSchemaEnum(child, field); values != nil {
				return values
			}
		}
	}
	return nil
}

func equalStrings(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}
