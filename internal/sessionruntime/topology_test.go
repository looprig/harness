package sessionruntime

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/identity"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/tool"
)

type activeFailAppender struct {
	mu     sync.Mutex
	events []event.Event
	fail   bool
}

type primerTestTool struct{ name string }

func (t primerTestTool) Info(context.Context) (*tool.ToolInfo, error) {
	return &tool.ToolInfo{Name: t.name}, nil
}
func (primerTestTool) InvokableRun(context.Context, string) (*tool.ToolResult, error) {
	return tool.TextResult(""), nil
}

func TestTopologyBindsEveryPrimerOnceToSessionContext(t *testing.T) {
	var mu sync.Mutex
	calls := map[string]int{}
	contexts := map[string]context.Context{}
	definition := func(name identity.AgentName) loop.Definition {
		toolDefinition := tool.NewDefinition(string(name)+"-tool", 0, func(ctx context.Context, _ tool.Bindings) ([]tool.InvokableTool, error) {
			mu.Lock()
			defer mu.Unlock()
			calls[string(name)]++
			contexts[string(name)] = ctx
			return []tool.InvokableTool{primerTestTool{name: string(name) + "-tool"}}, nil
		})
		defined, err := loop.Define(
			loop.WithName(name), loop.WithInference(&stubLLM{}, validModel("model")),
			loop.WithTools(toolDefinition),
		)
		if err != nil {
			t.Fatal(err)
		}
		return defined
	}
	s, err := NewTopology(context.Background(), Topology{
		Definitions:  []loop.Definition{definition("planner"), definition("builder")},
		Primers:      []identity.AgentName{"planner", "builder"},
		ActivePrimer: "planner",
	}, WithFingerprintProvider(testFingerprintProvider))
	if err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	if calls["planner"] != 1 || calls["builder"] != 1 {
		t.Fatalf("bind calls = %v, want one per primer", calls)
	}
	plannerCtx, builderCtx := contexts["planner"], contexts["builder"]
	mu.Unlock()
	if plannerCtx != builderCtx {
		t.Fatal("primers were not bound to the same session lifetime context")
	}
	if err := s.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-plannerCtx.Done():
	default:
		t.Fatal("bound session context was not cancelled on shutdown")
	}
}

func (a *activeFailAppender) AppendEvent(_ context.Context, ev event.Event) (uint64, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if _, active := ev.(event.ActiveLoopChanged); active && a.fail {
		return 0, errFault
	}
	a.events = append(a.events, ev)
	return uint64(len(a.events)), nil
}

func TestActiveLoopChangeIsDurableBeforeObservable(t *testing.T) {
	appender := &activeFailAppender{}
	planner := cfgWithAgent(&stubLLM{}, "planner")
	builder := cfgWithAgent(&stubLLM{}, "builder")
	s, err := NewTopology(context.Background(), Topology{
		Definitions:  []loop.Definition{planner, builder},
		Primers:      []identity.AgentName{"planner", "builder"},
		ActivePrimer: "planner",
	}, WithEventAppender(appender), WithFingerprintProvider(func(loop.BoundDefinition) event.ConfigFingerprint { return event.ConfigFingerprint{} }))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Shutdown(context.Background()) })
	var builderID uuid.UUID
	s.loopsMu.RLock()
	for id, handle := range s.loops {
		if handle.bound.Name() == "builder" {
			builderID = id
		}
	}
	s.loopsMu.RUnlock()
	if builderID.IsZero() {
		t.Fatal("builder root missing")
	}

	if err := s.SetActiveLoop(context.Background(), builderID); err != nil {
		t.Fatal(err)
	}
	if s.ActiveLoop().ID() != builderID {
		t.Fatal("committed active loop was not observable")
	}

	previous := s.ActiveLoop().ID()
	appender.mu.Lock()
	appender.fail = true
	appender.mu.Unlock()
	err = s.SetActiveLoop(context.Background(), s.findLoopIDByName("planner"))
	var sessionErr *SessionError
	if !errors.As(err, &sessionErr) || sessionErr.Kind != SessionFaulted {
		t.Fatalf("SetActiveLoop error = %v, want SessionFaulted", err)
	}
	if s.ActiveLoop().ID() != previous {
		t.Fatal("failed durable append changed active loop")
	}
	if !errors.Is(err, errFault) {
		t.Fatalf("error does not wrap append failure: %v", err)
	}
}

func (s *Session) findLoopIDByName(name identity.AgentName) uuid.UUID {
	s.loopsMu.RLock()
	defer s.loopsMu.RUnlock()
	for id, handle := range s.loops {
		if handle.bound.Name() == name {
			return id
		}
	}
	return uuid.UUID{}
}
