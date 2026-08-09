package sessionruntime

import (
	"bytes"
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/looprig/core/content"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/command"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/foreign"
	"github.com/looprig/harness/pkg/identity"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/tool"
)

func TestCollabLifecycleStartsBrokerBeforeForeignServicesConstruction(t *testing.T) {
	var (
		mu       sync.Mutex
		services []foreign.Services
	)
	build := foreign.ServicesBuilder(func(_ context.Context, _, _ uuid.UUID, _ loop.Provenance,
		_ foreign.EventPublisher, _ loop.BoundDefinition, _ func() (uuid.UUID, error), _ *event.Factory,
		received foreign.Services) (loop.Backend, string, error) {
		mu.Lock()
		services = append(services, received.Clone())
		mu.Unlock()
		return newFakeBackend(), fixedForeignSID, nil
	})
	restored := foreign.ServicesRestoredBuilder(func(_ context.Context, _, _ uuid.UUID, _ loop.Provenance,
		_ foreign.EventPublisher, _ loop.BoundDefinition, _ func() (uuid.UUID, error), _ *event.Factory,
		_ foreign.RestoredForeign, _ foreign.Services) (loop.Backend, error) {
		return newFakeBackend(), nil
	})
	cfg := engineCfg(&stubLLM{chunks: []content.Chunk{textChunk("x")}}, loop.EngineForeignClaude, "x")

	s, err := newSession(context.Background(), cfg, uuid.New, time.Now,
		WithFingerprintProvider(testFingerprintProvider),
		WithForeignServicesBuilders(build, restored))
	if !collabPlatformSupported() {
		if !errors.Is(err, errCollabBrokerUnsupportedPlatform) {
			t.Fatalf("unsupported-platform newSession error = %v, want fixed broker error", err)
		}
		if s != nil {
			t.Fatal("unsupported-platform construction returned a live session")
		}
		return
	}
	skipCollabLifecycleUnavailable(t, err)
	if err != nil {
		t.Fatalf("newSession: %v", err)
	}
	t.Cleanup(func() { _ = s.Shutdown(context.Background()) })

	mu.Lock()
	got := append([]foreign.Services(nil), services...)
	mu.Unlock()
	if len(got) != 1 {
		t.Fatalf("foreign services calls = %d, want 1", len(got))
	}
	if got[0].Broker.Endpoint() == "" {
		t.Fatal("foreign builder received an empty broker endpoint")
	}
	if capability := got[0].Broker.Capability(); len(capability) != collabCapabilityBytes {
		t.Fatalf("foreign builder capability length = %d, want %d", len(capability), collabCapabilityBytes)
	}
}

func TestCollabLifecycleMintsOneCapabilityPerForeignOrigin(t *testing.T) {
	if !collabPlatformSupported() {
		t.Skip("collaboration broker requires Unix-domain sockets")
	}
	var (
		mu       sync.Mutex
		services []foreign.Services
	)
	build := foreign.ServicesBuilder(func(_ context.Context, _, _ uuid.UUID, _ loop.Provenance,
		_ foreign.EventPublisher, _ loop.BoundDefinition, _ func() (uuid.UUID, error), _ *event.Factory,
		received foreign.Services) (loop.Backend, string, error) {
		mu.Lock()
		services = append(services, received.Clone())
		mu.Unlock()
		return newFakeBackend(), fixedForeignSID, nil
	})
	restored := foreign.ServicesRestoredBuilder(func(_ context.Context, _, _ uuid.UUID, _ loop.Provenance,
		_ foreign.EventPublisher, _ loop.BoundDefinition, _ func() (uuid.UUID, error), _ *event.Factory,
		_ foreign.RestoredForeign, _ foreign.Services) (loop.Backend, error) {
		return newFakeBackend(), nil
	})
	first := engineCfg(&stubLLM{chunks: []content.Chunk{textChunk("one")}}, loop.EngineForeignClaude, "one")
	second := mustDefine(
		loop.WithName("second"),
		loop.WithInference(&stubLLM{chunks: []content.Chunk{textChunk("two")}}, validModel("m")),
		loop.WithEngine(loop.EngineForeignClaude),
		loop.WithSystem("two"),
		loop.WithDrainTimeout(100*time.Millisecond),
	)
	topology := Topology{
		Definitions:  []loop.Definition{first, second},
		Primers:      []identity.AgentName{first.Name(), second.Name()},
		ActivePrimer: first.Name(),
	}
	s, err := newSessionTopology(context.Background(), topology, uuid.New, time.Now,
		WithFingerprintProvider(testFingerprintProvider),
		WithForeignServicesBuilders(build, restored))
	skipCollabLifecycleUnavailable(t, err)
	if err != nil {
		t.Fatalf("newSessionTopology: %v", err)
	}
	t.Cleanup(func() { _ = s.Shutdown(context.Background()) })
	mu.Lock()
	got := append([]foreign.Services(nil), services...)
	mu.Unlock()
	if len(got) != 2 {
		t.Fatalf("foreign services calls = %d, want 2", len(got))
	}
	firstCapability := got[0].Broker.Capability()
	secondCapability := got[1].Broker.Capability()
	if len(firstCapability) != collabCapabilityBytes || len(secondCapability) != collabCapabilityBytes {
		t.Fatalf("capability lengths = %d, %d; want %d each", len(firstCapability), len(secondCapability), collabCapabilityBytes)
	}
	if bytes.Equal(firstCapability, secondCapability) {
		t.Fatal("foreign origins received the same capability")
	}
	s.collabBrokerMu.Lock()
	broker := s.collabBroker
	s.collabBrokerMu.Unlock()
	if broker == nil {
		t.Fatal("session did not retain its collaboration broker")
	}
	broker.mu.RLock()
	principalCount := len(broker.byLoop)
	broker.mu.RUnlock()
	if principalCount != 2 {
		t.Fatalf("broker principals = %d, want 2", principalCount)
	}
}

func TestCollabLifecycleLeavesLegacyAndNativeLoopsWithoutDescriptor(t *testing.T) {
	legacy := &fakeForeignBuilder{sid: fixedForeignSID, backend: newFakeBackend()}
	foreignCfg := engineCfg(&stubLLM{chunks: []content.Chunk{textChunk("foreign")}}, loop.EngineForeignClaude, "foreign")
	foreignSession, err := newSession(context.Background(), foreignCfg, uuid.New, time.Now,
		WithFingerprintProvider(testFingerprintProvider),
		WithForeignBuilders(legacy.build, legacy.buildRestored))
	if err != nil {
		t.Fatalf("legacy foreign newSession: %v", err)
	}
	t.Cleanup(func() { _ = foreignSession.Shutdown(context.Background()) })
	if foreignSession.collabBroker != nil {
		t.Fatal("legacy foreign session started a collaboration broker")
	}

	nativeCfg := engineCfg(&stubLLM{chunks: []content.Chunk{textChunk("native")}}, loop.EngineNative, "native")
	nativeSession, err := newSession(context.Background(), nativeCfg, uuid.New, time.Now,
		WithFingerprintProvider(testFingerprintProvider))
	if err != nil {
		t.Fatalf("native newSession: %v", err)
	}
	t.Cleanup(func() { _ = nativeSession.Shutdown(context.Background()) })
	if nativeSession.collabBroker != nil {
		t.Fatal("native session started a collaboration broker")
	}
}

func TestCollabLifecycleRevokesBeforeForeignLoopClose(t *testing.T) {
	if !collabPlatformSupported() {
		t.Skip("collaboration broker requires Unix-domain sockets")
	}
	var (
		mu        sync.Mutex
		broker    *collabBroker
		originID  uuid.UUID
		revokedAt bool
		closedAt  bool
	)
	backend := newLifecycleBackend(func() {
		mu.Lock()
		defer mu.Unlock()
		closedAt = true
		if broker != nil {
			broker.mu.RLock()
			principal := broker.byLoop[originID]
			broker.mu.RUnlock()
			if principal != nil {
				principal.mu.Lock()
				revokedAt = principal.revoked
				principal.mu.Unlock()
			}
		}
	})
	build := foreign.ServicesBuilder(func(_ context.Context, _, loopID uuid.UUID, _ loop.Provenance,
		_ foreign.EventPublisher, _ loop.BoundDefinition, _ func() (uuid.UUID, error), _ *event.Factory,
		_ foreign.Services) (loop.Backend, string, error) {
		originID = loopID
		return backend, fixedForeignSID, nil
	})
	restored := foreign.ServicesRestoredBuilder(func(_ context.Context, _, _ uuid.UUID, _ loop.Provenance,
		_ foreign.EventPublisher, _ loop.BoundDefinition, _ func() (uuid.UUID, error), _ *event.Factory,
		_ foreign.RestoredForeign, _ foreign.Services) (loop.Backend, error) {
		return backend, nil
	})
	cfg := engineCfg(&stubLLM{chunks: []content.Chunk{textChunk("x")}}, loop.EngineForeignClaude, "x")
	s, err := newSession(context.Background(), cfg, uuid.New, time.Now,
		WithFingerprintProvider(testFingerprintProvider),
		WithForeignServicesBuilders(build, restored))
	skipCollabLifecycleUnavailable(t, err)
	if err != nil {
		t.Fatalf("newSession: %v", err)
	}
	mu.Lock()
	broker = s.collabBroker
	mu.Unlock()
	if err := s.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	mu.Lock()
	gotRevoked, gotClosed := revokedAt, closedAt
	mu.Unlock()
	if !gotClosed {
		t.Fatal("foreign loop did not close during shutdown")
	}
	if !gotRevoked {
		t.Fatal("foreign capability was not revoked before loop close")
	}
}

func TestCollabLifecycleReleasesPendingCallBeforeEndpointClose(t *testing.T) {
	if !collabPlatformSupported() {
		t.Skip("collaboration broker requires Unix-domain sockets")
	}
	s := &Session{sessionID: mustUUID(), sessionCtx: context.Background()}
	b, err := newCollabBroker(s)
	if err != nil {
		t.Skipf("Unix socket unavailable in this runner: %v", err)
	}
	s.collabBroker = b
	controller := &blockingDelegateController{}
	origin := mustUUID()
	descriptor, err := b.Mint(origin, controller)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	conn, err := net.Dial("unix", descriptor.Endpoint())
	if err != nil {
		t.Fatalf("dial broker: %v", err)
	}
	defer conn.Close()
	if err := writeCollabFrame(conn, descriptor.Capability(), collabCapabilityBytes); err != nil {
		t.Fatalf("write capability: %v", err)
	}
	request := []byte(`{"agent_id":"55555555-5555-4555-8555-555555555555","message":"wait","wait_for_response":false}`)
	if err := writeCollabFrame(conn, request, collabMaxArgumentBytes); err != nil {
		t.Fatalf("write request: %v", err)
	}
	deadline := time.Now().Add(time.Second)
	for {
		b.mu.RLock()
		principal := b.principals[b.digestForTest(descriptor.Capability())]
		b.mu.RUnlock()
		if principal != nil {
			principal.mu.Lock()
			pending := len(principal.callCancels)
			principal.mu.Unlock()
			if pending > 0 {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("broker did not admit pending call")
		}
		time.Sleep(time.Millisecond)
	}
	if err := b.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := os.Stat(descriptor.Endpoint()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("broker endpoint stat = %v, want removed after pending call release", err)
	}
	if _, err := os.Stat(filepath.Dir(descriptor.Endpoint())); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("broker directory stat = %v, want removed after pending call release", err)
	}
}

func TestCollabLifecycleRotatesCapabilityBeforeRestoredBuilder(t *testing.T) {
	if !collabPlatformSupported() {
		t.Skip("collaboration broker requires Unix-domain sockets")
	}
	folded := foldLoop([]event.Event{
		event.TurnStarted{Message: foldUserMsg("hello")},
		foldStepGroup(aiMessage("hi")),
		event.TurnDone{Message: aiMessage("hi")},
	})
	build := foreign.ServicesBuilder(func(_ context.Context, _, _ uuid.UUID, _ loop.Provenance,
		_ foreign.EventPublisher, _ loop.BoundDefinition, _ func() (uuid.UUID, error), _ *event.Factory,
		_ foreign.Services) (loop.Backend, string, error) {
		backend := newFakeBackend()
		backend.msgs = folded.Msgs
		backend.turnIndex = folded.TurnIndex
		return backend, fixedForeignSID, nil
	})
	var (
		mu           sync.Mutex
		capabilities [][]byte
	)
	restored := foreign.ServicesRestoredBuilder(func(_ context.Context, _, _ uuid.UUID, _ loop.Provenance,
		_ foreign.EventPublisher, _ loop.BoundDefinition, _ func() (uuid.UUID, error), _ *event.Factory,
		_ foreign.RestoredForeign, services foreign.Services) (loop.Backend, error) {
		backend := newFakeBackend()
		backend.msgs = folded.Msgs
		backend.turnIndex = folded.TurnIndex
		mu.Lock()
		capabilities = append(capabilities, services.Broker.Capability())
		mu.Unlock()
		return backend, nil
	})
	restoreOnce := func() []byte {
		sessionID, rootLoopID := mustUUID(), mustUUID()
		cfg := bindCfg(engineCfg(&stubLLM{chunks: []content.Chunk{textChunk("x")}}, loop.EngineForeignClaude, "x"), sessionID, rootLoopID)
		ctx, cancel := context.WithCancel(context.Background())
		s, err := buildRestoredSession(ctx, cancel, cfg, tool.Bindings{SessionID: sessionID, LoopID: rootLoopID, Delegate: &recordingDelegateController{}},
			sessionID, rootLoopID, fixedForeignSID, 0, folded, restoredInference{}, nil, nil,
			fakeSessionJournal{}, event.NewFactory(uuid.New, time.Now), uuid.New, time.Now,
			WithForeignServicesBuilders(build, restored))
		skipCollabLifecycleUnavailable(t, err)
		if err != nil {
			t.Fatalf("buildRestoredSession: %v", err)
		}
		if err := s.Shutdown(context.Background()); err != nil {
			t.Fatalf("restored Shutdown: %v", err)
		}
		cancel()
		mu.Lock()
		defer mu.Unlock()
		return append([]byte(nil), capabilities[len(capabilities)-1]...)
	}
	firstCapability := restoreOnce()
	secondCapability := restoreOnce()
	if len(firstCapability) != collabCapabilityBytes || len(secondCapability) != collabCapabilityBytes {
		t.Fatalf("restored capability lengths = %d, %d; want %d each", len(firstCapability), len(secondCapability), collabCapabilityBytes)
	}
	if bytes.Equal(firstCapability, secondCapability) {
		t.Fatal("restored builder received a replayed capability")
	}
}

type lifecycleBackend struct {
	commands chan command.Command
	done     chan struct{}
	onClose  func()
}

func newLifecycleBackend(onClose func()) *lifecycleBackend {
	b := &lifecycleBackend{commands: make(chan command.Command), done: make(chan struct{}), onClose: onClose}
	go func() {
		for raw := range b.commands {
			switch cmd := raw.(type) {
			case command.Shutdown:
				cmd.Ack <- nil
				if b.onClose != nil {
					b.onClose()
				}
				close(b.done)
				return
			}
		}
	}()
	return b
}

func (b *lifecycleBackend) CommandSink() chan<- command.Command { return b.commands }
func (b *lifecycleBackend) DoneChan() <-chan struct{}           { return b.done }
func (b *lifecycleBackend) Snapshot(context.Context) (content.AgenticMessages, event.TurnIndex, error) {
	return nil, 0, nil
}

func skipCollabLifecycleUnavailable(t *testing.T, err error) {
	t.Helper()
	if collabPlatformSupported() && errors.Is(err, errCollabBrokerProtocol) {
		t.Skipf("Unix socket unavailable in this runner: %v", err)
	}
}
