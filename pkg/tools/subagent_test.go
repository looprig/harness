package tools

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/looprig/harness/pkg/identity"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/tool"
	"github.com/looprig/core/uuid"
)

// mustUUID generates a fresh non-zero UUID for building a non-root parent
// Provenance; it fails the test if the generator errors.
func mustUUID(t *testing.T) uuid.UUID {
	t.Helper()
	id, err := uuid.New()
	if err != nil {
		t.Fatalf("uuid.New() error = %v", err)
	}
	return id
}

// subagent_test.go exercises the agent-aware Subagent tool against a FAKE Spawner
// (DIP: the tool never touches the real session.Session). The fake records what it
// was called with so the tests can assert the security-critical invariants:
//
//   - the tool forwards the {agent} name (typed identity.AgentName) and {message}
//     to Spawn,
//   - the tool reads its OWN provenance from ctx (loop.ProvenanceFrom) and passes
//     it as the `parent` to Spawn (zero provenance when ctx carries none),
//   - Info().Desc renders the available-subagents catalog so the model can pick an
//     agent, and
//   - the audit summary never leaks the (possibly sensitive) agent name or message.
//
// (textOf, the shared *tool.ToolResult → string helper, lives in fetch_test.go.)

// testCatalog is a small fixed catalog used to assert the <available_subagents>
// rendering in Info().Desc.
func testCatalog() []SubagentCatalogEntry {
	return []SubagentCatalogEntry{
		{Name: "operator", Description: "edits files and runs commands"},
		{Name: "explorer", Description: "searches the workspace"},
	}
}

// fakeSpawner is a fake Spawner. It records the parent provenance, agent name,
// message, and parent tool-use id it was asked to spawn with, and returns either
// reply or spawnErr. If echo is set it returns "echo: <agent>: <message>" instead of
// reply.
type fakeSpawner struct {
	mu           sync.Mutex
	reply        string
	echo         bool
	spawnErr     error
	called       bool
	gotParent    loop.Provenance
	gotAgent     identity.AgentName
	gotMessage   string
	gotToolUseID string
}

func (f *fakeSpawner) Spawn(_ context.Context, parent loop.Provenance, agent identity.AgentName, message string, parentToolUseID string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.called = true
	f.gotParent = parent
	f.gotAgent = agent
	f.gotMessage = message
	f.gotToolUseID = parentToolUseID
	if f.spawnErr != nil {
		return "", f.spawnErr
	}
	if f.echo {
		return "echo: " + string(agent) + ": " + message, nil
	}
	return f.reply, nil
}

// stubSpawnError is a typed error a fake Spawner returns to exercise the
// spawn-failed path.
type stubSpawnError struct{ msg string }

func (e *stubSpawnError) Error() string { return e.msg }

// TestSubagentInfo asserts the self-description: the name MUST be exactly
// "Subagent" (the classifyTool/manifest contract), the schema requires BOTH "agent"
// and "message", and Info().Desc renders the available-subagents catalog so the
// model knows which agents it may spawn.
func TestSubagentInfo(t *testing.T) {
	t.Parallel()
	s := NewSubagent(&fakeSpawner{}, testCatalog())
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
	schema := string(info.Schema)
	if !strings.Contains(schema, `"agent"`) {
		t.Errorf("Info().Schema = %q, want it to require an \"agent\" property", schema)
	}
	if !strings.Contains(schema, `"message"`) {
		t.Errorf("Info().Schema = %q, want it to require a \"message\" property", schema)
	}
	if !strings.Contains(schema, `["agent", "message"]`) {
		t.Errorf("Info().Schema = %q, want \"agent\" and \"message\" listed in required", schema)
	}

	// The catalog is rendered as an <available_subagents> listing naming each agent.
	if !strings.Contains(info.Desc, "<available_subagents>") || !strings.Contains(info.Desc, "</available_subagents>") {
		t.Errorf("Info().Desc = %q, want an <available_subagents> block", info.Desc)
	}
	for _, e := range testCatalog() {
		if !strings.Contains(info.Desc, string(e.Name)) {
			t.Errorf("Info().Desc = %q, want it to list agent %q", info.Desc, e.Name)
		}
		if !strings.Contains(info.Desc, e.Description) {
			t.Errorf("Info().Desc = %q, want it to list description %q", info.Desc, e.Description)
		}
	}
}

// TestSubagentInfoEmptyCatalog asserts an empty catalog renders only the static
// prefix (no empty <available_subagents> block) — the boundary case.
func TestSubagentInfoEmptyCatalog(t *testing.T) {
	t.Parallel()
	s := NewSubagent(&fakeSpawner{}, nil)
	info, err := s.Info(context.Background())
	if err != nil {
		t.Fatalf("Info() error = %v", err)
	}
	if info.Desc != subagentDescPrefix {
		t.Errorf("Info().Desc = %q, want the bare prefix %q for an empty catalog", info.Desc, subagentDescPrefix)
	}
	if strings.Contains(info.Desc, "<available_subagents>") {
		t.Errorf("Info().Desc = %q, want NO catalog block for an empty catalog", info.Desc)
	}
}

// TestSubagentAuditSummary asserts the audit summary is the constant "Subagent" and
// NEVER contains the agent name or message — both may carry sensitive context and
// must not reach the audit event.
func TestSubagentAuditSummary(t *testing.T) {
	t.Parallel()
	s := NewSubagent(&fakeSpawner{}, testCatalog())

	tests := []struct {
		name    string
		args    string
		notWant string
	}{
		{
			name:    "message redacted",
			args:    `{"agent":"operator","message":"my secret password is hunter2"}`,
			notWant: "hunter2",
		},
		{
			name:    "agent name redacted",
			args:    `{"agent":"super-secret-agent","message":"m"}`,
			notWant: "super-secret-agent",
		},
		{name: "unparsable args", args: `not json`},
		{name: "empty object", args: `{}`},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := s.AuditSummary(tt.args)
			if got != "Subagent" {
				t.Errorf("AuditSummary() = %q, want %q", got, "Subagent")
			}
			if tt.notWant != "" && strings.Contains(got, tt.notWant) {
				t.Errorf("AuditSummary() = %q leaks substring %q", got, tt.notWant)
			}
		})
	}
}

// TestSubagentRoundTrip asserts the happy path: the tool forwards the {agent} +
// {message} to the Spawner, returns the Spawner's final text, and passes the
// provenance carried in ctx (a wrapped parent vs. the zero/root provenance when ctx
// has none).
func TestSubagentRoundTrip(t *testing.T) {
	t.Parallel()
	parent := loop.Provenance{
		LoopID: mustUUID(t),
		TurnID: mustUUID(t),
		StepID: mustUUID(t),
	}

	tests := []struct {
		name       string
		ctx        context.Context
		wantParent loop.Provenance
	}{
		{
			name:       "provenance from ctx",
			ctx:        loop.WithProvenance(context.Background(), parent),
			wantParent: parent,
		},
		{
			name:       "no provenance is zero parent",
			ctx:        context.Background(),
			wantParent: loop.Provenance{},
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := &fakeSpawner{echo: true}
			s := NewSubagent(f, testCatalog())

			res, err := s.InvokableRun(tt.ctx, `{"agent":"operator","message":"hello there"}`)
			if err != nil {
				t.Fatalf("InvokableRun() Go error = %v (must be nil; failures are tool-result strings)", err)
			}
			if got := textOf(t, res); got != "echo: operator: hello there" {
				t.Errorf("result = %q, want %q", got, "echo: operator: hello there")
			}
			if !f.called {
				t.Error("Spawn was never called")
			}
			if f.gotAgent != identity.AgentName("operator") {
				t.Errorf("Spawn got agent %q, want %q", f.gotAgent, "operator")
			}
			if f.gotMessage != "hello there" {
				t.Errorf("Spawn got message %q, want %q", f.gotMessage, "hello there")
			}
			if f.gotParent != tt.wantParent {
				t.Errorf("Spawn got parent %+v, want %+v", f.gotParent, tt.wantParent)
			}
		})
	}
}

// TestSubagentForwardsToolUseID asserts the tool reads its OWN provider tool-use id
// from ctx (loop.ToolUseIDFrom) and forwards it as the parentToolUseID arg to Spawn,
// so the spawned loop can be correlated to the Subagent tool call that requested it.
func TestSubagentForwardsToolUseID(t *testing.T) {
	t.Parallel()
	ctx := loop.WithToolUseID(context.Background(), "toolu_55")
	fs := &fakeSpawner{}
	_, _ = NewSubagent(fs, testCatalog()).InvokableRun(ctx, `{"agent":"explorer","message":"map repo"}`)
	if fs.gotToolUseID != "toolu_55" {
		t.Fatalf("Spawn parentToolUseID = %q, want toolu_55", fs.gotToolUseID)
	}
}

// TestSubagentErrors covers the failure paths: a Spawn error, a missing/empty agent,
// a missing/empty message, and unparsable args. Every one is a tool-result error
// STRING (the Go error from InvokableRun is always nil).
func TestSubagentErrors(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		args     string
		spawnErr error
		wantSub  string
	}{
		{
			name:     "spawn error",
			args:     `{"agent":"operator","message":"m"}`,
			spawnErr: &stubSpawnError{msg: "unknown agent"},
			wantSub:  "error: subagent failed: unknown agent",
		},
		{name: "missing agent", args: `{"message":"m"}`, wantSub: "error: a non-empty 'agent' is required"},
		{name: "empty agent", args: `{"agent":"   ","message":"m"}`, wantSub: "error: a non-empty 'agent' is required"},
		{name: "missing message", args: `{"agent":"operator"}`, wantSub: "error: a non-empty 'message' is required"},
		{name: "empty message", args: `{"agent":"operator","message":"   "}`, wantSub: "error: a non-empty 'message' is required"},
		{name: "unparsable args", args: `not json`, wantSub: "error: invalid arguments: not a JSON object"},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := &fakeSpawner{echo: true, spawnErr: tt.spawnErr}
			s := NewSubagent(f, testCatalog())

			res, err := s.InvokableRun(context.Background(), tt.args)
			if err != nil {
				t.Fatalf("InvokableRun() Go error = %v (failures must be tool-result strings)", err)
			}
			if got := textOf(t, res); got != tt.wantSub {
				t.Errorf("result = %q, want %q", got, tt.wantSub)
			}
		})
	}
}

// TestSubagentCapabilities pins the capability surface: Subagent is an InvokableTool
// and Auditable, and is deliberately NOT a PermissionPrompter (AutoApprove) and NOT
// a WriteTarget.
func TestSubagentCapabilities(t *testing.T) {
	t.Parallel()
	var s any = NewSubagent(&fakeSpawner{}, testCatalog())
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

// FuzzSubagentArgs fuzzes the untrusted argsJSON decoder: InvokableRun parses model
// output, so it must NEVER panic and must ALWAYS return a nil Go error (every failure
// is a tool-result string). The fake Spawner echoes, so a well-formed call returns
// the echo and a malformed one returns an error string — either way, no panic, no Go
// error.
func FuzzSubagentArgs(f *testing.F) {
	seeds := []string{
		`{"agent":"operator","message":"hello"}`,
		`{"agent":"","message":"m"}`,
		`{"agent":"operator","message":""}`,
		`{"message":"m"}`,
		`{"agent":"operator"}`,
		`{}`,
		`not json`,
		``,
		`{"agent":123,"message":true}`,
		`{"agent":"x","message":"m"}`,
		`[1,2,3]`,
		"{\"agent\":\" \",\"message\":\"\uffff\"}",
		`{"agent":"operator","message":"m","extra":{"nested":[null]}}`,
	}
	for _, s := range seeds {
		f.Add(s)
	}
	s := NewSubagent(&fakeSpawner{echo: true}, testCatalog())
	f.Fuzz(func(t *testing.T, argsJSON string) {
		res, err := s.InvokableRun(context.Background(), argsJSON)
		if err != nil {
			t.Fatalf("InvokableRun() Go error = %v (failures must be tool-result strings, never a Go error)", err)
		}
		if res == nil {
			t.Fatal("InvokableRun() returned a nil result")
		}
	})
}
