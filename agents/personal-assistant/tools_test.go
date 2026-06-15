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

	// Belt-and-suspenders: the forbidden tools must never appear.
	forbidden := map[string]bool{"WriteFile": true, "EditFile": true, "Bash": true, "Subagent": true}
	for _, name := range got {
		if forbidden[name] {
			t.Errorf("registry contains forbidden tool %q (no write/exec in personal-assistant)", name)
		}
	}
}

// TestBuildToolSetCount is the simplest invariant: exactly seven tools, no more.
func TestBuildToolSetCount(t *testing.T) {
	t.Parallel()

	ts, err := buildToolSet("/tmp/workspace-root")
	if err != nil {
		t.Fatalf("buildToolSet() error = %v", err)
	}
	if got, want := len(ts.Registry), 7; got != want {
		t.Errorf("len(registry) = %d, want %d", got, want)
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

// TestApproveDelegatesToSession proves Approve forwards to the underlying
// session: a cancelled ctx drives the session's fire-and-route send onto its
// context-done path, which returns a typed *session.SessionError{ContextDone}.
// Observing that exact error proves the call reached the session, not a local
// short-circuit in the wrapper.
func TestApproveDelegatesToSession(t *testing.T) {
	t.Parallel()
	a := newAssistantForGateTest(t)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := a.Approve(ctx, mustUUID(t), tool.ScopeSession)
	assertSessionContextDone(t, err)
}

// TestDenyDelegatesToSession is the deny sibling of the approve delegation test.
func TestDenyDelegatesToSession(t *testing.T) {
	t.Parallel()
	a := newAssistantForGateTest(t)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := a.Deny(ctx, mustUUID(t))
	assertSessionContextDone(t, err)
}

// TestProvideAnswerDelegatesToSession proves the TUI-named ProvideAnswer
// forwards to the session's ProvideUserInput.
func TestProvideAnswerDelegatesToSession(t *testing.T) {
	t.Parallel()
	a := newAssistantForGateTest(t)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := a.ProvideAnswer(ctx, mustUUID(t), "the answer")
	assertSessionContextDone(t, err)
}

// newAssistantForGateTest builds an Assistant over a fake client and registers a
// Close cleanup. The trio-delegation tests drive its session via cancelled ctx.
func newAssistantForGateTest(t *testing.T) *Assistant {
	t.Helper()
	a, err := newWithClient(context.Background(), &fakeLLM{}, testSpec())
	if err != nil {
		t.Fatalf("newWithClient: %v", err)
	}
	t.Cleanup(func() { _ = a.Close(context.Background()) })
	return a
}

// assertSessionContextDone fails unless err is a *session.SessionError whose
// Kind is SessionContextDone — the signature of a fire-and-route send aborted by
// a cancelled ctx inside the session.
func assertSessionContextDone(t *testing.T, err error) {
	t.Helper()
	var se *session.SessionError
	if !errors.As(err, &se) || se.Kind != session.SessionContextDone {
		t.Fatalf("err = %v, want *session.SessionError{SessionContextDone}", err)
	}
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
