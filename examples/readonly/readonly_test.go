package readonly_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/gate"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/tool"
)

// writeFileDefinition is a prepared write tool: it asks the gate for a
// filesystem.write of the canonical target and writes only if approved.
func writeFileDefinition(base string) tool.Definition {
	return tool.NewDefinition("WriteFile", 0, func(context.Context, tool.Bindings) ([]tool.InvokableTool, error) {
		return []tool.InvokableTool{writeFile{base: base}}, nil
	})
}

type writeFile struct{ base string }

func (writeFile) Info(context.Context) (*tool.ToolInfo, error) {
	return &tool.ToolInfo{Name: "WriteFile", Desc: "Write a file", Schema: json.RawMessage(`{"type":"object"}`)}, nil
}

func (w writeFile) PrepareCall(_ context.Context, id uuid.UUID, argsJSON string) (tool.Request, tool.PreparedArtifact, error) {
	var args struct{ Path string }
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil || args.Path == "" {
		return tool.Request{}, nil, errors.New("a non-empty path is required")
	}
	target := filepath.Join(w.base, args.Path)
	return tool.Request{ToolName: "WriteFile", ExecutionID: id.String(), Requirements: []tool.Requirement{{
		Kind: "filesystem.write", Scope: target, Match: target, Description: "write " + args.Path,
	}}}, tool.TokenArtifact{Token: target}, nil
}

func (writeFile) InvokableRun(ctx context.Context, _ string) (*tool.ToolResult, error) {
	call, _ := loop.PreparedCallFromContext(ctx)
	artifact, _ := call.Artifact.(tool.TokenArtifact)
	if err := os.WriteFile(artifact.Token, []byte("written"), 0o600); err != nil {
		return tool.TextResult("error: " + err.Error()), nil
	}
	return tool.TextResult("wrote"), nil
}

func TestReadOnlyAccessReadsInsideRootOnly(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	repo, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	outside, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(repo, "src"), 0o700); err != nil {
		t.Fatal(err)
	}
	for path, text := range map[string]string{
		filepath.Join(repo, "src", "main.go"): "package main",
		filepath.Join(outside, "secret.txt"):  "secret",
	} {
		if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// A symlink inside the root that points outside it resolves outside and is
	// denied: the gate sees the canonical path the tool prepared.
	if err := os.Symlink(filepath.Join(outside, "secret.txt"), filepath.Join(repo, "escape.txt")); err != nil {
		t.Fatal(err)
	}

	model := &scriptedModel{calls: []toolCall{
		{name: "ReadFile", input: `{"path":"src/main.go"}`},
		{name: "ReadFile", input: `{"path":"` + filepath.Join(outside, "secret.txt") + `"}`},
		{name: "ReadFile", input: `{"path":"escape.txt"}`},
		{name: "WriteFile", input: `{"path":"new.txt"}`},
	}}
	agent, err := loop.Define(
		loop.WithName("reader"),
		loop.WithInference(model, fixtureModel()),
		loop.WithTools(readFileDefinition(repo), writeFileDefinition(repo)),
		loop.WithReadOnlyAccess(repo),
	)
	if err != nil {
		t.Fatalf("loop.Define() error = %v", err)
	}
	if err := runOneTurn(ctx, agent, "look around"); err != nil {
		t.Fatalf("runOneTurn() error = %v", err)
	}

	want := []string{
		"package main",
		"error: permission denied [out of scope]: read " + filepath.Join(outside, "secret.txt"),
		"error: permission denied [out of scope]: read escape.txt",
		"error: permission denied [out of scope]: write new.txt",
	}
	if got := model.Results(); !slices.Equal(got, want) {
		t.Fatalf("tool results = %q, want %q", got, want)
	}
	if _, err := os.Stat(filepath.Join(repo, "new.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("write tool ran: stat error = %v", err)
	}
}

// Without any access gate the same read is denied, unchanged from before
// WithReadOnlyAccess existed: a composition that runs gateless on purpose
// keeps failing closed.
func TestNoAccessGateStillDeniesReads(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	repo, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}
	model := &scriptedModel{calls: []toolCall{{name: "ReadFile", input: `{"path":"README.md"}`}}}
	agent, err := loop.Define(
		loop.WithName("reader"),
		loop.WithInference(model, fixtureModel()),
		loop.WithTools(readFileDefinition(repo)),
	)
	if err != nil {
		t.Fatalf("loop.Define() error = %v", err)
	}
	if err := runOneTurn(ctx, agent, "read"); err != nil {
		t.Fatalf("runOneTurn() error = %v", err)
	}
	if got, want := model.Results(), []string{"error: permission denied [unavailable]"}; !slices.Equal(got, want) {
		t.Fatalf("tool results = %q, want %q", got, want)
	}
}

// An approved read must not follow a symlink swapped in after preparation:
// the gate judged the prepared path, and execution goes through a root
// handle that refuses anything resolving outside the root.
func TestReadFileDoesNotFollowSymlinkSwappedInAfterApproval(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		// swap replaces what the prepared path names, pointing it outside.
		swap func(t *testing.T, repo, outside string)
	}{
		{name: "file swapped for a symlink", swap: func(t *testing.T, repo, outside string) {
			target := filepath.Join(repo, "sub", "notes.txt")
			if err := os.Remove(target); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(filepath.Join(outside, "notes.txt"), target); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "parent directory swapped for a symlink", swap: func(t *testing.T, repo, outside string) {
			if err := os.Rename(filepath.Join(repo, "sub"), filepath.Join(repo, "sub.old")); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(outside, filepath.Join(repo, "sub")); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			repo, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			outside, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(filepath.Join(repo, "sub"), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(repo, "sub", "notes.txt"), []byte("inside"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(outside, "notes.txt"), []byte("SECRET"), 0o600); err != nil {
				t.Fatal(err)
			}

			reader := readFile{root: repo}
			id, err := uuid.New()
			if err != nil {
				t.Fatal(err)
			}
			request, artifact, err := reader.PrepareCall(context.Background(), id, `{"path":"sub/notes.txt"}`)
			if err != nil {
				t.Fatalf("PrepareCall() error = %v", err)
			}
			evaluator, err := gate.NewReadOnlyEvaluator(repo)
			if err != nil {
				t.Fatal(err)
			}
			resolution, err := evaluator.Authorize(context.Background(), request)
			if err != nil || !resolution.Approved {
				t.Fatalf("Authorize() = %+v, %v; want the in-root read approved", resolution, err)
			}

			tt.swap(t, repo, outside)

			ctx := loop.WithPreparedCall(context.Background(), tool.PreparedCall{ExecutionID: id, Request: request, Artifact: artifact})
			result, err := reader.InvokableRun(ctx, "")
			if err != nil {
				t.Fatalf("InvokableRun() error = %v", err)
			}
			text := blockText(result.Content)
			if strings.Contains(text, "SECRET") || !strings.HasPrefix(text, "error: ") {
				t.Fatalf("InvokableRun() = %q, want an error and not the outside file", text)
			}
		})
	}
}
