package lifecycle_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/looprig/core/content"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/rig"
	"github.com/looprig/harness/pkg/sessionstore"
	"github.com/looprig/harness/pkg/tool"
	"github.com/looprig/inference"
	"github.com/looprig/inference/model"
	"github.com/looprig/inference/stream"
	"github.com/looprig/storage/memstore"
)

type offlineModel struct{}

func (offlineModel) Invoke(context.Context, inference.Request) (*inference.Response, error) {
	return nil, errors.New("streaming example does not call Invoke")
}

func (offlineModel) Stream(context.Context, inference.Request) (*stream.StreamReader[content.Chunk], error) {
	read := false
	return stream.NewStreamReader(func() (content.Chunk, error) {
		if read {
			return nil, io.EOF
		}
		read = true
		return &content.TextChunk{Text: "ready"}, nil
	}, nil), nil
}

type statusTool struct{}

func (statusTool) Info(context.Context) (*tool.ToolInfo, error) {
	return &tool.ToolInfo{Name: "status", Desc: "Return offline status", Schema: json.RawMessage(`{"type":"object"}`)}, nil
}

func (statusTool) InvokableRun(context.Context, string) (*tool.ToolResult, error) {
	return tool.TextResult("ok"), nil
}

func Example_toolLoopRigSessionLifecycle() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	status := tool.NewDefinition("status", 0, func(context.Context, tool.Bindings) ([]tool.InvokableTool, error) {
		return []tool.InvokableTool{statusTool{}}, nil
	})
	agent, err := loop.Define(
		loop.WithName("assistant"),
		loop.WithInference(offlineModel{}, model.Model{
			Provider: "offline", APIFormat: model.APIFormatOpenAI,
			BaseURL: "http://localhost", Name: "fixture",
		}),
		loop.WithTools(status),
	)
	if err != nil {
		panic(err)
	}
	store, err := sessionstore.Open(memstore.New())
	if err != nil {
		panic(err)
	}
	harness, err := rig.Define(
		rig.WithLoops(agent),
		rig.WithPrimers("assistant"),
		rig.WithSessionStore(store),
	)
	if err != nil {
		panic(err)
	}

	live, err := harness.NewSession(ctx)
	if err != nil {
		panic(err)
	}
	subscription, err := live.SubscribeEvents(event.EventFilter{Enduring: event.LoopScope{All: true}})
	if err != nil {
		panic(err)
	}
	_, err = live.Submit(ctx, []content.Block{&content.TextBlock{Text: "Say ready"}})
	if err != nil {
		panic(err)
	}
	for delivery := range subscription.Events() {
		if delivery.Event.EndsTurn() {
			break
		}
	}
	_ = subscription.Close()

	id := live.SessionID()
	if err := live.Shutdown(ctx); err != nil {
		panic(err)
	}
	restored, err := harness.RestoreSession(ctx, id)
	if err != nil {
		panic(err)
	}
	fmt.Println(!id.IsZero(), restored.SessionID() == id, restored.ActiveLoop().Model().Name)
	if err := restored.Shutdown(ctx); err != nil {
		panic(err)
	}
	// Output:
	// true true fixture
}
