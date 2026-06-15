package tools

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/inventivepotter/urvi/internal/tool"
)

// subagent_test.go exercises the Subagent tool against FAKE SubagentFactory /
// Subsession implementations (DIP: the tool never touches the real
// session.AgentSession). The fakes record what they were called with so the
// tests can assert the security-critical invariants:
//
//   - the depth cap is enforced BEFORE a child is created (fail-secure), and
//   - depth+1 is propagated into the child's ctx, so a Subagent invoked BY a
//     child sees the incremented depth and the cap actually bounds recursion.
//
// (textOf, the shared *tool.ToolResult → string helper, lives in fetch_test.go.)

// fakeSubsession is a fake child session. It records the ctx it was Invoked with
// (so a test can read the depth carried in that ctx via subagentDepth) plus the
// captured ctx itself (so a multi-level test can thread the incremented ctx into
// the next spawn). invokeErr, when set, is returned instead of a reply; otherwise
// it echoes the message when echo is true, else returns reply.
type fakeSubsession struct {
	mu         sync.Mutex
	reply      string
	echo       bool
	invokeErr  error
	invoked    bool
	gotMessage string
	gotDepth   int
	gotCtx     context.Context
}

func (f *fakeSubsession) Invoke(ctx context.Context, message string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.invoked = true
	f.gotMessage = message
	f.gotDepth = subagentDepth(ctx)
	f.gotCtx = ctx
	if f.invokeErr != nil {
		return "", f.invokeErr
	}
	if f.echo {
		return "echo: " + message, nil
	}
	return f.reply, nil
}

// fakeFactory is a fake SubagentFactory. It records the skill it was asked to
// build and the depth carried in the ctx New received, and returns either child
// or newErr. If failIfCalled is true, New fails the test when invoked at all —
// the way the cap tests prove the factory is never reached once the depth limit
// is hit.
type fakeFactory struct {
	t            *testing.T
	failIfCalled bool

	mu       sync.Mutex
	child    *fakeSubsession
	newErr   error
	called   bool
	gotSkill string
	gotDepth int
}

func (f *fakeFactory) New(ctx context.Context, skill string) (Subsession, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failIfCalled {
		f.t.Fatalf("factory.New must NOT be called (depth cap should have blocked it); got skill %q", skill)
	}
	f.called = true
	f.gotSkill = skill
	f.gotDepth = subagentDepth(ctx)
	if f.newErr != nil {
		return nil, f.newErr
	}
	return f.child, nil
}

// stubFactoryError is a typed error a fake factory returns to exercise the
// unknown-skill / construction-failure path.
type stubFactoryError struct{ msg string }

func (e *stubFactoryError) Error() string { return e.msg }

// stubInvokeError is a typed error a fake child returns to exercise the
// child-Invoke-failed path.
type stubInvokeError struct{ msg string }

func (e *stubInvokeError) Error() string { return e.msg }

// TestSubagentInfo asserts the self-description: the name MUST be exactly
// "Subagent" (the classifyTool/manifest contract) and the schema/desc are present.
func TestSubagentInfo(t *testing.T) {
	t.Parallel()
	s := NewSubagent(&fakeFactory{t: t}, context.Background())
	info, err := s.Info(context.Background())
	if err != nil {
		t.Fatalf("Info() error = %v", err)
	}
	if info.Name != "Subagent" {
		t.Errorf("Info().Name = %q, want %q", info.Name, "Subagent")
	}
	if info.Name != subagentToolName {
		t.Errorf("subagentToolName const = %q, want Info().Name %q", subagentToolName, info.Name)
	}
	if strings.TrimSpace(info.Desc) == "" {
		t.Error("Info().Desc is empty")
	}
	if len(info.Schema) == 0 {
		t.Error("Info().Schema is empty")
	}
}

// TestSubagentAuditSummary asserts the audit summary surfaces ONLY the skill
// name — the message may be sensitive and must NOT leak into the audit event.
func TestSubagentAuditSummary(t *testing.T) {
	t.Parallel()
	s := NewSubagent(&fakeFactory{t: t}, context.Background())

	tests := []struct {
		name    string
		args    string
		want    string
		notWant string
	}{
		{
			name:    "skill surfaced, message redacted",
			args:    `{"skill":"researcher","message":"my secret password is hunter2"}`,
			want:    "Subagent: researcher",
			notWant: "hunter2",
		},
		{name: "unparsable args", args: `not json`, want: "Subagent (unparsable args)"},
		{name: "missing skill", args: `{"message":"hi"}`, want: "Subagent (unparsable args)"},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := s.AuditSummary(tt.args)
			if got != tt.want {
				t.Errorf("AuditSummary() = %q, want %q", got, tt.want)
			}
			if tt.notWant != "" && strings.Contains(got, tt.notWant) {
				t.Errorf("AuditSummary() = %q leaks message substring %q", got, tt.notWant)
			}
		})
	}
}

// TestSubagentChildRoundTrip asserts the happy path: a fake factory builds an
// echoing child; the tool returns the child's final text and the factory saw the
// requested skill and the child the requested message.
func TestSubagentChildRoundTrip(t *testing.T) {
	t.Parallel()
	child := &fakeSubsession{echo: true}
	f := &fakeFactory{t: t, child: child}
	s := NewSubagent(f, context.Background())

	res, err := s.InvokableRun(context.Background(), `{"skill":"researcher","message":"hello there"}`)
	if err != nil {
		t.Fatalf("InvokableRun() Go error = %v (must be nil; failures are tool-result strings)", err)
	}
	if got := textOf(t, res); got != "echo: hello there" {
		t.Errorf("result = %q, want %q", got, "echo: hello there")
	}
	if f.gotSkill != "researcher" {
		t.Errorf("factory got skill %q, want %q", f.gotSkill, "researcher")
	}
	if !child.invoked {
		t.Error("child was never Invoked")
	}
	if child.gotMessage != "hello there" {
		t.Errorf("child got message %q, want %q", child.gotMessage, "hello there")
	}
}

// TestSubagentDepthIncrement asserts that a top-level call (absent depth = 0)
// creates a child that sees depth 1 — in both the ctx the factory received and
// the ctx the child's Invoke received — so the increment propagates to a nested
// spawn.
func TestSubagentDepthIncrement(t *testing.T) {
	t.Parallel()
	child := &fakeSubsession{echo: true}
	f := &fakeFactory{t: t, child: child}
	s := NewSubagent(f, context.Background())

	if _, err := s.InvokableRun(context.Background(), `{"skill":"x","message":"m"}`); err != nil {
		t.Fatalf("InvokableRun() Go error = %v", err)
	}
	if f.gotDepth != 1 {
		t.Errorf("factory.New saw depth %d, want 1 (depth+1 from top level)", f.gotDepth)
	}
	if child.gotDepth != 1 {
		t.Errorf("child.Invoke saw depth %d in its ctx, want 1", child.gotDepth)
	}
}

// TestSubagentDepthCap asserts the security-critical cap: a ctx already AT (or
// above) the max depth blocks the spawn entirely — the factory is NEVER called,
// and the result is a tool-result error mentioning the limit. Below the cap the
// child runs and sees depth+1.
func TestSubagentDepthCap(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		depth   int
		blocked bool
	}{
		{name: "below cap proceeds", depth: maxSubagentDepth - 1, blocked: false},
		{name: "at cap blocked", depth: maxSubagentDepth, blocked: true},
		{name: "above cap blocked", depth: maxSubagentDepth + 1, blocked: true},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			child := &fakeSubsession{echo: true}
			f := &fakeFactory{t: t, child: child, failIfCalled: tt.blocked}
			s := NewSubagent(f, context.Background())

			ctx := withSubagentDepth(context.Background(), tt.depth)
			res, err := s.InvokableRun(ctx, `{"skill":"x","message":"m"}`)
			if err != nil {
				t.Fatalf("InvokableRun() Go error = %v", err)
			}
			got := textOf(t, res)
			if tt.blocked {
				if !strings.Contains(got, "depth limit") {
					t.Errorf("result = %q, want a depth-limit error", got)
				}
				if f.called {
					t.Error("factory.New was called despite depth cap")
				}
				if child.invoked {
					t.Error("child was Invoked despite depth cap")
				}
				return
			}
			if !child.invoked {
				t.Error("child was not Invoked below the cap")
			}
			if child.gotDepth != tt.depth+1 {
				t.Errorf("child saw depth %d, want %d (depth+1)", child.gotDepth, tt.depth+1)
			}
		})
	}
}

// TestSubagentMultiLevelCap simulates the spawn chain to prove the cap bounds
// recursion: top (depth 0) → child sees 1 → grandchild sees 2 → great-grandchild
// is BLOCKED. Each level threads the ctx the previous level's child observed into
// the next level's InvokableRun — exactly what a real nested spawn does when the
// child, mid-Invoke, itself calls the Subagent tool with the ctx it was given.
func TestSubagentMultiLevelCap(t *testing.T) {
	t.Parallel()

	// Level 0 (top): depth absent = 0. Its child must see depth 1.
	c0 := &fakeSubsession{echo: true}
	s0 := NewSubagent(&fakeFactory{t: t, child: c0}, context.Background())
	if _, err := s0.InvokableRun(context.Background(), `{"skill":"x","message":"0"}`); err != nil {
		t.Fatalf("level 0 InvokableRun error = %v", err)
	}
	if c0.gotDepth != 1 {
		t.Fatalf("level 0 child saw depth %d, want 1", c0.gotDepth)
	}

	// Level 1: re-enter the tool with the ctx the level-0 child observed (depth 1).
	// Its child must see depth 2.
	c1 := &fakeSubsession{echo: true}
	s1 := NewSubagent(&fakeFactory{t: t, child: c1}, context.Background())
	if _, err := s1.InvokableRun(c0.gotCtx, `{"skill":"x","message":"1"}`); err != nil {
		t.Fatalf("level 1 InvokableRun error = %v", err)
	}
	if c1.gotDepth != 2 {
		t.Fatalf("level 1 child saw depth %d, want 2", c1.gotDepth)
	}

	// Level 2: re-enter with the ctx the level-1 child observed (depth 2 == max).
	// This spawn MUST be blocked: the factory must never be called.
	blocked := &fakeFactory{t: t, child: &fakeSubsession{echo: true}, failIfCalled: true}
	s2 := NewSubagent(blocked, context.Background())
	res, err := s2.InvokableRun(c1.gotCtx, `{"skill":"x","message":"2"}`)
	if err != nil {
		t.Fatalf("level 2 InvokableRun error = %v", err)
	}
	if got := textOf(t, res); !strings.Contains(got, "depth limit") {
		t.Errorf("level 2 (grandchild→great-grandchild) result = %q, want a depth-limit error", got)
	}
	if blocked.called {
		t.Error("level 2 factory was called despite reaching the depth cap")
	}
}

// TestSubagentErrors covers the failure paths: factory error, child-Invoke
// error, missing/empty skill, missing/empty message, and unparsable args. Every
// one is a tool-result error STRING (no Go error).
func TestSubagentErrors(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		args      string
		newErr    error
		invokeErr error
		wantSub   string
	}{
		{
			name:    "factory error (unknown skill)",
			args:    `{"skill":"nope","message":"m"}`,
			newErr:  &stubFactoryError{msg: "unknown skill"},
			wantSub: "error:",
		},
		{
			name:      "child invoke error",
			args:      `{"skill":"x","message":"m"}`,
			invokeErr: &stubInvokeError{msg: "turn failed"},
			wantSub:   "error:",
		},
		{name: "missing skill", args: `{"message":"m"}`, wantSub: "skill"},
		{name: "empty skill", args: `{"skill":"  ","message":"m"}`, wantSub: "skill"},
		{name: "missing message", args: `{"skill":"x"}`, wantSub: "message"},
		{name: "empty message", args: `{"skill":"x","message":"   "}`, wantSub: "message"},
		{name: "unparsable args", args: `not json`, wantSub: "invalid arguments"},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			child := &fakeSubsession{echo: true, invokeErr: tt.invokeErr}
			f := &fakeFactory{t: t, child: child, newErr: tt.newErr}
			s := NewSubagent(f, context.Background())

			res, err := s.InvokableRun(context.Background(), tt.args)
			if err != nil {
				t.Fatalf("InvokableRun() Go error = %v (failures must be tool-result strings)", err)
			}
			if got := textOf(t, res); !strings.Contains(got, tt.wantSub) {
				t.Errorf("result = %q, want it to contain %q", got, tt.wantSub)
			}
		})
	}
}

// TestSubagentCapabilities pins the capability surface: Subagent is an
// InvokableTool and Auditable, and is deliberately NOT a PermissionPrompter
// (AutoApprove) and NOT a WriteTarget.
func TestSubagentCapabilities(t *testing.T) {
	t.Parallel()
	var s any = NewSubagent(&fakeFactory{t: t}, context.Background())
	if _, ok := s.(tool.InvokableTool); !ok {
		t.Error("Subagent is not an InvokableTool")
	}
	if _, ok := s.(tool.Auditable); !ok {
		t.Error("Subagent is not Auditable")
	}
	if _, ok := s.(tool.PermissionPrompter); ok {
		t.Error("Subagent must NOT be a PermissionPrompter (it is AutoApprove)")
	}
	if _, ok := s.(tool.WriteTarget); ok {
		t.Error("Subagent must NOT be a WriteTarget")
	}
}
