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

// runEditFile invokes EditFile and extracts the single text block.
func runEditFile(t *testing.T, root string, args map[string]any) string {
	t.Helper()
	b, err := json.Marshal(args)
	if err != nil {
		t.Fatalf("marshal args: %v", err)
	}
	res, err := NewEditFile(root).InvokableRun(context.Background(), string(b))
	if err != nil {
		t.Fatalf("InvokableRun returned a Go error %v; edit tool returns tool-result strings", err)
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

func TestEditFileInfo(t *testing.T) {
	t.Parallel()
	info, err := NewEditFile(t.TempDir()).Info(context.Background())
	if err != nil {
		t.Fatalf("Info() error = %v", err)
	}
	if info.Name != "EditFile" {
		t.Errorf("Info().Name = %q, want %q", info.Name, "EditFile")
	}
	var schema map[string]json.RawMessage
	if err := json.Unmarshal(info.Schema, &schema); err != nil {
		t.Fatalf("Schema is not a JSON object: %v", err)
	}
}

func TestEditFile(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		seed        string // initial file body ("" means do not create the file)
		args        map[string]any
		wantErr     bool
		wantBody    string   // expected on-disk body when !wantErr
		wantContain []string // substrings the result string must contain
	}{
		{
			name:        "single unique match is replaced",
			seed:        "alpha\nbravo\ncharlie\n",
			args:        map[string]any{"path": "f.txt", "old": "bravo", "new": "BRAVO"},
			wantBody:    "alpha\nBRAVO\ncharlie\n",
			wantContain: []string{"- bravo", "+ BRAVO"},
		},
		{
			name:    "zero matches is not-found error",
			seed:    "alpha\nbravo\n",
			args:    map[string]any{"path": "f.txt", "old": "zulu", "new": "X"},
			wantErr: true,
		},
		{
			name:    "two matches without replace_all is ambiguous",
			seed:    "x\nx\n",
			args:    map[string]any{"path": "f.txt", "old": "x", "new": "y"},
			wantErr: true,
		},
		{
			name:     "two matches with replace_all replaces all",
			seed:     "x\nx\nother\n",
			args:     map[string]any{"path": "f.txt", "old": "x", "new": "y", "replace_all": true},
			wantBody: "y\ny\nother\n",
		},
		{
			name:     "replace_all with a single match still works",
			seed:     "only-one\n",
			args:     map[string]any{"path": "f.txt", "old": "only-one", "new": "two", "replace_all": true},
			wantBody: "two\n",
		},
		{
			name:    "missing file is an error",
			seed:    "", // not created
			args:    map[string]any{"path": "nope.txt", "old": "a", "new": "b"},
			wantErr: true,
		},
		{
			name:    "empty old is rejected",
			seed:    "hello\n",
			args:    map[string]any{"path": "f.txt", "old": "", "new": "b"},
			wantErr: true,
		},
		{
			name:    "escape path is rejected",
			seed:    "",
			args:    map[string]any{"path": "../f.txt", "old": "a", "new": "b"},
			wantErr: true,
		},
		{
			name:    "missing path is rejected",
			seed:    "",
			args:    map[string]any{"old": "a", "new": "b"},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			if tt.seed != "" {
				if err := os.WriteFile(filepath.Join(root, "f.txt"), []byte(tt.seed), 0o600); err != nil {
					t.Fatalf("seed: %v", err)
				}
			}
			out := runEditFile(t, root, tt.args)
			gotErr := strings.HasPrefix(out, "error:")
			if gotErr != tt.wantErr {
				t.Fatalf("result = %q, wantErr = %v", out, tt.wantErr)
			}
			if tt.wantErr {
				return
			}
			if tt.wantBody != "" {
				got, err := os.ReadFile(filepath.Join(root, "f.txt"))
				if err != nil {
					t.Fatalf("read edited file: %v", err)
				}
				if string(got) != tt.wantBody {
					t.Errorf("on-disk body = %q, want %q", got, tt.wantBody)
				}
			}
			for _, sub := range tt.wantContain {
				if !strings.Contains(out, sub) {
					t.Errorf("result %q missing %q", out, sub)
				}
			}
		})
	}
}

// TestEditFileSymlinkFinalComponentRejected ensures EditFile, like ReadFile,
// REFUSES to follow a final-component symlink (even one that points to an
// in-workspace regular file): the open is on the LEXICAL joined path with
// O_NOFOLLOW, so the symlink's target is never read and never modified.
func TestEditFileSymlinkFinalComponentRejected(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	// target is a real in-workspace file; link.txt -> target (final-component
	// symlink, both ends inside the workspace so containment passes).
	const targetBody = "alpha\nbravo\ncharlie\n"
	target := filepath.Join(root, "target.txt")
	if err := os.WriteFile(target, []byte(targetBody), 0o600); err != nil {
		t.Fatalf("seed target: %v", err)
	}
	if err := os.Symlink(target, filepath.Join(root, "link.txt")); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}

	out := runEditFile(t, root, map[string]any{"path": "link.txt", "old": "bravo", "new": "BRAVO"})
	if !strings.HasPrefix(out, "error:") {
		t.Fatalf("edit via final-component symlink = %q, want an error", out)
	}
	// The symlink target must be untouched (the edit did not follow the link).
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read target: %v", err)
	}
	if string(got) != targetBody {
		t.Fatalf("symlink target was modified: %q, want %q", got, targetBody)
	}
}

// TestEditFileDiffPreview verifies the result carries a unified-ish diff header
// and the changed lines.
func TestEditFileDiffPreview(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "f.txt"), []byte("one\ntwo\nthree\n"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	out := runEditFile(t, root, map[string]any{"path": "f.txt", "old": "two", "new": "TWO"})
	for _, want := range []string{"--- a/f.txt", "+++ b/f.txt", "- two", "+ TWO"} {
		if !strings.Contains(out, want) {
			t.Errorf("diff preview %q missing %q", out, want)
		}
	}
}

func TestEditFileWriteTarget(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	ef := NewEditFile(root)
	key, ok, err := ef.WriteTarget(`{"path":"sub/x.txt","old":"a","new":"b"}`)
	if err != nil || !ok {
		t.Fatalf("WriteTarget = (%q, %v, %v), want (path, true, nil)", key, ok, err)
	}
	want := resolvedJoin(t, root, filepath.Join("sub", "x.txt"))
	if key != want {
		t.Errorf("WriteTarget key = %q, want %q", key, want)
	}
	if _, ok, err := ef.WriteTarget(`{"path":"../x","old":"a","new":"b"}`); ok || err == nil {
		t.Errorf("WriteTarget(escape) = (ok=%v, err=%v), want (false, non-nil)", ok, err)
	}
}

func TestEditFileBuildRequest(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	req, err := NewEditFile(root).BuildRequest(`{"path":"x.txt","old":"secret-old","new":"secret-new"}`, nil)
	if err != nil {
		t.Fatalf("BuildRequest err = %v", err)
	}
	fw, ok := req.(tool.FileWriteRequest)
	if !ok {
		t.Fatalf("request type = %T, want tool.FileWriteRequest", req)
	}
	if !strings.HasSuffix(fw.Path, "x.txt") {
		t.Errorf("FileWriteRequest.Path = %q, want it to end with x.txt", fw.Path)
	}
	if strings.Contains(fw.Description(), "secret-old") || strings.Contains(fw.Description(), "secret-new") {
		t.Errorf("request Description leaked substrings: %q", fw.Description())
	}
}

func TestEditFileAuditSummary(t *testing.T) {
	t.Parallel()
	ef := NewEditFile(t.TempDir())
	got := ef.AuditSummary(`{"path":"a/b.txt","old":"secret-old","new":"secret-new"}`)
	if !strings.Contains(got, "a/b.txt") {
		t.Errorf("AuditSummary = %q, want the path", got)
	}
	if strings.Contains(got, "secret-old") || strings.Contains(got, "secret-new") {
		t.Errorf("AuditSummary leaked substrings: %q", got)
	}
}
