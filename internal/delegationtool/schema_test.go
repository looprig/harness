package delegationtool

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/looprig/harness/pkg/identity"
	"github.com/looprig/harness/pkg/loop"
	inferencemodel "github.com/looprig/inference/model"
)

func TestAgentToolSchemasAreClosedAndOperationSpecific(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		tool       preparedAgentTool
		properties []string
		required   []string
	}{
		{name: "StartAgent", tool: NewStartAgent(&fakeController{}, loop.DelegationManaged, agentCatalog(), emptyRuntimeCatalog(t)), properties: []string{"agent_type", "effort", "instructions", "model", "name", "timeout_seconds", "wait_for_response"}, required: []string{"agent_type", "instructions"}},
		{name: "MessageAgent", tool: NewMessageAgent(&fakeController{}, loop.DelegationManaged, agentCatalog()), properties: []string{"agent_id", "message", "timeout_seconds", "wait_for_response"}, required: []string{"agent_id", "message"}},
		{name: "ListAgents", tool: NewListAgents(&fakeController{}, loop.DelegationManaged, agentCatalog()), properties: []string{"agent_id"}},
		{name: "StopAgent", tool: NewStopAgent(&fakeController{}, loop.DelegationManaged, agentCatalog()), properties: []string{"agent_id"}, required: []string{"agent_id"}},
	}
	legacy := []string{"action", "subagent_type", "description", "prompt", "run_in_background", "delegate_id", "request_id", "pending"}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			info, err := tt.tool.Info(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			var schema map[string]any
			if err := json.Unmarshal(info.Schema, &schema); err != nil {
				t.Fatal(err)
			}
			if schema["type"] != "object" || schema["additionalProperties"] != false {
				t.Fatalf("schema is not a closed object: %s", info.Schema)
			}
			properties, ok := schema["properties"].(map[string]any)
			if !ok {
				t.Fatalf("schema properties = %T, want object", schema["properties"])
			}
			if got := sortedMapKeys(properties); !equalStrings(got, tt.properties) {
				t.Fatalf("properties = %v, want %v", got, tt.properties)
			}
			if got := schemaStrings(schema["required"]); !equalStrings(got, tt.required) {
				t.Fatalf("required = %v, want %v", got, tt.required)
			}
			for _, field := range legacy {
				if _, present := properties[field]; present {
					t.Errorf("legacy field %q is present", field)
				}
			}
			if wait, ok := properties["wait_for_response"].(map[string]any); ok && wait["default"] != true {
				t.Errorf("wait_for_response default = %v, want true", wait["default"])
			}
			if timeout, ok := properties["timeout_seconds"].(map[string]any); ok {
				if _, present := timeout["default"]; present {
					t.Errorf("timeout_seconds unexpectedly has a default")
				}
			}
		})
	}
}

func TestStartAgentModeSelectorRequiresMultipleExplicitModes(t *testing.T) {
	t.Parallel()

	t.Run("singleton mode is initial state, not a selector", func(t *testing.T) {
		config := newAgentToolConfig(loop.DelegationManaged, []AgentCatalogEntry{{Name: "worker", Modes: []loop.ModeName{"", "review", "review"}}})
		info, err := newStartAgent(&fakeController{}, config).Info(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		assertSchemaFieldPresence(t, info.Schema, []string{"agent_mode"}, false)
		_, err = config.prepareStartAgent(`{"agent_type":"worker","instructions":"review","agent_mode":"review"}`)
		assertPrepareCategory(t, err, errCategoryFieldNotAllowed)
	})

	t.Run("two explicit modes are selectable", func(t *testing.T) {
		config := newAgentToolConfig(loop.DelegationManaged, []AgentCatalogEntry{{Name: "worker", Modes: []loop.ModeName{"", "review", "build", "review"}}})
		info, err := newStartAgent(&fakeController{}, config).Info(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if got := schemaEnumValues(t, info.Schema, "agent_mode"); !equalStrings(got, []string{"build", "review"}) {
			t.Fatalf("agent_mode enum = %q, want distinct non-empty modes build and review", got)
		}
		for _, mode := range []string{"build", "review"} {
			prepared, err := config.prepareStartAgent(`{"agent_type":"worker","instructions":"work","agent_mode":"` + mode + `"}`)
			if err != nil {
				t.Fatalf("prepare agent_mode %q: %v", mode, err)
			}
			if prepared.AgentMode != mode {
				t.Fatalf("prepared agent_mode = %q, want %q", prepared.AgentMode, mode)
			}
		}
	})
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
			name:       "single default harness and model",
			catalog:    schemaCatalog(t, schemaEntry("worker", "claude-code", true, []string{"sonnet"}, []inferencemodel.Effort{inferencemodel.EffortMedium})),
			wantFields: []string{"model", "effort"},
			noFields:   []string{"agent_harness", "agent_source"},
			wantEnums:  map[string][]string{"model": {"sonnet"}, "effort": {"medium"}},
		},
		{
			name: "multiple harnesses only exposes harness",
			catalog: schemaCatalog(t,
				schemaEntry("worker", "claude-code", true, []string{"sonnet"}, []inferencemodel.Effort{inferencemodel.EffortMedium}),
				schemaEntry("worker", "codex", false, []string{"luna"}, []inferencemodel.Effort{inferencemodel.EffortHigh})),
			wantFields: []string{"agent_harness", "model", "effort"},
			noFields:   []string{"agent_source"},
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
			info, err := NewStartAgent(&fakeController{}, loop.DelegationManaged, []AgentCatalogEntry{{Name: "worker", Description: "builds"}}, tt.catalog).Info(context.Background())
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
	roles := []AgentCatalogEntry{{Name: "worker", Description: "builds"}, {Name: "reviewer", Description: "reviews"}}
	catalog := schemaCatalog(t, schemaEntry("worker", "claude-code", true, []string{"sonnet"}, []inferencemodel.Effort{inferencemodel.EffortMedium}))
	info, err := NewStartAgent(&fakeController{}, loop.DelegationManaged, roles, catalog).Info(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(info.Schema), "worker") || strings.Contains(string(info.Schema), "reviewer") {
		t.Fatalf("populated catalog schema = %s, want only catalogued role", info.Schema)
	}
	if !strings.Contains(info.Desc, "- worker: builds") || strings.Contains(info.Desc, "- reviewer: reviews") {
		t.Fatalf("populated catalog description = %q, want only catalogued role", info.Desc)
	}

	empty := emptyRuntimeCatalog(t)
	nativeInfo, err := NewStartAgent(&fakeController{}, loop.DelegationManaged, roles, empty).Info(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(nativeInfo.Schema), "reviewer") || !strings.Contains(nativeInfo.Desc, "- reviewer: reviews") {
		t.Fatalf("empty catalog native fallback omitted reviewer: schema=%s description=%q", nativeInfo.Schema, nativeInfo.Desc)
	}
}

func TestSchemaMixedSourcesAdvertisesAgentSourceWithoutManagedPlaceholders(t *testing.T) {
	gateway := schemaEntry("worker", "codex", true, []string{"luna"}, []inferencemodel.Effort{inferencemodel.EffortHigh})
	native := loop.RuntimeCatalogEntry{
		AgentType: "worker", AgentHarness: "codex", Profile: "profile/codex-native",
		Credential: loop.CredentialNativeAuth, Source: loop.RuntimeSourceNative,
		SelectionKind: loop.RuntimeSelectionHarnessManaged,
	}
	catalog := schemaCatalog(t, gateway, native)
	info, err := NewStartAgent(&fakeController{}, loop.DelegationManaged, []AgentCatalogEntry{{Name: "worker"}}, catalog).Info(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var schema map[string]any
	if err := json.Unmarshal(info.Schema, &schema); err != nil {
		t.Fatal(err)
	}
	properties := schema["properties"].(map[string]any)
	for _, field := range []string{"model", "effort"} {
		if _, present := properties[field]; !present {
			t.Fatalf("StartAgent root does not declare %s: %s", field, info.Schema)
		}
	}
	if got := schemaEnumValues(t, info.Schema, "agent_source"); !equalStrings(got, []string{"gateway", "native"}) {
		t.Fatalf("agent_source enum = %v, want gateway and native", got)
	}
	// The harness-managed native source contributes no model alias: only the
	// gateway's explicit option is a model value any agent can name.
	if got := schemaEnumValues(t, info.Schema, "model"); !equalStrings(got, []string{"luna"}) {
		t.Fatalf("model enum = %v, want only the explicit gateway option", got)
	}
	if strings.Contains(info.Desc, "model=harness-managed") || strings.Contains(info.Desc, "effort=harness-managed") {
		t.Fatalf("managed description contains a placeholder: %q", info.Desc)
	}
	managedRow := "  - harness=codex source=native"
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

func TestMixedSourceOverridesAreEnforcedAtPreparation(t *testing.T) {
	toolInstance := NewStartAgent(
		&fakeController{},
		loop.DelegationManaged,
		[]AgentCatalogEntry{{Name: "worker"}},
		singleEntryMixedSourcePreparationCatalog(t),
	)
	info, err := toolInstance.Info(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// The flat schema advertises the union of every source's options; which
	// option belongs to which source is enforced when the call is prepared.
	if got := schemaEnumValues(t, info.Schema, "model"); !equalStrings(got, []string{"gateway", "gateway-alt", "native", "native-alt"}) {
		t.Fatalf("model enum = %v, want the union of both sources' options", got)
	}
	_, _, err = toolInstance.PrepareCall(context.Background(), uuidForPreparation(), `{"agent_type":"worker","instructions":"p","agent_source":"gateway","model":"native"}`)
	assertPrepareCategory(t, err, errCategoryUnknownRuntime)
	if want := `available models: "gateway", "gateway-alt"`; err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("PrepareCall() error = %v, want it to name %s", err, want)
	}
}

func TestExplicitHarnessWithMultipleSourcesAcceptsOmittedSource(t *testing.T) {
	toolInstance := NewStartAgent(
		&fakeController{},
		loop.DelegationManaged,
		[]AgentCatalogEntry{{Name: "worker"}},
		explicitHarnessMixedSourcePreparationCatalog(t),
	)
	_, prepared, err := toolInstance.PrepareCall(context.Background(), uuidForPreparation(), `{"agent_type":"worker","instructions":"p","agent_harness":"codex"}`)
	if err != nil {
		t.Fatalf("PrepareCall() error = %v, want an explicit harness with an omitted source to resolve", err)
	}
	runtime := mustDelegateArtifact(t, prepared).Runtime
	if runtime == nil || runtime.Harness != "codex" || runtime.Explicit.Source {
		t.Fatalf("runtime = %+v, want codex with the source left to its default", runtime)
	}
}

func TestModelEffortPairsAreEnforcedAtPreparation(t *testing.T) {
	catalog := schemaCatalog(t, schemaEntryWithModels("worker", "claude-code", true, []schemaModel{
		{alias: "sonnet", efforts: []inferencemodel.Effort{inferencemodel.EffortLow}},
		{alias: "opus", efforts: []inferencemodel.Effort{inferencemodel.EffortHigh}},
	}))
	toolInstance := NewStartAgent(&fakeController{}, loop.DelegationManaged, []AgentCatalogEntry{{Name: "worker"}}, catalog)
	info, err := toolInstance.Info(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := schemaEnumValues(t, info.Schema, "model"); !equalStrings(got, []string{"opus", "sonnet"}) {
		t.Fatalf("model enum = %v, want opus and sonnet", got)
	}
	if got := schemaEnumValues(t, info.Schema, "effort"); !equalStrings(got, []string{"low", "high"}) {
		t.Fatalf("effort enum = %v, want low and high", got)
	}
	for model, effort := range map[string]string{"sonnet": "low", "opus": "high"} {
		args := `{"agent_type":"worker","instructions":"p","model":"` + model + `","effort":"` + effort + `"}`
		if _, _, err := toolInstance.PrepareCall(context.Background(), uuidForPreparation(), args); err != nil {
			t.Errorf("PrepareCall(%s/%s) error = %v, want resolvable pair", model, effort, err)
		}
	}
	for model, tt := range map[string]struct{ effort, allowed string }{"sonnet": {"high", `"low"`}, "opus": {"low", `"high"`}} {
		args := `{"agent_type":"worker","instructions":"p","model":"` + model + `","effort":"` + tt.effort + `"}`
		_, _, err := toolInstance.PrepareCall(context.Background(), uuidForPreparation(), args)
		assertPrepareCategory(t, err, errCategoryUnknownRuntime)
		if want := "available efforts: " + tt.allowed; !strings.Contains(err.Error(), want) {
			t.Errorf("PrepareCall(%s/%s) error = %v, want it to name %s", model, tt.effort, err, want)
		}
	}
}

func TestSchemaDescriptionBoundsAvailableAgentRuntimeRows(t *testing.T) {
	entries := make([]loop.RuntimeCatalogEntry, 0, 2)
	entries = append(entries, schemaEntryWithModels("worker", "claude-code", true, []schemaModel{{alias: "default", efforts: []inferencemodel.Effort{inferencemodel.EffortMedium}}}))
	for i := 0; i < maxAvailableAgentRuntimeRows+3; i++ {
		entries = append(entries, schemaEntryWithModels("worker", loop.AgentHarnessName(fmt.Sprintf("harness-%02d", i)), false, []schemaModel{{alias: loop.ModelAlias(fmt.Sprintf("model-%02d", i)), efforts: []inferencemodel.Effort{inferencemodel.EffortMedium}}}))
	}
	info, err := NewStartAgent(&fakeController{}, loop.DelegationManaged, []AgentCatalogEntry{{Name: "worker", Description: "builds"}}, schemaCatalog(t, entries...)).Info(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(info.Desc, "<available_agents>") || !strings.Contains(info.Desc, "<available_agent_runtimes>") || !strings.Contains(info.Desc, availableAgentElisionMarker) {
		t.Fatalf("description = %q, want bounded matrix with elision marker", info.Desc)
	}
	if got := strings.Count(info.Desc, "\n- agent_type=") + strings.Count(info.Desc, "\n  - harness="); got != maxAvailableAgentRuntimeRows {
		t.Fatalf("description runtime rows = %d, want %d", got, maxAvailableAgentRuntimeRows)
	}
}

func TestSyncOnlySchemaIsStartOnlyForeground(t *testing.T) {
	for _, built := range []preparedAgentTool{
		NewStartAgent(&fakeController{}, loop.DelegationSyncOnly, []AgentCatalogEntry{{Name: "worker"}}, emptyRuntimeCatalog(t)),
		NewMessageAgent(&fakeController{}, loop.DelegationSyncOnly, []AgentCatalogEntry{{Name: "worker"}}),
	} {
		info, err := built.Info(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		var schema map[string]any
		if err := json.Unmarshal(info.Schema, &schema); err != nil {
			t.Fatal(err)
		}
		properties := schema["properties"].(map[string]any)
		wait := properties["wait_for_response"].(map[string]any)
		if _, hasConst := wait["const"]; hasConst {
			t.Fatalf("%s sync-only wait_for_response = %v, want no const", info.Name, wait)
		}
		if enum, _ := wait["enum"].([]any); len(enum) != 1 || enum[0] != true {
			t.Fatalf("%s sync-only wait_for_response = %v, want enum [true]", info.Name, wait)
		}
	}
	toolInstance := NewStartAgent(&fakeController{}, loop.DelegationSyncOnly, []AgentCatalogEntry{{Name: "worker"}}, emptyRuntimeCatalog(t))
	_, _, err := toolInstance.PrepareCall(context.Background(), uuidForPreparation(), `{"agent_type":"worker","instructions":"p","wait_for_response":false}`)
	assertPrepareCategory(t, err, errCategoryInvalidValue)
	if want := `field "wait_for_response" must be true`; !strings.Contains(err.Error(), want) {
		t.Fatalf("PrepareCall() error = %v, want %s", err, want)
	}
	for _, managed := range []preparedAgentTool{
		NewMessageAgent(&fakeController{}, loop.DelegationSyncOnly, nil),
		NewListAgents(&fakeController{}, loop.DelegationSyncOnly, nil),
		NewStopAgent(&fakeController{}, loop.DelegationSyncOnly, nil),
	} {
		_, _, err := managed.PrepareCall(context.Background(), uuidForPreparation(), `{}`)
		assertPrepareCategory(t, err, errCategoryInvalidValue)
		if want := "only foreground delegation is available; use StartAgent"; !strings.Contains(err.Error(), want) {
			t.Fatalf("sync-only PrepareCall() error = %v, want %s", err, want)
		}
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
	return loop.RuntimeCatalogEntry{AgentType: agent, AgentHarness: harness, Profile: loop.RuntimeProfileName("profile/" + string(harness)), Credential: loop.CredentialGatewayBacked, Default: defaultHarness, DefaultModel: models[0].alias, Models: options}
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
	for _, field := range fields {
		got := schemaContainsProperty(schema, field)
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
	values := findSchemaEnum(schema, field)
	return values
}

func sortedMapKeys(values map[string]any) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func schemaStrings(value any) []string {
	raw, _ := value.([]any)
	values := make([]string, len(raw))
	for i := range raw {
		values[i], _ = raw[i].(string)
	}
	sort.Strings(values)
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
