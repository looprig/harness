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

func TestForeignServicesBuilderReceivesConfiguredSnapshot(t *testing.T) {
	t.Parallel()

	backend := newFakeBackend()
	capability := []byte("live-capability")
	services := foreign.NewServices(foreign.NewBrokerDescriptor("broker://live", capability), nil)
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
		WithForeignServicesBuilders(build, restored, services))
	if err != nil {
		t.Fatalf("newSession: %v", err)
	}
	t.Cleanup(func() { _ = s.Shutdown(context.Background()) })
	if got.Broker.Endpoint() != "broker://live" {
		t.Fatalf("services endpoint = %q, want broker://live", got.Broker.Endpoint())
	}
	if string(got.Broker.Capability()) != string(capability) {
		t.Fatalf("services capability = %q, want %q", got.Broker.Capability(), capability)
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

func TestServicesRestoredBuilderReceivesConfiguredSnapshot(t *testing.T) {
	t.Parallel()

	folded := foldLoop([]event.Event{
		event.TurnStarted{Message: foldUserMsg("hello")},
		foldStepGroup(aiMessage("hi")),
		event.TurnDone{Message: aiMessage("hi")},
	})
	backend := newFakeBackend()
	backend.msgs = folded.Msgs
	backend.turnIndex = folded.TurnIndex
	services := foreign.NewServices(foreign.NewBrokerDescriptor("broker://restore", []byte("restore-capability")), nil)
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
		WithForeignServicesBuilders(build, restored, services))
	if err != nil {
		t.Fatalf("buildRestoredSession: %v", err)
	}
	t.Cleanup(func() { _ = s.Shutdown(context.Background()) })
	if got.Broker.Endpoint() != "broker://restore" {
		t.Fatalf("restored services endpoint = %q, want broker://restore", got.Broker.Endpoint())
	}
}
