package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/inventivepotter/urvi/internal/content"
	"github.com/inventivepotter/urvi/internal/tool"
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
