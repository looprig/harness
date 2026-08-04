package delegationtool

import (
	"context"
	"testing"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/tool"
)

func TestAgentToolDefinitionProducesFourInvokableTools(t *testing.T) {
	t.Parallel()

	want := []string{"ListAgents", "MessageAgent", "StartAgent", "StopAgent"}
	definition := Definition(loop.DelegationManaged, []AgentCatalogEntry{{Name: "worker"}})
	if got := definition.ProducedToolNames(); !equalStrings(got, want) {
		t.Fatalf("ProducedToolNames() = %q, want %q", got, want)
	}

	tools, err := definition.Build(context.Background(), tool.Bindings{
		SessionID: mustDefinitionUUID(t),
		LoopID:    mustDefinitionUUID(t),
		Delegate:  &fakeController{},
	})
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	if len(tools) != len(want) {
		t.Fatalf("Build() returned %d tools, want %d", len(tools), len(want))
	}
	for i, built := range tools {
		info, infoErr := built.Info(context.Background())
		if infoErr != nil {
			t.Fatalf("tools[%d].Info() error = %v", i, infoErr)
		}
		if info.Name != want[i] {
			t.Errorf("tools[%d].Info().Name = %q, want %q", i, info.Name, want[i])
		}
	}
}

func mustDefinitionUUID(t *testing.T) uuid.UUID {
	t.Helper()
	id, err := uuid.New()
	if err != nil {
		t.Fatal(err)
	}
	return id
}
