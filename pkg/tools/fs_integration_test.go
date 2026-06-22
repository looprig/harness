//go:build integration

package tools

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/ciram-co/looprig/pkg/loop"
	"github.com/ciram-co/looprig/pkg/tool"
)

// fs_integration_test.go exercises the REAL filesystem tools (ReadFile, Glob,
// Grep, WriteFile, EditFile, Bash) end to end over a t.TempDir() workspace, wired
// with a REAL *PermissionChecker as the loop.ReadGuard for the read tools (it
// satisfies DeniedRead/MaxReadBytes). It proves the design §5e obligations against
// the actual OS: a workspace `.env` is excluded from Glob AND Grep AND ReadFile
// (the secret never leaks via any backend — rg or the stdlib fallback); an
// in-workspace symlink pointing OUT is rejected by every path tool (the outside
// secret is never read/listed/written); MaxReadBytes caps a read with a truncation
// notice; and the atomic write/edit leave no temp litter behind. Tagged
// `integration` so it is excluded from the default `go test ./...` run — run with
// `go test -tags integration -race ./tools/`.
//
// SPLIT (vs the existing integration files): permission_integration_test.go covers
// the policy STORE on disk; web_integration_test.go covers the WEB tools' TLS
// floor. This file is the FILESYSTEM-TOOLS layer with a real PermissionChecker as
// the ReadGuard.

// theSecret is the canonical secret planted in `.env` and the outside file. Its
// ABSENCE from every tool's output/error is asserted across every case — a leak of
// this exact string anywhere is a security failure.
const theSecret = "SUPER_SECRET_TOKEN_e3b0c44298fc1c149afbf4c8996fb924"

// fsWorkspace builds an EvalSymlinks-resolved temp workspace (the canonical form
// containedPath and the ReadGuard resolve to — on macOS /var → /private/var).
func fsWorkspace(t *testing.T) string {
	t.Helper()
	ws := t.TempDir()
	resolved, err := filepath.EvalSymlinks(ws)
	if err != nil {
		t.Fatalf("EvalSymlinks workspace: %v", err)
	}
	return resolved
}

// fsChecker builds a REAL PermissionChecker for root with the given per-file read
// cap (0 → the package default). It is used as the loop.ReadGuard for the read
// tools, so DeniedRead (the **/.env etc. globs) and MaxReadBytes are the genuine
// production logic, not a fake. The home-dir seam is pointed at an isolated temp
// dir so the ~/-relative deny globs never touch the real host home.
func fsChecker(t *testing.T, root string, maxReadBytes int64) *PermissionChecker {
	t.Helper()
	hd := DefaultHardDeny()
	if maxReadBytes > 0 {
		hd.MaxReadBytes = maxReadBytes
	}
	pc := NewPermissionChecker(PermissionPolicy{WorkspaceRoot: root, HardDeny: hd})
	home := t.TempDir()
	pc.SetHomeDir(func() (string, error) { return home, nil })
	return pc
}

// fsWrite writes data to a workspace-relative path under root via the OS (test
// fixture setup — NOT through the tools), creating parent dirs as needed.
func fsWrite(t *testing.T, root, rel, data string) {
	t.Helper()
	abs := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		t.Fatalf("mkdir for %q: %v", rel, err)
	}
	if err := os.WriteFile(abs, []byte(data), 0o600); err != nil {
		t.Fatalf("write %q: %v", rel, err)
	}
}

// assertNoSecret fails the test if the canonical secret appears anywhere in s.
// label identifies the producing tool/case for a readable failure.
func assertNoSecret(t *testing.T, label, s string) {
	t.Helper()
	if strings.Contains(s, theSecret) {
		t.Fatalf("SECURITY: secret leaked via %s: %q", label, s)
	}
}

// rgOnPath reports whether ripgrep is resolvable (so a test can note which Grep
// backend it exercised). Both backends MUST keep the secret out regardless.
func rgOnPath() bool {
	_, err := exec.LookPath(rgBinary)
	return err == nil
}

// TestFSEnvExcludedFromGlobGrepReadFile is the headline §5e case: a real `.env`
// holding theSecret, alongside normal files, must be invisible to Glob and Grep
// and unreadable by ReadFile — and the secret must never appear in any output or
// error. Grep runs the REAL backend present on this host (rg if on PATH, else the
// stdlib fallback): the assertion holds for either, exercising BOTH the rg
// `--glob '!**/.env'` skip AND the authoritative DeniedRead filter.
func TestFSEnvExcludedFromGlobGrepReadFile(t *testing.T) {
	root := fsWorkspace(t)
	pc := fsChecker(t, root, 0)

	// Fixtures: a secret-bearing .env, a nested .env, and ordinary source files
	// (one of which mentions the env file by name but NOT the secret).
	fsWrite(t, root, ".env", "API_TOKEN="+theSecret+"\n")
	fsWrite(t, root, "config/.env", "DB_PASSWORD="+theSecret+"\n")
	fsWrite(t, root, "main.go", "package main\n\nfunc main() {}\n")
	fsWrite(t, root, "internal/util.go", "package internal\n\n// loads .env at boot\n")
	fsWrite(t, root, "README.md", "# Project\nSet your secrets in .env\n")

	glob := NewGlob(root, pc)
	grep := NewGrep(root, pc)
	read := NewReadFile(root, pc)
	ctx := context.Background()

	// Glob("**") must list the normal files but NEVER the .env files.
	t.Run("Glob excludes .env", func(t *testing.T) {
		res, err := glob.InvokableRun(ctx, `{"pattern":"**"}`)
		if err != nil {
			t.Fatalf("Glob InvokableRun() Go error = %v", err)
		}
		got := textOf(t, res)
		assertNoSecret(t, "Glob output", got)
		for _, denied := range []string{".env", "config/.env"} {
			for _, line := range strings.Split(got, "\n") {
				if line == denied {
					t.Errorf("Glob listed a denied path %q; full output:\n%s", denied, got)
				}
			}
		}
		// Sanity: it DID find the normal files (so the exclusion is meaningful, not
		// an empty listing).
		for _, want := range []string{"main.go", "internal/util.go", "README.md"} {
			if !strings.Contains(got, want) {
				t.Errorf("Glob did not list expected file %q; output:\n%s", want, got)
			}
		}
	})

	// Grep for the secret must return NO match, and neither the secret nor a .env
	// path may appear in the output (exercises the real backend on this host).
	t.Run("Grep excludes .env and never leaks the secret", func(t *testing.T) {
		res, err := grep.InvokableRun(ctx, `{"pattern":"`+theSecret+`"}`)
		if err != nil {
			t.Fatalf("Grep InvokableRun() Go error = %v", err)
		}
		got := textOf(t, res)
		assertNoSecret(t, "Grep secret-search output (backend rg="+strconv.FormatBool(rgOnPath())+")", got)
		if strings.Contains(got, ".env") {
			t.Errorf("Grep output referenced a .env path: %q", got)
		}
		if got != "no matches" {
			t.Errorf("Grep for the secret = %q, want \"no matches\" (the secret lives only in .env)", got)
		}

		// A pattern that DOES occur in the .env (the variable name) must still not
		// surface a .env line — the file is excluded entirely, not just the secret.
		res2, err := grep.InvokableRun(ctx, `{"pattern":"API_TOKEN"}`)
		if err != nil {
			t.Fatalf("Grep InvokableRun(API_TOKEN) Go error = %v", err)
		}
		got2 := textOf(t, res2)
		assertNoSecret(t, "Grep API_TOKEN output", got2)
		if strings.Contains(got2, ".env") {
			t.Errorf("Grep(API_TOKEN) surfaced a .env path: %q", got2)
		}
	})

	// ReadFile(".env") must be denied with a non-secret error (no contents echoed).
	t.Run("ReadFile .env is denied without leaking", func(t *testing.T) {
		res, err := read.InvokableRun(ctx, `{"path":".env"}`)
		if err != nil {
			t.Fatalf("ReadFile InvokableRun() Go error = %v", err)
		}
		got := textOf(t, res)
		assertNoSecret(t, "ReadFile .env error", got)
		if !strings.HasPrefix(got, "error: read denied") {
			t.Errorf("ReadFile(.env) = %q, want a \"read denied\" error", got)
		}
		// The nested one is also denied.
		res2, err := read.InvokableRun(ctx, `{"path":"config/.env"}`)
		if err != nil {
			t.Fatalf("ReadFile(config/.env) Go error = %v", err)
		}
		got2 := textOf(t, res2)
		assertNoSecret(t, "ReadFile config/.env error", got2)
		if !strings.HasPrefix(got2, "error: read denied") {
			t.Errorf("ReadFile(config/.env) = %q, want a \"read denied\" error", got2)
		}
	})
}

// TestFSContainmentSymlinkRejected proves every path tool refuses an in-workspace
// symlink that points OUTSIDE the workspace (to a separate temp dir holding a
// secret file) and a "../escape" path — and the outside secret is never read,
// listed, written, or executed against. The outside dir lives under a SEPARATE
// t.TempDir() so it is genuinely outside the workspace root.
func TestFSContainmentSymlinkRejected(t *testing.T) {
	root := fsWorkspace(t)
	pc := fsChecker(t, root, 0)

	// An outside directory with a secret file, reachable only by following the
	// symlink we plant inside the workspace.
	outside := fsWorkspace(t)
	outsideSecretRel := "outside-secret.txt"
	if err := os.WriteFile(filepath.Join(outside, outsideSecretRel), []byte("LEAK="+theSecret+"\n"), 0o600); err != nil {
		t.Fatalf("write outside secret: %v", err)
	}

	// A symlink INSIDE the workspace pointing at the outside dir, and a symlink
	// pointing directly at the outside secret file.
	linkDir := filepath.Join(root, "escape-dir")
	if err := os.Symlink(outside, linkDir); err != nil {
		t.Fatalf("symlink escape-dir -> outside: %v", err)
	}
	linkFile := filepath.Join(root, "escape-file")
	if err := os.Symlink(filepath.Join(outside, outsideSecretRel), linkFile); err != nil {
		t.Fatalf("symlink escape-file -> outside secret: %v", err)
	}
	// A normal in-workspace file so the tools have something legitimate present.
	fsWrite(t, root, "keep.txt", "hello\n")

	read := NewReadFile(root, pc)
	glob := NewGlob(root, pc)
	grep := NewGrep(root, pc)
	write := NewWriteFile(root)
	edit := NewEditFile(root)
	bash := NewBash(root)
	ctx := context.Background()

	// Every (tool, args) pair below must be REJECTED and must not leak the secret.
	// "escape-file/.." is irrelevant; we use the symlink path itself and a literal
	// "../" climb.
	type call struct {
		name string
		run  func() (*tool.ToolResult, error)
	}
	calls := []call{
		{"ReadFile via symlinked file", func() (*tool.ToolResult, error) {
			return read.InvokableRun(ctx, `{"path":"escape-file"}`)
		}},
		{"ReadFile through symlinked dir", func() (*tool.ToolResult, error) {
			return read.InvokableRun(ctx, `{"path":"escape-dir/`+outsideSecretRel+`"}`)
		}},
		{"ReadFile ../escape", func() (*tool.ToolResult, error) {
			return read.InvokableRun(ctx, `{"path":"../`+filepath.Base(outside)+`/`+outsideSecretRel+`"}`)
		}},
		{"Grep through symlinked dir", func() (*tool.ToolResult, error) {
			return grep.InvokableRun(ctx, `{"pattern":"LEAK","path":"escape-dir"}`)
		}},
		{"WriteFile through symlinked dir", func() (*tool.ToolResult, error) {
			return write.InvokableRun(ctx, `{"path":"escape-dir/planted.txt","content":"x"}`)
		}},
		{"EditFile via symlinked file", func() (*tool.ToolResult, error) {
			return edit.InvokableRun(ctx, `{"path":"escape-file","old":"LEAK","new":"PWNED"}`)
		}},
		{"Bash with escaping workdir", func() (*tool.ToolResult, error) {
			return bash.InvokableRun(ctx, `{"command":"cat `+outsideSecretRel+`","workdir":"escape-dir"}`)
		}},
	}
	for _, c := range calls {
		c := c
		t.Run(c.name, func(t *testing.T) {
			res, err := c.run()
			if err != nil {
				t.Fatalf("%s: unexpected Go error = %v", c.name, err)
			}
			got := textOf(t, res)
			assertNoSecret(t, c.name, got)
			if !strings.HasPrefix(got, "error:") {
				t.Errorf("%s = %q, want a tool-result error (containment/symlink rejection)", c.name, got)
			}
		})
	}

	// Glob("**") must NOT traverse the symlink into the outside tree: the outside
	// secret file must not appear in the listing, and the secret must not leak.
	t.Run("Glob does not list through the symlink", func(t *testing.T) {
		res, err := glob.InvokableRun(ctx, `{"pattern":"**"}`)
		if err != nil {
			t.Fatalf("Glob InvokableRun() Go error = %v", err)
		}
		got := textOf(t, res)
		assertNoSecret(t, "Glob symlink listing", got)
		if strings.Contains(got, outsideSecretRel) {
			t.Errorf("Glob listed the outside secret file %q through the symlink:\n%s", outsideSecretRel, got)
		}
	})

	// The outside secret file must STILL be intact (no WriteFile/EditFile clobbered
	// it through the symlink).
	body, err := os.ReadFile(filepath.Join(outside, outsideSecretRel))
	if err != nil {
		t.Fatalf("re-read outside secret: %v", err)
	}
	if !strings.Contains(string(body), theSecret) {
		t.Errorf("outside secret file was modified through the symlink: %q", string(body))
	}
	// And no file was planted inside the outside dir via WriteFile.
	if _, err := os.Stat(filepath.Join(outside, "planted.txt")); !os.IsNotExist(err) {
		t.Errorf("WriteFile planted a file in the outside dir through the symlink; stat err=%v", err)
	}
}

// TestFSMaxReadBytesCaps proves ReadFile caps its output at the ReadGuard's
// MaxReadBytes and appends a truncation notice. A small cap is configured on the
// REAL PermissionChecker so the test stays fast.
func TestFSMaxReadBytesCaps(t *testing.T) {
	const readCap = 64
	root := fsWorkspace(t)
	pc := fsChecker(t, root, readCap)
	if pc.MaxReadBytes() != readCap {
		t.Fatalf("MaxReadBytes() = %d, want %d", pc.MaxReadBytes(), readCap)
	}

	tests := []struct {
		name      string
		size      int
		wantTrunc bool
	}{
		{name: "well under the cap", size: readCap / 2, wantTrunc: false},
		{name: "exactly at the cap", size: readCap, wantTrunc: false},
		{name: "one over the cap", size: readCap + 1, wantTrunc: true},
		{name: "far over the cap", size: readCap * 4, wantTrunc: true},
	}
	read := NewReadFile(root, pc)
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			// A single long line of 'a' so byte length is predictable (no newline so
			// the whole payload is one line; the cap is on raw bytes read).
			content := strings.Repeat("a", tt.size)
			rel := "big.txt"
			fsWrite(t, root, rel, content)

			res, err := read.InvokableRun(context.Background(), `{"path":"`+rel+`"}`)
			if err != nil {
				t.Fatalf("ReadFile InvokableRun() Go error = %v", err)
			}
			got := textOf(t, res)

			hasNotice := strings.Contains(got, "truncated") && strings.Contains(got, "read cap")
			if hasNotice != tt.wantTrunc {
				t.Errorf("truncation notice present = %v, want %v; output:\n%q", hasNotice, tt.wantTrunc, got)
			}
			// Strip the truncation-notice line (which itself contains 'a' letters)
			// before counting the file's own 'a' bytes, which must never exceed the
			// cap. The notice, when present, is the final line.
			contentPart := got
			if hasNotice {
				if i := strings.LastIndex(got, "\n[truncated:"); i >= 0 {
					contentPart = got[:i]
				}
			}
			if n := strings.Count(contentPart, "a"); n > readCap {
				t.Errorf("ReadFile emitted %d content bytes, exceeds the %d-byte cap", n, readCap)
			}
		})
	}
}

// TestFSAtomicWrite proves WriteFile and EditFile produce the expected on-disk
// content via the atomic temp+rename, create nested parent dirs, end at the
// owner-only 0600 mode, and leave NO temp litter (.urvi-write-*) behind.
func TestFSAtomicWrite(t *testing.T) {
	root := fsWorkspace(t)
	write := NewWriteFile(root)
	edit := NewEditFile(root)
	ctx := context.Background()

	// WriteFile a brand-new file under nested dirs.
	t.Run("WriteFile creates nested file atomically", func(t *testing.T) {
		rel := "a/b/c/new.txt"
		res, err := write.InvokableRun(ctx, `{"path":"`+rel+`","content":"first contents\n"}`)
		if err != nil {
			t.Fatalf("WriteFile InvokableRun() Go error = %v", err)
		}
		got := textOf(t, res)
		if !strings.HasPrefix(got, "wrote ") {
			t.Fatalf("WriteFile result = %q, want a \"wrote ...\" success", got)
		}
		abs := filepath.Join(root, rel)
		body, err := os.ReadFile(abs)
		if err != nil {
			t.Fatalf("read back written file: %v", err)
		}
		if string(body) != "first contents\n" {
			t.Errorf("written content = %q, want %q", string(body), "first contents\n")
		}
		fi, err := os.Stat(abs)
		if err != nil {
			t.Fatalf("stat written file: %v", err)
		}
		if perm := fi.Mode().Perm(); perm != newFilePerm {
			t.Errorf("written file perm = %o, want %o", perm, newFilePerm)
		}
		assertNoTempLitter(t, filepath.Dir(abs))
	})

	// EditFile replaces an existing file's content.
	t.Run("EditFile replaces content atomically", func(t *testing.T) {
		rel := "edit/target.txt"
		fsWrite(t, root, rel, "alpha BETA gamma\n")
		res, err := edit.InvokableRun(ctx, `{"path":"`+rel+`","old":"BETA","new":"DELTA"}`)
		if err != nil {
			t.Fatalf("EditFile InvokableRun() Go error = %v", err)
		}
		got := textOf(t, res)
		if strings.HasPrefix(got, "error:") {
			t.Fatalf("EditFile result = %q, want a diff preview", got)
		}
		abs := filepath.Join(root, rel)
		body, err := os.ReadFile(abs)
		if err != nil {
			t.Fatalf("read back edited file: %v", err)
		}
		if string(body) != "alpha DELTA gamma\n" {
			t.Errorf("edited content = %q, want %q", string(body), "alpha DELTA gamma\n")
		}
		assertNoTempLitter(t, filepath.Dir(abs))
	})

	// WriteFile overwrites an existing file (the rename-over path).
	t.Run("WriteFile overwrites existing file", func(t *testing.T) {
		rel := "over.txt"
		fsWrite(t, root, rel, "old body\n")
		res, err := write.InvokableRun(ctx, `{"path":"`+rel+`","content":"new body\n"}`)
		if err != nil {
			t.Fatalf("WriteFile InvokableRun() Go error = %v", err)
		}
		if got := textOf(t, res); !strings.HasPrefix(got, "wrote ") {
			t.Fatalf("WriteFile overwrite result = %q, want success", got)
		}
		body, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			t.Fatalf("read back overwritten file: %v", err)
		}
		if string(body) != "new body\n" {
			t.Errorf("overwritten content = %q, want %q", string(body), "new body\n")
		}
		assertNoTempLitter(t, root)
	})
}

// assertNoTempLitter fails if any .urvi-write-* temp file remains in dir after an
// atomic write (the temp must have been renamed into place or removed on failure).
func assertNoTempLitter(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir %q: %v", dir, err)
	}
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, ".urvi-write-") || strings.HasSuffix(name, ".tmp") {
			t.Errorf("leftover temp file %q in %q after atomic write", name, dir)
		}
	}
}

// TestFSBashRunsInWorkspace proves Bash executes a command in the workspace (its
// output captured) and that an escaping workdir is rejected without touching the
// outside tree. It is cheap (a single echo / cat).
func TestFSBashRunsInWorkspace(t *testing.T) {
	root := fsWorkspace(t)
	fsWrite(t, root, "hello.txt", "in-workspace content\n")
	bash := NewBash(root)
	ctx := context.Background()

	t.Run("runs in the workspace root", func(t *testing.T) {
		res, err := bash.InvokableRun(ctx, `{"command":"cat hello.txt"}`)
		if err != nil {
			t.Fatalf("Bash InvokableRun() Go error = %v", err)
		}
		got := textOf(t, res)
		if !strings.Contains(got, "in-workspace content") {
			t.Errorf("Bash did not run in the workspace root; output:\n%q", got)
		}
		if !strings.Contains(got, "[exit code: 0]") {
			t.Errorf("Bash result missing a zero exit code; output:\n%q", got)
		}
	})

	t.Run("runs in a contained subdir", func(t *testing.T) {
		fsWrite(t, root, "sub/inner.txt", "nested\n")
		res, err := bash.InvokableRun(ctx, `{"command":"cat inner.txt","workdir":"sub"}`)
		if err != nil {
			t.Fatalf("Bash InvokableRun() Go error = %v", err)
		}
		if got := textOf(t, res); !strings.Contains(got, "nested") {
			t.Errorf("Bash did not run in the contained subdir; output:\n%q", got)
		}
	})

	t.Run("escaping workdir is rejected", func(t *testing.T) {
		res, err := bash.InvokableRun(ctx, `{"command":"pwd","workdir":"../"}`)
		if err != nil {
			t.Fatalf("Bash InvokableRun() Go error = %v", err)
		}
		got := textOf(t, res)
		if !strings.HasPrefix(got, "error: workdir is outside the workspace") {
			t.Errorf("Bash escaping workdir = %q, want a workdir-outside error", got)
		}
	})
}

// Compile-time guard: the read tools accept the REAL PermissionChecker as their
// loop.ReadGuard. This documents the wiring at the type level (the constructors
// above already depend on it, but this makes the contract explicit).
var _ loop.ReadGuard = (*PermissionChecker)(nil)
