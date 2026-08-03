package delegationtool

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/tool"
	inferencemodel "github.com/looprig/inference/model"
)

func TestPrepareStartAgentRuntimeDefaultsAndExplicitTuple(t *testing.T) {
	t.Parallel()
	catalog := testPreparationCatalog(t)
	toolInstance := NewStartAgent(&fakeController{}, loop.DelegationManaged, agentCatalog(), catalog)

	t.Run("omitted selectors use defaults", func(t *testing.T) {
		request, prepared, err := toolInstance.PrepareCall(context.Background(), uuidForPreparation(), `{"agent_type":"worker","name":"inspect","instructions":"map the repo"}`)
		if err != nil {
			t.Fatalf("PrepareCall() error = %v", err)
		}
		artifact := mustDelegateArtifact(t, prepared)
		if !reflect.DeepEqual(request, tool.Request{}) {
			t.Fatalf("PrepareCall() request = %#v, want empty access request", request)
		}
		if artifact.Request.Operation != tool.DelegateStart || artifact.Request.Agent != "worker" || artifact.Request.Message != "map the repo" || !artifact.Request.Wait {
			t.Fatalf("prepared request = %#v, want foreground start request", artifact.Request)
		}
		if artifact.Runtime == nil {
			t.Fatal("prepared runtime is nil")
		}
		want := tool.DelegateRuntime{Harness: "claude-code", Profile: "acp/claude-code", Source: "gateway", SelectionKind: "explicit", Model: "sonnet", SmallModel: "sonnet-small", Effort: "medium", Advertised: tool.DelegateRuntimeAdvertised{Harness: true, Model: true, Effort: true}}
		if *artifact.Runtime != want {
			t.Fatalf("runtime = %#v, want %#v", *artifact.Runtime, want)
		}
	})

	t.Run("explicit tuple preserves explicitness", func(t *testing.T) {
		_, prepared, err := toolInstance.PrepareCall(context.Background(), uuidForPreparation(), `{"agent_type":"worker","name":"inspect","instructions":"run it","agent_harness":"codex","model":"luna","effort":"none"}`)
		if err != nil {
			t.Fatalf("PrepareCall() error = %v", err)
		}
		runtime := mustDelegateArtifact(t, prepared).Runtime
		if runtime == nil {
			t.Fatal("prepared runtime is nil")
		}
		want := tool.DelegateRuntime{Harness: "codex", Profile: "acp/codex", Source: "gateway", SelectionKind: "explicit", Model: "luna", SmallModel: "luna-small", Effort: "none", Explicit: tool.DelegateRuntimeExplicit{Harness: true, Model: true, Effort: true}, Advertised: tool.DelegateRuntimeAdvertised{Harness: true, Model: true, Effort: true}}
		if *runtime != want {
			t.Fatalf("runtime = %#v, want %#v", *runtime, want)
		}
	})
}

func TestPrepareStartAgentRuntimeSelectorErrorsAreBounded(t *testing.T) {
	t.Parallel()
	catalog := testPreparationCatalog(t)
	tests := []struct {
		name     string
		args     string
		category string
	}{
		{name: "unknown advertised harness is unknown runtime", args: `{"agent_type":"worker","instructions":"p","agent_harness":"missing"}`, category: errCategoryUnknownRuntime},
		{name: "unknown advertised model is unknown runtime", args: `{"agent_type":"worker","instructions":"p","agent_harness":"claude-code","model":"missing"}`, category: errCategoryUnknownRuntime},
		{name: "incompatible effort is unknown runtime", args: `{"agent_type":"worker","instructions":"p","agent_harness":"claude-code","model":"sonnet","effort":"low"}`, category: errCategoryUnknownRuntime},
		{name: "unknown role is unknown runtime", args: `{"agent_type":"missing","instructions":"p"}`, category: errCategoryUnknownRuntime},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			toolInstance := NewStartAgent(&fakeController{}, loop.DelegationManaged, agentCatalog(), catalog)
			_, _, err := toolInstance.PrepareCall(context.Background(), uuidForPreparation(), tt.args)
			if err == nil || !strings.Contains(err.Error(), tt.category) {
				t.Fatalf("PrepareCall() error = %v, want category %q", err, tt.category)
			}
			if strings.Contains(err.Error(), "missing") || strings.Contains(err.Error(), `"low"`) {
				t.Fatalf("error echoed selector: %v", err)
			}
		})
	}
}

func TestPrepareStartAgentRuntimeIsParentScopedAndOptional(t *testing.T) {
	t.Parallel()
	claudeCatalog, err := loop.NewRuntimeCatalog([]loop.RuntimeCatalogEntry{testPreparationEntry("claude-code", "acp/claude-code", "sonnet", inferencemodel.EffortMedium)})
	if err != nil {
		t.Fatal(err)
	}
	noChoice, err := loop.NewRuntimeCatalog(nil)
	if err != nil {
		t.Fatal(err)
	}

	t.Run("other parent cannot grant codex", func(t *testing.T) {
		toolInstance := NewStartAgent(&fakeController{}, loop.DelegationManaged, agentCatalog(), claudeCatalog)
		_, _, err := toolInstance.PrepareCall(context.Background(), uuidForPreparation(), `{"agent_type":"worker","instructions":"p","agent_harness":"codex"}`)
		if err == nil || !strings.Contains(err.Error(), errCategoryFieldNotAllowed) {
			t.Fatalf("PrepareCall() error = %v, want %s", err, errCategoryFieldNotAllowed)
		}
	})

	t.Run("no runtime choices leave runtime nil", func(t *testing.T) {
		toolInstance := NewStartAgent(&fakeController{}, loop.DelegationManaged, agentCatalog(), noChoice)
		_, prepared, err := toolInstance.PrepareCall(context.Background(), uuidForPreparation(), `{"agent_type":"worker","instructions":"p"}`)
		if err != nil {
			t.Fatalf("PrepareCall() error = %v", err)
		}
		if got := mustDelegateArtifact(t, prepared).Runtime; got != nil {
			t.Fatalf("runtime = %#v, want nil", got)
		}
	})

	t.Run("explicit harness with no choices is not allowed", func(t *testing.T) {
		toolInstance := NewStartAgent(&fakeController{}, loop.DelegationManaged, agentCatalog(), noChoice)
		_, _, err := toolInstance.PrepareCall(context.Background(), uuidForPreparation(), `{"agent_type":"worker","instructions":"p","agent_harness":"claude-code"}`)
		if err == nil || !strings.Contains(err.Error(), errCategoryFieldNotAllowed) {
			t.Fatalf("PrepareCall() error = %v, want %s", err, errCategoryFieldNotAllowed)
		}
	})

	t.Run("unrelated role entries make the missing role unavailable", func(t *testing.T) {
		entry := testPreparationEntry("claude-code", "acp/claude-code", "sonnet", inferencemodel.EffortMedium)
		entry.SubagentType = "other"
		unrelated, err := loop.NewRuntimeCatalog([]loop.RuntimeCatalogEntry{entry})
		if err != nil {
			t.Fatal(err)
		}
		toolInstance := NewStartAgent(&fakeController{}, loop.DelegationManaged, []AgentCatalogEntry{{Name: "worker"}}, unrelated)
		_, _, err = toolInstance.PrepareCall(context.Background(), uuidForPreparation(), `{"agent_type":"worker","instructions":"p"}`)
		if err == nil || !strings.Contains(err.Error(), errCategoryUnknownRuntime) {
			t.Fatalf("PrepareCall() omitted selectors error = %v, want %s", err, errCategoryUnknownRuntime)
		}
		_, _, err = toolInstance.PrepareCall(context.Background(), uuidForPreparation(), `{"agent_type":"worker","instructions":"p","agent_harness":"claude-code"}`)
		if err == nil || !strings.Contains(err.Error(), errCategoryUnknownRuntime) {
			t.Fatalf("PrepareCall() explicit selector error = %v, want %s", err, errCategoryUnknownRuntime)
		}
	})
}

func TestPrepareStartAgentRuntimeAllowsModelEffortAndRejectsUnselectableHarness(t *testing.T) {
	t.Parallel()
	catalog := singleChoicePreparationCatalog(t)
	toolInstance := NewStartAgent(&fakeController{}, loop.DelegationManaged, []AgentCatalogEntry{{Name: "worker"}}, catalog)
	_, _, err := toolInstance.PrepareCall(context.Background(), uuidForPreparation(), `{"agent_type":"worker","instructions":"p","agent_harness":"claude-code"}`)
	if err == nil || !strings.Contains(err.Error(), errCategoryFieldNotAllowed) {
		t.Fatalf("single harness error = %v, want %s", err, errCategoryFieldNotAllowed)
	}
	_, prepared, err := toolInstance.PrepareCall(context.Background(), uuidForPreparation(), `{"agent_type":"worker","instructions":"p","model":"sonnet","effort":"medium"}`)
	if err != nil {
		t.Fatalf("single model/effort selection error = %v", err)
	}
	runtime := mustDelegateArtifact(t, prepared).Runtime
	if runtime == nil || runtime.Model != "sonnet" || runtime.Effort != "medium" || !runtime.Explicit.Model || !runtime.Explicit.Effort {
		t.Fatalf("single model/effort runtime = %+v", runtime)
	}
}

func TestPrepareStartAgentRuntimeResolvesHarnessManagedNativeWithoutSelectors(t *testing.T) {
	t.Parallel()

	catalog, err := loop.NewRuntimeCatalog([]loop.RuntimeCatalogEntry{{
		SubagentType:  "worker",
		AgentHarness:  "codex",
		Profile:       "acp/codex-native",
		Credential:    loop.CredentialNativeAuth,
		Source:        loop.RuntimeSourceNative,
		SelectionKind: loop.RuntimeSelectionHarnessManaged,
		Default:       true,
	}})
	if err != nil {
		t.Fatal(err)
	}
	toolInstance := NewStartAgent(&fakeController{}, loop.DelegationManaged, []AgentCatalogEntry{{Name: "worker"}}, catalog)

	_, prepared, err := toolInstance.PrepareCall(context.Background(), uuidForPreparation(), `{"agent_type":"worker","name":"inspect","instructions":"use your configured model"}`)
	if err != nil {
		t.Fatalf("PrepareCall() error = %v", err)
	}
	runtime := mustDelegateArtifact(t, prepared).Runtime
	if runtime == nil {
		t.Fatal("prepared runtime is nil")
	}
	if runtime.Source != string(loop.RuntimeSourceNative) || runtime.SelectionKind != string(loop.RuntimeSelectionHarnessManaged) {
		t.Fatalf("runtime source/selection = %q/%q, want native/harness-managed", runtime.Source, runtime.SelectionKind)
	}
	if runtime.Model != "" || runtime.SmallModel != "" || runtime.Effort != "" {
		t.Fatalf("runtime concrete selectors = model %q small %q effort %q, want empty/empty/empty", runtime.Model, runtime.SmallModel, runtime.Effort)
	}
	if runtime.Advertised.Any() {
		t.Fatalf("harness-managed runtime advertised selectors = %+v, want none", runtime.Advertised)
	}

	for _, args := range []string{
		`{"agent_type":"worker","instructions":"p","model":"luna"}`,
		`{"agent_type":"worker","instructions":"p","effort":"high"}`,
	} {
		_, _, err := toolInstance.PrepareCall(context.Background(), uuidForPreparation(), args)
		if err == nil || !strings.Contains(err.Error(), errCategoryFieldNotAllowed) {
			t.Errorf("PrepareCall(%s) error = %v, want %s", args, err, errCategoryFieldNotAllowed)
		}
	}

	info, err := toolInstance.Info(context.Background())
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
	for _, field := range []string{"model", "effort"} {
		if _, present := properties[field]; !present {
			t.Fatalf("harness-managed schema does not declare %s: %s", field, info.Schema)
		}
	}
}

func TestPrepareStartAgentRuntimeResolvesMixedSourcesWithAgentSourceSelector(t *testing.T) {
	t.Parallel()

	catalog := mixedSourcePreparationCatalog(t)
	toolInstance := NewStartAgent(&fakeController{}, loop.DelegationManaged, []AgentCatalogEntry{{Name: "worker"}}, catalog)

	info, err := toolInstance.Info(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	assertSchemaFieldPresence(t, info.Schema, []string{"agent_source"}, true)
	if strings.Contains(info.Desc, "model=harness-managed") || strings.Contains(info.Desc, "effort=harness-managed") {
		t.Fatalf("managed description contains a placeholder: %q", info.Desc)
	}

	tests := []struct {
		name           string
		args           string
		wantSource     string
		wantKind       string
		wantModel      string
		wantEffort     string
		wantExplicit   tool.DelegateRuntimeExplicit
		wantAdvertised tool.DelegateRuntimeAdvertised
	}{
		{
			name:       "omitted source keeps gateway default",
			args:       `{"agent_type":"worker","instructions":"p"}`,
			wantSource: "gateway", wantKind: "explicit", wantModel: "luna", wantEffort: "high",
			wantAdvertised: tool.DelegateRuntimeAdvertised{Source: true, Model: true, Effort: true},
		},
		{
			name:       "native source delegates model selection",
			args:       `{"agent_type":"worker","instructions":"p","agent_source":"native"}`,
			wantSource: "native", wantKind: "harness-managed",
			wantExplicit:   tool.DelegateRuntimeExplicit{Source: true},
			wantAdvertised: tool.DelegateRuntimeAdvertised{Source: true},
		},
		{
			name:       "gateway source selects concrete default",
			args:       `{"agent_type":"worker","instructions":"p","agent_source":"gateway"}`,
			wantSource: "gateway", wantKind: "explicit", wantModel: "luna", wantEffort: "high",
			wantExplicit:   tool.DelegateRuntimeExplicit{Source: true},
			wantAdvertised: tool.DelegateRuntimeAdvertised{Source: true, Model: true, Effort: true},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, prepared, err := toolInstance.PrepareCall(context.Background(), uuidForPreparation(), tt.args)
			if err != nil {
				t.Fatalf("PrepareCall() error = %v", err)
			}
			runtime := mustDelegateArtifact(t, prepared).Runtime
			if runtime == nil {
				t.Fatal("prepared runtime is nil")
			}
			if runtime.Source != tt.wantSource || runtime.SelectionKind != tt.wantKind || runtime.Model != tt.wantModel || runtime.Effort != tt.wantEffort {
				t.Fatalf("runtime = %+v, want source/kind/model/effort %s/%s/%s/%s", *runtime, tt.wantSource, tt.wantKind, tt.wantModel, tt.wantEffort)
			}
			if runtime.Explicit != tt.wantExplicit || runtime.Advertised != tt.wantAdvertised {
				t.Fatalf("runtime selector metadata = explicit=%+v advertised=%+v, want explicit=%+v advertised=%+v", runtime.Explicit, runtime.Advertised, tt.wantExplicit, tt.wantAdvertised)
			}
		})
	}

	for _, args := range []string{
		`{"agent_type":"worker","instructions":"p","agent_source":"native","model":"luna"}`,
		`{"agent_type":"worker","instructions":"p","agent_source":"native","effort":"high"}`,
		`{"agent_type":"worker","instructions":"p","agent_source":"unknown"}`,
	} {
		_, _, err := toolInstance.PrepareCall(context.Background(), uuidForPreparation(), args)
		if err == nil || !strings.Contains(err.Error(), errCategoryFieldNotAllowed) && !strings.Contains(err.Error(), errCategoryUnknownRuntime) {
			t.Errorf("PrepareCall(%s) error = %v, want bounded source/model/effort rejection", args, err)
		}
	}
}

func TestPrepareStartAgentRuntimeResolvesPerModelSourcesWithinOneEntry(t *testing.T) {
	t.Parallel()

	catalog := singleEntryMixedSourcePreparationCatalog(t)
	toolInstance := NewStartAgent(&fakeController{}, loop.DelegationManaged, []AgentCatalogEntry{{Name: "worker"}}, catalog)
	info, err := toolInstance.Info(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	assertSchemaFieldPresence(t, info.Schema, []string{"agent_source"}, true)
	var schema map[string]any
	if err := json.Unmarshal(info.Schema, &schema); err != nil {
		t.Fatal(err)
	}
	if !schemaContainsSourceModelPair(schema, "native", "native") || !schemaContainsSourceModelPair(schema, "gateway", "gateway") {
		t.Fatalf("schema does not bind each source to its effective model: %s", info.Schema)
	}

	for _, tt := range []struct {
		name   string
		source string
		model  string
		effort string
	}{
		{name: "gateway option", source: "gateway", model: "gateway", effort: "high"},
		{name: "native option", source: "native", model: "native", effort: "medium"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			args := `{"agent_type":"worker","instructions":"p","agent_source":"` + tt.source + `"}`
			_, prepared, err := toolInstance.PrepareCall(context.Background(), uuidForPreparation(), args)
			if err != nil {
				t.Fatalf("PrepareCall() error = %v", err)
			}
			runtime := mustDelegateArtifact(t, prepared).Runtime
			if runtime == nil {
				t.Fatal("prepared runtime is nil")
			}
			if runtime.Source != tt.source || runtime.SelectionKind != "explicit" || runtime.Model != tt.model || runtime.Effort != tt.effort {
				t.Fatalf("runtime = %+v, want source/model/effort %s/%s/%s", *runtime, tt.source, tt.model, tt.effort)
			}
			if !runtime.Explicit.Source || runtime.Explicit.Model {
				t.Fatalf("source-only request explicitness = %+v, want source only", runtime.Explicit)
			}
		})
	}

	_, _, err = toolInstance.PrepareCall(context.Background(), uuidForPreparation(), `{"agent_type":"worker","instructions":"p","agent_source":"native","model":"gateway"}`)
	if err == nil || !strings.Contains(err.Error(), errCategoryFieldNotAllowed) && !strings.Contains(err.Error(), errCategoryUnknownRuntime) {
		t.Fatalf("model from another effective source error = %v, want bounded rejection", err)
	}
}

func mustDelegateArtifact(t *testing.T, prepared tool.PreparedArtifact) tool.DelegateArtifact {
	t.Helper()
	artifact, ok := prepared.(tool.DelegateArtifact)
	if !ok {
		t.Fatalf("prepared artifact = %T, want tool.DelegateArtifact", prepared)
	}
	return artifact
}

func testPreparationCatalog(t *testing.T) loop.RuntimeCatalog {
	t.Helper()
	catalog, err := loop.NewRuntimeCatalog([]loop.RuntimeCatalogEntry{
		testPreparationEntry("claude-code", "acp/claude-code", "sonnet", inferencemodel.EffortMedium),
		testPreparationEntry("codex", "acp/codex", "luna", inferencemodel.EffortHigh),
	})
	if err != nil {
		t.Fatal(err)
	}
	return catalog
}

func testPreparationEntry(harness loop.AgentHarnessName, profile loop.RuntimeProfileName, alias loop.ModelAlias, defaultEffort inferencemodel.Effort) loop.RuntimeCatalogEntry {
	efforts := []inferencemodel.Effort{inferencemodel.EffortNone, inferencemodel.EffortHigh}
	if defaultEffort != inferencemodel.EffortHigh {
		efforts = append(efforts, defaultEffort)
	}
	modelName := string(alias)
	return loop.RuntimeCatalogEntry{
		SubagentType: "worker", AgentHarness: harness, Profile: profile, Credential: loop.CredentialGatewayBacked,
		Default: harness == "claude-code", DefaultModel: alias, SmallModel: alias + "-small",
		Models: []loop.RuntimeModelOption{
			{Alias: alias, Target: inferencemodel.Model{Provider: "provider", Name: modelName, Sampling: inferencemodel.Sampling{Effort: defaultEffort}}, DefaultEffort: defaultEffort, Efforts: efforts},
			{Alias: alias + "-small", Target: inferencemodel.Model{Provider: "provider", Name: modelName + "-small", Sampling: inferencemodel.Sampling{Effort: inferencemodel.EffortLow}}, DefaultEffort: inferencemodel.EffortLow, Efforts: []inferencemodel.Effort{inferencemodel.EffortLow}},
		},
	}
}

func singleChoicePreparationCatalog(t *testing.T) loop.RuntimeCatalog {
	t.Helper()
	entry := testPreparationEntry("claude-code", "acp/claude-code", "sonnet", inferencemodel.EffortMedium)
	entry.SmallModel = ""
	entry.Models = entry.Models[:1]
	entry.Models[0].Efforts = []inferencemodel.Effort{inferencemodel.EffortMedium}
	catalog, err := loop.NewRuntimeCatalog([]loop.RuntimeCatalogEntry{entry})
	if err != nil {
		t.Fatal(err)
	}
	return catalog
}

func uuidForPreparation() uuid.UUID {
	return uuid.MustParse("11111111-1111-4111-8111-111111111111")
}

func mixedSourcePreparationCatalog(t *testing.T) loop.RuntimeCatalog {
	t.Helper()
	catalog, err := loop.NewRuntimeCatalog([]loop.RuntimeCatalogEntry{
		{
			SubagentType: "worker", AgentHarness: "codex", Profile: "acp/codex-gateway",
			Credential: loop.CredentialGatewayBacked, Source: loop.RuntimeSourceGateway, Default: true,
			DefaultModel: "luna",
			Models: []loop.RuntimeModelOption{{
				Alias: "luna", Target: inferencemodel.Model{Provider: "provider", Name: "luna", Sampling: inferencemodel.Sampling{Effort: inferencemodel.EffortHigh}},
				DefaultEffort: inferencemodel.EffortHigh, Efforts: []inferencemodel.Effort{inferencemodel.EffortHigh},
			}},
		},
		{
			SubagentType: "worker", AgentHarness: "codex", Profile: "acp/codex-native",
			Credential: loop.CredentialNativeAuth, Source: loop.RuntimeSourceNative,
			SelectionKind: loop.RuntimeSelectionHarnessManaged,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return catalog
}

func singleEntryMixedSourcePreparationCatalog(t *testing.T) loop.RuntimeCatalog {
	t.Helper()
	catalog, err := loop.NewRuntimeCatalog([]loop.RuntimeCatalogEntry{{
		SubagentType: "worker", AgentHarness: "codex", Profile: "acp/codex-mixed",
		Credential: loop.CredentialGatewayBacked, Source: loop.RuntimeSourceGateway, Default: true,
		DefaultModel: "gateway",
		Models: []loop.RuntimeModelOption{
			{Alias: "gateway", Source: loop.RuntimeSourceGateway, Credential: loop.CredentialGatewayBacked, Target: inferencemodel.Model{Provider: "provider", Name: "gateway", Sampling: inferencemodel.Sampling{Effort: inferencemodel.EffortHigh}}, DefaultEffort: inferencemodel.EffortHigh, Efforts: []inferencemodel.Effort{inferencemodel.EffortHigh}},
			{Alias: "gateway-alt", Source: loop.RuntimeSourceGateway, Credential: loop.CredentialGatewayBacked, Target: inferencemodel.Model{Provider: "provider", Name: "gateway-alt", Sampling: inferencemodel.Sampling{Effort: inferencemodel.EffortHigh}}, DefaultEffort: inferencemodel.EffortHigh, Efforts: []inferencemodel.Effort{inferencemodel.EffortHigh}},
			{Alias: "native", Source: loop.RuntimeSourceNative, Credential: loop.CredentialNativeAuth, Target: inferencemodel.Model{Provider: "provider", Name: "native", Sampling: inferencemodel.Sampling{Effort: inferencemodel.EffortMedium}}, DefaultEffort: inferencemodel.EffortMedium, Efforts: []inferencemodel.Effort{inferencemodel.EffortMedium}},
			{Alias: "native-alt", Source: loop.RuntimeSourceNative, Credential: loop.CredentialNativeAuth, Target: inferencemodel.Model{Provider: "provider", Name: "native-alt", Sampling: inferencemodel.Sampling{Effort: inferencemodel.EffortMedium}}, DefaultEffort: inferencemodel.EffortMedium, Efforts: []inferencemodel.Effort{inferencemodel.EffortMedium}},
		},
	}})
	if err != nil {
		t.Fatal(err)
	}
	return catalog
}
