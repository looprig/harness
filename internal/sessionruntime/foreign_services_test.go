package sessionruntime

import (
	"context"
	"testing"
	"time"

	"github.com/looprig/core/content"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/foreign"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/tool"
)

func TestForeignServicesBuilderReceivesZeroSnapshotUntilPerLoopBinding(t *testing.T) {
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
	if got.Broker.Endpoint() != "" || got.Broker.Capability() != nil || got.Delivery != nil {
		t.Fatalf("services = %#v, want zero Services before per-loop binding", got)
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

func TestServicesRestoredBuilderReceivesZeroSnapshotUntilPerLoopBinding(t *testing.T) {
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
	if got.Broker.Endpoint() != "" || got.Broker.Capability() != nil || got.Delivery != nil {
		t.Fatalf("restored services = %#v, want zero Services before per-loop binding", got)
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
