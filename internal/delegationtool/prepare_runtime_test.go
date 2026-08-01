package delegationtool

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/tool"
	inferencemodel "github.com/looprig/inference/model"
)

func TestPrepareCallResolvesRuntimeDefaultsAndExplicitTuple(t *testing.T) {
	t.Parallel()
	catalog := testPreparationCatalog(t)
	toolInstance := NewSubagentWithRuntimeCatalog(&fakeController{}, loop.DelegationManaged, subagentCatalog(), catalog)

	t.Run("omitted selectors use defaults", func(t *testing.T) {
		request, prepared, err := toolInstance.PrepareCall(context.Background(), uuidForPreparation(), `{"action":"start","description":"inspect","prompt":"map the repo","subagent_type":"worker"}`)
		if err != nil {
			t.Fatalf("PrepareCall() error = %v", err)
		}
		artifact := mustDelegateArtifact(t, prepared)
		if !reflect.DeepEqual(request, tool.Request{}) {
			t.Fatalf("PrepareCall() request = %#v, want empty access request", request)
		}
		if artifact.Request.Operation != tool.DelegateStart || artifact.Request.Agent != "worker" || artifact.Request.Message != "map the repo" || artifact.Request.Wait {
			t.Fatalf("prepared request = %#v, want background start request", artifact.Request)
		}
		if artifact.Runtime == nil {
			t.Fatal("prepared runtime is nil")
		}
		want := tool.DelegateRuntime{Harness: "claude-code", Profile: "acp/claude-code", Model: "sonnet", SmallModel: "sonnet-small", Effort: "medium"}
		if *artifact.Runtime != want {
			t.Fatalf("runtime = %#v, want %#v", *artifact.Runtime, want)
		}
	})

	t.Run("explicit tuple preserves explicitness", func(t *testing.T) {
		_, prepared, err := toolInstance.PrepareCall(context.Background(), uuidForPreparation(), `{"action":"start","description":"inspect","prompt":"run it","subagent_type":"worker","agent_harness":"codex","model":"luna","effort":"none","run_in_background":false}`)
		if err != nil {
			t.Fatalf("PrepareCall() error = %v", err)
		}
		runtime := mustDelegateArtifact(t, prepared).Runtime
		if runtime == nil {
			t.Fatal("prepared runtime is nil")
		}
		want := tool.DelegateRuntime{Harness: "codex", Profile: "acp/codex", Model: "luna", SmallModel: "luna-small", Effort: "none", Explicit: tool.DelegateRuntimeExplicit{Harness: true, Model: true, Effort: true}}
		if *runtime != want {
			t.Fatalf("runtime = %#v, want %#v", *runtime, want)
		}
	})
}

func TestPrepareCallRuntimeSelectorErrorsAreBounded(t *testing.T) {
	t.Parallel()
	catalog := testPreparationCatalog(t)
	tests := []struct {
		name     string
		args     string
		category string
	}{
		{name: "unknown harness is not allowed", args: `{"action":"start","description":"d","prompt":"p","subagent_type":"worker","agent_harness":"missing"}`, category: errCategoryFieldNotAllowed},
		{name: "unknown model is not allowed", args: `{"action":"start","description":"d","prompt":"p","subagent_type":"worker","agent_harness":"claude-code","model":"missing"}`, category: errCategoryFieldNotAllowed},
		{name: "incompatible effort is unknown runtime", args: `{"action":"start","description":"d","prompt":"p","subagent_type":"worker","agent_harness":"claude-code","model":"sonnet","effort":"low"}`, category: errCategoryUnknownRuntime},
		{name: "unknown role is unknown runtime", args: `{"action":"start","description":"d","prompt":"p","subagent_type":"missing"}`, category: errCategoryUnknownRuntime},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			toolInstance := NewSubagentWithRuntimeCatalog(&fakeController{}, loop.DelegationManaged, subagentCatalog(), catalog)
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

func TestPrepareCallRuntimeIsParentScopedAndOptional(t *testing.T) {
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
		toolInstance := NewSubagentWithRuntimeCatalog(&fakeController{}, loop.DelegationManaged, subagentCatalog(), claudeCatalog)
		_, _, err := toolInstance.PrepareCall(context.Background(), uuidForPreparation(), `{"action":"start","description":"d","prompt":"p","subagent_type":"worker","agent_harness":"codex"}`)
		if err == nil || !strings.Contains(err.Error(), errCategoryFieldNotAllowed) {
			t.Fatalf("PrepareCall() error = %v, want %s", err, errCategoryFieldNotAllowed)
		}
	})

	t.Run("no runtime choices leave runtime nil", func(t *testing.T) {
		toolInstance := NewSubagentWithRuntimeCatalog(&fakeController{}, loop.DelegationManaged, subagentCatalog(), noChoice)
		_, prepared, err := toolInstance.PrepareCall(context.Background(), uuidForPreparation(), `{"action":"start","description":"d","prompt":"p","subagent_type":"worker"}`)
		if err != nil {
			t.Fatalf("PrepareCall() error = %v", err)
		}
		if got := mustDelegateArtifact(t, prepared).Runtime; got != nil {
			t.Fatalf("runtime = %#v, want nil", got)
		}
	})

	t.Run("explicit harness with no choices is not allowed", func(t *testing.T) {
		toolInstance := NewSubagentWithRuntimeCatalog(&fakeController{}, loop.DelegationManaged, subagentCatalog(), noChoice)
		_, _, err := toolInstance.PrepareCall(context.Background(), uuidForPreparation(), `{"action":"start","description":"d","prompt":"p","subagent_type":"worker","agent_harness":"claude-code"}`)
		if err == nil || !strings.Contains(err.Error(), errCategoryFieldNotAllowed) {
			t.Fatalf("PrepareCall() error = %v, want %s", err, errCategoryFieldNotAllowed)
		}
	})
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

func uuidForPreparation() uuid.UUID {
	return uuid.MustParse("11111111-1111-4111-8111-111111111111")
}
