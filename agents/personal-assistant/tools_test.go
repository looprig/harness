package personalassistant

import (
	"context"
	"errors"
	"sort"
	"testing"

	"github.com/inventivepotter/urvi/internal/agent/session"
	"github.com/inventivepotter/urvi/internal/tool"
	"github.com/inventivepotter/urvi/internal/uuid"
)

// TestBuildToolSetRegistersExactlySafeSubset proves the manifest wires EXACTLY
// the seven safe tools and NOTHING else — no WriteFile/EditFile/Bash/Subagent.
// The subset is the security contract of the personal assistant: it can read,
// search, fetch, ask, and track todos, but never mutate the filesystem or run a
// shell. The set is asserted by name (each name is the tool's Info().Name).
func TestBuildToolSetRegistersExactlySafeSubset(t *testing.T) {
	t.Parallel()

	ts, err := buildToolSet("/tmp/workspace-root")
	if err != nil {
		t.Fatalf("buildToolSet() error = %v", err)
	}
	if ts.Permission == nil {
		t.Fatal("buildToolSet() ToolSet.Permission = nil, want non-nil PermissionChecker")
	}

	want := []string{"AskUser", "Fetch", "Glob", "Grep", "ReadFile", "Todo", "WebSearch"}
	got := toolNames(t, ts.Registry)
	if !equalStrings(got, want) {
		t.Errorf("registry tool names = %v, want %v", got, want)
	}
	// Exactly seven tools, no more — the count invariant folded in here.
	if l := len(ts.Registry); l != len(want) {
		t.Errorf("len(registry) = %d, want %d", l, len(want))
	}

	// Belt-and-suspenders: the forbidden tools must never appear.
	forbidden := map[string]bool{"WriteFile": true, "EditFile": true, "Bash": true, "Subagent": true}
	for _, name := range got {
		if forbidden[name] {
			t.Errorf("registry contains forbidden tool %q (no write/exec in personal-assistant)", name)
		}
	}
}

// toolNames collects the sorted Info().Name of every tool in the registry.
func toolNames(t *testing.T, reg []tool.InvokableTool) []string {
	t.Helper()
	names := make([]string, 0, len(reg))
	for _, tl := range reg {
		info, err := tl.Info(context.Background())
		if err != nil {
			t.Fatalf("Info() error = %v", err)
		}
		names = append(names, info.Name)
	}
	sort.Strings(names)
	return names
}

// TestGateTrioDelegatesToSession proves the three gate wrappers — Approve, Deny,
// and ProvideAnswer — each forward to the underlying session rather than
// short-circuiting locally.
//
// The proof is deterministic by construction. We Close the assistant FIRST: Close
// calls session.Shutdown, which blocks until the actor goroutine has exited and
// loop.Done is closed, so nothing reads loop.Commands any longer. We then invoke
// each wrapper with a LIVE (non-cancelled) ctx. Inside session.routeCommand the
// select has the unbuffered send to loop.Commands (blocks forever — no reader),
// <-loop.Done (closed — READY), and <-ctx.Done() (live — never ready). Only the
// Done case can fire, so every call returns *session.SessionError{SessionLoopExited}
// every time — no race between a winning send and a cancelled ctx.
//
// Observing that exact typed error proves the call reached session.routeCommand:
// a local short-circuit in the wrapper could not synthesize a *session.SessionError.
func TestGateTrioDelegatesToSession(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		invoke func(ctx context.Context, a *Assistant) error
	}{
		{
			name: "Approve",
			invoke: func(ctx context.Context, a *Assistant) error {
				return a.Approve(ctx, mustUUID(t), tool.ScopeSession)
			},
		},
		{
			name: "Deny",
			invoke: func(ctx context.Context, a *Assistant) error {
				return a.Deny(ctx, mustUUID(t))
			},
		},
		{
			name: "ProvideAnswer",
			invoke: func(ctx context.Context, a *Assistant) error {
				return a.ProvideAnswer(ctx, mustUUID(t), "the answer")
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			a := newClosedAssistantForGateTest(t)

			err := tt.invoke(context.Background(), a)

			var se *session.SessionError
			if !errors.As(err, &se) || se.Kind != session.SessionLoopExited {
				t.Fatalf("err = %v, want *session.SessionError{SessionLoopExited}", err)
			}
		})
	}
}

// newClosedAssistantForGateTest builds an Assistant over a fake client and Closes
// it before returning. Close blocks until the session's actor goroutine has exited
// and loop.Done is closed, so a subsequent gate call deterministically routes onto
// the loop-exited path inside the session. Close is idempotent, so the registered
// cleanup's second Close is a safe no-op.
func newClosedAssistantForGateTest(t *testing.T) *Assistant {
	t.Helper()
	a, err := newWithClient(context.Background(), &fakeLLM{}, testSpec())
	if err != nil {
		t.Fatalf("newWithClient: %v", err)
	}
	t.Cleanup(func() { _ = a.Close(context.Background()) })
	if err := a.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	return a
}

// mustUUID mints a UUID for a gate test, failing the test on the crypto/rand
// error path rather than passing a zero ID.
func mustUUID(t *testing.T) uuid.UUID {
	t.Helper()
	id, err := uuid.New()
	if err != nil {
		t.Fatalf("uuid.New: %v", err)
	}
	return id
}
