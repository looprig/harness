package operator

import (
	"context"
	"encoding/xml"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/inventivepotter/urvi/internal/agent/loop"
	"github.com/inventivepotter/urvi/internal/agent/loop/identity"
	"github.com/inventivepotter/urvi/internal/tool"
)

// toolNames collects the sorted Info().Name of every tool in the registry.
func toolNames(t *testing.T, reg []tool.InvokableTool) []string {
	t.Helper()
	names := make([]string, 0, len(reg))
	for _, tl := range reg {
		info, err := tl.Info(context.Background())
		if err != nil {
			t.Fatalf("Info() error = %v", err)
		}
		names = append(names, info.Name)
	}
	sort.Strings(names)
	return names
}

// byName indexes a registry by Info().Name for per-tool Check assertions.
func byName(t *testing.T, reg []tool.InvokableTool) map[string]tool.InvokableTool {
	t.Helper()
	m := make(map[string]tool.InvokableTool, len(reg))
	for _, tl := range reg {
		info, err := tl.Info(context.Background())
		if err != nil {
			t.Fatalf("Info() error = %v", err)
		}
		m[info.Name] = tl
	}
	return m
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestBuildToolSetAllowlist proves operator wires EXACTLY its allowlist
// (ReadFile, Glob, Grep, WriteFile, EditFile, Bash, Todo, AskUser) — the
// write+exec implementer — and that the auto-approve set is exactly the five
// side-effect-free read/search/plan/ask tools, so the three mutating tools
// (WriteFile, EditFile, Bash) stay human-gated. It also proves NO Subagent tool
// is wired (a leaf cannot spawn) and NO network tool (Fetch/WebSearch) is present.
func TestBuildToolSetAllowlist(t *testing.T) {
	t.Parallel()

	ts := BuildTools("/tmp/workspace-root")
	if ts.Permission == nil {
		t.Fatal("BuildTools() ToolSet.Permission = nil, want non-nil PermissionChecker")
	}

	wantTools := []string{"AskUser", "Bash", "EditFile", "Glob", "Grep", "ReadFile", "Todo", "WriteFile"}
	got := toolNames(t, ts.Registry)
	if !equalStrings(got, wantTools) {
		t.Errorf("registry tool names = %v, want %v", got, wantTools)
	}
	if l := len(ts.Registry); l != len(wantTools) {
		t.Errorf("len(registry) = %d, want %d", l, len(wantTools))
	}

	// Operator must not spawn and must not reach the network.
	for _, n := range got {
		switch n {
		case "Subagent":
			t.Fatal("operator wired a Subagent tool; a leaf must not be able to spawn")
		case "Fetch", "WebSearch":
			t.Errorf("operator wired %q; it has no network access", n)
		}
	}

	// Auto-approve allowlist is exactly the five side-effect-free tools.
	assertAutoApproveSet(t, []string{"AskUser", "Glob", "Grep", "ReadFile", "Todo"})

	// Behavioral proof through the wired PermissionChecker against a REAL root:
	// the read/todo/ask tools auto-approve; the mutating tools stay Ask.
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "f.txt"), []byte("x"), 0o600); err != nil {
		t.Fatalf("seed file: %v", err)
	}
	tsReal := BuildTools(root)
	reg := byName(t, tsReal.Registry)
	cases := []struct {
		tool string
		args string
		want loop.Effect
	}{
		{tool: "ReadFile", args: `{"path":"f.txt"}`, want: loop.EffectAutoApprove},
		{tool: "Glob", args: `{"pattern":"*"}`, want: loop.EffectAutoApprove},
		{tool: "Grep", args: `{"pattern":"x"}`, want: loop.EffectAutoApprove},
		{tool: "Todo", args: `{"todos":[]}`, want: loop.EffectAutoApprove},
		{tool: "AskUser", args: `{"question":"q"}`, want: loop.EffectAutoApprove},
		{tool: "WriteFile", args: `{"path":"g.txt","content":"y"}`, want: loop.EffectAsk},
		{tool: "EditFile", args: `{"path":"f.txt","old":"x","new":"z"}`, want: loop.EffectAsk},
		{tool: "Bash", args: `{"command":"go test ./..."}`, want: loop.EffectAsk},
	}
	for _, tc := range cases {
		tl, ok := reg[tc.tool]
		if !ok {
			t.Fatalf("tool %q not in registry", tc.tool)
		}
		if eff := tsReal.Permission.Check(context.Background(), tl, tc.tool, tc.args); eff != tc.want {
			t.Errorf("Check(%q) effect = %v, want %v", tc.tool, eff, tc.want)
		}
	}
}

// assertAutoApproveSet proves the package-level hard-approve allowlist is
// exactly want (order-independent).
func assertAutoApproveSet(t *testing.T, want []string) {
	t.Helper()
	got := append([]string(nil), autoApprovedTools...)
	sort.Strings(got)
	w := append([]string(nil), want...)
	sort.Strings(w)
	if !equalStrings(got, w) {
		t.Errorf("autoApprovedTools = %v, want %v", got, w)
	}
}

// TestName pins the attribution name.
func TestName(t *testing.T) {
	t.Parallel()
	if Name != identity.AgentName("operator") {
		t.Errorf("Name = %q, want %q", Name, "operator")
	}
}

// TestDescriptionNonEmpty proves the catalog description is present.
func TestDescriptionNonEmpty(t *testing.T) {
	t.Parallel()
	if strings.TrimSpace(Description) == "" {
		t.Fatal("Description is empty")
	}
}

// TestRoleContent proves the role carries operator's craft: fix at the root
// cause, read before editing, prefer editing to creating, state the plan before a
// gated mutation, validate the narrowest test first, and don't fix unrelated
// failures.
func TestRoleContent(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		want string
	}{
		{name: "root cause", want: "root cause"},
		{name: "read before editing", want: "read it first"},
		{name: "prefer editing to creating", want: "prefer editing"},
		{name: "states the plan", want: "plan"},
		{name: "approval-gated mutation", want: "approval"},
		{name: "narrowest test first", want: "narrowest test"},
		{name: "does not fix unrelated", want: "unrelated"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if !strings.Contains(strings.ToLower(Role), strings.ToLower(tt.want)) {
				t.Errorf("Role is missing %q", tt.want)
			}
		})
	}
}

// TestRoleIsWellFormedXML proves the role is a single <role name="operator">.
func TestRoleIsWellFormedXML(t *testing.T) {
	t.Parallel()
	var probe struct {
		XMLName xml.Name `xml:"role"`
		RoleNm  string   `xml:"name,attr"`
	}
	if err := xml.Unmarshal([]byte(Role), &probe); err != nil {
		t.Fatalf("Role is not well-formed XML: %v", err)
	}
	if probe.RoleNm != "operator" {
		t.Errorf("Role name attr = %q, want %q", probe.RoleNm, "operator")
	}
}
