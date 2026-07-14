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
)

type liveCompactionCounter struct {
	mu         sync.Mutex
	capability inference.CounterCapability
	counts     []content.TokenCount
	calls      int
}

func (c *liveCompactionCounter) CountContext(_ context.Context, request inference.Request) (inference.ContextCount, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	index := c.calls
	c.calls++
	if len(c.counts) == 0 {
		return inference.ContextCount{}, errors.New("test counter has no values")
	}
	if index >= len(c.counts) {
		index = len(c.counts) - 1
	}
	return inference.ContextCount{Model: request.Model.Key(), InputTokens: c.counts[index], Quality: c.capability.Quality}, nil
}

func (c *liveCompactionCounter) CounterCapability() inference.CounterCapability { return c.capability }

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

func (c *liveCompactionClient) Stream(_ context.Context, _ inference.Request) (*inference.StreamReader[content.Chunk], error) {
	if c.streamStarted != nil {
		close(c.streamStarted)
		<-c.streamRelease
	}
	emitted := false
	return inference.NewStreamReader(func() (content.Chunk, error) {
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
			if !tt.automatic && !tt.idle {
				client.streamStarted = make(chan struct{})
				client.streamRelease = make(chan struct{})
			}
			capability := inference.CounterCapability{
				Transport: inference.CounterTransportLocal, Retention: inference.RetentionNone,
				TokenizerRev: "live-exact-v1", Quality: inference.CountQualityExactLocal,
			}
			counter := &liveCompactionCounter{capability: capability, counts: tt.counts}
			model := validModel("live-compaction")
			model.Limits = inference.ContextLimits{WindowTokens: 100, MaxInputTokens: 80, MaxOutputTokens: 20}
			policy := loop.CompactionPolicy{
				Automatic: tt.automatic, CounterPolicy: loop.CounterPolicyRequireExact,
				CompactAt: 8_000, RearmBelow: 6_000, ReservedOutput: 20,
				MaxSummaryTokens: 10, CountTimeout: time.Second, Hustle: "context.compact",
			}
			definition := mustDefine(
				loop.WithName("agent"), loop.WithInference(client, model), loop.WithDrainTimeout(200*time.Millisecond),
				loop.WithContextCounter(counter),
				loop.WithInferenceCapability(inference.InferenceCapability{
					Transport: inference.InferenceTransportLocal, Retention: inference.RetentionNone,
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
