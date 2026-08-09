package sessionruntime

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/looprig/core/content"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/foreign"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/tool"
)

func TestForeignServicesBuilderFailureUnregistersDeliveryHook(t *testing.T) {
	t.Parallel()
	var sessions []*Session
	build := foreign.ServicesBuilder(func(_ context.Context, _, _ uuid.UUID, _ loop.Provenance,
		publisher foreign.EventPublisher, _ loop.BoundDefinition, _ func() (uuid.UUID, error), _ *event.Factory,
		_ foreign.Services) (loop.Backend, string, error) {
		if session, ok := publisher.(*Session); ok {
			sessions = append(sessions, session)
		}
		return nil, "", errors.New("builder failed")
	})
	restored := foreign.ServicesRestoredBuilder(func(_ context.Context, _, _ uuid.UUID, _ loop.Provenance,
		_ foreign.EventPublisher, _ loop.BoundDefinition, _ func() (uuid.UUID, error), _ *event.Factory,
		_ foreign.RestoredForeign, _ foreign.Services) (loop.Backend, error) {
		return nil, nil
	})
	cfg := engineCfg(&stubLLM{chunks: []content.Chunk{textChunk("x")}}, loop.EngineForeignClaude, "x")
	for attempt := 0; attempt < 8; attempt++ {
		_, err := newSession(context.Background(), cfg, uuid.New, time.Now,
			WithFingerprintProvider(testFingerprintProvider), WithForeignServicesBuilders(build, restored))
		if err == nil {
			t.Fatal("newSession unexpectedly succeeded")
		}
	}
	if len(sessions) != 8 {
		t.Fatalf("builder captured %d sessions, want 8", len(sessions))
	}
	for i, session := range sessions {
		session.foreignDeliveryMu.RLock()
		retained := len(session.foreignDeliveryHooks)
		session.foreignDeliveryMu.RUnlock()
		if retained != 0 {
			t.Fatalf("failed construction %d retained %d foreign delivery hooks", i, retained)
		}
	}
}

func TestForeignDeliveryHookUnregisterIsIdentitySafe(t *testing.T) {
	t.Parallel()
	loopID := mustUUID()
	session := &Session{sessionID: mustUUID()}
	other := &Session{sessionID: mustUUID()}
	first := newForeignDeliveryHook(session, loopID)
	second := newForeignDeliveryHook(session, loopID)
	session.unregisterForeignDeliveryHook(first)
	if got := session.foreignDeliveryHookFor(loopID); got != second {
		t.Fatalf("unregister old hook removed replacement: got %p want %p", got, second)
	}
	other.unregisterForeignDeliveryHook(second)
	if got := session.foreignDeliveryHookFor(loopID); got != second {
		t.Fatalf("unregister from another session changed hook: got %p want %p", got, second)
	}
	session.unregisterForeignDeliveryHook(second)
	if got := session.foreignDeliveryHookFor(loopID); got != nil {
		t.Fatalf("unregister current hook retained %p", got)
	}
}

func TestForeignServicesBuilderReceivesFreshPerLoopDeliveryHook(t *testing.T) {
	t.Parallel()

	backend := newFakeBackend()
	var got foreign.Services
	build := foreign.ServicesBuilder(func(_ context.Context, _, _ uuid.UUID, _ loop.Provenance,
		_ foreign.EventPublisher, _ loop.BoundDefinition, _ func() (uuid.UUID, error), _ *event.Factory,
		received foreign.Services) (loop.Backend, string, error) {
		got = received.Clone()
		return backend, fixedForeignSID, nil
	})
	restored := foreign.ServicesRestoredBuilder(func(_ context.Context, _, _ uuid.UUID, _ loop.Provenance,
		_ foreign.EventPublisher, _ loop.BoundDefinition, _ func() (uuid.UUID, error), _ *event.Factory,
		_ foreign.RestoredForeign, _ foreign.Services) (loop.Backend, error) {
		return backend, nil
	})

	c := engineCfg(&stubLLM{chunks: []content.Chunk{textChunk("x")}}, loop.EngineForeignClaude, "x")
	s, err := newSession(context.Background(), c, uuid.New, time.Now,
		WithFingerprintProvider(testFingerprintProvider),
		WithForeignServicesBuilders(build, restored))
	if err != nil {
		t.Fatalf("newSession: %v", err)
	}
	t.Cleanup(func() { _ = s.Shutdown(context.Background()) })
	if got.Broker.Endpoint() != "" || got.Broker.Capability() != nil || got.Delivery == nil {
		t.Fatalf("services = %#v, want empty broker plus a per-loop delivery hook", got)
	}
}

func TestLegacyForeignBuilderPathRemainsUsableWithZeroServices(t *testing.T) {
	t.Parallel()

	builder := &fakeForeignBuilder{sid: fixedForeignSID, backend: newFakeBackend()}
	c := engineCfg(&stubLLM{chunks: []content.Chunk{textChunk("x")}}, loop.EngineForeignClaude, "x")
	s, err := newSession(context.Background(), c, uuid.New, time.Now,
		WithFingerprintProvider(testFingerprintProvider),
		WithForeignBuilders(builder.build, builder.buildRestored))
	if err != nil {
		t.Fatalf("newSession: %v", err)
	}
	t.Cleanup(func() { _ = s.Shutdown(context.Background()) })
	builder.mu.Lock()
	calls := builder.calls
	builder.mu.Unlock()
	if calls != 1 {
		t.Fatalf("legacy builder calls = %d, want 1", calls)
	}
}

func TestServicesRestoredBuilderReceivesFreshPerLoopDeliveryHook(t *testing.T) {
	t.Parallel()

	folded := foldLoop([]event.Event{
		event.TurnStarted{Message: foldUserMsg("hello")},
		foldStepGroup(aiMessage("hi")),
		event.TurnDone{Message: aiMessage("hi")},
	})
	backend := newFakeBackend()
	backend.msgs = folded.Msgs
	backend.turnIndex = folded.TurnIndex
	var got foreign.Services
	build := foreign.ServicesBuilder(func(_ context.Context, _, _ uuid.UUID, _ loop.Provenance,
		_ foreign.EventPublisher, _ loop.BoundDefinition, _ func() (uuid.UUID, error), _ *event.Factory,
		_ foreign.Services) (loop.Backend, string, error) {
		return backend, fixedForeignSID, nil
	})
	restored := foreign.ServicesRestoredBuilder(func(_ context.Context, _, _ uuid.UUID, _ loop.Provenance,
		_ foreign.EventPublisher, _ loop.BoundDefinition, _ func() (uuid.UUID, error), _ *event.Factory,
		_ foreign.RestoredForeign, received foreign.Services) (loop.Backend, error) {
		got = received.Clone()
		return backend, nil
	})

	sessionID := mustUUID()
	rootLoopID := mustUUID()
	c := bindCfg(engineCfg(&stubLLM{chunks: []content.Chunk{textChunk("x")}}, loop.EngineForeignClaude, "x"), sessionID, rootLoopID)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	s, err := buildRestoredSession(ctx, cancel, c, tool.Bindings{SessionID: sessionID, LoopID: rootLoopID}, sessionID, rootLoopID,
		fixedForeignSID, 0, folded, restoredInference{}, nil, nil, fakeSessionJournal{},
		event.NewFactory(uuid.New, time.Now), uuid.New, time.Now,
		WithForeignServicesBuilders(build, restored))
	if err != nil {
		t.Fatalf("buildRestoredSession: %v", err)
	}
	t.Cleanup(func() { _ = s.Shutdown(context.Background()) })
	if got.Broker.Endpoint() != "" || got.Broker.Capability() != nil || got.Delivery == nil {
		t.Fatalf("restored services = %#v, want empty broker plus a per-loop delivery hook", got)
	}
}

func TestLifecycleForeignServicesBuildersCarryNoFixedServices(t *testing.T) {
	var got foreign.Services
	build := foreign.ServicesBuilder(func(_ context.Context, _, _ uuid.UUID, _ loop.Provenance,
		_ foreign.EventPublisher, _ loop.BoundDefinition, _ func() (uuid.UUID, error), _ *event.Factory,
		received foreign.Services) (loop.Backend, string, error) {
		got = received
		return nil, "", nil
	})
	restored := foreign.ServicesRestoredBuilder(func(_ context.Context, _, _ uuid.UUID, _ loop.Provenance,
		_ foreign.EventPublisher, _ loop.BoundDefinition, _ func() (uuid.UUID, error), _ *event.Factory,
		_ foreign.RestoredForeign, received foreign.Services) (loop.Backend, error) {
		got = received
		return nil, nil
	})

	lifecycle := &Lifecycle{}
	WithLifecycleForeignServicesBuilders(build, restored)(lifecycle)
	if len(lifecycle.baseOpts) != 1 {
		t.Fatalf("lifecycle base options = %d, want one services option", len(lifecycle.baseOpts))
	}
	session := &Session{}
	lifecycle.baseOpts[0](session)
	if session.foreignBuildServices == nil || session.foreignBuildRestoredServices == nil {
		t.Fatal("lifecycle did not forward both services builders")
	}
	if _, _, err := session.foreignBuildServices(context.Background(), uuid.UUID{}, uuid.UUID{}, loop.Provenance{}, nil, nil, nil, nil, foreign.Services{}); err != nil {
		t.Fatalf("forwarded services builder: %v", err)
	}
	if got.Broker.Endpoint() != "" || got.Broker.Capability() != nil || got.Delivery != nil {
		t.Fatalf("forwarded services = %#v, want zero Services", got)
	}
}
