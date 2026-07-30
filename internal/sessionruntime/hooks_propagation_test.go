package sessionruntime

import (
	"context"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/looprig/core/content"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/hook"
	"github.com/looprig/harness/pkg/identity"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/tool"
)

func TestLifecycleHooksReachSpawnedAndRestoredNativeLoops(t *testing.T) {
	t.Parallel()

	runner := compiledTurnObserver(t, nil)
	parent := delegateParent(loop.DelegationManaged, "child")
	child := delegateChild("child", "child done")
	topology := Topology{
		Definitions:  []loop.Definition{parent, child},
		Primers:      []identity.AgentName{parent.Name()},
		ActivePrimer: parent.Name(),
	}
	lifecycle, err := NewTopologyLifecycle(
		topology,
		newRestoreStore(t),
		WithLifecycleFingerprintProvider(testFingerprintProvider),
		WithLifecycleHooks(runner),
	)
	if err != nil {
		t.Fatalf("NewTopologyLifecycle: %v", err)
	}
	live, err := lifecycle.NewSession(context.Background(), "")
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	rootID := live.ActiveLoopID()
	assertNativeBackendHooks(t, live, rootID, runner)

	childID, err := live.NewLoop(loop.Provenance{LoopID: rootID}, child)
	if err != nil {
		t.Fatalf("NewLoop child: %v", err)
	}
	assertNativeBackendHooks(t, live, childID, runner)
	sessionID := live.SessionID()
	if err := live.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}

	restored, err := lifecycle.RestoreSession(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("RestoreSession: %v", err)
	}
	t.Cleanup(func() { _ = restored.Shutdown(context.Background()) })
	assertNativeBackendHooks(t, restored, rootID, runner)
	assertNativeBackendHooks(t, restored, childID, runner)
}

func TestForeignBuildersRemainIsolatedFromNativeHooks(t *testing.T) {
	t.Parallel()

	var turnBegins atomic.Int64
	runner := compiledTurnObserver(t, &turnBegins)
	definition := engineCfg(
		&stubLLM{chunks: []content.Chunk{textChunk("unused")}},
		loop.EngineForeignClaude,
		"foreign",
	)

	liveBackend := newFakeBackend()
	liveBuilder := &fakeForeignBuilder{sid: fixedForeignSID, backend: liveBackend}
	live, err := newTestSession(
		context.Background(),
		definition,
		WithHooks(runner),
		WithForeignBuilders(liveBuilder.build, liveBuilder.buildRestored),
	)
	if err != nil {
		t.Fatalf("new foreign session: %v", err)
	}
	if got, ok := live.loopFor(live.ActiveLoopID()); !ok || got != liveBackend {
		t.Fatalf("live foreign backend = %T/%v, want exact builder backend", got, ok)
	}
	if _, err := live.Submit(context.Background(), []content.Block{&content.TextBlock{Text: "foreign live"}}); err != nil {
		t.Fatalf("live Submit: %v", err)
	}
	if turnBegins.Load() != 0 {
		t.Fatalf("native turn hook ran %d times in live foreign backend", turnBegins.Load())
	}
	if err := live.Shutdown(context.Background()); err != nil {
		t.Fatalf("live Shutdown: %v", err)
	}

	sessionID := mustUUID()
	rootLoopID := mustUUID()
	restoredBackend := newFakeBackend()
	restoredBuilder := &fakeForeignBuilder{backend: restoredBackend}
	bound := bindCfg(definition, sessionID, rootLoopID)
	restoreCtx, restoreCancel := context.WithCancel(context.Background())
	t.Cleanup(restoreCancel)
	restored, err := buildRestoredSession(
		restoreCtx,
		restoreCancel,
		bound,
		tool.Bindings{SessionID: sessionID, LoopID: rootLoopID},
		sessionID,
		rootLoopID,
		fixedForeignSID,
		0,
		foldResult{},
		restoredInference{},
		nil,
		fakeSessionJournal{},
		event.NewFactory(uuid.New, time.Now),
		uuid.New,
		time.Now,
		WithHooks(runner),
		WithForeignBuilders(restoredBuilder.build, restoredBuilder.buildRestored),
	)
	if err != nil {
		t.Fatalf("build restored foreign session: %v", err)
	}
	t.Cleanup(func() { _ = restored.Shutdown(context.Background()) })
	if got, ok := restored.loopFor(rootLoopID); !ok || got != restoredBackend {
		t.Fatalf("restored foreign backend = %T/%v, want exact restored builder backend", got, ok)
	}
	if _, err := restored.Submit(context.Background(), []content.Block{&content.TextBlock{Text: "foreign restored"}}); err != nil {
		t.Fatalf("restored Submit: %v", err)
	}
	if turnBegins.Load() != 0 {
		t.Fatalf("native turn hook ran %d times in restored foreign backend", turnBegins.Load())
	}
}

func compiledTurnObserver(t *testing.T, begins *atomic.Int64) *hook.Runner {
	t.Helper()
	runner, err := hook.Compile(hook.Set{Around: []hook.Around{{
		Operation: hook.OperationTurn,
		Begin: func(ctx context.Context, _ hook.Call) (context.Context, hook.FinishFunc) {
			if begins != nil {
				begins.Add(1)
			}
			return ctx, nil
		},
	}}})
	if err != nil {
		t.Fatalf("hook.Compile: %v", err)
	}
	return runner
}

func assertNativeBackendHooks(t *testing.T, session *Session, loopID uuid.UUID, want *hook.Runner) {
	t.Helper()
	backend, ok := session.loopFor(loopID)
	if !ok {
		t.Fatalf("loop %v not registered", loopID)
	}
	value := reflect.ValueOf(backend)
	if value.Kind() != reflect.Pointer || value.IsNil() {
		t.Fatalf("loop %v backend = %T, want native pointer", loopID, backend)
	}
	field := value.Elem().FieldByName("hooks")
	if !field.IsValid() || field.Kind() != reflect.Pointer {
		t.Fatalf("loop %v backend %T does not retain native hooks", loopID, backend)
	}
	if field.Pointer() != reflect.ValueOf(want).Pointer() {
		t.Fatalf("loop %v hooks pointer = %#x, want %#x", loopID, field.Pointer(), reflect.ValueOf(want).Pointer())
	}
}
