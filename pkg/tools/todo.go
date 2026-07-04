package tools

import (
	"context"
	"encoding/json"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/looprig/harness/pkg/tool"
	"github.com/looprig/core/uuid"
)

// todo.go implements the Todo tool (design §4b, row Todo; impl plan row 6.10).
// Todo is a session-scoped, in-memory task list the model can create, update, and
// list items in to plan and track multi-step work.
//
// LEAST PRIVILEGE: Todo takes NO dependencies (no filesystem, no network). Its
// entire state is an in-memory map on the struct, guarded by a mutex. One Todo is
// constructed per session, so the list is naturally session-scoped and dies with
// the session.
//
// CONCURRENCY: the runner may invoke tools in parallel within a batch, so every
// state access is guarded by a sync.Mutex (the test suite asserts this is
// -race-clean under concurrent create/update). IDs come from crypto/rand via
// internal/uuid.
//
// AUTO-APPROVE: Todo is AutoApprove — it has no external side effects worth
// gating. It deliberately does NOT implement tool.PermissionPrompter. It DOES
// implement tool.Auditable (the action verb is a safe, non-secret summary).
//
// FAILURE MODEL: every failure — unparsable args, a bad/missing action, a missing
// required field, an unknown id, or a bad status — is a tool-result error STRING.
// InvokableRun never returns a Go error.

// todoToolName is the EXACT tool name. Like AskUser it is classUnknown to
// classifyTool (no path/command boundary), so it reaches AutoApprove only via the
// manifest's HardApprove list (which names "Todo").
const todoToolName = "Todo"

// todoAction is the typed enum of the action field. Validation rejects any value
// outside this set (fail-secure: an unknown action is an error, never a no-op).
type todoAction string

const (
	actionCreate todoAction = "create"
	actionUpdate todoAction = "update"
	actionList   todoAction = "list"
)

// todoStatus is the typed enum of an item's status. An empty status on create
// defaults to statusPending; an empty status on update leaves it unchanged.
type todoStatus string

const (
	statusPending    todoStatus = "pending"
	statusInProgress todoStatus = "in_progress"
	statusDone       todoStatus = "done"
)

// validStatus reports whether s is a recognized status (the empty string is
// handled by the caller, not here).
func validStatus(s todoStatus) bool {
	switch s {
	case statusPending, statusInProgress, statusDone:
		return true
	default:
		return false
	}
}

const todoSchema = `{
  "type": "object",
  "properties": {
    "action": {"type": "string", "enum": ["create", "update", "list"], "description": "create a new item, update an existing item by id, or list all items."},
    "id": {"type": "string", "description": "The item id to update (required for action=update)."},
    "title": {"type": "string", "description": "The item title (required for action=create; optional on update to rename)."},
    "status": {"type": "string", "enum": ["pending", "in_progress", "done"], "description": "Item status (optional; defaults to pending on create; left unchanged on update when omitted)."}
  },
  "required": ["action"]
}`

const todoDesc = "Maintain a session-scoped todo list to plan and track multi-step work: create items, update an item's title/status by id, or list all items. Has no filesystem or network access."

// todoArgs is the typed decode of Todo's untrusted argsJSON. The JSON field
// contract is {action string, id string, title string, status string}.
type todoArgs struct {
	Action todoAction `json:"action"`
	ID     string     `json:"id"`
	Title  string     `json:"title"`
	Status todoStatus `json:"status"`
}

// todoItem is one task. ID is the canonical string form of a crypto/rand uuid;
// Title and Status are the model-supplied fields.
type todoItem struct {
	ID     string
	Title  string
	Status todoStatus
}

// Todo is the in-memory, session-scoped task list. mu guards items. The map is
// keyed by the canonical uuid STRING (the form the model echoes back in an
// update's id field), so no uuid re-parse is needed on the hot path.
type Todo struct {
	mu    sync.Mutex
	items map[string]todoItem
}

// NewTodo constructs an empty Todo. State lives for the lifetime of the value
// (one per session).
func NewTodo() *Todo {
	return &Todo{items: make(map[string]todoItem)}
}

// Info returns Todo's self-description. Name MUST equal "Todo".
func (td *Todo) Info(context.Context) (*tool.ToolInfo, error) {
	return &tool.ToolInfo{
		Name:   todoToolName,
		Desc:   todoDesc,
		Schema: json.RawMessage(todoSchema),
	}, nil
}

// AuditSummary returns "Todo: <action>". The action verb is non-secret. An
// unparsable args document or a missing action yields a generic summary.
func (td *Todo) AuditSummary(argsJSON string) string {
	var args todoArgs
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil || args.Action == "" {
		return "Todo (unparsable args)"
	}
	return "Todo: " + string(args.Action)
}

// InvokableRun parses the args and dispatches on action. Every failure is a
// tool-result error STRING; it never returns a Go error.
func (td *Todo) InvokableRun(_ context.Context, argsJSON string) (*tool.ToolResult, error) {
	var args todoArgs
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return tool.TextResult("error: invalid arguments: not a JSON object"), nil
	}

	switch args.Action {
	case actionCreate:
		return td.create(args)
	case actionUpdate:
		return td.update(args)
	case actionList:
		return td.list()
	case "":
		return tool.TextResult("error: 'action' is required (one of: create, update, list)"), nil
	default:
		return tool.TextResult("error: unknown action " + strconv.Quote(string(args.Action)) + " (want create, update, or list)"), nil
	}
}

// create adds a new item with a fresh uuid id and returns the id. Title is
// required; status defaults to pending and, if supplied, must be valid.
func (td *Todo) create(args todoArgs) (*tool.ToolResult, error) {
	if strings.TrimSpace(args.Title) == "" {
		return tool.TextResult("error: 'title' is required for action=create"), nil
	}
	status := args.Status
	if status == "" {
		status = statusPending
	}
	if !validStatus(status) {
		return tool.TextResult("error: invalid status " + strconv.Quote(string(args.Status)) + " (want pending, in_progress, or done)"), nil
	}

	u, err := uuid.New()
	if err != nil {
		// crypto/rand failure — surface a generic error (this is exceptional).
		return tool.TextResult("error: could not generate todo id"), nil
	}
	id := u.String()

	td.mu.Lock()
	td.items[id] = todoItem{ID: id, Title: args.Title, Status: status}
	td.mu.Unlock()

	return tool.TextResult("created todo " + id + " (" + args.Title + ", " + string(status) + ")"), nil
}

// update mutates an existing item by id: an empty title/status leaves that field
// unchanged; a supplied status must be valid. An unknown id is a tool-result
// error.
func (td *Todo) update(args todoArgs) (*tool.ToolResult, error) {
	id := strings.TrimSpace(args.ID)
	if id == "" {
		return tool.TextResult("error: 'id' is required for action=update"), nil
	}
	if args.Status != "" && !validStatus(args.Status) {
		return tool.TextResult("error: invalid status " + strconv.Quote(string(args.Status)) + " (want pending, in_progress, or done)"), nil
	}

	td.mu.Lock()
	defer td.mu.Unlock()
	item, ok := td.items[id]
	if !ok {
		return tool.TextResult("error: no todo with id " + strconv.Quote(id)), nil
	}
	if args.Title != "" {
		item.Title = args.Title
	}
	if args.Status != "" {
		item.Status = args.Status
	}
	td.items[id] = item
	return tool.TextResult("updated todo " + id + " (" + item.Title + ", " + string(item.Status) + ")"), nil
}

// list renders all items, sorted by id for a stable order. An empty list yields a
// friendly message.
func (td *Todo) list() (*tool.ToolResult, error) {
	td.mu.Lock()
	items := make([]todoItem, 0, len(td.items))
	for _, it := range td.items {
		items = append(items, it)
	}
	td.mu.Unlock()

	if len(items) == 0 {
		return tool.TextResult("No todos."), nil
	}
	sort.Slice(items, func(i, j int) bool {
		return items[i].ID < items[j].ID
	})

	var sb strings.Builder
	for i, it := range items {
		if i > 0 {
			sb.WriteString("\n")
		}
		sb.WriteString("- ")
		sb.WriteString(it.ID)
		sb.WriteString(": ")
		sb.WriteString(it.Title)
		sb.WriteString(" [")
		sb.WriteString(string(it.Status))
		sb.WriteString("]")
	}
	return tool.TextResult(sb.String()), nil
}

// compile-time assertions: Todo is an InvokableTool and Auditable. It is
// deliberately NOT a PermissionPrompter (AutoApprove) and NOT a WriteTarget.
var (
	_ tool.InvokableTool = (*Todo)(nil)
	_ tool.Auditable     = (*Todo)(nil)
)
