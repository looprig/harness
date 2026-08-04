package delegationtool

import (
	"fmt"
	"strings"
	"testing"

	"github.com/looprig/harness/pkg/identity"
	"github.com/looprig/harness/pkg/loop"
	inferencemodel "github.com/looprig/inference/model"
)

func TestBuildStartAgentDescriptionRendersAdmittedCapabilities(t *testing.T) {
	roles := []AgentCatalogEntry{
		{Name: "reviewer", Description: "Reviews changes."},
		{Name: "ghost", Description: "Must not be rendered."},
		{Name: "planner", Description: "Plans implementation work."},
	}
	entries := []loop.RuntimeCatalogEntry{
		descriptionEntry("hidden", "private", loop.RuntimeSourceGateway, true, "Hidden harness.", descriptionModel("hidden", "Hidden model.", inferencemodel.EffortMedium)),
		descriptionEntry("planner", "codex", loop.RuntimeSourceNative, false, "Codex native harness.", descriptionModel("native", "Use native auth.", inferencemodel.EffortMedium)),
		descriptionEntry("reviewer", "looprig", loop.RuntimeSourceNative, true, "In-process review harness.", descriptionModel("review", "Use for code review.", inferencemodel.EffortHigh)),
		descriptionEntry("planner", "codex", loop.RuntimeSourceGateway, false, "Codex gateway harness.", descriptionModel("beta", "Use beta for broad analysis.", inferencemodel.EffortHigh)),
		descriptionEntry("planner", "looprig", loop.RuntimeSourceNative, true, "In-process Loop harness.",
			descriptionModel("zeta", "Use zeta for difficult planning.", inferencemodel.EffortLow, inferencemodel.EffortHigh),
			descriptionModel("alpha", "Use alpha for focused planning.", inferencemodel.EffortLow, inferencemodel.EffortMedium)),
	}
	entries[4].DefaultModel = "zeta"
	entries[4].Models[0].Target.Provider = "secret-provider"
	entries[4].Models[0].Target.Name = "internal-target-name"
	entries[4].Models[0].Target.BaseURL = "https://private.example/v1"
	catalog := descriptionCatalog(t, entries...)

	got := buildStartAgentDescription(roles, catalog)

	if !strings.Contains(got, "<available_agents>\n- planner: Plans implementation work.\n- reviewer: Reviews changes.\n</available_agents>") {
		t.Fatalf("agent section = %q, want admitted roles in name order", got)
	}
	if strings.Contains(got, "ghost") || strings.Contains(got, "hidden") || strings.Contains(got, "private") {
		t.Fatalf("description leaked an unadmitted capability: %q", got)
	}
	if count := strings.Count(got, " default:"); count != 2 {
		t.Fatalf("default tuple markers = %d, want one per admitted agent: %q", count, got)
	}
	assertDescriptionOrder(t, got,
		"agent_type=planner default: harness=looprig source=native model=zeta effort=high",
		"harness=looprig source=native model=alpha efforts=[low,medium]",
		"harness=looprig source=native model=zeta efforts=[low,high]",
		"harness=codex source=gateway model=beta efforts=[high]",
		"harness=codex source=native model=native efforts=[medium]",
		"agent_type=reviewer default: harness=looprig source=native model=review effort=high",
	)
	for _, visible := range []string{
		"In-process Loop harness.",
		"Codex gateway harness.",
		"Codex native harness.",
		"Use alpha for focused planning.",
		"Use zeta for difficult planning.",
		"Use beta for broad analysis.",
		"Use native auth.",
	} {
		if count := strings.Count(got, visible); count != 1 {
			t.Errorf("visible description %q count = %d, want 1: %q", visible, count, got)
		}
	}
	for _, secret := range []string{"secret-provider", "internal-target-name", "https://private.example/v1", "profile/"} {
		if strings.Contains(got, secret) {
			t.Errorf("description contains runtime internal %q: %q", secret, got)
		}
	}
}

func TestBuildStartAgentDescriptionRendersNativeRuntimeWithoutACP(t *testing.T) {
	catalog := descriptionCatalog(t, descriptionEntry(
		"worker", "looprig", loop.RuntimeSourceNative, true, "In-process Loop harness.",
		descriptionModel("local", "Use the local model for implementation.", inferencemodel.EffortLow, inferencemodel.EffortHigh),
	))

	got := buildStartAgentDescription([]AgentCatalogEntry{{Name: "worker", Description: "Builds changes."}}, catalog)

	for _, want := range []string{"<available_agents>", "<available_agent_runtimes>", "harness=looprig", "source=native", "model=local", "efforts=[low,high]"} {
		if !strings.Contains(got, want) {
			t.Errorf("description missing %q: %q", want, got)
		}
	}
	for _, acp := range []string{"harness=codex", "harness=claude-code", "source=gateway"} {
		if strings.Contains(got, acp) {
			t.Errorf("description unexpectedly contains ACP capability %q: %q", acp, got)
		}
	}
}

func TestBuildStartAgentDescriptionBoundsRowsAndTotalBytes(t *testing.T) {
	t.Run("agent row count", func(t *testing.T) {
		roles := make([]AgentCatalogEntry, 0, maxAvailableAgentRows+5)
		for i := 0; i < maxAvailableAgentRows+5; i++ {
			roles = append(roles, AgentCatalogEntry{Name: identity.AgentName(fmt.Sprintf("agent-%03d", i)), Description: "Admitted role."})
		}

		got := buildStartAgentDescription(roles, loop.RuntimeCatalog{})
		assertBoundedDescription(t, got)
		agentSection := got[strings.Index(got, "<available_agents>"):strings.Index(got, "</available_agents>")]
		if rows := strings.Count(agentSection, "\n- "); rows != maxAvailableAgentRows {
			t.Fatalf("agent rows = %d, want %d", rows, maxAvailableAgentRows)
		}
	})

	t.Run("row count", func(t *testing.T) {
		entries := make([]loop.RuntimeCatalogEntry, 0, maxAvailableAgentRuntimeRows+5)
		for i := 0; i < maxAvailableAgentRuntimeRows+5; i++ {
			entries = append(entries, descriptionEntry(
				"worker",
				loop.AgentHarnessName(fmt.Sprintf("harness-%03d", i)),
				loop.RuntimeSourceGateway,
				i == 0,
				"Gateway harness.",
				descriptionModel(loop.ModelAlias(fmt.Sprintf("model-%03d", i)), "Use this model.", inferencemodel.EffortMedium),
			))
		}

		got := buildStartAgentDescription([]AgentCatalogEntry{{Name: "worker", Description: "Builds changes."}}, descriptionCatalog(t, entries...))
		assertBoundedDescription(t, got)
		if rows := strings.Count(got, "\n- agent_type=") + strings.Count(got, "\n  - harness="); rows > maxAvailableAgentRuntimeRows {
			t.Fatalf("runtime rows = %d, want at most %d", rows, maxAvailableAgentRuntimeRows)
		}
	})

	t.Run("total bytes", func(t *testing.T) {
		entries := make([]loop.RuntimeCatalogEntry, 0, 60)
		for i := 0; i < 60; i++ {
			entries = append(entries, descriptionEntry(
				"worker",
				loop.AgentHarnessName(fmt.Sprintf("large-%03d", i)),
				loop.RuntimeSourceGateway,
				i == 0,
				strings.Repeat("h", 256),
				descriptionModel(loop.ModelAlias(fmt.Sprintf("model-%03d", i)), strings.Repeat("m", 256), inferencemodel.EffortMedium),
			))
		}

		got := buildStartAgentDescription([]AgentCatalogEntry{{Name: "worker", Description: "Builds changes."}}, descriptionCatalog(t, entries...))
		assertBoundedDescription(t, got)
		if strings.Contains(got, strings.Repeat("m", 128)) && !strings.Contains(got, strings.Repeat("m", 256)) {
			t.Fatalf("description cut through a model description: %q", got)
		}
	})
}

func TestBuildStartAgentDescriptionIsStableAcrossShuffledInputs(t *testing.T) {
	roles := []AgentCatalogEntry{{Name: "worker", Description: "Builds."}, {Name: "planner", Description: "Plans."}}
	entries := []loop.RuntimeCatalogEntry{
		descriptionEntry("worker", "codex", loop.RuntimeSourceGateway, false, "Codex harness.", descriptionModel("beta", "Use beta.", inferencemodel.EffortHigh)),
		descriptionEntry("planner", "looprig", loop.RuntimeSourceNative, true, "Planner harness.", descriptionModel("plan", "Use plan.", inferencemodel.EffortMedium)),
		descriptionEntry("worker", "looprig", loop.RuntimeSourceNative, true, "Worker harness.",
			descriptionModel("alpha", "Use alpha.", inferencemodel.EffortLow, inferencemodel.EffortHigh),
			descriptionModel("gamma", "Use gamma.", inferencemodel.EffortMedium)),
	}
	want := buildStartAgentDescription(roles, descriptionCatalog(t, entries...))

	for left, right := 0, len(roles)-1; left < right; left, right = left+1, right-1 {
		roles[left], roles[right] = roles[right], roles[left]
	}
	for left, right := 0, len(entries)-1; left < right; left, right = left+1, right-1 {
		entries[left], entries[right] = entries[right], entries[left]
	}
	entries[0].Models[0], entries[0].Models[1] = entries[0].Models[1], entries[0].Models[0]
	got := buildStartAgentDescription(roles, descriptionCatalog(t, entries...))
	if got != want {
		t.Fatalf("shuffled description differs:\nwant:\n%s\n\ngot:\n%s", want, got)
	}
}

func assertDescriptionOrder(t *testing.T, description string, values ...string) {
	t.Helper()
	previous := -1
	for _, value := range values {
		index := strings.Index(description, value)
		if index < 0 {
			t.Fatalf("description missing %q: %q", value, description)
		}
		if index <= previous {
			t.Fatalf("description order is not deterministic at %q: %q", value, description)
		}
		previous = index
	}
}

func assertBoundedDescription(t *testing.T, description string) {
	t.Helper()
	if got := strings.Count(description, availableAgentElisionMarker); got != 1 {
		t.Fatalf("elision marker count = %d, want 1: %q", got, description)
	}
	if len(description) > maxStartAgentDescriptionBytes {
		t.Fatalf("description bytes = %d, want at most %d", len(description), maxStartAgentDescriptionBytes)
	}
}

func descriptionModel(alias loop.ModelAlias, description string, efforts ...inferencemodel.Effort) loop.RuntimeModelOption {
	return loop.RuntimeModelOption{
		Alias:         alias,
		Description:   description,
		Target:        inferencemodel.Model{Provider: "provider", Name: string(alias), Sampling: inferencemodel.Sampling{Effort: efforts[0]}},
		DefaultEffort: efforts[len(efforts)-1],
		Efforts:       append([]inferencemodel.Effort(nil), efforts...),
	}
}

func descriptionEntry(agent identity.AgentName, harness loop.AgentHarnessName, source loop.RuntimeSourceName, defaultRuntime bool, description string, models ...loop.RuntimeModelOption) loop.RuntimeCatalogEntry {
	credential := loop.CredentialGatewayBacked
	if source == loop.RuntimeSourceNative {
		credential = loop.CredentialNativeAuth
	}
	return loop.RuntimeCatalogEntry{
		AgentType:    agent,
		AgentHarness: harness,
		Profile:      loop.RuntimeProfileName(fmt.Sprintf("profile/%s/%s", harness, source)),
		Description:  description,
		Source:       source,
		Credential:   credential,
		Default:      defaultRuntime,
		DefaultModel: models[0].Alias,
		Models:       models,
	}
}

func descriptionCatalog(t *testing.T, entries ...loop.RuntimeCatalogEntry) loop.RuntimeCatalog {
	t.Helper()
	catalog, err := loop.NewRuntimeCatalog(entries)
	if err != nil {
		t.Fatalf("NewRuntimeCatalog() error = %v", err)
	}
	return catalog
}
