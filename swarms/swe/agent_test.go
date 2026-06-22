package swe

import (
	"context"
	"errors"
	"testing"

	"github.com/ciram-co/looprig/pkg/llm"
	"github.com/ciram-co/looprig/pkg/loop"
	"github.com/ciram-co/looprig/pkg/session"
	"github.com/ciram-co/looprig/pkg/tool"
	"github.com/ciram-co/looprig/pkg/tui"
	"github.com/ciram-co/looprig/pkg/uuid"
)

// Compile-time proof that *sessionAgent satisfies the TUI's Agent surface (the
// streaming/lifecycle methods plus the Approve/Deny/ProvideAnswer trio). If a
// method's signature drifts this fails to build.
var _ tui.Agent = (*sessionAgent)(nil)

// testPrimaryCfg builds a minimal valid primary loop.Config over a fake client for
// wrapper construction tests.
func testPrimaryCfg(spec llm.ModelSpec) loop.Config {
	return loop.Config{Client: &fakeLLM{}, Model: spec, Tools: loop.ToolSet{}}
}

// TestNewSessionAgentHappy proves construction over a fake client succeeds and
// yields a non-nil wrapper whose session is releasable via Close.
func TestNewSessionAgentHappy(t *testing.T) {
	t.Parallel()

	a, err := newSessionAgent(context.Background(), testPrimaryCfg(testSpec()))
	if err != nil {
		t.Fatalf("newSessionAgent() error = %v", err)
	}
	if a == nil {
		t.Fatal("newSessionAgent() returned nil agent")
	}
	t.Cleanup(func() { _ = a.Close(context.Background()) })

	if a.PrimaryLoopID().IsZero() {
		t.Error("PrimaryLoopID() is zero, want a minted loop id")
	}
}

// TestNewSessionAgentPreCancelledCtx proves construction fails fast on an
// already-cancelled caller ctx with the typed session error — even though the
// session itself runs under an agent-owned (background-derived) root, so the
// fail-fast check must be on the CALLER ctx, not the session root.
func TestNewSessionAgentPreCancelledCtx(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	a, err := newSessionAgent(ctx, testPrimaryCfg(testSpec()))
	if a != nil {
		t.Errorf("expected nil agent, got %v", a)
		_ = a.Close(context.Background())
	}
	var se *session.SessionError
	if !errors.As(err, &se) || se.Kind != session.SessionContextDone {
		t.Fatalf("err = %v, want *session.SessionError{SessionContextDone}", err)
	}
}

// TestSessionAgentCloseIdempotent proves Close releases the session and is safe to
// call more than once (Shutdown blocks until the actor exits; the second call is a
// no-op).
func TestSessionAgentCloseIdempotent(t *testing.T) {
	t.Parallel()

	a, err := newSessionAgent(context.Background(), testPrimaryCfg(testSpec()))
	if err != nil {
		t.Fatalf("newSessionAgent() error = %v", err)
	}
	if err := a.Close(context.Background()); err != nil {
		t.Fatalf("first Close() error = %v", err)
	}
	if err := a.Close(context.Background()); err != nil {
		t.Fatalf("second Close() error = %v", err)
	}
}

// TestSessionAgentAcceptsImages proves AcceptsImages reflects the primary config's
// model modality flag exactly, with no inversion or defaulting.
func TestSessionAgentAcceptsImages(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		want bool
	}{
		{name: "text-only model", want: false},
		{name: "model accepts images", want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			spec := llm.ModelSpec{Provider: llm.ProviderLMStudio, Model: "fake-model", AcceptsImages: tt.want}
			a, err := newSessionAgent(context.Background(), testPrimaryCfg(spec))
			if err != nil {
				t.Fatalf("newSessionAgent() error = %v", err)
			}
			t.Cleanup(func() { _ = a.Close(context.Background()) })

			if got := a.AcceptsImages(); got != tt.want {
				t.Errorf("AcceptsImages() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestSessionAgentReplayBacklogNilForNewSession proves a fresh (non-restored)
// session has no backlog to repaint: ReplayBacklog returns nil/nil, so the TUI
// skips the cold-restore repaint.
func TestSessionAgentReplayBacklogNilForNewSession(t *testing.T) {
	t.Parallel()

	a, err := newSessionAgent(context.Background(), testPrimaryCfg(testSpec()))
	if err != nil {
		t.Fatalf("newSessionAgent() error = %v", err)
	}
	t.Cleanup(func() { _ = a.Close(context.Background()) })

	events, err := a.ReplayBacklog(context.Background())
	if err != nil {
		t.Fatalf("ReplayBacklog() error = %v", err)
	}
	if events != nil {
		t.Errorf("ReplayBacklog() = %v, want nil (new session has no backlog)", events)
	}
}

// TestSessionAgentGateTrioDelegatesToSession proves the three gate wrappers —
// Approve, Deny, ProvideAnswer — each forward to the underlying session rather
// than short-circuiting locally. The proof is deterministic: Close FIRST (blocks
// until the actor exits and loop.Done is closed), then each call deterministically
// routes onto the loop-exited path inside the session.
func TestSessionAgentGateTrioDelegatesToSession(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		invoke func(ctx context.Context, a *sessionAgent) error
	}{
		{
			name: "Approve",
			invoke: func(ctx context.Context, a *sessionAgent) error {
				return a.Approve(ctx, a.PrimaryLoopID(), mustUUID(t), tool.ScopeSession)
			},
		},
		{
			name: "Deny",
			invoke: func(ctx context.Context, a *sessionAgent) error {
				return a.Deny(ctx, a.PrimaryLoopID(), mustUUID(t))
			},
		},
		{
			name: "ProvideAnswer",
			invoke: func(ctx context.Context, a *sessionAgent) error {
				return a.ProvideAnswer(ctx, a.PrimaryLoopID(), mustUUID(t), "the answer")
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			a := newClosedSessionAgentForGateTest(t)

			err := tt.invoke(context.Background(), a)

			var se *session.SessionError
			if !errors.As(err, &se) || se.Kind != session.SessionLoopExited {
				t.Fatalf("err = %v, want *session.SessionError{SessionLoopExited}", err)
			}
		})
	}
}

// newClosedSessionAgentForGateTest builds a sessionAgent over a fake client and
// Closes it before returning, so a subsequent gate call deterministically routes
// onto the loop-exited path. Close is idempotent, so the cleanup's second Close is
// a safe no-op.
func newClosedSessionAgentForGateTest(t *testing.T) *sessionAgent {
	t.Helper()
	a, err := newSessionAgent(context.Background(), testPrimaryCfg(testSpec()))
	if err != nil {
		t.Fatalf("newSessionAgent: %v", err)
	}
	t.Cleanup(func() { _ = a.Close(context.Background()) })
	if err := a.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	return a
}

// mustUUID mints a UUID for a gate test, failing the test on the crypto/rand error
// path rather than passing a zero ID.
func mustUUID(t *testing.T) uuid.UUID {
	t.Helper()
	id, err := uuid.New()
	if err != nil {
		t.Fatalf("uuid.New: %v", err)
	}
	return id
}
