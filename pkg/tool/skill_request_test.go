package tool

import (
	"strings"
	"testing"

	"github.com/looprig/harness/pkg/identity"
)

// Compile-time assertions: SkillLoadRequest satisfies the sealed
// PermissionRequest interface (and its unexported permissionRequest marker), and
// SkillArtifact satisfies the sealed PreparedArtifact interface (and its
// unexported preparedArtifact marker). A type in another package cannot supply
// these unexported markers, which is why both concrete types live in this package.
var (
	_ PermissionRequest = SkillLoadRequest{}
	_ PreparedArtifact  = SkillArtifact{}
)

// TestSkillLoadRequestContract pins SkillLoadRequest's PermissionRequest
// behavior: the tool header, the redacted Description, and the fail-secure
// ScopeOnce-only AllowedScopes.
func TestSkillLoadRequestContract(t *testing.T) {
	t.Parallel()

	onceOnly := []ApprovalScope{ScopeOnce}

	tests := []struct {
		name        string
		req         SkillLoadRequest
		wantTool    string
		descSubstr  []string // substrings that MUST appear in Description()
		descMissing []string // substrings that MUST NOT appear in Description()
		wantScopes  []ApprovalScope
	}{
		{
			name: "happy path renders metadata",
			req: SkillLoadRequest{
				RelPath: ".skills/refactor/SKILL.md",
				Agent:   identity.AgentName("explorer"),
				Size:    1234,
				SHA256:  "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
			},
			wantTool:    "Skill",
			descSubstr:  []string{".skills/refactor/SKILL.md", "explorer", "1234", "01234567"},
			descMissing: nil,
			wantScopes:  onceOnly,
		},
		{
			name:        "zero value",
			req:         SkillLoadRequest{},
			wantTool:    "Skill",
			descSubstr:  nil,
			descMissing: nil,
			wantScopes:  onceOnly,
		},
		{
			name: "short hash truncates full digest in description",
			req: SkillLoadRequest{
				RelPath: ".skills/x/SKILL.md",
				Agent:   identity.AgentName("researcher"),
				Size:    42,
				SHA256:  "deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef0",
			},
			wantTool: "Skill",
			descSubstr: []string{
				".skills/x/SKILL.md", "researcher", "42",
				"deadbeef", // short prefix is present
			},
			descMissing: []string{
				// the FULL 64-char digest must not appear verbatim — only a short prefix
				"deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef0",
			},
			wantScopes: onceOnly,
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := tt.req.ToolName(); got != tt.wantTool {
				t.Errorf("ToolName() = %q, want %q", got, tt.wantTool)
			}
			desc := tt.req.Description()
			for _, sub := range tt.descSubstr {
				if !strings.Contains(desc, sub) {
					t.Errorf("Description() = %q, want substring %q", desc, sub)
				}
			}
			for _, sub := range tt.descMissing {
				if strings.Contains(desc, sub) {
					t.Errorf("Description() = %q, must NOT contain %q", desc, sub)
				}
			}
			gotScopes := tt.req.AllowedScopes()
			if len(gotScopes) != len(tt.wantScopes) {
				t.Fatalf("AllowedScopes() = %v, want %v", gotScopes, tt.wantScopes)
			}
			for i := range tt.wantScopes {
				if gotScopes[i] != tt.wantScopes[i] {
					t.Fatalf("AllowedScopes() = %v, want %v", gotScopes, tt.wantScopes)
				}
			}
		})
	}
}

// TestSkillLoadRequestNeverExposesBody is the load-bearing security assertion:
// the human gate prompt (Description) must render only safe metadata, never the
// skill body. SkillLoadRequest carries no body field at all, so this guards
// against a future field leaking into the prompt via Description.
func TestSkillLoadRequestNeverExposesBody(t *testing.T) {
	t.Parallel()

	const secretBody = "SECRET-INJECTED-INSTRUCTIONS-IGNORE-ALL-PRIOR"

	tests := []struct {
		name string
		req  SkillLoadRequest
	}{
		{
			name: "metadata only",
			req: SkillLoadRequest{
				RelPath: ".skills/evil/SKILL.md",
				Agent:   identity.AgentName("explorer"),
				Size:    int64(len(secretBody)),
				SHA256:  "abcdef00abcdef00abcdef00abcdef00abcdef00abcdef00abcdef00abcdef00a",
			},
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := tt.req.Description(); strings.Contains(got, secretBody) {
				t.Errorf("Description() leaked the skill body: %q", got)
			}
		})
	}
}

// TestSkillLoadRequestScopeOnce pins the fail-secure choice: a workspace skill
// load is untrusted, so it offers only ScopeOnce — never session- or
// workspace-persisted; every load re-prompts (design §7a).
func TestSkillLoadRequestScopeOnce(t *testing.T) {
	t.Parallel()
	scopes := SkillLoadRequest{}.AllowedScopes()
	if len(scopes) != 1 || scopes[0] != ScopeOnce {
		t.Fatalf("SkillLoadRequest.AllowedScopes() = %v, want [ScopeOnce]", scopes)
	}
}

// TestSkillArtifactCarriesBodyAndMetadata documents that SkillArtifact carries
// both the snapshot body (for execution) and the metadata/hash (for the request)
// — one artifact bound to the call, never a re-read.
func TestSkillArtifactCarriesBodyAndMetadata(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		art           SkillArtifact
		wantWorkspace bool
		wantRelPath   string
		wantSize      int64
		wantSHA256    string
		wantBody      string
	}{
		{
			name: "workspace artifact",
			art: SkillArtifact{
				Workspace: true,
				RelPath:   ".skills/refactor/SKILL.md",
				Size:      11,
				SHA256:    "00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff",
				Body:      "hello world",
			},
			wantWorkspace: true,
			wantRelPath:   ".skills/refactor/SKILL.md",
			wantSize:      11,
			wantSHA256:    "00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff",
			wantBody:      "hello world",
		},
		{
			name: "zero artifact",
			art:  SkillArtifact{},
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if tt.art.Workspace != tt.wantWorkspace {
				t.Errorf("Workspace = %v, want %v", tt.art.Workspace, tt.wantWorkspace)
			}
			if tt.art.RelPath != tt.wantRelPath {
				t.Errorf("RelPath = %q, want %q", tt.art.RelPath, tt.wantRelPath)
			}
			if tt.art.Size != tt.wantSize {
				t.Errorf("Size = %d, want %d", tt.art.Size, tt.wantSize)
			}
			if tt.art.SHA256 != tt.wantSHA256 {
				t.Errorf("SHA256 = %q, want %q", tt.art.SHA256, tt.wantSHA256)
			}
			if tt.art.Body != tt.wantBody {
				t.Errorf("Body = %q, want %q", tt.art.Body, tt.wantBody)
			}
		})
	}
}
