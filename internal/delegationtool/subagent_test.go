package delegationtool

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/looprig/core/content"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/tool"
)

func textOf(t *testing.T, result *tool.ToolResult) string {
	t.Helper()
	if result == nil {
		t.Fatal("nil ToolResult")
	}
	if len(result.Content) != 1 {
		t.Fatalf("want 1 content block, got %d", len(result.Content))
	}
	block, ok := result.Content[0].(*content.TextBlock)
	if !ok {
		t.Fatalf("want *content.TextBlock, got %T", result.Content[0])
	}
	return block.Text
}

func invokePrepared(t *testing.T, s *SubagentTool, args string) (*tool.ToolResult, error) {
	t.Helper()
	executionID, err := uuid.New()
	if err != nil {
		t.Fatal(err)
	}
	request, artifact, err := s.PrepareCall(context.Background(), executionID, args)
	if err != nil {
		return tool.TextResult("error: subagent action unavailable"), nil
	}
	ctx := loop.WithPreparedCall(context.Background(), tool.PreparedCall{Request: request, Artifact: artifact})
	return s.InvokableRun(ctx, args)
}

func TestSubagentInfoSchemaBytesDeterministicAcrossConcurrentCalls(t *testing.T) {
	t.Parallel()
	s := NewSubagent(&fakeController{}, loop.DelegationManaged, subagentCatalog())
	baseline, err := s.Info(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	const calls = 256
	results := make(chan []byte, calls)
	var wg sync.WaitGroup
	for i := 0; i < calls; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			info, infoErr := s.Info(context.Background())
			if infoErr != nil {
				results <- nil
				return
			}
			results <- append([]byte(nil), info.Schema...)
		}()
	}
	wg.Wait()
	close(results)
	for got := range results {
		if !bytes.Equal(got, baseline.Schema) {
			t.Fatalf("schema bytes changed:\nbase=%s\n got=%s", baseline.Schema, got)
		}
	}
}

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

// TestSubagentStartDefaults asserts the new background-preserving defaults: a missing
// action means "start" and a missing "run_in_background" means true, and the envelope is
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
		{name: "omitted action is background start", args: `{"description":"d","prompt":"map repo","subagent_type":"explorer"}`, wantOp: tool.DelegateStart, wantWait: false},
		{name: "explicit start foreground", args: `{"action":"start","description":"d","prompt":"m","subagent_type":"explorer","run_in_background":false}`, wantOp: tool.DelegateStart, wantWait: true},
		{name: "start carries the selected mode", args: `{"action":"start","description":"d","prompt":"m","subagent_type":"explorer","mode":"review"}`, wantOp: tool.DelegateStart, wantWait: false, wantMode: "review"},
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
			if _, err := invokePrepared(t, s, tt.args); err != nil {
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
		`{"description":"d","prompt":"map repo","subagent_type":"explorer","run_in_background":false}`,
		`{"action":"start","description":"d","prompt":"map repo","subagent_type":"explorer","run_in_background":false}`,
	} {
		if _, err := invokePrepared(t, s, args); err != nil {
			t.Fatalf("InvokableRun(%s): %v", args, err)
		}
		if got := fc.last(); got.Operation != tool.DelegateStart || !got.Wait {
			t.Fatalf("request = %+v, want synchronous start", got)
		}
	}

	before := len(fc.requests)
	res, err := invokePrepared(t, s, `{"action":"start","description":"d","prompt":"map repo","subagent_type":"explorer","run_in_background":true}`)
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
		{name: "unknown field", args: `{"description":"d","prompt":"m","subagent_type":"explorer","extra":true}`},
		{name: "trailing JSON", args: `{"description":"d","prompt":"m","subagent_type":"explorer"} {}`},
		{name: "fractional timeout", args: `{"description":"d","prompt":"m","subagent_type":"explorer","timeout_seconds":1.5}`},
		{name: "start forbids delegate", args: `{"description":"d","prompt":"m","subagent_type":"explorer","delegate_id":"` + del + `"}`},
		{name: "start forbids request", args: `{"description":"d","prompt":"m","subagent_type":"explorer","request_id":"` + req + `"}`},
		{name: "send forbids agent", args: `{"action":"send","delegate_id":"` + del + `","prompt":"m","subagent_type":"explorer"}`},
		{name: "send forbids mode", args: `{"action":"send","delegate_id":"` + del + `","prompt":"m","mode":"review"}`},
		{name: "send forbids request", args: `{"action":"send","delegate_id":"` + del + `","prompt":"m","request_id":"` + req + `"}`},
		{name: "wait forbids background", args: `{"action":"wait","delegate_id":"` + del + `","request_id":"` + req + `","run_in_background":true}`},
		{name: "wait forbids prompt", args: `{"action":"wait","delegate_id":"` + del + `","request_id":"` + req + `","prompt":"m"}`},
		{name: "interrupt forbids timeout", args: `{"action":"interrupt","delegate_id":"` + del + `","timeout_seconds":1}`},
		{name: "status forbids prompt", args: `{"action":"status","prompt":"m"}`},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			fc := &fakeController{}
			s := NewSubagent(fc, loop.DelegationManaged, subagentCatalog())
			res, err := invokePrepared(t, s, tt.args)
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

func TestSubagentSchemaActionBranchesAreClosedAndExplicit(t *testing.T) {
	t.Parallel()
	info, err := NewSubagent(&fakeController{}, loop.DelegationManaged, subagentCatalog()).Info(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var schema map[string]any
	if err := json.Unmarshal(info.Schema, &schema); err != nil {
		t.Fatal(err)
	}
	if schema["additionalProperties"] != false {
		t.Fatal("schema must set additionalProperties:false")
	}
	branches := schema["allOf"].([]any)
	if len(branches) != 6 {
		t.Fatalf("allOf branches=%d, want 6", len(branches))
	}
	fields := []string{"description", "prompt", "subagent_type", "mode", "run_in_background", "delegate_id", "request_id", "timeout_seconds"}
	expected := []struct {
		index             int
		action            string
		required, allowed []string
	}{
		{0, "start", []string{"description", "prompt", "subagent_type"}, []string{"description", "prompt", "subagent_type", "mode", "run_in_background", "timeout_seconds"}},
		{2, "send", []string{"delegate_id", "prompt"}, []string{"delegate_id", "prompt", "run_in_background", "timeout_seconds"}},
		{3, "wait", []string{"delegate_id", "request_id"}, []string{"delegate_id", "request_id", "timeout_seconds"}},
		{4, "interrupt", []string{"delegate_id"}, []string{"delegate_id"}},
		{5, "status", nil, []string{"delegate_id"}},
	}
	stringSet := func(values any) map[string]bool {
		out := map[string]bool{}
		if values == nil {
			return out
		}
		for _, value := range values.([]any) {
			out[value.(string)] = true
		}
		return out
	}
	for _, want := range expected {
		branch := branches[want.index].(map[string]any)
		predicate := branch["if"].(map[string]any)
		if !stringSet(predicate["required"])["action"] {
			t.Fatalf("%s predicate does not require action", want.action)
		}
		gotAction := predicate["properties"].(map[string]any)["action"].(map[string]any)["const"]
		if gotAction != want.action {
			t.Fatalf("branch %d action=%v, want %s", want.index, gotAction, want.action)
		}
		then := branch["then"].(map[string]any)
		required := stringSet(then["required"])
		for _, field := range want.required {
			if !required[field] {
				t.Fatalf("%s missing required %s", want.action, field)
			}
		}
		allowed := map[string]bool{}
		for _, field := range want.allowed {
			allowed[field] = true
		}
		forbidden := map[string]bool{}
		for _, item := range then["not"].(map[string]any)["anyOf"].([]any) {
			for field := range stringSet(item.(map[string]any)["required"]) {
				forbidden[field] = true
			}
		}
		for _, field := range fields {
			if forbidden[field] == allowed[field] {
				t.Fatalf("%s field %s allowed=%v forbidden=%v", want.action, field, allowed[field], forbidden[field])
			}
		}
	}
	defaultBranch := branches[1].(map[string]any)
	defaultIf := defaultBranch["if"].(map[string]any)["not"].(map[string]any)
	if !stringSet(defaultIf["required"])["action"] {
		t.Fatal("default branch is not 'not required(action)'")
	}
	defaultRequired := stringSet(defaultBranch["then"].(map[string]any)["required"])
	if !defaultRequired["description"] || !defaultRequired["prompt"] || !defaultRequired["subagent_type"] {
		t.Fatal("default start must require description+prompt+subagent_type")
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
		{name: "send", args: `{"action":"send","delegate_id":"` + del + `","prompt":"progress?"}`, wantOp: tool.DelegateSend, wantDelegate: true},
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
			if _, err := invokePrepared(t, s, tt.args); err != nil {
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
		{name: "unparsable", args: `not json`, wantSub: "error:"},
		{name: "unknown action", args: `{"action":"destroy"}`, wantSub: "error:"},
		{name: "start missing role", args: `{"action":"start","description":"d","prompt":"m"}`, wantSub: "error:"},
		{name: "start missing prompt", args: `{"action":"start","description":"d","subagent_type":"explorer"}`, wantSub: "error:"},
		{name: "send missing delegate", args: `{"action":"send","prompt":"m"}`, wantSub: "error:"},
		{name: "send missing prompt", args: `{"action":"send","delegate_id":"` + del + `"}`, wantSub: "error:"},
		{name: "wait missing delegate", args: `{"action":"wait","request_id":"` + del + `"}`, wantSub: "error:"},
		{name: "wait missing request", args: `{"action":"wait","delegate_id":"` + del + `"}`, wantSub: "error:"},
		{name: "wait zero request", args: `{"action":"wait","delegate_id":"` + del + `","request_id":"` + zero + `"}`, wantSub: "error:"},
		{name: "interrupt missing delegate", args: `{"action":"interrupt"}`, wantSub: "error:"},
		{name: "negative timeout", args: `{"action":"start","description":"d","prompt":"m","subagent_type":"explorer","timeout_seconds":-1}`, wantSub: "error:"},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			fc := &fakeController{}
			s := NewSubagent(fc, loop.DelegationManaged, subagentCatalog())
			res, err := invokePrepared(t, s, tt.args)
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
		{name: "execute error", execErr: &stubControllerError{msg: "not owned"}, want: "request failed", wantSub: true},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			fc := &fakeController{result: tt.result, execErr: tt.execErr}
			s := NewSubagent(fc, loop.DelegationManaged, subagentCatalog())
			res, err := invokePrepared(t, s, `{"action":"start","description":"d","prompt":"m","subagent_type":"explorer","run_in_background":false}`)
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
	res, err := invokePrepared(t, s, `{"action":"start","description":"d","prompt":"m","subagent_type":"explorer","run_in_background":true}`)
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
	res, err := invokePrepared(t, s, `{"action":"status","delegate_id":"`+del.String()+`"}`)
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

// TestSubagentCapabilities pins the capability surface: Subagent is an
// InvokableTool, Auditable, and a pure CallPreparer, deliberately NOT a
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
	if _, ok := s.(tool.CallPreparer); !ok {
		t.Error("Subagent must implement the mandatory CallPreparer capability")
	}
	if _, ok := s.(tool.WriteTarget); ok {
		t.Error("Subagent must NOT be a WriteTarget")
	}
}

func TestSubagentExecutionRequiresTypedPreparedArtifact(t *testing.T) {
	t.Parallel()
	fc := &fakeController{}
	s := NewSubagent(fc, loop.DelegationManaged, subagentCatalog())
	for name, ctx := range map[string]context.Context{
		"missing":    context.Background(),
		"wrong type": loop.WithPreparedCall(context.Background(), tool.PreparedCall{Artifact: tool.TokenArtifact{Token: "not a delegation"}}),
	} {
		name, ctx := name, ctx
		t.Run(name, func(t *testing.T) {
			result, err := s.InvokableRun(ctx, `{"description":"ignored","prompt":"ignored","subagent_type":"explorer"}`)
			if err != nil {
				t.Fatal(err)
			}
			if got := textOf(t, result); got != "error: subagent call unavailable" {
				t.Fatalf("result = %q, want bounded unavailable error", got)
			}
		})
	}
	if got := len(fc.requests); got != 0 {
		t.Fatalf("controller calls = %d, want 0", got)
	}
}

func TestSubagentExecutionUsesPreparedRequestAndTrustedToolUseID(t *testing.T) {
	t.Parallel()
	fc := &fakeController{result: tool.DelegateResult{Status: tool.DelegateStatusQueued}}
	s := NewSubagent(fc, loop.DelegationManaged, subagentCatalog())
	request, artifact, err := s.PrepareCall(context.Background(), mustParseUUID(t, "11111111-1111-4111-8111-111111111111"), `{"description":"d","prompt":"p","subagent_type":"explorer"}`)
	if err != nil {
		t.Fatal(err)
	}
	ctx := loop.WithPreparedCall(context.Background(), tool.PreparedCall{Request: request, Artifact: artifact})
	ctx = loop.WithToolUseID(ctx, "trusted-tool-use-id")
	if _, err := s.InvokableRun(ctx, `not JSON and deliberately unrelated`); err != nil {
		t.Fatal(err)
	}
	got := fc.last()
	if got.Agent != "explorer" || got.Message != "p" || got.ParentToolUseID != "trusted-tool-use-id" {
		t.Fatalf("request = %+v, want prepared values plus trusted tool id", got)
	}
}

func TestSubagentQueuedRuntimeResultUsesJSONAndOmitsNativeHarnessWhenEmpty(t *testing.T) {
	t.Parallel()
	catalog := testPreparationCatalog(t)
	fc := &fakeController{result: tool.DelegateResult{DelegateID: mustParseUUID(t, "55555555-5555-4555-8555-555555555555"), RequestID: mustParseUUID(t, "66666666-6666-4666-8666-666666666666"), Status: tool.DelegateStatusQueued}}
	s := NewSubagentWithRuntimeCatalog(fc, loop.DelegationManaged, subagentCatalog(), catalog)
	result, err := invokePrepared(t, s, `{"action":"start","description":"d","prompt":"p","subagent_type":"worker","agent_harness":"claude-code","model":"sonnet","effort":"medium"}`)
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		Runtime *runtimeResult `json:"runtime"`
	}
	if err := json.Unmarshal([]byte(textOf(t, result)), &out); err != nil {
		t.Fatal(err)
	}
	if out.Runtime == nil || out.Runtime.AgentHarness != "claude-code" || out.Runtime.Model != "sonnet" || out.Runtime.Effort != "medium" {
		t.Fatalf("runtime result = %+v, want resolved tuple", out.Runtime)
	}
}

func TestSubagentQueuedNativeNoChoiceOmitsRuntime(t *testing.T) {
	t.Parallel()
	fc := &fakeController{result: tool.DelegateResult{
		DelegateID: mustParseUUID(t, "55555555-5555-4555-8555-555555555555"),
		RequestID:  mustParseUUID(t, "66666666-6666-4666-8666-666666666666"),
		Status:     tool.DelegateStatusQueued,
	}}
	s := NewSubagentWithRuntimeCatalog(fc, loop.DelegationManaged, []SubagentCatalogEntry{{Name: "worker"}}, emptyRuntimeCatalog(t))
	result, err := invokePrepared(t, s, `{"action":"start","description":"d","prompt":"p","subagent_type":"worker"}`)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]json.RawMessage
	if err := json.Unmarshal([]byte(textOf(t, result)), &out); err != nil {
		t.Fatal(err)
	}
	if _, present := out["runtime"]; present {
		t.Fatalf("native no-choice result = %s, must omit runtime", textOf(t, result))
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

// TestSubagentPrepareCallProducesArtifact verifies that preparation owns the
// envelope and execution receives a typed artifact even for native/no-choice calls.
func TestSubagentPrepareCallIsPure(t *testing.T) {
	var _ tool.CallPreparer = (*SubagentTool)(nil)
	st := NewSubagent(&fakeController{}, loop.DelegationManaged, subagentCatalog())
	id, err := uuid.New()
	if err != nil {
		t.Fatalf("uuid.New: %v", err)
	}
	request, artifact, err := st.PrepareCall(context.Background(), id, `{"description":"d","prompt":"m","subagent_type":"explorer"}`)
	if err != nil {
		t.Fatalf("PrepareCall() error = %v", err)
	}
	if len(request.Requirements) != 0 || request.ExecutionID != "" {
		t.Errorf("PrepareCall() request = %+v, want a pure empty request", request)
	}
	if _, ok := artifact.(tool.DelegateArtifact); !ok {
		t.Errorf("PrepareCall() artifact = %T, want tool.DelegateArtifact", artifact)
	}
}
