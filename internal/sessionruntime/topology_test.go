package sessionruntime

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

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

type blockingActiveAppender struct {
	mu      sync.Mutex
	events  []event.Event
	entered chan struct{}
	release chan struct{}
}

func (a *blockingActiveAppender) AppendEvent(_ context.Context, ev event.Event) (uint64, error) {
	if _, ok := ev.(event.ActiveLoopChanged); ok && a.entered != nil {
		close(a.entered)
		<-a.release
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.events = append(a.events, ev)
	return uint64(len(a.events)), nil
}

func TestActiveLoopChangeSerializesWithShutdown(t *testing.T) {
	appender := &blockingActiveAppender{}
	planner := cfgWithAgent(&stubLLM{}, "planner")
	builder := cfgWithAgent(&stubLLM{}, "builder")
	s, err := NewTopology(context.Background(), Topology{Definitions: []loop.Definition{planner, builder}, Primers: []identity.AgentName{"planner", "builder"}, ActivePrimer: "planner"}, WithEventAppender(appender), WithFingerprintProvider(testFingerprintProvider))
	if err != nil {
		t.Fatal(err)
	}
	appender.entered, appender.release = make(chan struct{}), make(chan struct{})
	changeDone := make(chan error, 1)
	go func() { changeDone <- s.SetActiveLoop(context.Background(), s.findLoopIDByName("builder")) }()
	<-appender.entered
	shutdownDone := make(chan error, 1)
	go func() { shutdownDone <- s.Shutdown(context.Background()) }()
	select {
	case err := <-shutdownDone:
		t.Fatalf("Shutdown crossed active-loop append barrier: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	s.loopsMu.RLock()
	closing := s.closing
	s.loopsMu.RUnlock()
	if closing {
		t.Fatal("Shutdown latched closing before active-loop commit")
	}
	close(appender.release)
	if err := <-changeDone; err != nil {
		t.Fatal(err)
	}
	if err := <-shutdownDone; err != nil {
		t.Fatal(err)
	}
}

func TestRestoreTopologyMissingPrimerFailsBeforeRestoreDone(t *testing.T) {
	store := newRestoreStore(t)
	planner := cfgWithAgent(&stubLLM{}, "planner")
	builder := cfgWithAgent(&stubLLM{}, "builder")
	lifecycle, err := NewTopologyLifecycle(singleDefinitionTopology(planner), store, WithLifecycleFingerprintProvider(testFingerprintProvider))
	if err != nil {
		t.Fatal(err)
	}
	original, err := lifecycle.NewSession(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	sessionID, primaryID := original.SessionID(), original.PrimaryLoopID()
	if err := original.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}

	restored, err := RestoreTopology(context.Background(), Topology{
		Definitions: []loop.Definition{planner, builder}, Primers: []identity.AgentName{"planner", "builder"}, ActivePrimer: "planner",
	}, sessionID, store, WithFingerprintProvider(testFingerprintProvider))
	if err == nil || restored != nil {
		t.Fatalf("RestoreTopology = (%v, %v), want nil error controller", restored, err)
	}
	tail := restoreEventTail(t, store, sessionID, primaryID)
	if !lastIs(tail, event.RestoreErrored{}) {
		t.Fatalf("tail does not end RestoreErrored: %T", tail[len(tail)-1])
	}
	for _, ev := range tail {
		if _, done := ev.(event.RestoreDone); done {
			t.Fatal("failed topology restore appended RestoreDone")
		}
	}
}

func TestRestoreTopologyAcquiresLeaseBeforeBinding(t *testing.T) {
	store := newRestoreStore(t)
	var binds atomic.Int32
	define := func(name identity.AgentName) loop.Definition {
		d, err := loop.Define(
			loop.WithName(name), loop.WithInference(&stubLLM{}, validModel("model")),
			loop.WithPolicyRevision("lease-test"),
			loop.WithPermissionFactory(func(context.Context, tool.Bindings) (loop.PermissionGate, error) {
				binds.Add(1)
				return permissionGateStub{}, nil
			}),
		)
		if err != nil {
			t.Fatal(err)
		}
		return d
	}
	topology := Topology{Definitions: []loop.Definition{define("planner"), define("builder")}, Primers: []identity.AgentName{"planner", "builder"}, ActivePrimer: "planner"}
	lifecycle, err := NewTopologyLifecycle(topology, store, WithLifecycleFingerprintProvider(testFingerprintProvider))
	if err != nil {
		t.Fatal(err)
	}
	live, err := lifecycle.NewSession(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	binds.Store(0)
	restored, err := lifecycle.RestoreSession(context.Background(), live.SessionID())
	if restored != nil {
		t.Fatal("concurrent restore returned a controller")
	}
	var restoreErr *RestoreError
	if !errors.As(err, &restoreErr) || restoreErr.Kind != RestoreLeaseFailed {
		t.Fatalf("error = %v, want lease failure", err)
	}
	if binds.Load() != 0 {
		t.Fatalf("restore bound %d loops before acquiring lease", binds.Load())
	}
	if err := live.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestRestoreTopologyBindFailureHasNoRestoreDone(t *testing.T) {
	store := newRestoreStore(t)
	var fail atomic.Bool
	define := func(name identity.AgentName) loop.Definition {
		d, err := loop.Define(
			loop.WithName(name), loop.WithInference(&stubLLM{}, validModel("model")), loop.WithPolicyRevision("bind-test"),
			loop.WithPermissionFactory(func(context.Context, tool.Bindings) (loop.PermissionGate, error) {
				if fail.Load() && name == "builder" {
					return nil, errFault
				}
				return permissionGateStub{}, nil
			}),
		)
		if err != nil {
			t.Fatal(err)
		}
		return d
	}
	topology := Topology{Definitions: []loop.Definition{define("planner"), define("builder")}, Primers: []identity.AgentName{"planner", "builder"}, ActivePrimer: "planner"}
	lifecycle, err := NewTopologyLifecycle(topology, store, WithLifecycleFingerprintProvider(testFingerprintProvider))
	if err != nil {
		t.Fatal(err)
	}
	original, err := lifecycle.NewSession(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	sessionID, primaryID := original.SessionID(), original.PrimaryLoopID()
	if err := original.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	fail.Store(true)
	restored, err := lifecycle.RestoreSession(context.Background(), sessionID)
	if err == nil || restored != nil {
		t.Fatalf("RestoreSession = (%v, %v), want bind failure", restored, err)
	}
	if !errors.Is(err, errFault) {
		t.Fatalf("error does not chain bind failure: %v", err)
	}
	tail := restoreEventTail(t, store, sessionID, primaryID)
	if !lastIs(tail, event.RestoreErrored{}) {
		t.Fatalf("tail = %v", tailTypes(tail))
	}
	for _, ev := range tail {
		if _, ok := ev.(event.RestoreDone); ok {
			t.Fatal("bind failure appended RestoreDone")
		}
	}
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
