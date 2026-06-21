package researcher

import (
	"context"
	"crypto/tls"
	"encoding/xml"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/inventivepotter/urvi/internal/agent/loop"
	"github.com/inventivepotter/urvi/internal/agent/loop/identity"
	"github.com/inventivepotter/urvi/internal/tool"
)

// testHTTPClient builds the kind of client the swarm would pass: explicit
// timeout, TLS 1.2 floor. The leaf tests never make a real request.
func testHTTPClient() *http.Client {
	return &http.Client{
		Timeout:   5 * time.Second,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12}},
	}
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

// TestBuildToolSetAllowlist proves researcher wires EXACTLY its allowlist
// (Glob, Grep, ReadFile, WebSearch, Fetch, AskUser) — read-only investigation
// plus web research — and that the auto-approve set is exactly the four safe
// read/ask tools, with WebSearch and Fetch deliberately left at Ask (they reach
// the network). It also proves NO Subagent tool is wired: a leaf cannot spawn.
func TestBuildToolSetAllowlist(t *testing.T) {
	t.Parallel()

	ts := BuildTools("/tmp/workspace-root", testHTTPClient())
	if ts.Permission == nil {
		t.Fatal("BuildTools() ToolSet.Permission = nil, want non-nil PermissionChecker")
	}

	wantTools := []string{"AskUser", "Fetch", "Glob", "Grep", "ReadFile", "WebSearch"}
	got := toolNames(t, ts.Registry)
	if !equalStrings(got, wantTools) {
		t.Errorf("registry tool names = %v, want %v", got, wantTools)
	}
	if l := len(ts.Registry); l != len(wantTools) {
		t.Errorf("len(registry) = %d, want %d", l, len(wantTools))
	}

	// No leaf may spawn (least privilege): Subagent must be absent.
	for _, n := range got {
		if n == "Subagent" {
			t.Fatal("researcher wired a Subagent tool; a leaf must not be able to spawn")
		}
	}

	// The wired PermissionChecker's HardApprove set must be EXACTLY the policy
	// the agent declares: assert the policy that BuildTools fed the checker is
	// the auto-approve allowlist, then prove it behaviorally via Check below.
	assertAutoApproveSet(t, []string{"AskUser", "Glob", "Grep", "ReadFile"})

	// Behavioral proof through the wired PermissionChecker (the public gate):
	// against a REAL workspace root with a real in-root path, ReadFile/Glob/Grep/
	// AskUser auto-approve, while the networked WebSearch/Fetch stay Ask.
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "f.txt"), []byte("x"), 0o600); err != nil {
		t.Fatalf("seed file: %v", err)
	}
	tsReal := BuildTools(root, testHTTPClient())
	reg := byName(t, tsReal.Registry)
	cases := []struct {
		tool string
		args string
		want loop.Effect
	}{
		{tool: "ReadFile", args: `{"path":"f.txt"}`, want: loop.EffectAutoApprove},
		{tool: "Glob", args: `{"pattern":"*"}`, want: loop.EffectAutoApprove},
		{tool: "Grep", args: `{"pattern":"x"}`, want: loop.EffectAutoApprove},
		{tool: "AskUser", args: `{"question":"q"}`, want: loop.EffectAutoApprove},
		{tool: "WebSearch", args: `{"query":"q"}`, want: loop.EffectAsk},
		{tool: "Fetch", args: `{"url":"https://example.com"}`, want: loop.EffectAsk},
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
// exactly want (order-independent). This is the same contract coding asserts on
// its own autoApprovedTools slice.
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
	if Name != identity.AgentName("researcher") {
		t.Errorf("Name = %q, want %q", Name, "researcher")
	}
}

// TestDescriptionNonEmpty proves the catalog description is present.
func TestDescriptionNonEmpty(t *testing.T) {
	t.Parallel()
	if strings.TrimSpace(Description) == "" {
		t.Fatal("Description is empty")
	}
}

// TestRoleContent proves the role carries researcher's defining duties: web
// research with citations, and labeling fetched web content as untrusted DATA.
func TestRoleContent(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		want string
	}{
		{name: "cites sources", want: "cite"},
		{name: "does web research", want: "web"},
		{name: "fetched content is data", want: "DATA"},
		{name: "read-only", want: "read-only"},
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

// TestRoleIsWellFormedXML proves the role is a single <role name="researcher">.
func TestRoleIsWellFormedXML(t *testing.T) {
	t.Parallel()
	var probe struct {
		XMLName xml.Name `xml:"role"`
		RoleNm  string   `xml:"name,attr"`
	}
	if err := xml.Unmarshal([]byte(Role), &probe); err != nil {
		t.Fatalf("Role is not well-formed XML: %v", err)
	}
	if probe.RoleNm != "researcher" {
		t.Errorf("Role name attr = %q, want %q", probe.RoleNm, "researcher")
	}
}
