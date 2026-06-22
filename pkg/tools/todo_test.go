package tools

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/ciram-co/looprig/pkg/tool"
)

// TestTodoInfo asserts the self-description: the name MUST be exactly "Todo"
// (the classifyTool/manifest contract) and the schema/desc are present.
func TestTodoInfo(t *testing.T) {
	t.Parallel()
	td := NewTodo()
	info, err := td.Info(context.Background())
	if err != nil {
		t.Fatalf("Info() error = %v", err)
	}
	if info.Name != "Todo" {
		t.Errorf("Info().Name = %q, want %q", info.Name, "Todo")
	}
	if info.Name != todoToolName {
		t.Errorf("todoToolName const = %q, want Info().Name %q", todoToolName, info.Name)
	}
	if strings.TrimSpace(info.Desc) == "" {
		t.Error("Info().Desc is empty")
	}
	if len(info.Schema) == 0 {
		t.Error("Info().Schema is empty")
	}
}

// TestTodoAuditSummary asserts the audit summary surfaces only the action.
func TestTodoAuditSummary(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		argsJSON string
		want     string
	}{
		{name: "create", argsJSON: `{"action":"create","title":"x"}`, want: "Todo: create"},
		{name: "update", argsJSON: `{"action":"update","id":"y","status":"done"}`, want: "Todo: update"},
		{name: "list", argsJSON: `{"action":"list"}`, want: "Todo: list"},
		{name: "unparsable args", argsJSON: `not json`, want: "Todo (unparsable args)"},
		{name: "missing action", argsJSON: `{}`, want: "Todo (unparsable args)"},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			td := NewTodo()
			if got := td.AuditSummary(tt.argsJSON); got != tt.want {
				t.Errorf("AuditSummary() = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestTodoNotPermissionPrompter asserts Todo is AutoApprove: it must NOT
// implement PermissionPrompter. It MAY (and does) implement Auditable.
func TestTodoNotPermissionPrompter(t *testing.T) {
	t.Parallel()
	var ti tool.InvokableTool = NewTodo()
	if _, ok := ti.(tool.PermissionPrompter); ok {
		t.Error("Todo must NOT implement PermissionPrompter (it is AutoApprove)")
	}
	if _, ok := ti.(tool.Auditable); !ok {
		t.Error("Todo should implement Auditable")
	}
}

// TestTodoActions exercises the single-call action surface: bad action, missing
// fields, unknown id, and list-empty. The create→update→list round-trip is
// covered separately so it can assert on the generated id.
func TestTodoActions(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name         string
		argsJSON     string
		wantErrText  bool
		wantContains string
	}{
		{
			name:         "list empty",
			argsJSON:     `{"action":"list"}`,
			wantContains: "No todos",
		},
		{
			name:         "create requires title",
			argsJSON:     `{"action":"create"}`,
			wantErrText:  true,
			wantContains: "error:",
		},
		{
			name:         "update unknown id is error",
			argsJSON:     `{"action":"update","id":"does-not-exist","status":"done"}`,
			wantErrText:  true,
			wantContains: "error:",
		},
		{
			name:         "update requires id",
			argsJSON:     `{"action":"update","status":"done"}`,
			wantErrText:  true,
			wantContains: "error:",
		},
		{
			name:         "bad action is error",
			argsJSON:     `{"action":"frobnicate"}`,
			wantErrText:  true,
			wantContains: "error:",
		},
		{
			name:         "missing action is error",
			argsJSON:     `{}`,
			wantErrText:  true,
			wantContains: "error:",
		},
		{
			name:         "unparsable args is error",
			argsJSON:     `not json`,
			wantErrText:  true,
			wantContains: "error:",
		},
		{
			name:         "create rejects unknown status",
			argsJSON:     `{"action":"create","title":"x","status":"bogus"}`,
			wantErrText:  true,
			wantContains: "error:",
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			td := NewTodo()
			res, err := td.InvokableRun(context.Background(), tt.argsJSON)
			if err != nil {
				t.Fatalf("InvokableRun() Go error = %v; failures must be tool-result strings", err)
			}
			got := textOf(t, res)
			if tt.wantErrText && !strings.HasPrefix(got, "error:") {
				t.Errorf("result = %q, want error: string", got)
			}
			if !strings.Contains(got, tt.wantContains) {
				t.Errorf("result = %q, want to contain %q", got, tt.wantContains)
			}
		})
	}
}

// TestTodoRoundTrip covers create→returns id, then update by that id, then list
// reflecting the change — the core stateful contract.
func TestTodoRoundTrip(t *testing.T) {
	t.Parallel()
	td := NewTodo()
	ctx := context.Background()

	// create → returns the new id.
	res, err := td.InvokableRun(ctx, `{"action":"create","title":"write tests","status":"pending"}`)
	if err != nil {
		t.Fatalf("create Go error = %v", err)
	}
	createText := textOf(t, res)
	if strings.HasPrefix(createText, "error:") {
		t.Fatalf("create returned error result: %q", createText)
	}
	id := extractTodoID(t, createText)
	if id == "" {
		t.Fatalf("create result %q did not surface a non-empty id", createText)
	}

	// list → reflects the created item.
	res, err = td.InvokableRun(ctx, `{"action":"list"}`)
	if err != nil {
		t.Fatalf("list Go error = %v", err)
	}
	listText := textOf(t, res)
	if !strings.Contains(listText, id) || !strings.Contains(listText, "write tests") || !strings.Contains(listText, "pending") {
		t.Fatalf("list %q missing id/title/status", listText)
	}

	// update → mutate status + title of the existing item.
	updArgs := todoTestArgs(t, "update", id, "ship tests", "done")
	res, err = td.InvokableRun(ctx, updArgs)
	if err != nil {
		t.Fatalf("update Go error = %v", err)
	}
	updText := textOf(t, res)
	if strings.HasPrefix(updText, "error:") {
		t.Fatalf("update returned error result: %q", updText)
	}

	// list → reflects the update.
	res, err = td.InvokableRun(ctx, `{"action":"list"}`)
	if err != nil {
		t.Fatalf("list Go error = %v", err)
	}
	listText = textOf(t, res)
	if !strings.Contains(listText, "ship tests") || !strings.Contains(listText, "done") {
		t.Fatalf("list after update %q did not reflect new title/status", listText)
	}
	if strings.Contains(listText, "write tests") {
		t.Errorf("list after update %q still shows the old title", listText)
	}
}

// TestTodoConcurrent spawns many goroutines creating and updating items
// concurrently. Under -race it asserts the mutex-guarded map is race-free, and it
// asserts the final item count is exactly the number of creates (no lost writes).
func TestTodoConcurrent(t *testing.T) {
	t.Parallel()
	td := NewTodo()
	ctx := context.Background()

	const creators = 16
	const perCreator = 8

	var wg sync.WaitGroup
	ids := make(chan string, creators*perCreator)
	for i := 0; i < creators; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < perCreator; j++ {
				res, err := td.InvokableRun(ctx, `{"action":"create","title":"concurrent","status":"pending"}`)
				if err != nil {
					t.Errorf("concurrent create Go error = %v", err)
					return
				}
				txt := textOf(t, res)
				if strings.HasPrefix(txt, "error:") {
					t.Errorf("concurrent create error result: %q", txt)
					return
				}
				ids <- extractTodoID(t, txt)
			}
		}()
	}
	wg.Wait()
	close(ids)

	// Concurrently update every created id.
	var updWG sync.WaitGroup
	for id := range ids {
		id := id
		updWG.Add(1)
		go func() {
			defer updWG.Done()
			args := todoTestArgs(t, "update", id, "", "done")
			if _, err := td.InvokableRun(ctx, args); err != nil {
				t.Errorf("concurrent update Go error = %v", err)
			}
		}()
	}
	updWG.Wait()

	// list → count is exactly creators*perCreator (no lost writes / double counts).
	res, err := td.InvokableRun(ctx, `{"action":"list"}`)
	if err != nil {
		t.Fatalf("final list Go error = %v", err)
	}
	listText := textOf(t, res)
	got := strings.Count(listText, "done")
	if got != creators*perCreator {
		t.Errorf("final list has %d done items, want %d:\n%s", got, creators*perCreator, listText)
	}
}

// extractTodoID pulls the first uuid-looking token (8-4-4-4-12 hex) out of a Todo
// result string so the round-trip test can drive an update against it without
// coupling to the exact result phrasing.
func extractTodoID(t *testing.T, s string) string {
	t.Helper()
	for _, f := range strings.FieldsFunc(s, func(r rune) bool {
		return r == ' ' || r == '\n' || r == '\t' || r == '(' || r == ')' || r == ':'
	}) {
		if looksLikeUUID(f) {
			return f
		}
	}
	return ""
}

// looksLikeUUID reports whether tok is in canonical 8-4-4-4-12 hex form.
func looksLikeUUID(tok string) bool {
	parts := strings.Split(tok, "-")
	if len(parts) != 5 {
		return false
	}
	want := []int{8, 4, 4, 4, 12}
	for i, p := range parts {
		if len(p) != want[i] {
			return false
		}
		for _, c := range p {
			isHex := (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')
			if !isHex {
				return false
			}
		}
	}
	return true
}

// todoTestArgs builds a JSON update/create args document, escaping fields safely.
func todoTestArgs(t *testing.T, action, id, title, status string) string {
	t.Helper()
	m := map[string]string{"action": action}
	if id != "" {
		m["id"] = id
	}
	if title != "" {
		m["title"] = title
	}
	if status != "" {
		m["status"] = status
	}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal args: %v", err)
	}
	return string(b)
}
