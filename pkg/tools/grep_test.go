package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/looprig/core/content"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/tool"
)

func runGrep(t *testing.T, root string, guard loop.ReadGuard, args map[string]any) string {
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

// TestGrepHonorsEnvDenyGuard mirrors TestReadFileHonorsEnvDenyGuard for Grep: a
// ReadGuard that denies the §5.3 "**/.env*" secret set (a policy-derived RULE via
// patternReadGuard) must make Grep SKIP every .env-family file — never emitting the
// filename or the secret line — while still matching a sibling non-.env file. This
// pins the §10.5 seam for the content-search read tool: the one guard the swe
// sandbox adapter builds binds Grep identically to a sandboxed search. The
// deterministic WalkDir backend is forced (via runGrep) so the assertion holds
// whether or not ripgrep is installed.
func TestGrepHonorsEnvDenyGuard(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, ".env"), "SECRET_TOKEN=needle\n")
	mustWrite(t, filepath.Join(root, ".env.local"), "SECRET_TOKEN=needle\n")
	mustWrite(t, filepath.Join(root, "config", ".env.production"), "SECRET_TOKEN=needle\n")
	mustWrite(t, filepath.Join(root, "app.go"), "// needle in a non-secret file\n")

	guard := &patternReadGuard{deny: denyDotEnv, maxBytes: 1 << 20}
	got := runGrep(t, root, guard, map[string]any{"pattern": "needle"})

	if !strings.Contains(got, "app.go") || !strings.Contains(got, "non-secret") {
		t.Errorf("Grep did not match the permitted non-.env file; output:\n%s", got)
	}
	for _, absent := range []string{".env", "SECRET_TOKEN"} {
		if strings.Contains(got, absent) {
			t.Errorf("Grep leaked denied .env content/name %q; output:\n%s", absent, got)
		}
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

// TestGrepFallbackTimeout asserts the fallback WalkDir backend honors a cancelled
// context: an already-expired ctx aborts the walk and the tool returns the timeout
// tool-result rather than scanning the tree. This is the cheap in-process
// cancellability required by the "no unbounded I/O" rule (FIX 1).
func TestGrepFallbackTimeout(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "a.go"), "// findme\n")

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // expire before the walk begins.

	g := newGrepWithBackend(root, newFakeReadGuard(1<<20), false)
	res, err := g.InvokableRun(ctx, `{"pattern":"findme"}`)
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

// TestGrepTimeoutBoundIsApplied asserts InvokableRun derives a bounded context for
// the rg backend so the subprocess exec cannot hang past grepTimeout (FIX 1).
// grepTimeout must be a positive bound, and (when rg is present) an already-expired
// parent ctx must surface the timeout tool-result via the CommandContext kill path
// rather than hanging or silently returning no matches.
func TestGrepTimeoutBoundIsApplied(t *testing.T) {
	t.Parallel()
	if grepTimeout <= 0 {
		t.Fatalf("grepTimeout must be a positive bound, got %v", grepTimeout)
	}
	if _, err := exec.LookPath(rgBinary); err != nil {
		t.Skipf("ripgrep not on PATH (%v); the positive-bound assertion still holds", err)
	}

	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "a.go"), "// findme\n")

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // expire before the exec begins; CommandContext refuses to start it.

	g := newGrepWithBackend(root, newFakeReadGuard(1<<20), true) // force the rg backend.
	res, err := g.InvokableRun(ctx, `{"pattern":"findme"}`)
	if err != nil {
		t.Fatalf("InvokableRun returned a Go error %v; read tools return tool-result strings", err)
	}
	tb, ok := res.Content[0].(*content.TextBlock)
	if !ok {
		t.Fatalf("block type = %T, want *content.TextBlock", res.Content[0])
	}
	if !strings.Contains(tb.Text, "timed out") {
		t.Errorf("output = %q, want it to report the timeout (rg exec was bounded)", tb.Text)
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

// TestAsExitCode covers the trimmed-down asExitCode (FIX 2): it reports an
// *exec.ExitError's code via errors.As (wrapped-error robust) and rejects a
// non-exit error.
func TestAsExitCode(t *testing.T) {
	t.Parallel()

	// Produce a real *exec.ExitError (exit status 1) deterministically.
	exitErr := exec.Command("false").Run()
	if exitErr == nil {
		t.Skip("`false` not available or did not exit non-zero")
	}
	var ee *exec.ExitError
	if !errors.As(exitErr, &ee) {
		t.Fatalf("expected an *exec.ExitError, got %T", exitErr)
	}
	wantCode := ee.ExitCode()

	tests := []struct {
		name   string
		err    error
		wantOk bool
		want   int
	}{
		{name: "exit error yields its code", err: exitErr, wantOk: true, want: wantCode},
		{name: "wrapped exit error is unwrapped", err: fmt.Errorf("rg: %w", exitErr), wantOk: true, want: wantCode},
		{name: "non-exit error is not ok", err: errStopWalk, wantOk: false},
		{name: "nil error is not ok", err: nil, wantOk: false},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			code, ok := asExitCode(tt.err)
			if ok != tt.wantOk {
				t.Fatalf("asExitCode ok = %v, want %v", ok, tt.wantOk)
			}
			if tt.wantOk && code != tt.want {
				t.Errorf("asExitCode code = %d, want %d", code, tt.want)
			}
		})
	}
}

// TestDenyFilteredRel is the canonical, self-documenting unit test for the SINGLE
// shared deny-filter (glob.go) used by Glob's walk and BOTH Grep backends
// (collectRgLines + the WalkDir fallback). It covers all three of the helper's
// outcomes:
//   - a permitted path yields its workspace-relative slash form;
//   - a guard-denied path (DeniedRead) is reported denied;
//   - the security-critical ESCAPE branch: a symlink whose EvalSymlinks target
//     resolves OUTSIDE the workspace root is reported denied (so it is excluded,
//     never emitted with a "../"-climbing path that would leak the target);
//   - and, to prove no over-rejection, an in-workspace symlink whose target stays
//     INSIDE the root is NOT denied and yields the resolved in-workspace rel.
//
// Because this exercises the shared helper directly, it is the portable, rg-
// independent coverage for the escape branch. That branch is what Grep's rg
// backend (collectRgLines) relies on SOLELY to exclude an out-of-workspace path
// rg emits; in the WalkDir fallback it is the first of two barriers (the second
// being grepFile's O_NOFOLLOW). See TestGrepFallbackNoLeakThroughSymlink for the
// fallback's end-to-end no-leak proof and fs_integration_test.go for the rg
// backend's on-host coverage.
func TestDenyFilteredRel(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	resolvedRoot, err = filepath.Abs(resolvedRoot)
	if err != nil {
		t.Fatalf("Abs: %v", err)
	}
	mustWrite(t, filepath.Join(root, "ok", "app.go"), "x")
	mustWrite(t, filepath.Join(root, ".env"), "SECRET=1")
	mustWrite(t, filepath.Join(root, "inside-target.txt"), "y")

	// A symlink INSIDE the workspace whose target is ALSO inside the workspace:
	// must NOT be denied (no over-rejection); its resolved rel is the target's.
	insideLink := filepath.Join(root, "inside-link")
	if err := os.Symlink(filepath.Join(root, "inside-target.txt"), insideLink); err != nil {
		t.Fatalf("symlink inside-link: %v", err)
	}

	// A symlink INSIDE the workspace whose target resolves OUTSIDE the root (a
	// sibling temp dir): the escape branch must report it denied.
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("SECRET"), 0o600); err != nil {
		t.Fatalf("write outside secret: %v", err)
	}
	escapeLink := filepath.Join(root, "escape-link")
	if err := os.Symlink(filepath.Join(outside, "secret.txt"), escapeLink); err != nil {
		t.Fatalf("symlink escape-link: %v", err)
	}

	okAbs := filepath.Join(resolvedRoot, "ok", "app.go")
	envAbs := filepath.Join(resolvedRoot, ".env")
	guard := newFakeReadGuard(1<<20, envAbs)

	tests := []struct {
		name       string
		abs        string
		wantDenied bool
		wantRel    string
	}{
		{name: "permitted path yields slash rel", abs: okAbs, wantDenied: false, wantRel: "ok/app.go"},
		{name: "denied path is excluded", abs: envAbs, wantDenied: true},
		{name: "in-workspace symlink to in-workspace target is not denied", abs: insideLink, wantDenied: false, wantRel: "inside-target.txt"},
		{name: "symlink resolving outside root is denied (escape branch)", abs: escapeLink, wantDenied: true},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			rel, denied := denyFilteredRel(guard, resolvedRoot, tt.abs)
			if denied != tt.wantDenied {
				t.Fatalf("denied = %v, want %v", denied, tt.wantDenied)
			}
			if !tt.wantDenied && rel != tt.wantRel {
				t.Errorf("rel = %q, want %q", rel, tt.wantRel)
			}
		})
	}
}

// TestGrepFallbackNoLeakThroughSymlink proves the WalkDir fallback never leaks an
// out-of-workspace file's content through an in-workspace symlink, and never emits
// a "../"-climbing path, while still matching legitimate in-workspace files.
//
// Honest scope note: this is a defence-in-depth, end-to-end assertion — it does
// NOT isolate which mechanism does the work, because the fallback has two
// independent barriers and either alone suffices:
//  1. denyFilteredRel (grep.go runFallback) runs on each visited entry BEFORE the
//     file is opened; a symlink whose target resolves outside the root hits the
//     escape branch and is excluded — so the symlinked entry is dropped first.
//  2. grepFile then opens with O_NOFOLLOW (grep.go), so even a symlinked final
//     component that slipped past the filter is never read.
//
// The canonical, isolated proof that denyFilteredRel's escape branch actually
// returns denied lives in TestDenyFilteredRel (rg-independent, portable). The rg
// backend (collectRgLines, grep.go) — which has NO O_NOFOLLOW barrier and so
// relies SOLELY on denyFilteredRel to rewrite/exclude rg's emitted absolute paths
// — is exercised on rg-present hosts by fs_integration_test.go's symlink case.
func TestGrepFallbackNoLeakThroughSymlink(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("NEEDLE outside\n"), 0o600); err != nil {
		t.Fatalf("write outside file: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "keep.txt"), []byte("NEEDLE inside\n"), 0o600); err != nil {
		t.Fatalf("write keep.txt: %v", err)
	}
	if err := os.Symlink(filepath.Join(outside, "secret.txt"), filepath.Join(root, "link-file")); err != nil {
		t.Fatalf("symlink link-file: %v", err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "link-dir")); err != nil {
		t.Fatalf("symlink link-dir: %v", err)
	}

	got := runGrep(t, root, newFakeReadGuard(1<<20), map[string]any{"pattern": "NEEDLE"})

	if strings.Contains(got, "outside") {
		t.Errorf("Grep matched content in the outside tree through a symlink; output:\n%s", got)
	}
	if strings.Contains(got, "..") {
		t.Errorf("Grep emitted a workspace-escaping path; output:\n%s", got)
	}
	if !strings.Contains(got, "keep.txt") || !strings.Contains(got, "NEEDLE inside") {
		t.Errorf("Grep did not match the legitimate in-workspace file; output:\n%s", got)
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
