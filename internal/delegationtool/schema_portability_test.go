package delegationtool

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/tool"
	inferencemodel "github.com/looprig/inference/model"
)

// portableSchemaForbiddenKeywords are keywords a tool's input schema must not
// use. Grammar-constrained decoders (llama.cpp) silently skip combinators mixed
// with properties and emit empty arguments; Anthropic refuses oneOf/allOf/anyOf
// at the top level of input_schema; OpenAI strict mode refuses them outright.
var portableSchemaForbiddenKeywords = []string{"oneOf", "anyOf", "allOf", "not", "const", "if", "then", "else"}

// TestEveryAgentToolSchemaIsFlatAndPortable walks every schema the agent tool
// bundle builds, in every delegation style and over catalogs shaped like the
// ones products ship, and requires each to be one flat closed object.
func TestEveryAgentToolSchemaIsFlatAndPortable(t *testing.T) {
	t.Parallel()

	roles := []AgentCatalogEntry{
		{Name: "carbon", Description: "writes and runs code", Modes: []loop.ModeName{"", "build", "plan"}},
		{Name: "explorer", Description: "searches the workspace", Modes: []loop.ModeName{"", "review"}},
		{Name: "reviewer", Description: "reviews changes"},
	}
	catalogs := map[string]loop.RuntimeCatalog{
		"no runtime catalog":        emptyRuntimeCatalog(t),
		"carbon-like multi-runtime": carbonLikeRuntimeCatalog(t),
		"single entry mixed source": singleEntryMixedSourcePreparationCatalog(t),
	}
	for _, style := range []loop.DelegationStyle{loop.DelegationSyncOnly, loop.DelegationManaged} {
		for catalogName, runtimeCatalog := range catalogs {
			for _, agents := range [][]AgentCatalogEntry{nil, roles[:1], roles} {
				name := fmt.Sprintf("style=%d/%s/agents=%d", style, catalogName, len(agents))
				t.Run(name, func(t *testing.T) {
					t.Parallel()
					definition := Definition(style, agents, runtimeCatalog)
					built, err := definition.Build(context.Background(), tool.Bindings{
						SessionID: mustDefinitionUUID(t),
						LoopID:    mustDefinitionUUID(t),
						Delegate:  &fakeController{},
					})
					if err != nil {
						t.Fatalf("Build() error = %v", err)
					}
					if len(built) != len(agentToolNames) {
						t.Fatalf("Build() returned %d tools, want %d", len(built), len(agentToolNames))
					}
					for _, agentTool := range built {
						info, err := agentTool.Info(context.Background())
						if err != nil {
							t.Fatal(err)
						}
						assertPortableToolSchema(t, info.Name, info.Schema)
					}
				})
			}
		}
	}
}

func assertPortableToolSchema(t *testing.T, name string, raw json.RawMessage) {
	t.Helper()
	var root map[string]any
	if err := json.Unmarshal(raw, &root); err != nil {
		t.Fatalf("%s schema is not a JSON object: %v", name, err)
	}
	if root["type"] != "object" || root["additionalProperties"] != false {
		t.Errorf("%s root is not a closed object: %s", name, raw)
	}
	for _, keyword := range append([]string{"enum"}, portableSchemaForbiddenKeywords...) {
		if _, present := root[keyword]; present {
			t.Errorf("%s root carries %q: %s", name, keyword, raw)
		}
	}
	properties, ok := root["properties"].(map[string]any)
	if !ok {
		t.Fatalf("%s root properties = %T, want object", name, root["properties"])
	}
	for _, required := range schemaStrings(root["required"]) {
		if _, declared := properties[required]; !declared {
			t.Errorf("%s requires undeclared property %q: %s", name, required, raw)
		}
	}
	for property, value := range properties {
		schema, ok := value.(map[string]any)
		if !ok {
			t.Errorf("%s property %q = %T, want object schema", name, property, value)
			continue
		}
		if _, typed := schema["type"].(string); !typed {
			t.Errorf("%s property %q has no scalar type: %v", name, property, schema)
		}
		for _, keyword := range append([]string{"properties", "items"}, portableSchemaForbiddenKeywords...) {
			if _, present := schema[keyword]; present {
				t.Errorf("%s property %q carries %q: %v", name, property, keyword, schema)
			}
		}
		if enum, present := schema["enum"]; present {
			if values, _ := enum.([]any); len(values) == 0 {
				t.Errorf("%s property %q has an empty enum", name, property)
			}
		}
	}
}

// carbonLikeRuntimeCatalog mirrors Carbon's compiled catalog: a default
// in-process harness with several models whose efforts differ, an ACP
// claude-code gateway row, and a codex harness offered through both a gateway
// row and a harness-managed native row.
func carbonLikeRuntimeCatalog(t *testing.T) loop.RuntimeCatalog {
	t.Helper()
	low, medium, high, maxEffort := inferencemodel.EffortLow, inferencemodel.EffortMedium, inferencemodel.EffortHigh, inferencemodel.EffortMax
	looprig := schemaEntryWithModels("carbon", "looprig", true, []schemaModel{
		{alias: "opus", efforts: []inferencemodel.Effort{medium, high, maxEffort}},
		{alias: "sonnet", efforts: []inferencemodel.Effort{low, medium, high}},
		{alias: "qwen", efforts: []inferencemodel.Effort{inferencemodel.EffortNone}},
	})
	looprig.Source = loop.RuntimeSourceNative
	looprig.Credential = loop.CredentialNativeAuth
	claude := schemaEntryWithModels("carbon", "claude-code", false, []schemaModel{
		{alias: "opus", efforts: []inferencemodel.Effort{medium, high}},
		{alias: "sonnet", efforts: []inferencemodel.Effort{low, medium}},
	})
	claude.Source = loop.RuntimeSourceGateway
	claude.NeedsSmallModel = true
	claude.SmallModel = "sonnet"
	codexGateway := schemaEntryWithModels("carbon", "codex", false, []schemaModel{{alias: "luna", efforts: []inferencemodel.Effort{high}}})
	codexGateway.Source = loop.RuntimeSourceGateway
	codexNative := loop.RuntimeCatalogEntry{
		AgentType: "carbon", AgentHarness: "codex", Profile: "acp/codex-native",
		Credential: loop.CredentialNativeAuth, Source: loop.RuntimeSourceNative,
		SelectionKind: loop.RuntimeSelectionHarnessManaged,
	}
	explorer := schemaEntryWithModels("explorer", "looprig", true, []schemaModel{{alias: "haiku", efforts: []inferencemodel.Effort{low}}})
	explorer.Profile = "profile/explorer"
	return schemaCatalog(t, looprig, claude, codexGateway, codexNative, explorer)
}

// TestFlatStartAgentSchemaCarriesEveryValueAnAgentAccepts pins the union
// semantics of the flat schema's enums: they never exclude a value some agent
// accepts, so the per-agent rules live in preparation alone.
func TestFlatStartAgentSchemaCarriesEveryValueAnAgentAccepts(t *testing.T) {
	t.Parallel()
	roles := []AgentCatalogEntry{{Name: "carbon", Modes: []loop.ModeName{"build", "plan"}}, {Name: "explorer"}, {Name: "reviewer"}}
	toolInstance := NewStartAgent(&fakeController{}, loop.DelegationManaged, roles, carbonLikeRuntimeCatalog(t))
	info, err := toolInstance.Info(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := map[string][]string{
		"agent_type":    {"carbon", "explorer"},
		"agent_harness": {"claude-code", "codex", "looprig"},
		"agent_source":  {"gateway", "native"},
		"model":         {"haiku", "luna", "opus", "qwen", "sonnet"},
		"effort":        {"none", "low", "medium", "high", "max"},
		"agent_mode":    {"build", "plan"},
	}
	for field, values := range want {
		if got := schemaEnumValues(t, info.Schema, field); !equalStrings(got, values) {
			t.Errorf("%s enum = %v, want %v", field, got, values)
		}
	}

	// A value in the union that the chosen agent does not accept is refused
	// with the values that agent does accept.
	_, _, err = toolInstance.PrepareCall(context.Background(), uuidForPreparation(), `{"agent_type":"explorer","instructions":"p","model":"opus"}`)
	assertPrepareCategory(t, err, errCategoryUnknownRuntime)
	if want := `available models: "haiku"`; !strings.Contains(err.Error(), want) {
		t.Fatalf("PrepareCall() error = %v, want it to name %s", err, want)
	}
	_, _, err = toolInstance.PrepareCall(context.Background(), uuidForPreparation(), `{"agent_type":"explorer","instructions":"p","agent_harness":"codex"}`)
	assertPrepareCategory(t, err, errCategoryFieldNotAllowed)
	if want := `field "agent_harness" is not selectable for agent type "explorer"; omit it`; !strings.Contains(err.Error(), want) {
		t.Fatalf("PrepareCall() error = %v, want %s", err, want)
	}
	_, _, err = toolInstance.PrepareCall(context.Background(), uuidForPreparation(), `{"agent_type":"reviewer","instructions":"p"}`)
	assertPrepareCategory(t, err, errCategoryUnknownRuntime)
	if want := `available agent types: "carbon", "explorer"`; !strings.Contains(err.Error(), want) {
		t.Fatalf("PrepareCall() error = %v, want it to name %s", err, want)
	}
}
