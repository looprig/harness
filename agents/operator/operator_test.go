package operator

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/ciram-co/looprig/pkg/identity"
	"github.com/ciram-co/looprig/pkg/loop"
	"github.com/ciram-co/looprig/pkg/tool"
)

// fakeSkill is a minimal tool.InvokableTool named "Skill" used to prove the leaf
// wiring: BuildTools adds the injected skill tool to the registry and lists
// "Skill" in HardApprove (so it auto-approves) ONLY when the tool is non-nil. The
// real tools.Skill is unit-tested in the tools package; here we only assert wiring.
type fakeSkill struct{}

func (fakeSkill) Info(context.Context) (*tool.ToolInfo, error) {
	return &tool.ToolInfo{Name: "Skill", Desc: "fake", Schema: json.RawMessage(`{"type":"object"}`)}, nil
}

func (fakeSkill) InvokableRun(context.Context, string) (*tool.ToolResult, error) {
	return tool.TextResult("fake"), nil
}

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

	ts := BuildTools("/tmp/workspace-root", nil)
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
	tsReal := BuildTools(root, nil)
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

// TestBuildToolSetWithSkill proves that when a non-nil Skill tool is injected,
// BuildTools adds it to the registry AND it auto-approves through the wired
// PermissionChecker (operator lists "Skill" in HardApprove only when the tool is
// present — a scoped, side-effect-free read, the same class as ReadFile). The
// base allowlist (the eight tools) is otherwise unchanged.
func TestBuildToolSetWithSkill(t *testing.T) {
	t.Parallel()

	ts := BuildTools("/tmp/workspace-root", fakeSkill{})
	if ts.Permission == nil {
		t.Fatal("BuildTools() ToolSet.Permission = nil, want non-nil PermissionChecker")
	}

	wantTools := []string{"AskUser", "Bash", "EditFile", "Glob", "Grep", "ReadFile", "Skill", "Todo", "WriteFile"}
	got := toolNames(t, ts.Registry)
	if !equalStrings(got, wantTools) {
		t.Errorf("registry tool names = %v, want %v (Skill added)", got, wantTools)
	}

	// The Skill tool auto-approves: it is classUnknown (no path arg) and reaches
	// AutoApprove only by being named in HardApprove — the same wiring as Subagent.
	reg := byName(t, ts.Registry)
	tl, ok := reg["Skill"]
	if !ok {
		t.Fatal("Skill tool not in registry")
	}
	if eff := ts.Permission.Check(context.Background(), tl, "Skill", `{"name":"code-style"}`); eff != loop.EffectAutoApprove {
		t.Errorf("Check(Skill) effect = %v, want %v (Skill must auto-approve)", eff, loop.EffectAutoApprove)
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
