package tools

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/inventivepotter/urvi/internal/content"
	"github.com/inventivepotter/urvi/internal/tool"
)

func runGrep(t *testing.T, root string, guard *fakeReadGuard, args map[string]any) string {
	t.Helper()
	b, err := json.Marshal(args)
	if err != nil {
		t.Fatalf("marshal args: %v", err)
	}
	// Force the deterministic WalkDir fallback so tests do not depend on whether
	// ripgrep is installed on the host.
	g := newGrepWithBackend(root, guard, false)
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

func TestGrepInfo(t *testing.T) {
	t.Parallel()
	g := NewGrep(t.TempDir(), newFakeReadGuard(1<<20))
	info, err := g.Info(context.Background())
	if err != nil {
		t.Fatalf("Info() error = %v", err)
	}
	if info.Name != "Grep" {
		t.Errorf("Info().Name = %q, want %q", info.Name, "Grep")
	}
	var schema map[string]json.RawMessage
	if err := json.Unmarshal(info.Schema, &schema); err != nil {
		t.Fatalf("Schema is not a JSON object: %v", err)
	}
}

func TestGrep(t *testing.T) {
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
			name: "pattern match reports file and line",
			setup: func(t *testing.T, root string) {
				mustWrite(t, filepath.Join(root, "a.go"), "package main\nfunc target() {}\n")
			},
			args:        func(root string) map[string]any { return map[string]any{"pattern": "target"} },
			guard:       func(root string) *fakeReadGuard { return newFakeReadGuard(1 << 20) },
			wantContain: []string{"a.go", "2", "func target"},
		},
		{
			name: "dash-leading pattern is treated as a value not a flag",
			setup: func(t *testing.T, root string) {
				mustWrite(t, filepath.Join(root, "a.go"), "x := -1\ny := 2\n")
			},
			args:        func(root string) map[string]any { return map[string]any{"pattern": "-1"} },
			guard:       func(root string) *fakeReadGuard { return newFakeReadGuard(1 << 20) },
			wantContain: []string{"a.go", "x := -1"},
			wantAbsent:  []string{"error"},
		},
		{
			name: "denied file is skipped",
			setup: func(t *testing.T, root string) {
				mustWrite(t, filepath.Join(root, ".env"), "TOKEN=needle\n")
				mustWrite(t, filepath.Join(root, "app.go"), "// needle here\n")
			},
			args: func(root string) map[string]any { return map[string]any{"pattern": "needle"} },
			guard: func(root string) *fakeReadGuard {
				return newFakeReadGuard(1<<20, resolvedJoin(t, root, ".env"))
			},
			wantContain: []string{"app.go"},
			wantAbsent:  []string{".env", "TOKEN"},
		},
		{
			name: "recursive search descends into subdirs",
			setup: func(t *testing.T, root string) {
				mustWrite(t, filepath.Join(root, "deep", "nested", "z.go"), "// findme\n")
			},
			args:        func(root string) map[string]any { return map[string]any{"pattern": "findme", "recursive": true} },
			guard:       func(root string) *fakeReadGuard { return newFakeReadGuard(1 << 20) },
			wantContain: []string{"deep/nested/z.go", "findme"},
		},
		{
			name: "noise dir is skipped",
			setup: func(t *testing.T, root string) {
				mustWrite(t, filepath.Join(root, ".git", "config.go"), "// findme\n")
				mustWrite(t, filepath.Join(root, "real.go"), "// findme\n")
			},
			args:        func(root string) map[string]any { return map[string]any{"pattern": "findme"} },
			guard:       func(root string) *fakeReadGuard { return newFakeReadGuard(1 << 20) },
			wantContain: []string{"real.go"},
			wantAbsent:  []string{".git"},
		},
		{
			name: "ignore_case matches across case",
			setup: func(t *testing.T, root string) {
				mustWrite(t, filepath.Join(root, "a.go"), "Hello World\n")
			},
			args:        func(root string) map[string]any { return map[string]any{"pattern": "hello", "ignore_case": true} },
			guard:       func(root string) *fakeReadGuard { return newFakeReadGuard(1 << 20) },
			wantContain: []string{"a.go", "Hello World"},
		},
		{
			name: "no matches reports none",
			setup: func(t *testing.T, root string) {
				mustWrite(t, filepath.Join(root, "a.go"), "nothing\n")
			},
			args:        func(root string) map[string]any { return map[string]any{"pattern": "absent"} },
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
		{
			name:        "invalid regex is an error",
			setup:       func(t *testing.T, root string) {},
			args:        func(root string) map[string]any { return map[string]any{"pattern": "("} },
			guard:       func(root string) *fakeReadGuard { return newFakeReadGuard(1 << 20) },
			wantContain: []string{"error"},
		},
		{
			name: "path escape is rejected",
			setup: func(t *testing.T, root string) {
				mustWrite(t, filepath.Join(root, "a.go"), "x\n")
			},
			args:        func(root string) map[string]any { return map[string]any{"pattern": "x", "path": "../"} },
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
			got := runGrep(t, root, tt.guard(root), tt.args(root))
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

// TestGrepRgArgList asserts the ripgrep argument vector puts the pattern AND path
// AFTER --regexp / -- so a "-"-leading pattern or path can never be parsed as a
// flag (flag-injection defense). It checks the pure arg-builder, no exec.
func TestGrepRgArgList(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		opts    grepOptions
		pattern string
		path    string
		check   func(t *testing.T, args []string)
	}{
		{
			name:    "pattern follows --regexp",
			pattern: "-x",
			path:    "/ws",
			check: func(t *testing.T, args []string) {
				assertAdjacent(t, args, "--regexp", "-x")
			},
		},
		{
			name:    "path follows the -- terminator",
			pattern: "foo",
			path:    "-weird",
			check: func(t *testing.T, args []string) {
				assertAdjacent(t, args, "--", "-weird")
			},
		},
		{
			name:    "ignore_case adds the flag",
			opts:    grepOptions{ignoreCase: true},
			pattern: "foo",
			path:    "/ws",
			check: func(t *testing.T, args []string) {
				if !containsArg(args, "--ignore-case") {
					t.Errorf("args %v missing --ignore-case", args)
				}
			},
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			args := buildRgArgs(tt.pattern, tt.path, tt.opts, nil)
			tt.check(t, args)
		})
	}
}

func TestGrepAuditSummary(t *testing.T) {
	t.Parallel()
	g := NewGrep(t.TempDir(), newFakeReadGuard(1<<20))
	s := g.AuditSummary(`{"pattern":"needle","path":"src"}`)
	if !strings.Contains(s, "needle") {
		t.Errorf("AuditSummary = %q, want it to name the pattern", s)
	}
}

func TestGrepCapabilities(t *testing.T) {
	t.Parallel()
	var it tool.InvokableTool = NewGrep(t.TempDir(), newFakeReadGuard(1<<20))
	if _, ok := it.(tool.PermissionPrompter); ok {
		t.Error("Grep must not implement PermissionPrompter (AutoApprove)")
	}
	if _, ok := it.(tool.Auditable); !ok {
		t.Error("Grep must implement Auditable")
	}
}

// assertAdjacent fails unless want immediately follows prev somewhere in args.
func assertAdjacent(t *testing.T, args []string, prev, want string) {
	t.Helper()
	for i := 0; i+1 < len(args); i++ {
		if args[i] == prev && args[i+1] == want {
			return
		}
	}
	t.Errorf("args %v: expected %q immediately after %q", args, want, prev)
}

func containsArg(args []string, want string) bool {
	for _, a := range args {
		if a == want {
			return true
		}
	}
	return false
}
