package tools

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/inventivepotter/urvi/internal/agent/loop"
	"github.com/inventivepotter/urvi/internal/tool"
	"github.com/inventivepotter/urvi/internal/uuid"
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

// subagent_test.go exercises the Subagent tool against a FAKE Spawner (DIP: the
// tool never touches the real session.Session). The fake records what it was
// called with so the tests can assert the security-critical invariants:
//
//   - the tool reads its OWN provenance from ctx (loop.ProvenanceFrom) and passes
//     it as the `parent` to Spawn (zero provenance when ctx carries none), and
//   - the audit summary never leaks the (possibly sensitive) message.
//
// (textOf, the shared *tool.ToolResult → string helper, lives in fetch_test.go.)

// fakeSpawner is a fake Spawner. It records the parent provenance and message it
// was asked to spawn with, and returns either reply or spawnErr. If echo is set
// it returns "echo: <message>" instead of reply.
type fakeSpawner struct {
	mu         sync.Mutex
	reply      string
	echo       bool
	spawnErr   error
	called     bool
	gotParent  loop.Provenance
	gotMessage string
}

func (f *fakeSpawner) Spawn(_ context.Context, parent loop.Provenance, message string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.called = true
	f.gotParent = parent
	f.gotMessage = message
	if f.spawnErr != nil {
		return "", f.spawnErr
	}
	if f.echo {
		return "echo: " + message, nil
	}
	return f.reply, nil
}

// stubSpawnError is a typed error a fake Spawner returns to exercise the
// spawn-failed path.
type stubSpawnError struct{ msg string }

func (e *stubSpawnError) Error() string { return e.msg }

// TestSubagentInfo asserts the self-description: the name MUST be exactly
// "Subagent" (the classifyTool/manifest contract), the schema requires "message",
// and nothing mentions a skill (the skill arg was removed).
func TestSubagentInfo(t *testing.T) {
	t.Parallel()
	s := NewSubagent(&fakeSpawner{})
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
	if !strings.Contains(schema, `"message"`) {
		t.Errorf("Info().Schema = %q, want it to require a \"message\" property", schema)
	}
	if !strings.Contains(schema, `"required"`) || !strings.Contains(schema, `["message"]`) {
		t.Errorf("Info().Schema = %q, want \"message\" listed in required", schema)
	}
	if strings.Contains(strings.ToLower(schema), "skill") {
		t.Errorf("Info().Schema = %q must NOT mention skill (skill arg removed)", schema)
	}
	if strings.Contains(strings.ToLower(info.Desc), "skill") {
		t.Errorf("Info().Desc = %q must NOT mention skill (skill arg removed)", info.Desc)
	}
}

// TestSubagentAuditSummary asserts the audit summary is the constant "Subagent"
// and NEVER contains the message — the message may carry sensitive context and
// must not reach the audit event.
func TestSubagentAuditSummary(t *testing.T) {
	t.Parallel()
	s := NewSubagent(&fakeSpawner{})

	tests := []struct {
		name    string
		args    string
		notWant string
	}{
		{
			name:    "message redacted",
			args:    `{"message":"my secret password is hunter2"}`,
			notWant: "hunter2",
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
				t.Errorf("AuditSummary() = %q leaks message substring %q", got, tt.notWant)
			}
		})
	}
}

// TestSubagentRoundTrip asserts the happy path: the tool returns the Spawner's
// final text, the Spawner saw the message, and it saw the provenance carried in
// ctx (a wrapped parent vs. the zero/root provenance when ctx has none).
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
			s := NewSubagent(f)

			res, err := s.InvokableRun(tt.ctx, `{"message":"hello there"}`)
			if err != nil {
				t.Fatalf("InvokableRun() Go error = %v (must be nil; failures are tool-result strings)", err)
			}
			if got := textOf(t, res); got != "echo: hello there" {
				t.Errorf("result = %q, want %q", got, "echo: hello there")
			}
			if !f.called {
				t.Error("Spawn was never called")
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

// TestSubagentErrors covers the failure paths: a Spawn error, an empty/whitespace
// message, and unparsable args. Every one is a tool-result error STRING (the Go
// error from InvokableRun is always nil).
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
			args:     `{"message":"m"}`,
			spawnErr: &stubSpawnError{msg: "loop crashed"},
			wantSub:  "error: subagent failed: loop crashed",
		},
		{name: "missing message", args: `{}`, wantSub: "error: a non-empty 'message' is required"},
		{name: "empty message", args: `{"message":"   "}`, wantSub: "error: a non-empty 'message' is required"},
		{name: "unparsable args", args: `not json`, wantSub: "error: invalid arguments: not a JSON object"},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := &fakeSpawner{echo: true, spawnErr: tt.spawnErr}
			s := NewSubagent(f)

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

// TestSubagentCapabilities pins the capability surface: Subagent is an
// InvokableTool and Auditable, and is deliberately NOT a PermissionPrompter
// (AutoApprove) and NOT a WriteTarget.
func TestSubagentCapabilities(t *testing.T) {
	t.Parallel()
	var s any = NewSubagent(&fakeSpawner{})
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
