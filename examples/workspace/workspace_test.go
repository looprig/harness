package workspace_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/looprig/harness/pkg/gate"
	"github.com/looprig/harness/pkg/loop"
)

func canonicalTempDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

// runWrites runs one turn of scripted writes against repo and returns the
// tool results and the number of approval prompts.
func runWrites(t *testing.T, repo string, approve func(gate.ApprovalPrompt), calls ...toolCall) ([]string, int32) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var prompts atomic.Int32
	model := &scriptedModel{calls: calls}
	agent, err := loop.Define(
		loop.WithName("editor"),
		loop.WithInference(model, fixtureModel()),
		loop.WithTools(readFileDefinition(repo), writeFileDefinition(repo)),
		loop.WithWorkspaceAccess(loop.WorkspaceAccess{Roots: []string{repo}, Approver: gate.ApproverFunc(
			func(_ context.Context, prompt gate.ApprovalPrompt) (gate.ApprovalAction, error) {
				prompts.Add(1)
				if approve != nil {
					approve(prompt)
				}
				return gate.ApprovalApprove, nil
			})}),
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := runOneTurn(ctx, agent, "write"); err != nil {
		t.Fatal(err)
	}
	return model.Results(), prompts.Load()
}

// TestWriteThroughAnInRootSymlinkLeadingOutIsDenied: the tool prepares the
// canonical target, so a write through a symlink inside the root that points
// outside it is judged where it would land, and denied without a prompt.
func TestWriteThroughAnInRootSymlinkLeadingOutIsDenied(t *testing.T) {
	t.Parallel()
	repo, outside := canonicalTempDir(t), canonicalTempDir(t)
	if err := os.Symlink(outside, filepath.Join(repo, "escape")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	results, prompts := runWrites(t, repo, nil,
		toolCall{name: "WriteFile", input: `{"path":"escape/x.txt","content":"x"}`},
		toolCall{name: "WriteFile", input: `{"path":"sub/../../x.txt","content":"x"}`},
	)
	if prompts != 0 {
		t.Fatalf("prompts = %d, want out-of-root writes denied without asking", prompts)
	}
	for i, result := range results {
		if !strings.Contains(result, "permission denied [out of scope]") {
			t.Fatalf("result %d = %q, want an out-of-scope denial", i, result)
		}
	}
	if _, err := os.Stat(filepath.Join(outside, "x.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a denied write landed outside the root: %v", err)
	}
}

// TestWriteSwappedForASymlinkAfterApprovalStaysInRoot: the gate is lexical,
// so the tool must confine its own I/O. Here the approver swaps the approved
// file's parent directory for a symlink leading outside before answering; the
// os.Root-bound write refuses to follow it.
func TestWriteSwappedForASymlinkAfterApprovalStaysInRoot(t *testing.T) {
	t.Parallel()
	repo, outside := canonicalTempDir(t), canonicalTempDir(t)
	sub := filepath.Join(repo, "sub")
	if err := os.Mkdir(sub, 0o700); err != nil {
		t.Fatal(err)
	}
	swap := func(gate.ApprovalPrompt) {
		if err := os.Remove(sub); err != nil {
			t.Error(err)
		}
		if err := os.Symlink(outside, sub); err != nil {
			t.Error(err)
		}
	}
	results, prompts := runWrites(t, repo, swap, toolCall{name: "WriteFile", input: `{"path":"sub/x.txt","content":"x"}`})
	if prompts != 1 {
		t.Fatalf("prompts = %d, want the in-root write to ask once", prompts)
	}
	if len(results) != 1 || !strings.HasPrefix(results[0], "error:") {
		t.Fatalf("results = %q, want the swapped write to fail", results)
	}
	if _, err := os.Stat(filepath.Join(outside, "x.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the approved write followed a swapped symlink out of the root: %v", err)
	}
}
