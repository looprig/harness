package coding

import (
	"context"
	"errors"
	"sort"
	"testing"

	"github.com/inventivepotter/urvi/agents/coding/prompts"
	"github.com/inventivepotter/urvi/internal/agent/loop"
	"github.com/inventivepotter/urvi/internal/agent/session"
	"github.com/inventivepotter/urvi/internal/llm"
	"github.com/inventivepotter/urvi/internal/tool"
	"github.com/inventivepotter/urvi/internal/uuid"
	"github.com/inventivepotter/urvi/tui"
)

// Compile-time proof that *Coding satisfies the TUI's Agent surface (the four
// streaming/lifecycle methods plus the Approve/Deny/ProvideAnswer trio). If a
// method's signature drifts this fails to build.
var _ tui.Agent = (*Coding)(nil)

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
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

// fakeSpawner is a no-op tools.Spawner for buildToolSet wiring tests that only
// inspect the tool set's shape. It never runs a sub-loop.
type fakeSpawner struct{}

func (fakeSpawner) Spawn(ctx context.Context, parent loop.Provenance, message string) (string, error) {
	return "", errors.New("fakeSpawner.Spawn not used")
}

// TestBuildToolSetRegistersAllEleven proves the coding manifest wires EXACTLY the
// eleven tools, no more and no fewer, asserted by Info().Name. This is the
// capability contract of the coding agent: full read/search, write/edit/exec,
// web, ask/todo, and Subagent.
func TestBuildToolSetRegistersAllEleven(t *testing.T) {
	t.Parallel()

	ts := buildToolSet("/tmp/workspace-root", newHTTPClient(), fakeSpawner{})
	if ts.Permission == nil {
		t.Fatal("buildToolSet() ToolSet.Permission = nil, want non-nil PermissionChecker")
	}

	want := []string{
		"AskUser", "Bash", "EditFile", "Fetch", "Glob", "Grep",
		"ReadFile", "Subagent", "Todo", "WebSearch", "WriteFile",
	}
	got := toolNames(t, ts.Registry)
	if !equalStrings(got, want) {
		t.Errorf("registry tool names = %v, want %v", got, want)
	}
	if l := len(ts.Registry); l != len(want) {
		t.Errorf("len(registry) = %d, want %d (all eleven)", l, len(want))
	}
}

// TestAutoApproveSetIsTheSixSafeTools proves the HardApprove set is exactly the
// six AutoApprove tools (design §4c) and that the five Ask tools
// (WriteFile/EditFile/Bash/Fetch/WebSearch) are deliberately absent — they
// mutate the filesystem, run a shell, or reach the network and must stay gated.
func TestAutoApproveSetIsTheSixSafeTools(t *testing.T) {
	t.Parallel()

	want := []string{"AskUser", "Glob", "Grep", "ReadFile", "Subagent", "Todo"}
	got := append([]string(nil), autoApprovedTools...)
	sort.Strings(got)
	if !equalStrings(got, want) {
		t.Errorf("autoApprovedTools = %v, want %v", got, want)
	}

	approved := make(map[string]bool, len(autoApprovedTools))
	for _, n := range autoApprovedTools {
		approved[n] = true
	}
	for _, ask := range []string{"WriteFile", "EditFile", "Bash", "Fetch", "WebSearch"} {
		if approved[ask] {
			t.Errorf("%q is auto-approved but must stay Ask", ask)
		}
	}
}

// TestNewWithClientHappy proves construction over a fake client succeeds and
// yields a non-nil agent whose session is releasable.
func TestNewWithClientHappy(t *testing.T) {
	t.Parallel()
	c, err := newWithClient(context.Background(), &fakeLLM{}, testSpec())
	if err != nil {
		t.Fatalf("newWithClient() error = %v", err)
	}
	if c == nil {
		t.Fatal("newWithClient() returned nil agent")
	}
	t.Cleanup(func() { _ = c.Close(context.Background()) })
}

// TestNewWithClientPreCancelledCtx proves construction fails fast on an
// already-cancelled ctx with the typed session error.
func TestNewWithClientPreCancelledCtx(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	c, err := newWithClient(ctx, &fakeLLM{}, testSpec())
	if c != nil {
		t.Errorf("expected nil agent, got %v", c)
	}
	var se *session.SessionError
	if !errors.As(err, &se) || se.Kind != session.SessionContextDone {
		t.Fatalf("err = %v, want *session.SessionError{SessionContextDone}", err)
	}
}

// TestAcceptsImages proves AcceptsImages reflects the constructed spec's modality
// flag exactly, with no inversion or defaulting. v1's Kimi K2 is text-only so the
// production agent reports false; the seam lets us prove both directions.
func TestAcceptsImages(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		want bool
	}{
		{name: "model is text-only (coding v1)", want: false},
		{name: "model accepts images", want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			spec := llm.ModelSpec{
				Provider:      llm.ProviderLMStudio,
				Model:         "fake-model",
				AcceptsImages: tt.want,
			}
			c, err := newWithClient(context.Background(), &fakeLLM{}, spec)
			if err != nil {
				t.Fatalf("newWithClient: %v", err)
			}
			t.Cleanup(func() { _ = c.Close(context.Background()) })

			if got := c.AcceptsImages(); got != tt.want {
				t.Errorf("AcceptsImages() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestProductionAcceptsImagesFalse proves the production model (Kimi K2) is
// text-only: a Coding agent built from the real spec reports AcceptsImages false.
func TestProductionAcceptsImagesFalse(t *testing.T) {
	t.Parallel()
	spec := model.Spec("unused-key", prompts.SystemPrompt)
	c, err := newWithClient(context.Background(), &fakeLLM{}, spec)
	if err != nil {
		t.Fatalf("newWithClient: %v", err)
	}
	t.Cleanup(func() { _ = c.Close(context.Background()) })
	if c.AcceptsImages() {
		t.Error("AcceptsImages() = true, want false (Kimi K2 is text-only)")
	}
}

// TestGateTrioDelegatesToSession proves the three gate wrappers — Approve, Deny,
// and ProvideAnswer — each forward to the underlying session rather than
// short-circuiting locally.
//
// The proof is deterministic by construction. We Close the agent FIRST: Close
// calls session.Shutdown, which blocks until the actor goroutine has exited and
// loop.Done is closed, so nothing reads loop.Commands any longer. We then invoke
// each wrapper with a LIVE (non-cancelled) ctx. Inside session.routeGate the
// select has the unbuffered send to loop.Commands (blocks forever — no reader),
// <-loop.Done (closed — READY), and <-ctx.Done() (live — never ready). Only the
// Done case can fire, so every call returns *session.SessionError{SessionLoopExited}
// every time — no race between a winning send and a cancelled ctx.
func TestGateTrioDelegatesToSession(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		invoke func(ctx context.Context, c *Coding) error
	}{
		{
			name: "Approve",
			invoke: func(ctx context.Context, c *Coding) error {
				return c.Approve(ctx, c.PrimaryLoopID(), mustUUID(t), tool.ScopeSession)
			},
		},
		{
			name: "Deny",
			invoke: func(ctx context.Context, c *Coding) error {
				return c.Deny(ctx, c.PrimaryLoopID(), mustUUID(t))
			},
		},
		{
			name: "ProvideAnswer",
			invoke: func(ctx context.Context, c *Coding) error {
				return c.ProvideAnswer(ctx, c.PrimaryLoopID(), mustUUID(t), "the answer")
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			c := newClosedCodingForGateTest(t)

			err := tt.invoke(context.Background(), c)

			var se *session.SessionError
			if !errors.As(err, &se) || se.Kind != session.SessionLoopExited {
				t.Fatalf("err = %v, want *session.SessionError{SessionLoopExited}", err)
			}
		})
	}
}

// newClosedCodingForGateTest builds a Coding agent over a fake client and Closes
// it before returning. Close blocks until the session's actor goroutine has
// exited and loop.Done is closed, so a subsequent gate call deterministically
// routes onto the loop-exited path inside the session. Close is idempotent, so
// the registered cleanup's second Close is a safe no-op.
func newClosedCodingForGateTest(t *testing.T) *Coding {
	t.Helper()
	c, err := newWithClient(context.Background(), &fakeLLM{}, testSpec())
	if err != nil {
		t.Fatalf("newWithClient: %v", err)
	}
	t.Cleanup(func() { _ = c.Close(context.Background()) })
	if err := c.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	return c
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
