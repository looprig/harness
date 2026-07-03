package tool

import (
	"fmt"

	"github.com/looprig/harness/pkg/identity"
)

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
	// ~/.looprig/workspaces/<hash>/approvals.json (out of the repo).
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

// skillHashPrefixLen is the number of leading hex characters of a skill body's
// SHA-256 surfaced in the human gate prompt. A short prefix is enough for a
// person to eyeball-compare the snapshot the gate approves against an expected
// digest, without rendering an unwieldy 64-char string; the full digest is
// retained in the SHA256 field (and the persisted codec) for exact binding.
const skillHashPrefixLen = 12

// SkillLoadRequest is the approval prompt for loading an UNTRUSTED workspace
// skill (`.skills/<name>/SKILL.md`) into an agent's context (design §7a). A
// workspace skill body becomes instructions in the agent's context, so the load
// is a human-gated trust boundary — distinct from the trusted, auto-approved
// embedded skill source.
//
// The fields are the SAFE metadata of a TOCTOU-safe snapshot taken BEFORE the
// prompt (an os.Root descriptor-relative read + SHA-256): RelPath is the
// workspace-relative path the snapshot came from, Agent is the requesting agent,
// Size is the snapshot byte length, and SHA256 is the snapshot's full digest.
// Description renders ONLY this metadata — NEVER the body — so the prompt cannot
// itself smuggle injected instructions past the human.
//
// AllowedScopes is fail-secure {ScopeOnce} only: a workspace skill is untrusted,
// so an approval is NEVER session- or workspace-persisted — every load
// re-prompts and re-binds a fresh snapshot.
type SkillLoadRequest struct {
	RelPath string             // workspace-relative path, e.g. ".skills/<name>/SKILL.md"
	Agent   identity.AgentName // the agent requesting the load
	Size    int64              // snapshot length in bytes
	SHA256  string             // full hex SHA-256 of the snapshot bytes
}

func (SkillLoadRequest) permissionRequest() {}
func (SkillLoadRequest) ToolName() string   { return "Skill" }

// Description renders the safe snapshot metadata for the human gate: the relative
// path, the requesting agent, the size, and a SHORT hash prefix. It never renders
// the skill body (SkillLoadRequest carries no body field by construction).
func (r SkillLoadRequest) Description() string {
	short := r.SHA256
	if len(short) > skillHashPrefixLen {
		short = short[:skillHashPrefixLen]
	}
	return fmt.Sprintf("agent %s load %s (%d bytes, sha256:%s)", r.Agent, r.RelPath, r.Size, short)
}

// AllowedScopes is {ScopeOnce} only — a workspace skill load is never persisted.
func (SkillLoadRequest) AllowedScopes() []ApprovalScope { return []ApprovalScope{ScopeOnce} }
