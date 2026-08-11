package composition_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/looprig/core/content"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/hustle"
	"github.com/looprig/harness/pkg/identity"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/rig"
	"github.com/looprig/harness/pkg/sessionstore"
	"github.com/looprig/harness/pkg/tool"
	"github.com/looprig/inference"
	"github.com/looprig/inference/contextcount"
	"github.com/looprig/inference/model"
	"github.com/looprig/inference/stream"
	"github.com/looprig/storage/memstore"
)

type unusedModel struct{}

func (unusedModel) Invoke(context.Context, inference.Request) (*inference.Response, error) {
	return nil, errors.New("not invoked")
}
func (unusedModel) Stream(context.Context, inference.Request) (*stream.StreamReader[content.Chunk], error) {
	return nil, errors.New("not invoked")
}

type inspectTool struct{}

func (inspectTool) Info(context.Context) (*tool.ToolInfo, error) {
	return &tool.ToolInfo{Name: "inspect", Schema: json.RawMessage(`{"type":"object"}`)}, nil
}
func (inspectTool) InvokableRun(context.Context, string) (*tool.ToolResult, error) {
	return tool.TextResult("inspected"), nil
}

func definition(name string, options ...loop.Option) loop.Definition {
	base := []loop.Option{
		loop.WithName(identity.AgentName(name)),
		loop.WithInference(unusedModel{}, model.Model{
			Provider: "offline", APIFormat: model.APIFormatOpenAI,
			BaseURL: "http://localhost", Name: name,
		}),
	}
	defined, err := loop.Define(append(base, options...)...)
	if err != nil {
		panic(err)
	}
	return defined
}

func Example_compactionAndDelegationComposition() {
	ctx := context.Background()
	tools := tool.NewDefinition("inspect", 0, func(context.Context, tool.Bindings) ([]tool.InvokableTool, error) {
		return []tool.InvokableTool{inspectTool{}}, nil
	})
	sessionID, _ := uuid.New()
	loopID, _ := uuid.New()
	built, err := tools.Build(ctx, tool.Bindings{SessionID: sessionID, LoopID: loopID})
	if err != nil {
		panic(err)
	}
	result, err := built[0].InvokableRun(ctx, `{}`)
	if err != nil {
		panic(err)
	}

	policy := loop.CompactionPolicy{
		ReservedOutput: 1, MaxSummaryTokens: 128,
		CountTimeout: time.Second, Hustle: hustle.Name("context.compact"),
	}
	err = policy.Validate(contextcount.CounterCapability{
		Transport:    contextcount.CounterTransportLocal,
		Retention:    contextcount.RetentionNone,
		TokenizerRev: "v1", Quality: contextcount.CountQualityExactLocal,
	})
	if err != nil {
		panic(err)
	}

	worker := definition("worker")
	planner := definition("planner", loop.WithTools(tools), loop.WithDelegates("worker"),
		loop.WithDelegation(loop.Delegation{Style: loop.DelegationManaged}))
	store, err := sessionstore.Open(memstore.New())
	if err != nil {
		panic(err)
	}
	assembled, err := rig.Define(
		rig.WithLoops(planner, worker), rig.WithPrimers("planner"), rig.WithSessionStore(store),
		rig.WithDelegationLimits(rig.DelegationLimits{Depth: 2, Quota: 4}),
	)
	if err != nil {
		panic(err)
	}
	text := result.Content[0].(*content.TextBlock).Text
	fmt.Println(text, policy.Hustle, planner.Delegates()[0], assembled != nil)
	// Output:
	// inspected context.compact worker true
}
