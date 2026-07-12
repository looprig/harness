package tools

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/tool"
	jsonschema "github.com/santhosh-tekuri/jsonschema/v6"
)

// subagent_test.go exercises the flat action-envelope Subagent tool against a FAKE
// tool.DelegateController (DIP: the tool never touches the real session). The fake
// records the DelegateRequest it was handed so the tests assert the envelope→request
// translation, and returns a programmed DelegateResult/error so the tests assert the
// result→tool-string formatting. The exposed JSON schema is derived from the active
// delegation style; the parent-scoped controller — not the schema — is the security
// boundary, so the tool forwards faithfully and the controller re-enforces.
//
// (textOf, the shared *tool.ToolResult → string helper, lives in fetch_test.go.)

// fakeController is a fake tool.DelegateController. It records each request and
// returns either result or execErr.
type fakeController struct {
	mu       sync.Mutex
	result   tool.DelegateResult
	execErr  error
	requests []tool.DelegateRequest
}

func (f *fakeController) Execute(_ context.Context, request tool.DelegateRequest) (tool.DelegateResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, request)
	if f.execErr != nil {
		return tool.DelegateResult{}, f.execErr
	}
	return f.result, nil
}

func (f *fakeController) last() tool.DelegateRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.requests) == 0 {
		return tool.DelegateRequest{}
	}
	return f.requests[len(f.requests)-1]
}

func subagentCatalog() []SubagentCatalogEntry {
	return []SubagentCatalogEntry{
		{Name: "operator", Description: "edits files and runs commands", Modes: []loop.ModeName{"", "build"}},
		{Name: "explorer", Description: "searches the workspace", Modes: []loop.ModeName{"", "review"}},
	}
}

type stubControllerError struct{ msg string }

func (e *stubControllerError) Error() string { return e.msg }

func mustParseUUID(t *testing.T, s string) uuid.UUID {
	t.Helper()
	id, err := uuid.Parse(s)
	if err != nil {
		t.Fatalf("uuid.Parse(%q): %v", s, err)
	}
	return id
}

func boolPtr(b bool) *bool { return &b }
func intPtr(i int) *int    { return &i }

// TestSubagentInfoSchemaPerStyle asserts the exposed schema is derived from the
// delegation style: sync-only advertises only "start", managed advertises all five
// actions. The name is exactly "Subagent" and the catalog is rendered.
func TestSubagentInfoSchemaPerStyle(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		style       loop.DelegationStyle
		wantActions []string
		notActions  []string
	}{
		{
			name:        "sync only exposes start",
			style:       loop.DelegationSyncOnly,
			wantActions: []string{"start"},
			notActions:  []string{"send", "wait", "interrupt", "status"},
		},
		{
			name:        "managed exposes all five",
			style:       loop.DelegationManaged,
			wantActions: []string{"start", "send", "wait", "interrupt", "status"},
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			s := NewSubagent(&fakeController{}, tt.style, subagentCatalog())
			info, err := s.Info(context.Background())
			if err != nil {
				t.Fatalf("Info() error = %v", err)
			}
			if info.Name != subagentToolName {
				t.Errorf("Info().Name = %q, want %q", info.Name, subagentToolName)
			}
			if len(info.Schema) == 0 {
				t.Fatal("Info().Schema is empty")
			}
			var schemaObj map[string]any
			if err := json.Unmarshal(info.Schema, &schemaObj); err != nil {
				t.Fatalf("Info().Schema is not valid JSON: %v", err)
			}
			properties := schemaObj["properties"].(map[string]any)
			actionSchema := properties["action"].(map[string]any)
			actionValues := actionSchema["enum"].([]any)
			actions := make(map[string]bool, len(actionValues))
			for _, value := range actionValues {
				actions[value.(string)] = true
			}
			for _, action := range tt.wantActions {
				if !actions[action] {
					t.Errorf("schema missing action %q: %s", action, info.Schema)
				}
			}
			for _, action := range tt.notActions {
				if actions[action] {
					t.Errorf("sync-only schema must not advertise action %q: %s", action, info.Schema)
				}
			}
			// The catalog is rendered so the model can pick a valid agent.
			for _, e := range subagentCatalog() {
				if !strings.Contains(info.Desc, string(e.Name)) {
					t.Errorf("Info().Desc = %q, want it to list agent %q", info.Desc, e.Name)
				}
			}
		})
	}
}

// TestSubagentStartDefaults asserts the synchronous-preserving defaults: a missing
// action means "start" and a missing "wait" on start means true, and the envelope is
// translated into the right DelegateRequest.
func TestSubagentStartDefaults(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		args     string
		wantOp   tool.DelegateOperation
		wantWait bool
		wantMode string
	}{
		{name: "omitted action is start with wait true", args: `{"agent":"explorer","message":"map repo"}`, wantOp: tool.DelegateStart, wantWait: true},
		{name: "explicit start honors wait false", args: `{"action":"start","agent":"explorer","message":"m","wait":false}`, wantOp: tool.DelegateStart, wantWait: false},
		{name: "start carries the selected mode", args: `{"action":"start","agent":"explorer","message":"m","mode":"review"}`, wantOp: tool.DelegateStart, wantWait: true, wantMode: "review"},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			fc := &fakeController{result: tool.DelegateResult{
				DelegateID: mustParseUUID(t, "55555555-5555-4555-8555-555555555555"),
				Status:     tool.DelegateStatusCompleted,
				Output:     "ok",
			}}
			s := NewSubagent(fc, loop.DelegationManaged, subagentCatalog())
			if _, err := s.InvokableRun(context.Background(), tt.args); err != nil {
				t.Fatalf("InvokableRun() Go error = %v (must be nil)", err)
			}
			got := fc.last()
			if got.Operation != tt.wantOp {
				t.Errorf("Operation = %v, want %v", got.Operation, tt.wantOp)
			}
			if got.Wait != tt.wantWait {
				t.Errorf("Wait = %v, want %v", got.Wait, tt.wantWait)
			}
			if got.Agent != "explorer" {
				t.Errorf("Agent = %q, want explorer", got.Agent)
			}
			if got.Mode != tt.wantMode {
				t.Errorf("Mode = %q, want %q", got.Mode, tt.wantMode)
			}
		})
	}
}

func TestSubagentSyncOnlyCannotCraftAsyncStart(t *testing.T) {
	t.Parallel()
	fc := &fakeController{result: tool.DelegateResult{Status: tool.DelegateStatusCompleted}}
	s := NewSubagent(fc, loop.DelegationSyncOnly, subagentCatalog())

	for _, args := range []string{
		`{"agent":"explorer","message":"map repo"}`,
		`{"action":"start","agent":"explorer","message":"map repo","wait":true}`,
	} {
		if _, err := s.InvokableRun(context.Background(), args); err != nil {
			t.Fatalf("InvokableRun(%s): %v", args, err)
		}
		if got := fc.last(); got.Operation != tool.DelegateStart || !got.Wait {
			t.Fatalf("request = %+v, want synchronous start", got)
		}
	}

	before := len(fc.requests)
	res, err := s.InvokableRun(context.Background(), `{"action":"start","agent":"explorer","message":"map repo","wait":false}`)
	if err != nil {
		t.Fatalf("InvokableRun crafted async start Go error = %v", err)
	}
	if got := textOf(t, res); !strings.Contains(got, "unavailable") {
		t.Fatalf("crafted async result = %q, want unavailable error", got)
	}
	if got := len(fc.requests); got != before {
		t.Fatalf("controller calls = %d, want %d", got, before)
	}
}

func TestSubagentStrictActionEnvelopes(t *testing.T) {
	t.Parallel()
	del := "55555555-5555-4555-8555-555555555555"
	req := "66666666-6666-4666-8666-666666666666"
	tests := []struct {
		name string
		args string
	}{
		{name: "unknown field", args: `{"agent":"explorer","message":"m","extra":true}`},
		{name: "trailing JSON", args: `{"agent":"explorer","message":"m"} {}`},
		{name: "fractional timeout", args: `{"agent":"explorer","message":"m","timeout_seconds":1.5}`},
		{name: "start forbids delegate", args: `{"agent":"explorer","message":"m","delegate_id":"` + del + `"}`},
		{name: "start forbids request", args: `{"agent":"explorer","message":"m","request_id":"` + req + `"}`},
		{name: "send forbids agent", args: `{"action":"send","delegate_id":"` + del + `","message":"m","agent":"explorer"}`},
		{name: "send forbids mode", args: `{"action":"send","delegate_id":"` + del + `","message":"m","mode":"review"}`},
		{name: "send forbids request", args: `{"action":"send","delegate_id":"` + del + `","message":"m","request_id":"` + req + `"}`},
		{name: "wait forbids wait", args: `{"action":"wait","delegate_id":"` + del + `","request_id":"` + req + `","wait":true}`},
		{name: "wait forbids message", args: `{"action":"wait","delegate_id":"` + del + `","request_id":"` + req + `","message":"m"}`},
		{name: "interrupt forbids timeout", args: `{"action":"interrupt","delegate_id":"` + del + `","timeout_seconds":1}`},
		{name: "status forbids message", args: `{"action":"status","message":"m"}`},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			fc := &fakeController{}
			s := NewSubagent(fc, loop.DelegationManaged, subagentCatalog())
			res, err := s.InvokableRun(context.Background(), tt.args)
			if err != nil {
				t.Fatalf("InvokableRun Go error = %v", err)
			}
			if got := textOf(t, res); !strings.Contains(got, "error:") {
				t.Fatalf("result = %q, want boundary error", got)
			}
			if len(fc.requests) != 0 {
				t.Fatal("invalid envelope reached controller")
			}
		})
	}
}

func TestSubagentSchemaIsClosedAndCatalogsModes(t *testing.T) {
	t.Parallel()
	for _, style := range []loop.DelegationStyle{loop.DelegationSyncOnly, loop.DelegationManaged} {
		info, err := NewSubagent(&fakeController{}, style, subagentCatalog()).Info(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		schema := string(info.Schema)
		for _, want := range []string{`"additionalProperties":false`, `"operator"`, `"explorer"`, `"build"`, `"review"`} {
			if !strings.Contains(schema, want) {
				t.Errorf("style %v schema missing %s: %s", style, want, schema)
			}
		}
	}
}

func TestSubagentSchemaValidatesActionEnvelopes(t *testing.T) {
	t.Parallel()
	del := "55555555-5555-4555-8555-555555555555"
	req := "66666666-6666-4666-8666-666666666666"
	tests := []struct {
		name  string
		input string
		valid bool
	}{
		{name: "omitted action defaults start", input: `{"agent":"explorer","message":"map"}`, valid: true},
		{name: "explicit start", input: `{"action":"start","agent":"operator","mode":"build","message":"build"}`, valid: true},
		{name: "send", input: `{"action":"send","delegate_id":"` + del + `","message":"more","wait":false}`, valid: true},
		{name: "wait", input: `{"action":"wait","delegate_id":"` + del + `","request_id":"` + req + `"}`, valid: true},
		{name: "interrupt", input: `{"action":"interrupt","delegate_id":"` + del + `"}`, valid: true},
		{name: "status all", input: `{"action":"status"}`, valid: true},
		{name: "send missing message", input: `{"action":"send","delegate_id":"` + del + `"}`},
		{name: "wait with forbidden wait", input: `{"action":"wait","delegate_id":"` + del + `","request_id":"` + req + `","wait":true}`},
		{name: "interrupt with message", input: `{"action":"interrupt","delegate_id":"` + del + `","message":"x"}`},
		{name: "status with request", input: `{"action":"status","request_id":"` + req + `"}`},
		{name: "wrong agent mode", input: `{"agent":"explorer","mode":"build","message":"x"}`},
	}
	info, err := NewSubagent(&fakeController{}, loop.DelegationManaged, subagentCatalog()).Info(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	compiler := jsonschema.NewCompiler()
	var schemaDocument any
	if err := json.Unmarshal(info.Schema, &schemaDocument); err != nil {
		t.Fatal(err)
	}
	if err := compiler.AddResource("subagent.json", schemaDocument); err != nil {
		t.Fatal(err)
	}
	schema, err := compiler.Compile("subagent.json")
	if err != nil {
		t.Fatalf("compile schema: %v\n%s", err, info.Schema)
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			var value any
			if err := json.Unmarshal([]byte(tt.input), &value); err != nil {
				t.Fatal(err)
			}
			err := schema.Validate(value)
			if (err == nil) != tt.valid {
				t.Fatalf("Validate(%s) error = %v, valid=%v", tt.input, err, tt.valid)
			}
		})
	}
	syncInfo, err := NewSubagent(&fakeController{}, loop.DelegationSyncOnly, subagentCatalog()).Info(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var syncDocument any
	if err := json.Unmarshal(syncInfo.Schema, &syncDocument); err != nil {
		t.Fatal(err)
	}
	syncCompiler := jsonschema.NewCompiler()
	if err := syncCompiler.AddResource("sync-subagent.json", syncDocument); err != nil {
		t.Fatal(err)
	}
	syncSchema, err := syncCompiler.Compile("sync-subagent.json")
	if err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		input string
		valid bool
	}{
		{input: `{"agent":"explorer","message":"map"}`, valid: true},
		{input: `{"action":"start","agent":"explorer","message":"map","wait":true}`, valid: true},
		{input: `{"action":"start","agent":"explorer","message":"map","wait":false}`},
		{input: `{"action":"status"}`},
	} {
		var value any
		if err := json.Unmarshal([]byte(tt.input), &value); err != nil {
			t.Fatal(err)
		}
		err := syncSchema.Validate(value)
		if (err == nil) != tt.valid {
			t.Fatalf("sync Validate(%s) error = %v, valid=%v", tt.input, err, tt.valid)
		}
	}
}

// TestSubagentActionMapping asserts each action verb maps to the right operation and
// forwards the addressing (delegate_id / request_id) faithfully so the controller can
// enforce ownership + the action set.
func TestSubagentActionMapping(t *testing.T) {
	t.Parallel()
	del := "55555555-5555-4555-8555-555555555555"
	req := "66666666-6666-4666-8666-666666666666"
	tests := []struct {
		name          string
		args          string
		wantOp        tool.DelegateOperation
		wantDelegate  bool
		wantRequestID bool
	}{
		{name: "send", args: `{"action":"send","delegate_id":"` + del + `","message":"progress?"}`, wantOp: tool.DelegateSend, wantDelegate: true},
		{name: "wait", args: `{"action":"wait","delegate_id":"` + del + `","request_id":"` + req + `"}`, wantOp: tool.DelegateWait, wantDelegate: true, wantRequestID: true},
		{name: "interrupt", args: `{"action":"interrupt","delegate_id":"` + del + `"}`, wantOp: tool.DelegateInterrupt, wantDelegate: true},
		{name: "status one", args: `{"action":"status","delegate_id":"` + del + `"}`, wantOp: tool.DelegateStatus, wantDelegate: true},
		{name: "status all", args: `{"action":"status"}`, wantOp: tool.DelegateStatus},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			fc := &fakeController{result: tool.DelegateResult{
				DelegateID: mustParseUUID(t, del),
				Status:     tool.DelegateStatusRunning,
			}}
			s := NewSubagent(fc, loop.DelegationManaged, subagentCatalog())
			if _, err := s.InvokableRun(context.Background(), tt.args); err != nil {
				t.Fatalf("InvokableRun() Go error = %v", err)
			}
			got := fc.last()
			if got.Operation != tt.wantOp {
				t.Errorf("Operation = %v, want %v", got.Operation, tt.wantOp)
			}
			if tt.wantDelegate && got.DelegateID.IsZero() {
				t.Error("DelegateID was not forwarded")
			}
			if !tt.wantDelegate && !got.DelegateID.IsZero() {
				t.Errorf("DelegateID = %v, want zero", got.DelegateID)
			}
			if tt.wantRequestID && (got.RequestID == nil || got.RequestID.IsZero()) {
				t.Errorf("RequestID = %v, want the supplied request id", got.RequestID)
			}
		})
	}
}

// TestSubagentEnvelopeErrors covers the boundary validation: every failure is a
// tool-result error STRING and InvokableRun never returns a Go error.
func TestSubagentEnvelopeErrors(t *testing.T) {
	t.Parallel()
	del := "55555555-5555-4555-8555-555555555555"
	zero := "00000000-0000-0000-0000-000000000000"
	tests := []struct {
		name    string
		args    string
		wantSub string
	}{
		{name: "unparsable", args: `not json`, wantSub: "invalid arguments"},
		{name: "unknown action", args: `{"action":"destroy"}`, wantSub: "unknown action"},
		{name: "start missing agent", args: `{"action":"start","message":"m"}`, wantSub: "'agent' is required"},
		{name: "start missing message", args: `{"action":"start","agent":"explorer"}`, wantSub: "'message' is required"},
		{name: "send missing delegate", args: `{"action":"send","message":"m"}`, wantSub: "'delegate_id' is required"},
		{name: "send missing message", args: `{"action":"send","delegate_id":"` + del + `"}`, wantSub: "'message' is required"},
		{name: "wait missing delegate", args: `{"action":"wait","request_id":"` + del + `"}`, wantSub: "'delegate_id' is required"},
		{name: "wait missing request", args: `{"action":"wait","delegate_id":"` + del + `"}`, wantSub: "'request_id' is required"},
		{name: "wait zero request", args: `{"action":"wait","delegate_id":"` + del + `","request_id":"` + zero + `"}`, wantSub: "'request_id' is required"},
		{name: "interrupt missing delegate", args: `{"action":"interrupt"}`, wantSub: "'delegate_id' is required"},
		{name: "negative timeout", args: `{"action":"start","agent":"explorer","message":"m","timeout_seconds":-1}`, wantSub: "'timeout_seconds' must be non-negative"},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			fc := &fakeController{}
			s := NewSubagent(fc, loop.DelegationManaged, subagentCatalog())
			res, err := s.InvokableRun(context.Background(), tt.args)
			if err != nil {
				t.Fatalf("InvokableRun() Go error = %v (failures must be tool-result strings)", err)
			}
			got := textOf(t, res)
			if !strings.Contains(got, tt.wantSub) {
				t.Errorf("result = %q, want containing %q", got, tt.wantSub)
			}
			// A boundary rejection must NEVER reach the controller.
			fc.mu.Lock()
			n := len(fc.requests)
			fc.mu.Unlock()
			if n != 0 {
				t.Errorf("controller was called %d times on a boundary rejection, want 0", n)
			}
		})
	}
}

// TestSubagentWaitResultFormatting asserts the DelegateResult → tool-string mapping
// for a synchronous (waited) request across every terminal status.
func TestSubagentWaitResultFormatting(t *testing.T) {
	t.Parallel()
	del := mustParseUUID(t, "55555555-5555-4555-8555-555555555555")
	tests := []struct {
		name    string
		result  tool.DelegateResult
		execErr error
		want    string
		wantSub bool // want is a substring rather than an exact match
	}{
		{name: "completed returns output", result: tool.DelegateResult{DelegateID: del, Status: tool.DelegateStatusCompleted, Output: "the answer"}, want: "the answer"},
		{name: "failed becomes error", result: tool.DelegateResult{DelegateID: del, Status: tool.DelegateStatusFailed}, want: "failed", wantSub: true},
		{name: "interrupted becomes error", result: tool.DelegateResult{DelegateID: del, Status: tool.DelegateStatusInterrupted}, want: "interrupted", wantSub: true},
		{name: "timed out becomes error", result: tool.DelegateResult{DelegateID: del, Status: tool.DelegateStatusTimedOut}, want: "timed out", wantSub: true},
		{name: "execute error", execErr: &stubControllerError{msg: "not owned"}, want: "not owned", wantSub: true},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			fc := &fakeController{result: tt.result, execErr: tt.execErr}
			s := NewSubagent(fc, loop.DelegationManaged, subagentCatalog())
			res, err := s.InvokableRun(context.Background(), `{"action":"start","agent":"explorer","message":"m","wait":true}`)
			if err != nil {
				t.Fatalf("InvokableRun() Go error = %v", err)
			}
			got := textOf(t, res)
			if tt.wantSub {
				if !strings.Contains(got, tt.want) {
					t.Errorf("result = %q, want containing %q", got, tt.want)
				}
				return
			}
			if got != tt.want {
				t.Errorf("result = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestSubagentQueuedResultFormatting asserts a wait:false start/send returns the
// {delegate_id, request_id, status:"queued"} handle the parent later waits on.
func TestSubagentQueuedResultFormatting(t *testing.T) {
	t.Parallel()
	del := mustParseUUID(t, "55555555-5555-4555-8555-555555555555")
	req := mustParseUUID(t, "66666666-6666-4666-8666-666666666666")
	fc := &fakeController{result: tool.DelegateResult{DelegateID: del, RequestID: req, Status: tool.DelegateStatusQueued}}
	s := NewSubagent(fc, loop.DelegationManaged, subagentCatalog())
	res, err := s.InvokableRun(context.Background(), `{"action":"start","agent":"explorer","message":"m","wait":false}`)
	if err != nil {
		t.Fatalf("InvokableRun() Go error = %v", err)
	}
	var out map[string]string
	if err := json.Unmarshal([]byte(textOf(t, res)), &out); err != nil {
		t.Fatalf("queued result is not JSON: %v (%q)", err, textOf(t, res))
	}
	if out["delegate_id"] != del.String() {
		t.Errorf("delegate_id = %q, want %q", out["delegate_id"], del.String())
	}
	if out["request_id"] != req.String() {
		t.Errorf("request_id = %q, want %q", out["request_id"], req.String())
	}
	if out["status"] != "queued" {
		t.Errorf("status = %q, want queued", out["status"])
	}
}

// TestSubagentStatusFormatting asserts a status result renders bounded mechanical
// facts (state + pending count), never a raw transcript or cursor.
func TestSubagentStatusFormatting(t *testing.T) {
	t.Parallel()
	del := mustParseUUID(t, "55555555-5555-4555-8555-555555555555")
	fc := &fakeController{result: tool.DelegateResult{DelegateID: del, Status: tool.DelegateStatusRunning, PendingRequests: 2}}
	s := NewSubagent(fc, loop.DelegationManaged, subagentCatalog())
	res, err := s.InvokableRun(context.Background(), `{"action":"status","delegate_id":"`+del.String()+`"}`)
	if err != nil {
		t.Fatalf("InvokableRun() Go error = %v", err)
	}
	got := textOf(t, res)
	if !strings.Contains(got, "running") {
		t.Errorf("status = %q, want it to report running", got)
	}
	if !strings.Contains(got, "2") {
		t.Errorf("status = %q, want it to report the pending-request count", got)
	}
}

// TestSubagentAuditSummary asserts the audit summary is the constant "Subagent" and
// never leaks the (possibly sensitive) message or agent name.
func TestSubagentAuditSummary(t *testing.T) {
	t.Parallel()
	s := NewSubagent(&fakeController{}, loop.DelegationManaged, subagentCatalog())
	tests := []struct {
		name    string
		args    string
		notWant string
	}{
		{name: "message redacted", args: `{"agent":"operator","message":"secret hunter2"}`, notWant: "hunter2"},
		{name: "agent redacted", args: `{"agent":"super-secret-agent","message":"m"}`, notWant: "super-secret-agent"},
		{name: "unparsable", args: `not json`},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := s.AuditSummary(tt.args)
			if got != "Subagent" {
				t.Errorf("AuditSummary() = %q, want Subagent", got)
			}
			if tt.notWant != "" && strings.Contains(got, tt.notWant) {
				t.Errorf("AuditSummary() = %q leaks %q", got, tt.notWant)
			}
		})
	}
}

// TestSubagentCapabilities pins the capability surface: Subagent is an InvokableTool
// and Auditable, deliberately NOT a PermissionPrompter (AutoApprove) and NOT a
// WriteTarget.
func TestSubagentCapabilities(t *testing.T) {
	t.Parallel()
	var s any = NewSubagent(&fakeController{}, loop.DelegationManaged, subagentCatalog())
	if _, ok := s.(tool.InvokableTool); !ok {
		t.Error("Subagent is not an InvokableTool")
	}
	if _, ok := s.(tool.Auditable); !ok {
		t.Error("Subagent is not Auditable")
	}
	if _, ok := s.(tool.PermissionPrompter); ok {
		t.Error("Subagent must NOT be a PermissionPrompter")
	}
	if _, ok := s.(tool.WriteTarget); ok {
		t.Error("Subagent must NOT be a WriteTarget")
	}
}

// FuzzSubagentArgs fuzzes the untrusted decoder: InvokableRun parses model output, so
// it must NEVER panic and must ALWAYS return a nil Go error (every failure is a
// tool-result string).
func FuzzSubagentArgs(f *testing.F) {
	seeds := []string{
		`{"agent":"operator","message":"hello"}`,
		`{"action":"send","delegate_id":"55555555-5555-4555-8555-555555555555","message":"m"}`,
		`{"action":"wait","delegate_id":"x","request_id":"y"}`,
		`{"action":"status"}`,
		`{"action":"start","timeout_seconds":-5}`,
		`{}`,
		`not json`,
		``,
		`{"agent":123,"message":true}`,
		`[1,2,3]`,
		`{"action":"start","agent":"x","message":"m","wait":"notabool"}`,
	}
	for _, s := range seeds {
		f.Add(s)
	}
	s := NewSubagent(&fakeController{result: tool.DelegateResult{
		DelegateID: uuid.MustParse("55555555-5555-4555-8555-555555555555"),
		Status:     tool.DelegateStatusCompleted,
		Output:     "ok",
	}}, loop.DelegationManaged, subagentCatalog())
	f.Fuzz(func(t *testing.T, argsJSON string) {
		res, err := s.InvokableRun(context.Background(), argsJSON)
		if err != nil {
			t.Fatalf("InvokableRun() Go error = %v (failures must be tool-result strings)", err)
		}
		if res == nil {
			t.Fatal("InvokableRun() returned a nil result")
		}
	})
}
