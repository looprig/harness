package sessionruntime

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/looprig/core/content"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/tool"
)

type bindingCapture struct {
	mu         sync.Mutex
	calls      int
	ctx        context.Context
	bindings   tool.Bindings
	factoryErr error
}

func (c *bindingCapture) factory(ctx context.Context, bindings tool.Bindings) (loop.PermissionGate, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	c.ctx = ctx
	c.bindings = bindings
	if c.factoryErr != nil {
		return nil, c.factoryErr
	}
	return permissionGateStub{}, nil
}

func (c *bindingCapture) snapshot() (int, context.Context, tool.Bindings) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls, c.ctx, c.bindings
}

type permissionGateStub struct{}

func (permissionGateStub) Check(context.Context, tool.InvokableTool, string, string) loop.Effect {
	return loop.EffectAsk
}
func (permissionGateStub) Grant(context.Context, string, string, tool.ApprovalScope) error {
	return nil
}

func capturedDefinition(c *bindingCapture, system string, engine loop.Engine) loop.Definition {
	return mustDefine(
		loop.WithName("agent"), loop.WithInference(&stubLLM{}, validModel("model-x")),
		loop.WithSystem(system), loop.WithEngine(engine), loop.WithPolicyRevision("test"), loop.WithPermissionFactory(c.factory),
	)
}

func assertCapturedBinding(t *testing.T, capture *bindingCapture, sessionID, loopID uuid.UUID) context.Context {
	t.Helper()
	calls, ctx, bindings := capture.snapshot()
	if calls != 1 {
		t.Fatalf("Bind calls = %d, want 1", calls)
	}
	if bindings.SessionID != sessionID || bindings.LoopID != loopID {
		t.Fatalf("bindings IDs = %v/%v, want %v/%v", bindings.SessionID, bindings.LoopID, sessionID, loopID)
	}
	if ctx == nil {
		t.Fatal("Bind context is nil")
	}
	return ctx
}

func TestNewPrimaryBindsOnceWithOwnedSessionContext(t *testing.T) {
	capture := &bindingCapture{}
	s, err := newTestSession(context.Background(), capturedDefinition(capture, "system", loop.EngineNative))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	boundCtx := assertCapturedBinding(t, capture, s.SessionID(), s.PrimaryLoopID())
	select {
	case <-boundCtx.Done():
		t.Fatal("bind context cancelled before shutdown")
	default:
	}
	if err := s.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	select {
	case <-boundCtx.Done():
	default:
		t.Fatal("bind context not cancelled by shutdown")
	}
}

func TestNewFailureCancelsBoundSessionContext(t *testing.T) {
	capture := &bindingCapture{}
	_, err := newTestSession(context.Background(), capturedDefinition(capture, "system", loop.EngineForeignClaude))
	var sessionErr *SessionError
	if !errors.As(err, &sessionErr) || sessionErr.Kind != SessionForeignBuilderMissing {
		t.Fatalf("New error = %v", err)
	}
	_, boundCtx, _ := capture.snapshot()
	select {
	case <-boundCtx.Done():
	default:
		t.Fatal("bind context not cancelled after construction failure")
	}
}

func TestNewBindFailureCancelsOwnedSessionContext(t *testing.T) {
	capture := &bindingCapture{factoryErr: errors.New("bind failed")}
	_, err := newTestSession(context.Background(), capturedDefinition(capture, "system", loop.EngineNative))
	if err == nil {
		t.Fatal("New succeeded, want bind failure")
	}
	_, boundCtx, _ := capture.snapshot()
	select {
	case <-boundCtx.Done():
	default:
		t.Fatal("new-session context not cancelled after bind failure")
	}
}

func TestRestorePrimaryBindsOnceWithTransferredSessionContext(t *testing.T) {
	store := newRestoreStore(t)
	originalDef := restoreCfg(&stubLLM{chunks: []content.Chunk{textChunk("reply")}}, "model-x", "system")
	orig := buildOriginalRun(t, store, fingerprintFromDefinition(originalDef), originalDef, 1)
	handOver(t, orig.lease)

	capture := &bindingCapture{}
	s, err := restoreTestSession(context.Background(), capturedDefinition(capture, "system", loop.EngineNative), orig.sessionID, store)
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	boundCtx := assertCapturedBinding(t, capture, orig.sessionID, orig.primaryLoopID)
	select {
	case <-boundCtx.Done():
		t.Fatal("bind context cancelled before shutdown")
	default:
	}
	if err := s.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	select {
	case <-boundCtx.Done():
	default:
		t.Fatal("restore bind context not cancelled by shutdown")
	}
}

func TestRestoreFailureCancelsBoundSessionContext(t *testing.T) {
	store := newRestoreStore(t)
	originalDef := restoreCfg(&stubLLM{chunks: []content.Chunk{textChunk("reply")}}, "model-x", "system")
	orig := buildOriginalRun(t, store, fingerprintFromDefinition(originalDef), originalDef, 1)
	handOver(t, orig.lease)

	capture := &bindingCapture{}
	_, err := restoreTestSession(context.Background(), capturedDefinition(capture, "different system", loop.EngineNative), orig.sessionID, store)
	if err == nil {
		t.Fatal("Restore succeeded, want fingerprint mismatch")
	}
	_, boundCtx, _ := capture.snapshot()
	select {
	case <-boundCtx.Done():
	default:
		t.Fatal("restore bind context not cancelled after failure")
	}
}

func TestRestoreBindFailureCancelsOwnedSessionContext(t *testing.T) {
	store := newRestoreStore(t)
	originalDef := restoreCfg(&stubLLM{chunks: []content.Chunk{textChunk("reply")}}, "model-x", "system")
	orig := buildOriginalRun(t, store, fingerprintFromDefinition(originalDef), originalDef, 1)
	handOver(t, orig.lease)

	capture := &bindingCapture{factoryErr: errors.New("bind failed")}
	_, err := restoreTestSession(context.Background(), capturedDefinition(capture, "system", loop.EngineNative), orig.sessionID, store)
	if err == nil {
		t.Fatal("Restore succeeded, want bind failure")
	}
	_, boundCtx, _ := capture.snapshot()
	select {
	case <-boundCtx.Done():
	default:
		t.Fatal("restore-owned context not cancelled after bind failure")
	}
}
