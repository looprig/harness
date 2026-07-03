package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/looprig/harness/pkg/content"
	"github.com/looprig/harness/pkg/tool"
)

func runGlob(t *testing.T, root string, guard *fakeReadGuard, args map[string]any) string {
	t.Helper()
	b, err := json.Marshal(args)
	if err != nil {
		t.Fatalf("marshal args: %v", err)
	}
	g := NewGlob(root, guard)
	res, err := g.InvokableRun(context.Background(), string(b))
	if err != nil {
		t.Fatalf("InvokableRun returned a Go error %v; read tools return tool-result strings", err)
	}
	if res == nil || len(res.Content) != 1 {
		t.Fatalf("result = %v, want exactly 1 block", res)
	}
	tb, ok := res.Content[0].(*content.TextBlock)
	if !ok {
		t.Fatalf("block type = %T, want *content.TextBlock", res.Content[0])
	}
	return tb.Text
}

func TestGlobInfo(t *testing.T) {
	t.Parallel()
	g := NewGlob(t.TempDir(), newFakeReadGuard(1<<20))
	info, err := g.Info(context.Background())
	if err != nil {
		t.Fatalf("Info() error = %v", err)
	}
	if info.Name != "Glob" {
		t.Errorf("Info().Name = %q, want %q", info.Name, "Glob")
	}
	var schema map[string]json.RawMessage
	if err := json.Unmarshal(info.Schema, &schema); err != nil {
		t.Fatalf("Schema is not a JSON object: %v", err)
	}
}

func TestGlob(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		setup       func(t *testing.T, root string)
		args        func(root string) map[string]any
		guard       func(root string) *fakeReadGuard
		wantContain []string
		wantAbsent  []string
	}{
		{
			name: "doublestar matches nested files",
			setup: func(t *testing.T, root string) {
				mustWrite(t, filepath.Join(root, "a", "b", "c.go"), "x")
				mustWrite(t, filepath.Join(root, "top.go"), "x")
				mustWrite(t, filepath.Join(root, "a", "note.txt"), "x")
			},
			args:        func(root string) map[string]any { return map[string]any{"pattern": "**/*.go"} },
			guard:       func(root string) *fakeReadGuard { return newFakeReadGuard(1 << 20) },
			wantContain: []string{"a/b/c.go", "top.go"},
			wantAbsent:  []string{"note.txt"},
		},
		{
			name: "single-segment star does not cross slash",
			setup: func(t *testing.T, root string) {
				mustWrite(t, filepath.Join(root, "x.go"), "x")
				mustWrite(t, filepath.Join(root, "sub", "y.go"), "x")
			},
			args:        func(root string) map[string]any { return map[string]any{"pattern": "*.go"} },
			guard:       func(root string) *fakeReadGuard { return newFakeReadGuard(1 << 20) },
			wantContain: []string{"x.go"},
			wantAbsent:  []string{"sub/y.go"},
		},
		{
			name: "denied entry is excluded from results",
			setup: func(t *testing.T, root string) {
				mustWrite(t, filepath.Join(root, ".env"), "SECRET=1")
				mustWrite(t, filepath.Join(root, "app.go"), "x")
			},
			args: func(root string) map[string]any { return map[string]any{"pattern": "**"} },
			guard: func(root string) *fakeReadGuard {
				return newFakeReadGuard(1<<20, resolvedJoin(t, root, ".env"))
			},
			wantContain: []string{"app.go"},
			wantAbsent:  []string{".env"},
		},
		{
			name: "scoped root narrows the search",
			setup: func(t *testing.T, root string) {
				mustWrite(t, filepath.Join(root, "src", "a.go"), "x")
				mustWrite(t, filepath.Join(root, "other", "b.go"), "x")
			},
			args:        func(root string) map[string]any { return map[string]any{"pattern": "**/*.go", "root": "src"} },
			guard:       func(root string) *fakeReadGuard { return newFakeReadGuard(1 << 20) },
			wantContain: []string{"src/a.go"},
			wantAbsent:  []string{"other/b.go"},
		},
		{
			name: "root escape is rejected",
			setup: func(t *testing.T, root string) {
				mustWrite(t, filepath.Join(root, "a.go"), "x")
			},
			args:        func(root string) map[string]any { return map[string]any{"pattern": "**", "root": "../"} },
			guard:       func(root string) *fakeReadGuard { return newFakeReadGuard(1 << 20) },
			wantContain: []string{"error"},
		},
		{
			name: "git and noise dirs are pruned by default",
			setup: func(t *testing.T, root string) {
				// VCS internals and heavy generated trees that must never reach the model.
				mustWrite(t, filepath.Join(root, ".git", "objects", "ab", "cdef"), "x")
				mustWrite(t, filepath.Join(root, ".git", "HEAD"), "ref: x")
				mustWrite(t, filepath.Join(root, "node_modules", "left-pad", "index.js"), "x")
				mustWrite(t, filepath.Join(root, "vendor", "dep", "v.go"), "x")
				// A real source file that SHOULD be listed.
				mustWrite(t, filepath.Join(root, "main.go"), "x")
			},
			args:        func(root string) map[string]any { return map[string]any{"pattern": "**"} },
			guard:       func(root string) *fakeReadGuard { return newFakeReadGuard(1 << 20) },
			wantContain: []string{"main.go"},
			wantAbsent:  []string{".git", "node_modules", "vendor"},
		},
		{
			name: "explicit noise-dir root is still searched",
			setup: func(t *testing.T, root string) {
				mustWrite(t, filepath.Join(root, ".git", "HEAD"), "ref: x")
				mustWrite(t, filepath.Join(root, ".git", "config"), "x")
			},
			args:        func(root string) map[string]any { return map[string]any{"pattern": "**", "root": ".git"} },
			guard:       func(root string) *fakeReadGuard { return newFakeReadGuard(1 << 20) },
			wantContain: []string{".git/HEAD", ".git/config"},
		},
		{
			name: "exactly the cap is not truncated",
			setup: func(t *testing.T, root string) {
				for i := 0; i < maxGlobResults; i++ {
					mustWrite(t, filepath.Join(root, fmt.Sprintf("f%04d.go", i)), "x")
				}
			},
			args:        func(root string) map[string]any { return map[string]any{"pattern": "*.go"} },
			guard:       func(root string) *fakeReadGuard { return newFakeReadGuard(1 << 20) },
			wantContain: []string{"f0000.go"},
			wantAbsent:  []string{"truncat", "more matches"},
		},
		{
			name: "one over the cap is truncated with a refine notice",
			setup: func(t *testing.T, root string) {
				for i := 0; i < maxGlobResults+1; i++ {
					mustWrite(t, filepath.Join(root, fmt.Sprintf("f%04d.go", i)), "x")
				}
			},
			args:        func(root string) map[string]any { return map[string]any{"pattern": "*.go"} },
			guard:       func(root string) *fakeReadGuard { return newFakeReadGuard(1 << 20) },
			wantContain: []string{"truncat", "refine"},
		},
		{
			name: "empty dir reports no matches",
			setup: func(t *testing.T, root string) {
				if err := os.MkdirAll(filepath.Join(root, "empty"), 0o700); err != nil {
					t.Fatalf("mkdir: %v", err)
				}
			},
			args:        func(root string) map[string]any { return map[string]any{"pattern": "**/*.go"} },
			guard:       func(root string) *fakeReadGuard { return newFakeReadGuard(1 << 20) },
			wantContain: []string{"no matches"},
		},
		{
			name: "over the cap is truncated with a notice",
			setup: func(t *testing.T, root string) {
				for i := 0; i < maxGlobResults+25; i++ {
					mustWrite(t, filepath.Join(root, fmt.Sprintf("f%04d.go", i)), "x")
				}
			},
			args:        func(root string) map[string]any { return map[string]any{"pattern": "*.go"} },
			guard:       func(root string) *fakeReadGuard { return newFakeReadGuard(1 << 20) },
			wantContain: []string{"truncat"},
		},
		{
			name: "no matches reports none",
			setup: func(t *testing.T, root string) {
				mustWrite(t, filepath.Join(root, "a.txt"), "x")
			},
			args:        func(root string) map[string]any { return map[string]any{"pattern": "*.go"} },
			guard:       func(root string) *fakeReadGuard { return newFakeReadGuard(1 << 20) },
			wantContain: []string{"no"},
		},
		{
			name:        "missing pattern is an error",
			setup:       func(t *testing.T, root string) {},
			args:        func(root string) map[string]any { return map[string]any{} },
			guard:       func(root string) *fakeReadGuard { return newFakeReadGuard(1 << 20) },
			wantContain: []string{"error"},
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			tt.setup(t, root)
			got := runGlob(t, root, tt.guard(root), tt.args(root))
			for _, want := range tt.wantContain {
				if !strings.Contains(got, want) {
					t.Errorf("output missing %q\n---\n%s", want, got)
				}
			}
			for _, absent := range tt.wantAbsent {
				if strings.Contains(got, absent) {
					t.Errorf("output should not contain %q\n---\n%s", absent, got)
				}
			}
		})
	}
}

// TestGlobWalkTimeout asserts the WalkDir walk honors a cancelled context: an
// already-expired ctx aborts the traversal and the tool returns the timeout
// tool-result rather than walking a (potentially huge) tree (FIX 1).
func TestGlobWalkTimeout(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "a.go"), "x")

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // expire before the walk begins.

	g := NewGlob(root, newFakeReadGuard(1<<20))
	res, err := g.InvokableRun(ctx, `{"pattern":"**"}`)
	if err != nil {
		t.Fatalf("InvokableRun returned a Go error %v; read tools return tool-result strings", err)
	}
	tb, ok := res.Content[0].(*content.TextBlock)
	if !ok {
		t.Fatalf("block type = %T, want *content.TextBlock", res.Content[0])
	}
	if !strings.Contains(tb.Text, "timed out") {
		t.Errorf("output = %q, want it to report the timeout", tb.Text)
	}
}

func TestGlobAuditSummary(t *testing.T) {
	t.Parallel()
	g := NewGlob(t.TempDir(), newFakeReadGuard(1<<20))
	s := g.AuditSummary(`{"pattern":"**/*.go","root":"src"}`)
	if !strings.Contains(s, "**/*.go") {
		t.Errorf("AuditSummary = %q, want it to name the pattern", s)
	}
}

func TestGlobCapabilities(t *testing.T) {
	t.Parallel()
	var it tool.InvokableTool = NewGlob(t.TempDir(), newFakeReadGuard(1<<20))
	if _, ok := it.(tool.PermissionPrompter); ok {
		t.Error("Glob must not implement PermissionPrompter (AutoApprove)")
	}
	if _, ok := it.(tool.Auditable); !ok {
		t.Error("Glob must implement Auditable")
	}
}

// TestGlobExcludesSymlinkEscapingWorkspace proves that a symlink ENTRY inside the
// workspace whose target resolves OUTSIDE the root is excluded from Glob results
// rather than emitted as a "../"-climbing path. WalkDir visits (but does not
// descend into) such a symlink; denyFilteredRel must reject the resolved escape so
// the outside target's location never leaks. This is the unit-level companion to
// the symlink case in fs_integration_test.go (covered in the DEFAULT test run).
func TestGlobExcludesSymlinkEscapingWorkspace(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	outside := t.TempDir()
	// A file in the outside tree, reachable only by following the planted symlink.
	if err := os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("x"), 0o600); err != nil {
		t.Fatalf("write outside file: %v", err)
	}
	// A legitimate in-workspace file (so the listing is non-empty).
	if err := os.WriteFile(filepath.Join(root, "keep.txt"), []byte("y"), 0o600); err != nil {
		t.Fatalf("write keep.txt: %v", err)
	}
	// Symlinks inside the workspace pointing OUT: one to the dir, one to the file.
	if err := os.Symlink(outside, filepath.Join(root, "link-dir")); err != nil {
		t.Fatalf("symlink link-dir: %v", err)
	}
	if err := os.Symlink(filepath.Join(outside, "secret.txt"), filepath.Join(root, "link-file")); err != nil {
		t.Fatalf("symlink link-file: %v", err)
	}

	got := runGlob(t, root, newFakeReadGuard(1<<20), map[string]any{"pattern": "**"})

	if strings.Contains(got, "..") {
		t.Errorf("Glob emitted a workspace-escaping path; output:\n%s", got)
	}
	if strings.Contains(got, "secret.txt") {
		t.Errorf("Glob listed the outside target through a symlink; output:\n%s", got)
	}
	if !strings.Contains(got, "keep.txt") {
		t.Errorf("Glob did not list the legitimate in-workspace file; output:\n%s", got)
	}
}

// mustWrite creates parent dirs and writes a file, failing the test on error.
func mustWrite(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
