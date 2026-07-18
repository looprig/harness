package sessionruntime

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/looprig/core/content"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/command"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/hustle"
	"github.com/looprig/harness/pkg/identity"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/inference"
	contextcount "github.com/looprig/inference/contextcount"
	stream "github.com/looprig/inference/stream"
)

type liveCompactionCounter struct {
	mu         sync.Mutex
	capability contextcount.CounterCapability
	counts     []content.TokenCount
	calls      int
}

type preStartCancellationCounter struct {
	mu         sync.Mutex
	capability contextcount.CounterCapability
	gate       *preStartCountGate
}

type preStartCountGate struct {
	started chan struct{}
	exited  chan struct{}
}

func (c *preStartCancellationCounter) arm() *preStartCountGate {
	c.mu.Lock()
	defer c.mu.Unlock()
	gate := &preStartCountGate{started: make(chan struct{}), exited: make(chan struct{})}
	c.gate = gate
	return gate
}

func (c *preStartCancellationCounter) CountContext(ctx context.Context, request inference.Request) (contextcount.ContextCount, error) {
	c.mu.Lock()
	gate := c.gate
	c.gate = nil
	c.mu.Unlock()
	if gate != nil {
		close(gate.started)
		<-ctx.Done()
		close(gate.exited)
		return contextcount.ContextCount{}, ctx.Err()
	}
	return contextcount.ContextCount{Model: request.Model.Key(), InputTokens: 40, Quality: c.capability.Quality}, nil
}

func (c *preStartCancellationCounter) CounterCapability() contextcount.CounterCapability {
	return c.capability
}

func (c *liveCompactionCounter) CountContext(_ context.Context, request inference.Request) (contextcount.ContextCount, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	index := c.calls
	c.calls++
	if len(c.counts) == 0 {
		return contextcount.ContextCount{}, errors.New("test counter has no values")
	}
	if index >= len(c.counts) {
		index = len(c.counts) - 1
	}
	return contextcount.ContextCount{Model: request.Model.Key(), InputTokens: c.counts[index], Quality: c.capability.Quality}, nil
}

func (c *liveCompactionCounter) CounterCapability() contextcount.CounterCapability {
	return c.capability
}

type liveCompactionClient struct {
	mu            sync.Mutex
	invokes       int
	invoked       chan struct{}
	streamStarted chan struct{}
	streamRelease chan struct{}
}

func (c *liveCompactionClient) Invoke(_ context.Context, request inference.Request) (*inference.Response, error) {
	c.mu.Lock()
	c.invokes++
	c.mu.Unlock()
	select {
	case c.invoked <- struct{}{}:
	default:
	}
	if len(request.Messages) != 1 {
		return nil, errors.New("unexpected compaction request shape")
	}
	message, ok := request.Messages[0].(*content.UserMessage)
	if !ok || message == nil || len(message.Blocks) != 1 {
		return nil, errors.New("unexpected compaction request message")
	}
	block, ok := message.Blocks[0].(*content.TextBlock)
	if !ok || block == nil {
		return nil, errors.New("unexpected compaction input block")
	}
	input, err := unmarshalCompactionInput([]byte(block.Text))
	if err != nil {
		return nil, err
	}
	output, err := json.Marshal(struct {
		Version            loop.CompactionWireVersion `json:"version"`
		Basis              event.ContextBasis         `json:"basis"`
		Model              compactionModelWire        `json:"model"`
		RequestFingerprint string                     `json:"request_fingerprint"`
		Summary            string                     `json:"summary"`
	}{
		Version: loop.CompactionWireV1, Basis: input.Basis,
		Model:              compactionModelWire{Provider: input.Model.Provider, Model: input.Model.Model},
		RequestFingerprint: hex.EncodeToString(input.RequestFingerprint[:]), Summary: validCompactionXML,
	})
	if err != nil {
		return nil, err
	}
	return &inference.Response{
		Message: &content.AIMessage{Message: content.Message{
			Role: content.RoleAssistant, Blocks: []content.Block{&content.TextBlock{Text: string(output)}},
		}},
		Usage: &content.Usage{OutputTokens: 1},
	}, nil
}

func (c *liveCompactionClient) Stream(_ context.Context, _ inference.Request) (*stream.StreamReader[content.Chunk], error) {
	if c.streamStarted != nil {
		close(c.streamStarted)
	}
	if c.streamRelease != nil {
		<-c.streamRelease
	}
	emitted := false
	return stream.NewStreamReader(func() (content.Chunk, error) {
		if emitted {
			return nil, io.EOF
		}
		emitted = true
		return textChunk("original response"), nil
	}, nil), nil
}

func (c *liveCompactionClient) invokeCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.invokes
}

func TestNativeSessionCompactionReachesRegisteredFocusedHustle(t *testing.T) {
	tests := []struct {
		name      string
		automatic bool
		restore   bool
		idle      bool
		counts    []content.TokenCount
	}{
		{name: "manual command during active turn", counts: []content.TokenCount{40, 40, 20}},
		{name: "manual command while idle", idle: true, counts: []content.TokenCount{40, 40, 20}},
		{name: "automatic threshold", automatic: true, counts: []content.TokenCount{65, 20, 20}},
		{name: "restored automatic threshold", automatic: true, restore: true, counts: []content.TokenCount{65, 20, 20}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := &liveCompactionClient{invoked: make(chan struct{}, 1)}
			if !tt.automatic {
				client.streamStarted = make(chan struct{})
				client.streamRelease = make(chan struct{})
			}
			capability := contextcount.CounterCapability{
				Transport: contextcount.CounterTransportLocal, Retention: contextcount.RetentionNone,
				TokenizerRev: "live-exact-v1", Quality: contextcount.CountQualityExactLocal,
			}
			counter := &liveCompactionCounter{capability: capability, counts: tt.counts}
			model := validModel("live-compaction")
			model.Limits = testContextLimits{WindowTokens: 100, MaxInputTokens: 80, MaxOutputTokens: 20}
			policy := loop.CompactionPolicy{
				Automatic: tt.automatic, CounterPolicy: loop.CounterPolicyRequireExact,
				CompactAt: 8_000, RearmBelow: 6_000, ReservedOutput: 20,
				MaxSummaryTokens: 10, CountTimeout: time.Second, Hustle: "context.compact",
			}
			definition := mustDefine(
				loop.WithName("agent"), loop.WithInference(client, model), loop.WithDrainTimeout(200*time.Millisecond),
				loop.WithContextCounter(counter),
				loop.WithInferenceCapability(contextcount.InferenceCapability{
					Transport: contextcount.InferenceTransportLocal, Retention: contextcount.RetentionNone,
				}),
				loop.WithCompaction(policy),
			)
			store := newRestoreStore(t)
			lifecycle, err := newTestLifecycle(
				definition, store,
				WithLifecycleHustles([]hustle.Definition{testHustleDefinition(t, "context.compact")}, testHustleLimits()),
			)
			if err != nil {
				t.Fatalf("NewTopologyLifecycle() error = %v", err)
			}
			session, err := lifecycle.NewSession(context.Background(), "")
			if err != nil {
				t.Fatalf("NewSession() error = %v", err)
			}
			if tt.restore {
				sessionID := session.SessionID()
				if err := session.Shutdown(context.Background()); err != nil {
					t.Fatalf("original Shutdown() error = %v", err)
				}
				session, err = lifecycle.RestoreSession(context.Background(), sessionID)
				if err != nil {
					t.Fatalf("RestoreSession() error = %v", err)
				}
			}
			t.Cleanup(func() { _ = session.Shutdown(context.Background()) })
			if _, err := session.Submit(context.Background(), []content.Block{&content.TextBlock{Text: "compact me"}}); err != nil {
				t.Fatalf("Submit() error = %v", err)
			}
			if !tt.automatic {
				if tt.idle {
					select {
					case <-client.streamStarted:
					case <-time.After(2 * time.Second):
						t.Fatal("primary inference did not start")
					}
					close(client.streamRelease)
					idleCtx, idleCancel := context.WithTimeout(context.Background(), 5*time.Second)
					defer idleCancel()
					if err := session.WaitIdle(idleCtx); err != nil {
						t.Fatalf("initial WaitIdle() error = %v", err)
					}
					// Restore the committed seed so the command is guaranteed to reach an
					// actor constructed idle, rather than racing the live turn's terminal.
					sessionID := session.SessionID()
					if err := session.Shutdown(context.Background()); err != nil {
						t.Fatalf("seed Shutdown() error = %v", err)
					}
					session, err = lifecycle.RestoreSession(context.Background(), sessionID)
					if err != nil {
						t.Fatalf("seed RestoreSession() error = %v", err)
					}
					if err := session.WaitIdle(idleCtx); err != nil {
						t.Fatalf("restored WaitIdle() error = %v", err)
					}
					handle := session.loops[session.ActiveLoopID()]
					if _, _, err := handle.backend.Snapshot(idleCtx); err != nil {
						t.Fatalf("idle Snapshot() barrier error = %v", err)
					}
				} else {
					select {
					case <-client.streamStarted:
					case <-time.After(2 * time.Second):
						t.Fatal("primary inference did not start")
					}
				}
				commandID, err := uuid.New()
				if err != nil {
					t.Fatalf("uuid.New() error = %v", err)
				}
				compact := command.Compact{
					Header:      command.Header{CommandID: commandID, Agency: identity.AgencyUser, CreatedAt: time.Now()},
					Coordinates: identity.Coordinates{SessionID: session.SessionID(), LoopID: session.ActiveLoopID()},
				}
				handle := session.loops[session.ActiveLoopID()]
				select {
				case handle.backend.CommandSink() <- compact:
				case <-time.After(2 * time.Second):
					t.Fatal("manual Compact was not admitted")
				}
				if !tt.idle {
					close(client.streamRelease)
				}
			}
			select {
			case <-client.invoked:
			case <-time.After(2 * time.Second):
				t.Fatal("registered focused hustle was not invoked")
			}
			waitCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := session.WaitIdle(waitCtx); err != nil {
				t.Fatalf("WaitIdle() error = %v", err)
			}
			if got := client.invokeCount(); got != 1 {
				t.Fatalf("registered focused hustle invocations = %d, want 1", got)
			}
		})
	}
}

func TestNativeSessionIdlePreStartCancellationPublishesWaiterOnly(t *testing.T) {
	tests := []struct {
		name     string
		restore  bool
		shutdown bool
		want     event.CompactRejectReason
	}{
		{name: "new interrupt", want: event.CompactRejectInterrupted},
		{name: "restored interrupt", restore: true, want: event.CompactRejectInterrupted},
		{name: "new shutdown", shutdown: true, want: event.CompactRejectShuttingDown},
		{name: "restored shutdown", restore: true, shutdown: true, want: event.CompactRejectShuttingDown},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := &liveCompactionClient{invoked: make(chan struct{}, 1)}
			capability := contextcount.CounterCapability{Transport: contextcount.CounterTransportLocal, Retention: contextcount.RetentionNone, TokenizerRev: "cancel-v1", Quality: contextcount.CountQualityExactLocal}
			counter := &preStartCancellationCounter{capability: capability}
			model := validModel("pre-start-cancel")
			model.Limits = testContextLimits{WindowTokens: 100, MaxInputTokens: 80, MaxOutputTokens: 20}
			definition := mustDefine(
				loop.WithName("agent"), loop.WithInference(client, model), loop.WithDrainTimeout(200*time.Millisecond),
				loop.WithContextCounter(counter),
				loop.WithInferenceCapability(contextcount.InferenceCapability{Transport: contextcount.InferenceTransportLocal, Retention: contextcount.RetentionNone}),
				loop.WithCompaction(loop.CompactionPolicy{CounterPolicy: loop.CounterPolicyRequireExact, ReservedOutput: 20, MaxSummaryTokens: 10, CountTimeout: 2 * time.Second, Hustle: "context.compact"}),
			)
			lifecycle, err := newTestLifecycle(definition, newRestoreStore(t), WithLifecycleHustles([]hustle.Definition{testHustleDefinition(t, "context.compact")}, testHustleLimits()))
			if err != nil {
				t.Fatalf("newTestLifecycle() error = %v", err)
			}
			session, err := lifecycle.NewSession(context.Background(), "")
			if err != nil {
				t.Fatalf("NewSession() error = %v", err)
			}
			shutdown := false
			t.Cleanup(func() {
				if !shutdown {
					_ = session.Shutdown(context.Background())
				}
			})
			seedSub, err := session.SubscribeEvents(allFilter())
			if err != nil {
				t.Fatalf("seed SubscribeEvents() error = %v", err)
			}
			if _, err := session.Submit(context.Background(), []content.Block{&content.TextBlock{Text: "seed"}}); err != nil {
				t.Fatalf("Submit() error = %v", err)
			}
			if _, ok := firstMatching[event.TurnDone](t, seedSub); !ok {
				t.Fatal("seed turn did not finish")
			}
			if _, ok := firstMatching[event.LoopIdle](t, seedSub); !ok {
				t.Fatal("seed loop did not become idle")
			}
			_ = seedSub.Close()
			idleCtx, idleCancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer idleCancel()
			if err := session.WaitIdle(idleCtx); err != nil {
				t.Fatalf("WaitIdle() error = %v", err)
			}
			if tt.restore {
				sessionID := session.SessionID()
				if err := session.Shutdown(context.Background()); err != nil {
					t.Fatalf("seed Shutdown() error = %v", err)
				}
				session, err = lifecycle.RestoreSession(context.Background(), sessionID)
				if err != nil {
					t.Fatalf("RestoreSession() error = %v", err)
				}
			}
			sub, err := session.SubscribeEvents(allFilter())
			if err != nil {
				t.Fatalf("SubscribeEvents() error = %v", err)
			}
			defer sub.Close()
			gate := counter.arm()
			commandID := uuid.UUID{0xd1}
			compact := command.Compact{Header: command.Header{CommandID: commandID, Agency: identity.AgencyUser, CreatedAt: time.Now()}, Coordinates: identity.Coordinates{SessionID: session.SessionID(), LoopID: session.ActiveLoopID()}}
			handle := session.loops[session.ActiveLoopID()]
			handle.backend.CommandSink() <- compact
			select {
			case <-gate.started:
			case <-time.After(2 * time.Second):
				t.Fatal("idle pre-count did not start")
			}
			if tt.shutdown {
				if err := session.Shutdown(context.Background()); err != nil {
					t.Fatalf("Shutdown() error = %v", err)
				}
				shutdown = true
			} else {
				if interrupted, err := session.Interrupt(context.Background()); err != nil || interrupted {
					t.Fatalf("Interrupt() = %v, %v, want idle false/nil", interrupted, err)
				}
			}
			select {
			case <-gate.exited:
			case <-time.After(2 * time.Second):
				t.Fatal("pre-count did not exit")
			}
			var waiter *event.CompactWaiterRejected
			deadline := time.After(500 * time.Millisecond)
		collect:
			for waiter == nil {
				select {
				case delivery, ok := <-sub.Events():
					if !ok {
						break collect
					}
					switch value := delivery.Event.(type) {
					case event.CompactWaiterRejected:
						copyOfValue := value
						waiter = &copyOfValue
					case event.CompactionStarted, event.CompactionRejected:
						t.Fatalf("pre-start cancellation published %T", value)
					}
				case <-deadline:
					break collect
				}
			}
			if waiter == nil || waiter.Cause.CommandID != commandID || waiter.Reason != tt.want || waiter.AttemptID == (event.CompactAttemptID{}) {
				t.Fatalf("waiter = %+v, want command/reason/attempt", waiter)
			}
			if got := client.invokeCount(); got != 0 {
				t.Fatalf("hustle invokes = %d, want 0", got)
			}
			if !tt.shutdown {
				nextID := uuid.UUID{0xd2}
				next := command.Compact{Header: command.Header{CommandID: nextID, Agency: identity.AgencyUser, CreatedAt: time.Now()}, Coordinates: compact.Coordinates}
				handle.backend.CommandSink() <- next
				select {
				case <-client.invoked:
				case <-time.After(2 * time.Second):
					t.Fatal("fresh compaction did not reuse cleared lane")
				}
				if got := client.invokeCount(); got != 1 {
					t.Fatalf("fresh hustle invokes = %d, want 1", got)
				}
			}
		})
	}
}
