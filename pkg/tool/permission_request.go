package tool

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"

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

// ApprovalScopeValue returns the stable wire value used in gate prompt options
// and gate responses.
func ApprovalScopeValue(scope ApprovalScope) (string, bool) {
	switch scope {
	case ScopeOnce:
		return "once", true
	case ScopeSession:
		return "session", true
	case ScopeWorkspace:
		return "workspace", true
	default:
		return "", false
	}
}

// ParseApprovalScopeValue parses the stable wire value used in gate responses.
func ParseApprovalScopeValue(value string) (ApprovalScope, bool) {
	switch value {
	case "once":
		return ScopeOnce, true
	case "session":
		return ScopeSession, true
	case "workspace":
		return ScopeWorkspace, true
	default:
		return 0, false
	}
}

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

// GrantDisplay is a MAC-verified escalation grant shown in a permission prompt:
// an opaque Token plus its bound human-readable Description (both from the sandbox
// executor; harness never mints or interprets them). A GrantDisplay only ever
// exists for a token whose executor-side MAC verification SUCCEEDED — a fabricated
// or tampered token is rejected before a GrantDisplay is built, so one can never
// reach a prompt (SPEC §10.7).
//
// Token is retained (not dropped after rendering Description) so the durable
// approval/audit record binds the operator's decision to the EXACT grants shown —
// the same tokens can be reconciled against what the executor later applies. It is
// NOT a capability at rest: harness cannot mint or apply it, and DescribeGrant
// re-verifies its MAC on any later use.
//
// The lowercase json tags are the durable wire form (SPEC §10.7): like the sibling
// requestType discriminator, "token"/"description" must NOT be renamed — old
// journals carry the old keys.
type GrantDisplay struct {
	Token       string `json:"token"`
	Description string `json:"description"`
}

// BashRequest is the approval prompt for the Bash tool. Command is the exact
// command string, which doubles as the persisted exact-command Match (§4b, §5d).
//
// Grants are the MAC-verified escalation grants the operator would apply by
// approving this call (SPEC §9.3): each carries the executor's opaque token and its
// bound human-readable description, so the prompt shows "allow network egress for:
// git push" rather than an opaque token. It is omitempty so a call that needs no
// escalation marshals byte-identically to a pre-Grants BashRequest (durable backward
// compatibility); the tag "grants" must NOT be renamed (SPEC §10.7 durability).
//
// Grants are deliberately NOT folded into Description() (which stays Command-only so
// the persisted exact-command Match is unchanged): they are durably journaled here
// and rendered by the cli TUI, which type-asserts tool.BashRequest and reads .Grants
// directly (the same way it renders richer file-diff detail). A maintainer should not
// read Grants as wired through Description() — the rendering lives in the TUI module.
type BashRequest struct {
	Command string
	Grants  []GrantDisplay `json:"grants,omitempty"`
}

func (BashRequest) permissionRequest() {}
func (BashRequest) ToolName() string   { return "Bash" }

// Description is Command only — Grants are intentionally excluded (the TUI renders
// them via type-assertion; see the BashRequest doc) so the persisted exact-command
// Match stays stable.
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

// maxExternalDescriptionBytes bounds the redacted description accepted by
// NewExternalRequest. It reuses the codec's existing maxPermissionRequestBytes
// cap — the only sanctioned bound on a permission request's redacted text —
// with headroom for the tool name, the "type" tag, and JSON escaping (a
// worst-case string can escape to 6 bytes per input byte). An externalRequest
// therefore ALWAYS marshals under the codec cap, so a request that a caller
// successfully constructs can never fail closed later at the restore boundary.
//
// Anything longer is truncated, not rejected: an over-long description is a
// caller sizing bug, not an attack the gate should fail on, and the request
// still carries a usable prompt. The bound is what makes the sealed contract's
// guarantee hold for a caller outside this package.
const maxExternalDescriptionBytes = maxPermissionRequestBytes / 8

// maxExternalToolNameBytes bounds the tool name accepted by NewExternalRequest.
// Without it the "always marshals under the codec cap" guarantee above is false:
// the description bound alone leaves the name free to carry the record past
// maxPermissionRequestBytes, journaling a request that then fails closed at
// restore. It matches the 128-byte name bound the durable
// event.LoopExternalToolsetChanged validator enforces — a longer name could not
// be journaled by the toolset record either, so accepting one here would only
// defer the failure.
//
// Unlike the description this REJECTS rather than truncates. A description is
// prose and survives truncation; a tool name is an IDENTIFIER, and a truncated
// one names a different tool than the one being approved. Silently rewriting it
// would make the permission record a lie about what the user authorized.
const maxExternalToolNameBytes = 128

// ExternalRequestErrorKind classifies a rejected NewExternalRequest argument.
type ExternalRequestErrorKind string

const (
	// ExternalToolNameEmpty reports an empty or whitespace-only tool name.
	ExternalToolNameEmpty ExternalRequestErrorKind = "tool_name_empty"
	// ExternalToolNameTooLong reports a tool name over maxExternalToolNameBytes.
	// It fails closed rather than truncating: a truncated identifier names a
	// different tool than the one being approved.
	ExternalToolNameTooLong ExternalRequestErrorKind = "tool_name_too_long"
	// ExternalScopesEmpty reports an empty or nil scope set. A request that
	// offers no scope is unapprovable, so it fails closed rather than silently
	// degrading to ScopeOnce.
	ExternalScopesEmpty ExternalRequestErrorKind = "scopes_empty"
	// ExternalScopeInvalid reports a scope outside the valid set.
	ExternalScopeInvalid ExternalRequestErrorKind = "scope_invalid"
)

// ExternalRequestError reports an invalid NewExternalRequest argument.
type ExternalRequestError struct {
	Kind ExternalRequestErrorKind
}

func (e *ExternalRequestError) Error() string {
	return fmt.Sprintf("tool: invalid external permission request (%s)", string(e.Kind))
}

// externalRequest is the approval prompt for a capability implemented OUTSIDE
// this module (an MCP tool, another protocol adapter). It is package-private and
// reachable only through NewExternalRequest: the sealed PermissionRequest
// interface stays sealed, and this is a validating CONSTRUCTOR, not an escape
// hatch. An external caller gets to supply a redacted summary and the scopes its
// capability can safely persist; it does not get to supply behavior.
//
// Summary is already redacted by the caller (the adapter is the only party that
// can tell a safe summary from raw arguments) and is bounded + normalized here,
// so this type upholds the same Description() contract as every sibling: a
// prompt body, never raw args. It mirrors UnknownRequest.Summary, whose role it
// generalizes.
//
// Scopes is what distinguishes this from UnknownRequest: an external capability
// with a stable permission identity (e.g. "mcp:<binding>:<raw-tool>") CAN be
// safely session- or workspace-persisted, which UnknownRequest's fail-secure
// ScopeOnce-only set cannot express.
//
// The type is persisted through an explicit wire form (externalRequestData), not
// struct tags, so the durable record carries the STABLE scope strings rather
// than the ApprovalScope iota values — renumbering the enum must never silently
// re-interpret an old journal.
type externalRequest struct {
	Tool    string
	Summary string
	Scopes  []ApprovalScope
}

// NewExternalRequest builds a PermissionRequest for capabilities implemented
// outside this module (e.g. MCP tools). description must ALREADY be redacted by
// the caller — this constructor bounds and normalizes it but cannot tell a safe
// summary from a leaked secret. scopes are validated and defensively copied.
//
// It fails closed on an empty or over-long tool name, an empty scope set, or a
// scope outside the valid set.
func NewExternalRequest(toolName, description string, scopes []ApprovalScope) (PermissionRequest, error) {
	name := strings.TrimSpace(toolName)
	if name == "" {
		return nil, &ExternalRequestError{Kind: ExternalToolNameEmpty}
	}
	if len(name) > maxExternalToolNameBytes {
		return nil, &ExternalRequestError{Kind: ExternalToolNameTooLong}
	}
	if len(scopes) == 0 {
		return nil, &ExternalRequestError{Kind: ExternalScopesEmpty}
	}
	copied := make([]ApprovalScope, 0, len(scopes))
	for _, scope := range scopes {
		if _, ok := ApprovalScopeValue(scope); !ok {
			return nil, &ExternalRequestError{Kind: ExternalScopeInvalid}
		}
		copied = append(copied, scope)
	}
	return externalRequest{
		Tool:    name,
		Summary: boundDescription(strings.TrimSpace(description)),
		Scopes:  copied,
	}, nil
}

// boundDescription truncates s to maxExternalDescriptionBytes without splitting
// a UTF-8 rune, so a bounded description is still valid UTF-8 and cannot produce
// a mangled prompt or a replacement-character audit record.
func boundDescription(s string) string {
	if len(s) <= maxExternalDescriptionBytes {
		return s
	}
	cut := maxExternalDescriptionBytes
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

func (externalRequest) permissionRequest()    {}
func (r externalRequest) ToolName() string    { return r.Tool }
func (r externalRequest) Description() string { return r.Summary }

// AllowedScopes returns a fresh slice per call so a caller can never mutate the
// request's approved scope set through a retained reference.
func (r externalRequest) AllowedScopes() []ApprovalScope {
	out := make([]ApprovalScope, len(r.Scopes))
	copy(out, r.Scopes)
	return out
}

// externalRequestData is the durable wire form of an externalRequest. Scopes are
// the STABLE ApprovalScopeValue strings, never the enum's iota values.
type externalRequestData struct {
	Tool    string   `json:"tool"`
	Summary string   `json:"summary,omitempty"`
	Scopes  []string `json:"scopes"`
}

// MarshalJSON writes the explicit wire form. A scope with no stable string is
// unrepresentable and fails closed rather than being dropped, which would
// silently widen or narrow what a restored request offers.
func (r externalRequest) MarshalJSON() ([]byte, error) {
	scopes := make([]string, 0, len(r.Scopes))
	for _, scope := range r.Scopes {
		value, ok := ApprovalScopeValue(scope)
		if !ok {
			return nil, &ExternalRequestError{Kind: ExternalScopeInvalid}
		}
		scopes = append(scopes, value)
	}
	return json.Marshal(externalRequestData{Tool: r.Tool, Summary: r.Summary, Scopes: scopes})
}

// UnmarshalJSON reconstructs an externalRequest from the wire form. Restore is
// an UNTRUSTED boundary, so it re-applies every NewExternalRequest invariant
// rather than trusting the record: a journal that names an unknown scope, no
// scopes, or no tool is rejected instead of yielding a request whose
// AllowedScopes silently differ from what the human originally approved.
func (r *externalRequest) UnmarshalJSON(data []byte) error {
	var raw externalRequestData
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	if strings.TrimSpace(raw.Tool) == "" {
		return &ExternalRequestError{Kind: ExternalToolNameEmpty}
	}
	if len(raw.Scopes) == 0 {
		return &ExternalRequestError{Kind: ExternalScopesEmpty}
	}
	scopes := make([]ApprovalScope, 0, len(raw.Scopes))
	for _, value := range raw.Scopes {
		scope, ok := ParseApprovalScopeValue(value)
		if !ok {
			return &ExternalRequestError{Kind: ExternalScopeInvalid}
		}
		scopes = append(scopes, scope)
	}
	r.Tool = raw.Tool
	r.Summary = boundDescription(raw.Summary)
	r.Scopes = scopes
	return nil
}

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
