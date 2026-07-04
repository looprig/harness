package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/looprig/core/content"
	"github.com/looprig/harness/pkg/tool"
)

// runReadFile is a small helper to invoke ReadFile and extract the single text
// block, failing the test on any structural surprise.
func runReadFile(t *testing.T, root string, guard *fakeReadGuard, args map[string]any) string {
	t.Helper()
	b, err := json.Marshal(args)
	if err != nil {
		t.Fatalf("marshal args: %v", err)
	}
	rf := NewReadFile(root, guard)
	res, err := rf.InvokableRun(context.Background(), string(b))
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

func TestReadFileInfo(t *testing.T) {
	t.Parallel()
	rf := NewReadFile(t.TempDir(), newFakeReadGuard(1<<20))
	info, err := rf.Info(context.Background())
	if err != nil {
		t.Fatalf("Info() error = %v", err)
	}
	if info.Name != "ReadFile" {
		t.Errorf("Info().Name = %q, want %q", info.Name, "ReadFile")
	}
	var schema map[string]json.RawMessage
	if err := json.Unmarshal(info.Schema, &schema); err != nil {
		t.Fatalf("Schema is not a JSON object: %v", err)
	}
}

func TestReadFile(t *testing.T) {
	t.Parallel()

	const fileBody = "line one\nline two\nline three\nline four\nline five\n"

	tests := []struct {
		name        string
		args        func(root string) map[string]any
		setup       func(t *testing.T, root string) // optional extra setup
		guard       func(root string) *fakeReadGuard
		wantContain []string
		wantAbsent  []string
	}{
		{
			name:        "whole file is line-numbered",
			args:        func(root string) map[string]any { return map[string]any{"path": "f.txt"} },
			guard:       func(root string) *fakeReadGuard { return newFakeReadGuard(1 << 20) },
			wantContain: []string{"1\tline one", "5\tline five"},
		},
		{
			name: "line range honored",
			args: func(root string) map[string]any {
				return map[string]any{"path": "f.txt", "start_line": 2, "end_line": 3}
			},
			guard:       func(root string) *fakeReadGuard { return newFakeReadGuard(1 << 20) },
			wantContain: []string{"2\tline two", "3\tline three"},
			wantAbsent:  []string{"line one", "line four", "line five"},
		},
		{
			name:        "denied path returns error and does not echo body",
			args:        func(root string) map[string]any { return map[string]any{"path": "f.txt"} },
			guard:       func(root string) *fakeReadGuard { return newFakeReadGuard(1<<20, resolvedJoin(t, root, "f.txt")) },
			wantContain: []string{"error", "denied"},
			wantAbsent:  []string{"line one", "line two"},
		},
		{
			name:        "oversize file is capped with truncation notice",
			args:        func(root string) map[string]any { return map[string]any{"path": "f.txt"} },
			guard:       func(root string) *fakeReadGuard { return newFakeReadGuard(12) }, // < file size
			wantContain: []string{"truncat"},
		},
		{
			name:        "escape attempt is rejected",
			args:        func(root string) map[string]any { return map[string]any{"path": "../outside.txt"} },
			guard:       func(root string) *fakeReadGuard { return newFakeReadGuard(1 << 20) },
			wantContain: []string{"error"},
			wantAbsent:  []string{"SECRET"},
		},
		{
			name:        "not found returns error",
			args:        func(root string) map[string]any { return map[string]any{"path": "missing.txt"} },
			guard:       func(root string) *fakeReadGuard { return newFakeReadGuard(1 << 20) },
			wantContain: []string{"error"},
		},
		{
			name:        "missing path arg returns error",
			args:        func(root string) map[string]any { return map[string]any{} },
			guard:       func(root string) *fakeReadGuard { return newFakeReadGuard(1 << 20) },
			wantContain: []string{"error"},
		},
		{
			name: "symlink final component is rejected",
			args: func(root string) map[string]any { return map[string]any{"path": "link.txt"} },
			setup: func(t *testing.T, root string) {
				target := filepath.Join(root, "f.txt")
				if err := os.Symlink(target, filepath.Join(root, "link.txt")); err != nil {
					t.Fatalf("symlink: %v", err)
				}
			},
			guard:       func(root string) *fakeReadGuard { return newFakeReadGuard(1 << 20) },
			wantContain: []string{"error"},
			wantAbsent:  []string{"line one"},
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			if err := os.WriteFile(filepath.Join(root, "f.txt"), []byte(fileBody), 0o600); err != nil {
				t.Fatalf("write fixture: %v", err)
			}
			// An outside-the-workspace secret to prove escape never reads it.
			outside := filepath.Join(filepath.Dir(root), "outside.txt")
			_ = os.WriteFile(outside, []byte("SECRET"), 0o600)
			t.Cleanup(func() { _ = os.Remove(outside) })

			if tt.setup != nil {
				tt.setup(t, root)
			}

			got := runReadFile(t, root, tt.guard(root), tt.args(root))
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

func TestReadFileAuditSummary(t *testing.T) {
	t.Parallel()
	rf := NewReadFile(t.TempDir(), newFakeReadGuard(1<<20))
	summary := rf.AuditSummary(`{"path":"sub/dir/file.go"}`)
	if !strings.Contains(summary, "sub/dir/file.go") {
		t.Errorf("AuditSummary = %q, want it to name the path", summary)
	}
}

// compile-time: ReadFile must NOT implement PermissionPrompter (it is
// AutoApprove). It MUST implement Auditable.
func TestReadFileCapabilities(t *testing.T) {
	t.Parallel()
	var it tool.InvokableTool = NewReadFile(t.TempDir(), newFakeReadGuard(1<<20))
	if _, ok := it.(tool.PermissionPrompter); ok {
		t.Error("ReadFile must not implement PermissionPrompter (AutoApprove)")
	}
	if _, ok := it.(tool.Auditable); !ok {
		t.Error("ReadFile must implement Auditable")
	}
}
