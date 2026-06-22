package tool

import (
	"strings"
	"testing"
)

// Compile-time assertions: every concrete type implements the sealed
// PermissionRequest interface (and thus the unexported permissionRequest marker).
var (
	_ PermissionRequest = FileWriteRequest{}
	_ PermissionRequest = BashRequest{}
	_ PermissionRequest = FetchRequest{}
	_ PermissionRequest = WebSearchRequest{}
	_ PermissionRequest = UnknownRequest{}
)

func TestPermissionRequestContract(t *testing.T) {
	t.Parallel()

	allThree := []ApprovalScope{ScopeOnce, ScopeSession, ScopeWorkspace}
	onceOnly := []ApprovalScope{ScopeOnce}

	tests := []struct {
		name       string
		req        PermissionRequest
		wantTool   string
		descSubstr []string // substrings that MUST appear in Description()
		wantScopes []ApprovalScope
	}{
		{
			name:       "file write happy path",
			req:        FileWriteRequest{Path: "/repo/main.go"},
			wantTool:   "WriteFile",
			descSubstr: []string{"/repo/main.go"},
			wantScopes: allThree,
		},
		{
			name:       "file write empty path",
			req:        FileWriteRequest{},
			wantTool:   "WriteFile",
			descSubstr: nil,
			wantScopes: allThree,
		},
		{
			name:       "bash happy path",
			req:        BashRequest{Command: "ls -la"},
			wantTool:   "Bash",
			descSubstr: []string{"ls -la"},
			wantScopes: allThree,
		},
		{
			name:       "bash empty command",
			req:        BashRequest{},
			wantTool:   "Bash",
			descSubstr: nil,
			wantScopes: allThree,
		},
		{
			name:       "fetch happy path",
			req:        FetchRequest{Method: "GET", URL: "https://example.com/x"},
			wantTool:   "Fetch",
			descSubstr: []string{"GET", "https://example.com/x"},
			wantScopes: allThree,
		},
		{
			name:       "fetch empty fields",
			req:        FetchRequest{},
			wantTool:   "Fetch",
			descSubstr: nil,
			wantScopes: allThree,
		},
		{
			name:       "web search happy path",
			req:        WebSearchRequest{Query: "golang sealed interface"},
			wantTool:   "WebSearch",
			descSubstr: []string{"golang sealed interface"},
			wantScopes: allThree,
		},
		{
			name:       "web search empty query",
			req:        WebSearchRequest{},
			wantTool:   "WebSearch",
			descSubstr: nil,
			wantScopes: allThree,
		},
		{
			name:       "unknown uses summary",
			req:        UnknownRequest{Tool: "MysteryTool", Summary: "does something redacted"},
			wantTool:   "MysteryTool",
			descSubstr: []string{"does something redacted"},
			wantScopes: onceOnly,
		},
		{
			name:       "unknown empty",
			req:        UnknownRequest{},
			wantTool:   "",
			descSubstr: nil,
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

// TestUnknownRequestNeverExposesRawArgs proves UnknownRequest.Description()
// returns exactly its Summary and nothing else — it must never leak raw args.
func TestUnknownRequestNeverExposesRawArgs(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		summary string
	}{
		{name: "plain summary", summary: "fetched a URL"},
		{name: "empty summary", summary: ""},
		{name: "summary with spaces", summary: "ran tool with redacted args"},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			req := UnknownRequest{Tool: "X", Summary: tt.summary}
			if got := req.Description(); got != tt.summary {
				t.Errorf("UnknownRequest.Description() = %q, want exactly Summary %q", got, tt.summary)
			}
		})
	}
}

// TestUnknownRequestOnlyOnceScope pins the fail-secure choice: an unknown call
// has no Match to persist, so it offers only ScopeOnce.
func TestUnknownRequestOnlyOnceScope(t *testing.T) {
	t.Parallel()
	scopes := UnknownRequest{}.AllowedScopes()
	if len(scopes) != 1 || scopes[0] != ScopeOnce {
		t.Fatalf("UnknownRequest.AllowedScopes() = %v, want [ScopeOnce]", scopes)
	}
}
