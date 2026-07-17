package tool

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestNewExternalRequest(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		toolName    string
		description string
		scopes      []ApprovalScope
		wantName    string
		wantDesc    string
		wantScopes  []ApprovalScope
		wantKind    ExternalRequestErrorKind
	}{
		{
			name:        "happy path",
			toolName:    "mcp__github__search_issues",
			description: "search_issues on github",
			scopes:      []ApprovalScope{ScopeOnce},
			wantName:    "mcp__github__search_issues",
			wantDesc:    "search_issues on github",
			wantScopes:  []ApprovalScope{ScopeOnce},
		},
		{
			// The whole point of the seam: UnknownRequest cannot express these.
			name:        "session and workspace scopes are reachable externally",
			toolName:    "mcp__github__search_issues",
			description: "search_issues on github",
			scopes:      []ApprovalScope{ScopeOnce, ScopeSession, ScopeWorkspace},
			wantName:    "mcp__github__search_issues",
			wantDesc:    "search_issues on github",
			wantScopes:  []ApprovalScope{ScopeOnce, ScopeSession, ScopeWorkspace},
		},
		{
			name:        "workspace scope alone",
			toolName:    "t",
			description: "d",
			scopes:      []ApprovalScope{ScopeWorkspace},
			wantName:    "t",
			wantDesc:    "d",
			wantScopes:  []ApprovalScope{ScopeWorkspace},
		},
		{
			name:        "name and description are trimmed",
			toolName:    "  spaced  ",
			description: "\n  padded \t",
			scopes:      []ApprovalScope{ScopeOnce},
			wantName:    "spaced",
			wantDesc:    "padded",
			wantScopes:  []ApprovalScope{ScopeOnce},
		},
		{
			name:        "empty description is allowed",
			toolName:    "t",
			description: "",
			scopes:      []ApprovalScope{ScopeOnce},
			wantName:    "t",
			wantDesc:    "",
			wantScopes:  []ApprovalScope{ScopeOnce},
		},
		{
			name:        "duplicate scopes are preserved verbatim",
			toolName:    "t",
			description: "d",
			scopes:      []ApprovalScope{ScopeOnce, ScopeOnce},
			wantName:    "t",
			wantDesc:    "d",
			wantScopes:  []ApprovalScope{ScopeOnce, ScopeOnce},
		},
		{
			name:        "empty tool name",
			toolName:    "",
			description: "d",
			scopes:      []ApprovalScope{ScopeOnce},
			wantKind:    ExternalToolNameEmpty,
		},
		{
			name:        "whitespace-only tool name",
			toolName:    "   \t\n ",
			description: "d",
			scopes:      []ApprovalScope{ScopeOnce},
			wantKind:    ExternalToolNameEmpty,
		},
		{
			name:        "empty scopes",
			toolName:    "t",
			description: "d",
			scopes:      []ApprovalScope{},
			wantKind:    ExternalScopesEmpty,
		},
		{
			name:        "nil scopes",
			toolName:    "t",
			description: "d",
			scopes:      nil,
			wantKind:    ExternalScopesEmpty,
		},
		{
			name:        "invalid scope",
			toolName:    "t",
			description: "d",
			scopes:      []ApprovalScope{ApprovalScope(200)},
			wantKind:    ExternalScopeInvalid,
		},
		{
			name:        "invalid scope among valid ones fails closed",
			toolName:    "t",
			description: "d",
			scopes:      []ApprovalScope{ScopeOnce, ApprovalScope(9)},
			wantKind:    ExternalScopeInvalid,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := NewExternalRequest(tt.toolName, tt.description, tt.scopes)
			if tt.wantKind != "" {
				var reqErr *ExternalRequestError
				if !errors.As(err, &reqErr) {
					t.Fatalf("NewExternalRequest() error = %v, want *ExternalRequestError", err)
				}
				if reqErr.Kind != tt.wantKind {
					t.Fatalf("kind = %q, want %q", reqErr.Kind, tt.wantKind)
				}
				if got != nil {
					t.Fatalf("NewExternalRequest() = %#v, want nil on error", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("NewExternalRequest() error = %v", err)
			}
			if got.ToolName() != tt.wantName {
				t.Errorf("ToolName() = %q, want %q", got.ToolName(), tt.wantName)
			}
			if got.Description() != tt.wantDesc {
				t.Errorf("Description() = %q, want %q", got.Description(), tt.wantDesc)
			}
			if !reflect.DeepEqual(got.AllowedScopes(), tt.wantScopes) {
				t.Errorf("AllowedScopes() = %v, want %v", got.AllowedScopes(), tt.wantScopes)
			}
		})
	}
}

// The returned value must satisfy the SEALED interface without unsealing it.
func TestExternalRequestSatisfiesSealedInterface(t *testing.T) {
	t.Parallel()

	got, err := NewExternalRequest("t", "d", []ApprovalScope{ScopeOnce})
	if err != nil {
		t.Fatalf("NewExternalRequest() error = %v", err)
	}
	var _ PermissionRequest = got
	if _, ok := got.(externalRequest); !ok {
		t.Fatalf("concrete type = %T, want externalRequest", got)
	}
	// The seal holds: the marker is unexported, so only this package can
	// implement PermissionRequest. NewExternalRequest is a constructor, not an
	// escape hatch — an external caller still cannot supply its own behavior.
	if _, ok := any(got).(interface{ permissionRequest() }); !ok {
		t.Fatal("externalRequest does not carry the sealing marker")
	}
}

func TestExternalRequestDescriptionIsBounded(t *testing.T) {
	t.Parallel()

	oversized := strings.Repeat("a", maxExternalDescriptionBytes*2)
	got, err := NewExternalRequest("t", oversized, []ApprovalScope{ScopeOnce})
	if err != nil {
		t.Fatalf("NewExternalRequest() error = %v", err)
	}
	if len(got.Description()) != maxExternalDescriptionBytes {
		t.Fatalf("len(Description()) = %d, want %d", len(got.Description()), maxExternalDescriptionBytes)
	}

	// A bounded request must still marshal under the codec cap, so a caller can
	// never construct something that fails closed later at restore.
	data, err := MarshalPermissionRequest(got)
	if err != nil {
		t.Fatalf("MarshalPermissionRequest() error = %v", err)
	}
	if len(data) > maxPermissionRequestBytes {
		t.Fatalf("marshaled len = %d, exceeds codec cap %d", len(data), maxPermissionRequestBytes)
	}
	if _, err := UnmarshalPermissionRequest(data); err != nil {
		t.Fatalf("UnmarshalPermissionRequest() error = %v", err)
	}
}

func TestExternalRequestBoundedDescriptionStaysValidUTF8(t *testing.T) {
	t.Parallel()

	// A multi-byte rune straddling the cap must not be split into a mangled
	// half-rune.
	oversized := strings.Repeat("é", maxExternalDescriptionBytes)
	got, err := NewExternalRequest("t", oversized, []ApprovalScope{ScopeOnce})
	if err != nil {
		t.Fatalf("NewExternalRequest() error = %v", err)
	}
	desc := got.Description()
	if len(desc) > maxExternalDescriptionBytes {
		t.Fatalf("len = %d, want <= %d", len(desc), maxExternalDescriptionBytes)
	}
	if !utf8ValidString(desc) {
		t.Fatal("bounded description is not valid UTF-8")
	}
	if strings.ContainsRune(desc, '�') {
		t.Fatal("bounded description contains a replacement character")
	}
}

func utf8ValidString(s string) bool {
	for _, r := range s {
		if r == '�' {
			return false
		}
	}
	return true
}

// The scope slice must be defensively copied on the way in and on the way out.
func TestExternalRequestScopesAreDefensivelyCopied(t *testing.T) {
	t.Parallel()

	scopes := []ApprovalScope{ScopeOnce, ScopeSession}
	got, err := NewExternalRequest("t", "d", scopes)
	if err != nil {
		t.Fatalf("NewExternalRequest() error = %v", err)
	}

	// Mutating the caller's slice must not reach the request.
	scopes[0] = ScopeWorkspace
	if got.AllowedScopes()[0] != ScopeOnce {
		t.Fatalf("AllowedScopes()[0] = %v, want ScopeOnce — input slice was aliased", got.AllowedScopes()[0])
	}

	// Mutating a returned slice must not reach the request either.
	returned := got.AllowedScopes()
	returned[0] = ScopeWorkspace
	if got.AllowedScopes()[0] != ScopeOnce {
		t.Fatalf("AllowedScopes()[0] = %v, want ScopeOnce — returned slice was aliased", got.AllowedScopes()[0])
	}
}

func TestExternalRequestCodecRoundTrip(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		scopes []ApprovalScope
	}{
		{name: "once", scopes: []ApprovalScope{ScopeOnce}},
		{name: "session", scopes: []ApprovalScope{ScopeSession}},
		{name: "workspace", scopes: []ApprovalScope{ScopeWorkspace}},
		{name: "all", scopes: []ApprovalScope{ScopeOnce, ScopeSession, ScopeWorkspace}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			original, err := NewExternalRequest("mcp__github__search_issues", "search_issues on github", tt.scopes)
			if err != nil {
				t.Fatalf("NewExternalRequest() error = %v", err)
			}
			data, err := MarshalPermissionRequest(original)
			if err != nil {
				t.Fatalf("MarshalPermissionRequest() error = %v", err)
			}
			got, err := UnmarshalPermissionRequest(data)
			if err != nil {
				t.Fatalf("UnmarshalPermissionRequest() error = %v", err)
			}
			if !reflect.DeepEqual(got, original) {
				t.Fatalf("round-trip = %#v, want %#v", got, original)
			}
			if got.ToolName() != original.ToolName() ||
				got.Description() != original.Description() ||
				!reflect.DeepEqual(got.AllowedScopes(), original.AllowedScopes()) {
				t.Fatal("round-tripped request does not report the original's fields")
			}
		})
	}
}

// Scopes persist as stable strings, never the ApprovalScope iota values.
func TestExternalRequestWireFormUsesStableScopeStrings(t *testing.T) {
	t.Parallel()

	req, err := NewExternalRequest("t", "d", []ApprovalScope{ScopeOnce, ScopeSession, ScopeWorkspace})
	if err != nil {
		t.Fatalf("NewExternalRequest() error = %v", err)
	}
	data, err := MarshalPermissionRequest(req)
	if err != nil {
		t.Fatalf("MarshalPermissionRequest() error = %v", err)
	}
	var wire struct {
		Type    string   `json:"type"`
		Tool    string   `json:"tool"`
		Summary string   `json:"summary"`
		Scopes  []string `json:"scopes"`
	}
	if err := json.Unmarshal(data, &wire); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}
	if wire.Type != string(typeExternal) {
		t.Errorf("type = %q, want %q", wire.Type, typeExternal)
	}
	if wire.Tool != "t" || wire.Summary != "d" {
		t.Errorf("tool/summary = %q/%q, want t/d", wire.Tool, wire.Summary)
	}
	want := []string{"once", "session", "workspace"}
	if !reflect.DeepEqual(wire.Scopes, want) {
		t.Errorf("scopes = %v, want %v", wire.Scopes, want)
	}
}

// Restore is untrusted: a hand-written record must not yield a request whose
// AllowedScopes differ from anything a constructor would permit.
func TestExternalRequestDecodeFailsClosed(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		data string
	}{
		{name: "unknown scope", data: `{"type":"external","tool":"t","summary":"d","scopes":["root"]}`},
		{name: "empty scopes", data: `{"type":"external","tool":"t","summary":"d","scopes":[]}`},
		{name: "missing scopes", data: `{"type":"external","tool":"t","summary":"d"}`},
		{name: "empty tool", data: `{"type":"external","tool":"","summary":"d","scopes":["once"]}`},
		{name: "whitespace tool", data: `{"type":"external","tool":"  ","summary":"d","scopes":["once"]}`},
		{name: "scopes as ints", data: `{"type":"external","tool":"t","scopes":[0,1]}`},
		{name: "scopes wrong type", data: `{"type":"external","tool":"t","scopes":"once"}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := UnmarshalPermissionRequest([]byte(tt.data))
			if err == nil {
				t.Fatalf("UnmarshalPermissionRequest() = %#v, want error", got)
			}
			if got != nil {
				t.Fatalf("UnmarshalPermissionRequest() = %#v, want nil on error", got)
			}
		})
	}
}

// An over-long description in a journal record is bounded on the way back in.
func TestExternalRequestDecodeBoundsDescription(t *testing.T) {
	t.Parallel()

	oversized := strings.Repeat("a", maxExternalDescriptionBytes+100)
	data := `{"type":"external","tool":"t","summary":"` + oversized + `","scopes":["once"]}`
	got, err := UnmarshalPermissionRequest([]byte(data))
	if err != nil {
		t.Fatalf("UnmarshalPermissionRequest() error = %v", err)
	}
	if len(got.Description()) != maxExternalDescriptionBytes {
		t.Fatalf("len(Description()) = %d, want %d", len(got.Description()), maxExternalDescriptionBytes)
	}
}
