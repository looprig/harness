package explorer

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

// TestBuildToolSetAllowlist proves explorer wires EXACTLY its allowlist
// (Glob, Grep, ReadFile, AskUser) — read-only codebase mapping, no network —
// and that the auto-approve set is ALL FOUR (explorer never gates: it only ever
// reads). It also proves NO Subagent tool is wired: a leaf cannot spawn, and no
// network tool (WebSearch/Fetch) is present.
func TestBuildToolSetAllowlist(t *testing.T) {
	t.Parallel()

	ts := BuildTools("/tmp/workspace-root")
	if ts.Permission == nil {
		t.Fatal("BuildTools() ToolSet.Permission = nil, want non-nil PermissionChecker")
	}

	wantTools := []string{"AskUser", "Glob", "Grep", "ReadFile"}
	got := toolNames(t, ts.Registry)
	if !equalStrings(got, wantTools) {
		t.Errorf("registry tool names = %v, want %v", got, wantTools)
	}
	if l := len(ts.Registry); l != len(wantTools) {
		t.Errorf("len(registry) = %d, want %d", l, len(wantTools))
	}

	// A read-only leaf must wire neither a spawn tool nor any network tool.
	for _, n := range got {
		switch n {
		case "Subagent":
			t.Fatal("explorer wired a Subagent tool; a leaf must not be able to spawn")
		case "WebSearch", "Fetch", "Bash", "WriteFile", "EditFile":
			t.Errorf("explorer wired %q; it must be read-only with no network", n)
		}
	}

	// Auto-approve allowlist is ALL FOUR — explorer never prompts.
	assertAutoApproveSet(t, wantTools)

	// Behavioral proof through the wired PermissionChecker against a REAL root:
	// every tool auto-approves.
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "f.txt"), []byte("x"), 0o600); err != nil {
		t.Fatalf("seed file: %v", err)
	}
	tsReal := BuildTools(root)
	reg := byName(t, tsReal.Registry)
	cases := []struct {
		tool string
		args string
	}{
		{tool: "ReadFile", args: `{"path":"f.txt"}`},
		{tool: "Glob", args: `{"pattern":"*"}`},
		{tool: "Grep", args: `{"pattern":"x"}`},
		{tool: "AskUser", args: `{"question":"q"}`},
	}
	for _, tc := range cases {
		tl, ok := reg[tc.tool]
		if !ok {
			t.Fatalf("tool %q not in registry", tc.tool)
		}
		if eff := tsReal.Permission.Check(context.Background(), tl, tc.tool, tc.args); eff != loop.EffectAutoApprove {
			t.Errorf("Check(%q) effect = %v, want EffectAutoApprove", tc.tool, eff)
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
	if Name != identity.AgentName("explorer") {
		t.Errorf("Name = %q, want %q", Name, "explorer")
	}
}

// TestDescriptionNonEmpty proves the catalog description is present.
func TestDescriptionNonEmpty(t *testing.T) {
	t.Parallel()
	if strings.TrimSpace(Description) == "" {
		t.Fatal("Description is empty")
	}
}

// TestRoleContent proves the role carries explorer's defining duties: read-only
// codebase mapping/navigation with no network.
func TestRoleContent(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		want string
	}{
		{name: "maps the codebase", want: "map"},
		{name: "read-only", want: "read-only"},
		{name: "no network", want: "network"},
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

// TestRoleIsWellFormedXML proves the role is a single <role name="explorer">.
func TestRoleIsWellFormedXML(t *testing.T) {
	t.Parallel()
	var probe struct {
		XMLName xml.Name `xml:"role"`
		RoleNm  string   `xml:"name,attr"`
	}
	if err := xml.Unmarshal([]byte(Role), &probe); err != nil {
		t.Fatalf("Role is not well-formed XML: %v", err)
	}
	if probe.RoleNm != "explorer" {
		t.Errorf("Role name attr = %q, want %q", probe.RoleNm, "explorer")
	}
}
