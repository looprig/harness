package rig

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/looprig/core/content"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/gate"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/session"
	"github.com/looprig/harness/pkg/sessionstore"
	"github.com/looprig/harness/pkg/tool"
	"github.com/looprig/inference"
	"github.com/looprig/inference/stream"
)

// workspaceWriteLLM asks for one WriteFile of the path named by the user's
// latest message, then ends the turn once it sees the tool result.
type workspaceWriteLLM struct {
	mu      sync.Mutex
	calls   int
	results []string
}

func (*workspaceWriteLLM) Invoke(context.Context, inference.Request) (*inference.Response, error) {
	return nil, errors.New("workspace write llm only streams")
}

func (l *workspaceWriteLLM) Stream(_ context.Context, request inference.Request) (*stream.StreamReader[content.Chunk], error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.calls++
	chunks := []content.Chunk{&content.TextChunk{Text: "done"}}
	last := request.Messages[len(request.Messages)-1]
	switch message := last.(type) {
	case *content.ToolResultMessage:
		l.results = append(l.results, workspaceBlockText(message.Blocks))
	case *content.UserMessage:
		input, _ := json.Marshal(map[string]string{"path": workspaceBlockText(message.Blocks)})
		chunks = []content.Chunk{&content.ToolUseChunk{ID: fmt.Sprintf("write-%d", l.calls), Name: "WriteFile", InputJSON: string(input)}}
	}
	index := 0
	return stream.NewStreamReader(func() (content.Chunk, error) {
		if index == len(chunks) {
			return nil, io.EOF
		}
		index++
		return chunks[index-1], nil
	}, nil), nil
}

func (l *workspaceWriteLLM) lastResult() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.results) == 0 {
		return ""
	}
	return l.results[len(l.results)-1]
}

func workspaceBlockText(blocks []content.Block) string {
	var text strings.Builder
	for _, block := range blocks {
		if b, ok := block.(*content.TextBlock); ok {
			text.WriteString(b.Text)
		}
	}
	return text.String()
}

// workspaceWriteTool writes "ok" to a path within root. It prepares the
// canonical target and writes only the approved path through an os.Root.
type workspaceWriteTool struct {
	root string
	runs *atomic.Int32
}

func (workspaceWriteTool) Info(context.Context) (*tool.ToolInfo, error) {
	return &tool.ToolInfo{Name: "WriteFile", Desc: "Write a file", Schema: json.RawMessage(`{"type":"object"}`)}, nil
}

func (w workspaceWriteTool) PrepareCall(_ context.Context, id uuid.UUID, argsJSON string) (tool.Request, tool.PreparedArtifact, error) {
	var args struct{ Path string }
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil || args.Path == "" {
		return tool.Request{}, nil, errors.New("a non-empty path is required")
	}
	target := args.Path
	if !filepath.IsAbs(target) {
		target = filepath.Join(w.root, target)
	}
	return tool.Request{ToolName: "WriteFile", ExecutionID: id.String(), Requirements: []tool.Requirement{{
		Kind: "filesystem.write", Scope: target, Match: target, Description: "write " + target,
		Candidates: []tool.RuleCandidate{{Kind: "filesystem.write", Match: target, Description: "write " + target}},
	}}}, tool.TokenArtifact{Token: target}, nil
}

func (w workspaceWriteTool) InvokableRun(ctx context.Context, _ string) (*tool.ToolResult, error) {
	call, _ := loop.PreparedCallFromContext(ctx)
	artifact, _ := call.Artifact.(tool.TokenArtifact)
	rel, err := filepath.Rel(w.root, artifact.Token)
	if err != nil || !filepath.IsLocal(rel) {
		return tool.TextResult("error: outside the root"), nil
	}
	root, err := os.OpenRoot(w.root)
	if err != nil {
		return tool.TextResult("error: " + err.Error()), nil
	}
	defer func() { _ = root.Close() }()
	if err := root.WriteFile(rel, []byte("ok"), 0o600); err != nil {
		return tool.TextResult("error: " + err.Error()), nil
	}
	w.runs.Add(1)
	return tool.TextResult("wrote"), nil
}

type workspaceFixture struct {
	root  string
	runs  atomic.Int32
	llm   *workspaceWriteLLM
	store *sessionstore.Store
}

func newWorkspaceFixture(t *testing.T) *workspaceFixture {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return &workspaceFixture{root: root, llm: &workspaceWriteLLM{}, store: sessionStoreT(t)}
}

func (f *workspaceFixture) rig(t *testing.T, access loop.WorkspaceAccess) *Rig {
	t.Helper()
	if access.Roots == nil {
		access.Roots = []string{f.root}
	}
	definition, err := loop.Define(
		loop.WithName("agent"),
		loop.WithInference(f.llm, validModel("workspace-access")),
		loop.WithTools(tool.NewDefinition("WriteFile", 0, func(context.Context, tool.Bindings) ([]tool.InvokableTool, error) {
			return []tool.InvokableTool{workspaceWriteTool{root: f.root, runs: &f.runs}}, nil
		})),
		loop.WithWorkspaceAccess(access),
	)
	if err != nil {
		t.Fatalf("loop.Define: %v", err)
	}
	r, err := Define(WithLoops(definition), WithPrimers("agent"), WithSessionStore(f.store))
	if err != nil {
		t.Fatalf("Define: %v", err)
	}
	return r
}

// writeTurn submits path as one turn, answers every opened gate with action,
// and returns how many gates opened.
func writeTurn(t *testing.T, ctx context.Context, live session.SessionController, path string, action gate.ApprovalAction) int {
	t.Helper()
	sub, err := live.SubscribeEvents(event.EventFilter{Enduring: event.LoopScope{All: true}})
	if err != nil {
		t.Fatalf("SubscribeEvents: %v", err)
	}
	defer sub.Close()
	if _, err := live.Submit(ctx, []content.Block{&content.TextBlock{Text: path}}); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	opened := 0
	for {
		select {
		case delivery := <-sub.Events():
			if gateOpened, ok := delivery.Event.(event.GateOpened); ok {
				opened++
				if err := live.RespondGate(ctx, gate.GateResponse{
					GateID: gateOpened.Gate.ID,
					Action: string(action),
					Source: gate.ResponseSource{Kind: gate.ResponseFromUser},
				}); err != nil {
					t.Fatalf("RespondGate: %v", err)
				}
			}
			if delivery.Event.EndsTurn() {
				if failed, ok := delivery.Event.(event.TurnFailed); ok {
					t.Fatalf("turn %q failed: %v", path, failed.Err)
				}
				return opened
			}
		case <-ctx.Done():
			t.Fatalf("turn %q timed out", path)
		}
	}
}

// TestWorkspaceAccessWriteGateAndSessionScopedAlways drives the default
// configuration end to end: a write parks on the loop's durable permission
// gate and proceeds on RespondGate; "always" spares the prompt for that file
// for the rest of that session only; and restoring the session after an
// "always" reports no configuration drift.
func TestWorkspaceAccessWriteGateAndSessionScopedAlways(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	f := newWorkspaceFixture(t)
	r := f.rig(t, loop.WorkspaceAccess{})

	first, err := r.NewSession(ctx)
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	id := first.SessionID()

	if opened := writeTurn(t, ctx, first, "a.txt", gate.ApprovalApproveAlwaysWorkspace); opened != 1 {
		t.Fatalf("first write opened %d gates, want 1", opened)
	}
	if f.runs.Load() != 1 {
		t.Fatalf("tool runs = %d after an approved write, want 1", f.runs.Load())
	}
	if _, err := os.Stat(filepath.Join(f.root, "a.txt")); err != nil {
		t.Fatalf("approved write did not land: %v", err)
	}
	if opened := writeTurn(t, ctx, first, "a.txt", gate.ApprovalDeny); opened != 0 {
		t.Fatalf("remembered write opened %d gates, want 0", opened)
	}
	if f.runs.Load() != 2 {
		t.Fatalf("tool runs = %d after a remembered write, want 2", f.runs.Load())
	}
	if opened := writeTurn(t, ctx, first, "b.txt", gate.ApprovalDeny); opened != 1 {
		t.Fatalf("write to another file opened %d gates, want 1", opened)
	}
	if f.runs.Load() != 2 {
		t.Fatal("a denied write ran")
	}
	if _, err := os.Stat(filepath.Join(f.root, "b.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("denied write landed: %v", err)
	}
	if opened := writeTurn(t, ctx, first, filepath.Join(filepath.Dir(f.root), "outside.txt"), gate.ApprovalApprove); opened != 0 {
		t.Fatalf("write outside the root opened %d gates, want a deny without a prompt", opened)
	}
	if got := f.llm.lastResult(); !strings.Contains(got, "permission denied") {
		t.Fatalf("outside write result = %q, want a permission denial", got)
	}

	other, err := r.NewSession(ctx)
	if err != nil {
		t.Fatalf("NewSession(other): %v", err)
	}
	defer func() { _ = other.Shutdown(context.Background()) }()
	if opened := writeTurn(t, ctx, other, "a.txt", gate.ApprovalApprove); opened != 1 {
		t.Fatalf("another session's write opened %d gates, want 1: always must not cross sessions", opened)
	}

	if err := first.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	restored, err := f.rig(t, loop.WorkspaceAccess{}).RestoreSession(ctx, id)
	if err != nil {
		t.Fatalf("RestoreSession after an always answer: %v", err)
	}
	defer func() { _ = restored.Shutdown(context.Background()) }()
	for _, ev := range replayRigEvents(t, f.store, id) {
		if adopted, ok := ev.(event.ConfigurationAdopted); ok {
			t.Fatalf("restore after an always answer adopted a configuration change: %+v", adopted.Drift)
		}
	}
	// Default rules are in memory: a restored session asks again.
	if opened := writeTurn(t, ctx, restored, "a.txt", gate.ApprovalApprove); opened != 1 {
		t.Fatalf("restored session's write opened %d gates, want 1", opened)
	}
}

// TestWorkspaceAccessRestoreWithOtherRootsIsDrift is the control for the
// no-drift assertion above: the same restore with different roots adopts a
// configuration change, so that assertion can fail.
func TestWorkspaceAccessRestoreWithOtherRootsIsDrift(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	f := newWorkspaceFixture(t)
	live, err := f.rig(t, loop.WorkspaceAccess{}).NewSession(ctx)
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	id := live.SessionID()
	if err := live.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	restored, err := f.rig(t, loop.WorkspaceAccess{Roots: []string{f.root, t.TempDir()}}).RestoreSession(ctx, id)
	if err == nil {
		defer func() { _ = restored.Shutdown(context.Background()) }()
		for _, ev := range replayRigEvents(t, f.store, id) {
			if _, ok := ev.(event.ConfigurationAdopted); ok {
				return
			}
		}
		t.Fatal("restore with different roots adopted no configuration change")
	}
	var rejected *session.RestoreRejectedError
	if !errors.As(err, &rejected) {
		t.Fatalf("RestoreSession with different roots = %T %v, want drift", err, err)
	}
}

// TestWorkspaceAccessHeadlessApproverFunc answers writes in-process: no gate
// is opened and the approved write runs.
func TestWorkspaceAccessHeadlessApproverFunc(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	f := newWorkspaceFixture(t)
	var prompts atomic.Int32
	r := f.rig(t, loop.WorkspaceAccess{Approver: gate.ApproverFunc(func(_ context.Context, prompt gate.ApprovalPrompt) (gate.ApprovalAction, error) {
		prompts.Add(1)
		if len(prompt.Unmet) != 1 || prompt.Unmet[0].Kind != "filesystem.write" {
			return gate.ApprovalDeny, nil
		}
		return gate.ApprovalApprove, nil
	})})
	live, err := r.NewSession(ctx)
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	defer func() { _ = live.Shutdown(context.Background()) }()
	if opened := writeTurn(t, ctx, live, "a.txt", gate.ApprovalDeny); opened != 0 {
		t.Fatalf("headless approver opened %d gates, want 0", opened)
	}
	if prompts.Load() != 1 || f.runs.Load() != 1 {
		t.Fatalf("prompts = %d, runs = %d; want the approver asked once and the write run", prompts.Load(), f.runs.Load())
	}
}
