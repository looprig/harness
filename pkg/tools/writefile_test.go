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

// runWriteFile invokes WriteFile (bound to the given per-loop observation map) and
// extracts the single text block, failing on any structural surprise (including a
// Go error — write tools return tool-result strings). The shared obs lets a test
// observe a file (via observeFile) before overwriting it, exercising the read-then-
// write optimistic-concurrency path.
func runWriteFile(t *testing.T, root string, obs *fileObservations, args map[string]any) string {
	t.Helper()
	b, err := json.Marshal(args)
	if err != nil {
		t.Fatalf("marshal args: %v", err)
	}
	res, err := NewWriteFile(root, obs).InvokableRun(context.Background(), string(b))
	if err != nil {
		t.Fatalf("InvokableRun returned a Go error %v; write tools return tool-result strings", err)
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

// observeFile runs a real ReadFile bound to obs so a subsequent WriteFile/EditFile
// on the same loop is authorized to overwrite/edit the path — the faithful read-
// then-write workflow. It fails if the read did not succeed.
func observeFile(t *testing.T, root string, obs *fileObservations, rel string) {
	t.Helper()
	b, err := json.Marshal(map[string]any{"path": rel})
	if err != nil {
		t.Fatalf("marshal args: %v", err)
	}
	res, err := NewReadFile(root, newFakeReadGuard(1<<20), obs).InvokableRun(context.Background(), string(b))
	if err != nil {
		t.Fatalf("observe read Go error: %v", err)
	}
	if got := res.Content[0].(*content.TextBlock).Text; strings.HasPrefix(got, "error:") {
		t.Fatalf("observe read of %q failed: %q", rel, got)
	}
}

func TestWriteFileInfo(t *testing.T) {
	t.Parallel()
	info, err := NewWriteFile(t.TempDir(), newFileObservations()).Info(context.Background())
	if err != nil {
		t.Fatalf("Info() error = %v", err)
	}
	if info.Name != "WriteFile" {
		t.Errorf("Info().Name = %q, want %q", info.Name, "WriteFile")
	}
	var schema map[string]json.RawMessage
	if err := json.Unmarshal(info.Schema, &schema); err != nil {
		t.Fatalf("Schema is not a JSON object: %v", err)
	}
	if _, ok := schema["properties"]; !ok {
		t.Errorf("Schema missing 'properties'")
	}
}

func TestWriteFile(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		args       map[string]any
		setup      func(t *testing.T, root string)
		observeRel string // if set, ReadFile this path first so an overwrite is authorized
		wantErr    bool   // result string begins with "error:"
		wantOnDisk string // relative path that should exist with wantBody
		wantBody   string
	}{
		{
			name:       "new file in root",
			args:       map[string]any{"path": "out.txt", "content": "hello\nworld\n"},
			wantOnDisk: "out.txt",
			wantBody:   "hello\nworld\n",
		},
		{
			name:       "nested dirs are created",
			args:       map[string]any{"path": "a/b/c/deep.txt", "content": "deep"},
			wantOnDisk: "a/b/c/deep.txt",
			wantBody:   "deep",
		},
		{
			name: "observed existing file is overwritten",
			args: map[string]any{"path": "exists.txt", "content": "new"},
			setup: func(t *testing.T, root string) {
				if err := os.WriteFile(filepath.Join(root, "exists.txt"), []byte("old contents here"), 0o600); err != nil {
					t.Fatalf("seed: %v", err)
				}
			},
			observeRel: "exists.txt",
			wantOnDisk: "exists.txt",
			wantBody:   "new",
		},
		{
			name: "unobserved existing file is rejected without clobbering",
			args: map[string]any{"path": "exists.txt", "content": "new"},
			setup: func(t *testing.T, root string) {
				if err := os.WriteFile(filepath.Join(root, "exists.txt"), []byte("old contents here"), 0o600); err != nil {
					t.Fatalf("seed: %v", err)
				}
			},
			wantErr:    true,
			wantOnDisk: "exists.txt",
			wantBody:   "old contents here", // unchanged: no observation, no clobber
		},
		{
			name:       "empty content writes an empty file",
			args:       map[string]any{"path": "empty.txt", "content": ""},
			wantOnDisk: "empty.txt",
			wantBody:   "",
		},
		{
			name:    "escape path is rejected",
			args:    map[string]any{"path": "../escape.txt", "content": "x"},
			wantErr: true,
		},
		{
			name:    "absolute path is anchored under root (not /etc)",
			args:    map[string]any{"path": "/etc/passwd", "content": "x"},
			wantErr: false, // anchored under root -> writes root/etc/passwd
		},
		{
			name:    "missing path is rejected",
			args:    map[string]any{"content": "x"},
			wantErr: true,
		},
		{
			name:    "empty path is rejected",
			args:    map[string]any{"path": "", "content": "x"},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			obs := newFileObservations()
			if tt.setup != nil {
				tt.setup(t, root)
			}
			if tt.observeRel != "" {
				observeFile(t, root, obs, tt.observeRel)
			}
			out := runWriteFile(t, root, obs, tt.args)
			gotErr := strings.HasPrefix(out, "error:")
			if gotErr != tt.wantErr {
				t.Fatalf("result = %q, wantErr = %v", out, tt.wantErr)
			}
			// The on-disk body is checked even on the expected-error rows so a
			// rejected write is proven NOT to have clobbered the existing bytes.
			if tt.wantOnDisk != "" {
				got, err := os.ReadFile(filepath.Join(root, tt.wantOnDisk))
				if err != nil {
					t.Fatalf("read written file: %v", err)
				}
				if string(got) != tt.wantBody {
					t.Errorf("on-disk body = %q, want %q", got, tt.wantBody)
				}
			}
		})
	}
}

// TestWriteFileSymlinkNotFollowed ensures a write through an in-workspace symlink
// that points OUTSIDE the workspace is rejected by containment (defense in depth;
// the gate also denies it).
func TestWriteFileSymlinkNotFollowed(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	outside := t.TempDir()
	// link -> outside (an absolute symlink escaping the workspace).
	link := filepath.Join(root, "link")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	out := runWriteFile(t, root, newFileObservations(), map[string]any{"path": "link/evil.txt", "content": "x"})
	if !strings.HasPrefix(out, "error:") {
		t.Fatalf("write via escaping symlink = %q, want an error", out)
	}
	if _, err := os.Stat(filepath.Join(outside, "evil.txt")); err == nil {
		t.Fatalf("write escaped to %s/evil.txt", outside)
	}
}

// TestWriteFileUnobservedSymlinkRejected asserts that a write to a path whose
// final component is an EXISTING in-workspace symlink is REFUSED under the
// optimistic-concurrency policy: a final-component symlink cannot be observed (a
// ReadFile of it fails O_NOFOLLOW with ELOOP and records no observation), so it is
// an existing-but-unverifiable path and any mutation is denied fail-secure. Neither
// the symlink node nor its target is touched — a strict hardening of the previous
// "replace the symlink" behavior.
func TestWriteFileUnobservedSymlinkRejected(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	const targetBody = "ORIGINAL TARGET BODY"
	target := filepath.Join(root, "target.txt")
	if err := os.WriteFile(target, []byte(targetBody), 0o600); err != nil {
		t.Fatalf("seed target: %v", err)
	}
	link := filepath.Join(root, "link.txt")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}

	out := runWriteFile(t, root, newFileObservations(), map[string]any{"path": "link.txt", "content": "NEW"})
	if !strings.HasPrefix(out, "error:") {
		t.Fatalf("write to unobserved symlink path = %q, want a fail-secure rejection", out)
	}

	// The symlink node is intact (not replaced) and still points at its target.
	fi, err := os.Lstat(link)
	if err != nil {
		t.Fatalf("lstat link: %v", err)
	}
	if fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("link.txt is no longer a symlink; the rejected write mutated it")
	}
	// The symlink's target must be UNTOUCHED (the write neither followed nor
	// clobbered it).
	gotTarget, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read target: %v", err)
	}
	if string(gotTarget) != targetBody {
		t.Fatalf("symlink target was clobbered: %q, want %q", gotTarget, targetBody)
	}
}

func TestWriteFileWriteTarget(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	wf := NewWriteFile(root, newFileObservations())

	// Valid args -> resolved path, ok=true.
	key, ok, err := wf.WriteTarget(`{"path":"sub/x.txt","content":"y"}`)
	if err != nil {
		t.Fatalf("WriteTarget err = %v", err)
	}
	if !ok {
		t.Fatalf("WriteTarget ok = false, want true")
	}
	want := resolvedJoin(t, root, filepath.Join("sub", "x.txt"))
	if key != want {
		t.Errorf("WriteTarget key = %q, want %q", key, want)
	}

	// Escape -> err, ok=false.
	if _, ok, err := wf.WriteTarget(`{"path":"../x.txt","content":"y"}`); ok || err == nil {
		t.Errorf("WriteTarget(escape) = (ok=%v, err=%v), want (false, non-nil)", ok, err)
	}
	// Unparseable args -> err, ok=false.
	if _, ok, err := wf.WriteTarget(`not json`); ok || err == nil {
		t.Errorf("WriteTarget(bad json) = (ok=%v, err=%v), want (false, non-nil)", ok, err)
	}
}

func TestWriteFileBuildRequest(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	wf := NewWriteFile(root, newFileObservations())

	req, err := wf.BuildRequest(`{"path":"sub/x.txt","content":"secret-content-here"}`, nil)
	if err != nil {
		t.Fatalf("BuildRequest err = %v", err)
	}
	fw, ok := req.(tool.FileWriteRequest)
	if !ok {
		t.Fatalf("request type = %T, want tool.FileWriteRequest", req)
	}
	want := resolvedJoin(t, root, filepath.Join("sub", "x.txt"))
	if fw.Path != want {
		t.Errorf("FileWriteRequest.Path = %q, want %q", fw.Path, want)
	}
	if strings.Contains(fw.Description(), "secret-content-here") {
		t.Errorf("request Description leaked content: %q", fw.Description())
	}

	if _, err := wf.BuildRequest(`{"path":"../escape","content":"x"}`, nil); err == nil {
		t.Errorf("BuildRequest(escape) err = nil, want non-nil")
	}
}

func TestWriteFileAuditSummary(t *testing.T) {
	t.Parallel()
	wf := NewWriteFile(t.TempDir(), newFileObservations())
	got := wf.AuditSummary(`{"path":"a/b.txt","content":"super secret payload"}`)
	if !strings.Contains(got, "a/b.txt") {
		t.Errorf("AuditSummary = %q, want it to contain the path", got)
	}
	if strings.Contains(got, "super secret payload") {
		t.Errorf("AuditSummary leaked content: %q", got)
	}
	if !strings.Contains(got, "bytes") {
		t.Errorf("AuditSummary = %q, want a byte count", got)
	}
	if got := wf.AuditSummary("not json"); !strings.Contains(got, "unparsable") {
		t.Errorf("AuditSummary(bad) = %q, want an unparsable note", got)
	}
}
