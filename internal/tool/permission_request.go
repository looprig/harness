package tool

import "fmt"

// PermissionRequest is the sealed approval-prompt contract a tool's
// PermissionPrompter.BuildRequest returns. The unexported permissionRequest
// marker SEALS the interface: only types declared in this package can implement
// it. A type in another package (e.g. tools/) cannot supply an unexported
// method from this package, so it cannot satisfy PermissionRequest and would
// fail to compile — which is why every concrete request type lives here.
type PermissionRequest interface {
	permissionRequest()  // unexported marker → sealed to this package
	ToolName() string    // approval-prompt header
	Description() string // approval-prompt body (redacted; never raw args)
	AllowedScopes() []ApprovalScope
}

// ApprovalScope is the persistence breadth a user may grant when approving a
// PermissionRequest.
type ApprovalScope uint8

const (
	// ScopeOnce approves this call only; nothing is persisted.
	ScopeOnce ApprovalScope = iota
	// ScopeSession adds an in-memory session policy for the rest of the session.
	ScopeSession
	// ScopeWorkspace persists an approval to
	// ~/.urvi/workspaces/<hash>/approvals.json (out of the repo).
	ScopeWorkspace
)

// persistableScopes is the scope set offered by a request whose action has a
// stable Match representation the gate can persist (path glob, exact command,
// METHOD scheme://host, or query). Returned as a fresh slice per call so a
// caller can never mutate shared state.
func persistableScopes() []ApprovalScope {
	return []ApprovalScope{ScopeOnce, ScopeSession, ScopeWorkspace}
}

// FileWriteRequest is the approval prompt for a file-mutating tool
// (WriteFile/EditFile). Path is the resolved write path — the load-bearing
// field for both the prompt and a persisted path-glob Match (design §4b, §5d).
type FileWriteRequest struct {
	Path string
}

func (FileWriteRequest) permissionRequest()             {}
func (FileWriteRequest) ToolName() string               { return "WriteFile" }
func (r FileWriteRequest) Description() string          { return r.Path }
func (FileWriteRequest) AllowedScopes() []ApprovalScope { return persistableScopes() }

// BashRequest is the approval prompt for the Bash tool. Command is the exact
// command string, which doubles as the persisted exact-command Match (§4b, §5d).
type BashRequest struct {
	Command string
}

func (BashRequest) permissionRequest()             {}
func (BashRequest) ToolName() string               { return "Bash" }
func (r BashRequest) Description() string          { return r.Command }
func (BashRequest) AllowedScopes() []ApprovalScope { return persistableScopes() }

// FetchRequest is the approval prompt for the Fetch tool. Method + URL render
// the prompt body; the persisted Match is "METHOD scheme://host" derived from
// them (§4b, §5d). Host is derivable from URL, so URL is retained whole.
type FetchRequest struct {
	Method string
	URL    string
}

func (FetchRequest) permissionRequest() {}
func (FetchRequest) ToolName() string   { return "Fetch" }
func (r FetchRequest) Description() string {
	return fmt.Sprintf("%s %s", r.Method, r.URL)
}
func (FetchRequest) AllowedScopes() []ApprovalScope { return persistableScopes() }

// WebSearchRequest is the approval prompt for the WebSearch tool. Query is the
// search string (§4b).
type WebSearchRequest struct {
	Query string
}

func (WebSearchRequest) permissionRequest()             {}
func (WebSearchRequest) ToolName() string               { return "WebSearch" }
func (r WebSearchRequest) Description() string          { return r.Query }
func (WebSearchRequest) AllowedScopes() []ApprovalScope { return persistableScopes() }

// UnknownRequest is the runner-built fallback when a tool has no
// PermissionPrompter (design §3a). It carries only a redacted Summary —
// Description returns that Summary and NEVER raw args. An unknown call has no
// stable Match to persist, so it offers only ScopeOnce (fail-secure: nothing
// that can't be safely matched is persisted).
type UnknownRequest struct {
	Tool    string
	Summary string
}

func (UnknownRequest) permissionRequest()             {}
func (r UnknownRequest) ToolName() string             { return r.Tool }
func (r UnknownRequest) Description() string          { return r.Summary }
func (UnknownRequest) AllowedScopes() []ApprovalScope { return []ApprovalScope{ScopeOnce} }
