package tool

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
