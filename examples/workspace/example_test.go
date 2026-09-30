package workspace_test

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
	"time"

	"github.com/looprig/core/content"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/gate"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/rig"
	"github.com/looprig/harness/pkg/sessionstore"
	"github.com/looprig/harness/pkg/tool"
	"github.com/looprig/inference"
	"github.com/looprig/inference/model"
	"github.com/looprig/inference/stream"
	"github.com/looprig/storage/memstore"
)

// Example_readAndApprovedWrites gives an agent a workspace with one option:
// reads inside the root run, writes inside the root ask for approval, and
// everything else is denied without asking.
//
// Here a headless gate.ApproverFunc answers each write. Leave Approver nil and
// the write instead opens the loop's durable permission gate, answered by the
// TUI, by RespondGate, or, on a Host, by Factory's gate_response. A real
// composition registers the standard file tools from github.com/looprig/tools:
//
//	loop.WithTools(tools.ReadFileDefinition(guard), tools.WriteFileDefinition(guard), tools.EditFileDefinition(guard)),
//	loop.WithWorkspaceAccess(loop.WorkspaceAccess{Roots: []string{repoRoot}}),
func Example_readAndApprovedWrites() {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	repo, err := os.MkdirTemp("", "workspace-example-")
	if err != nil {
		panic(err)
	}
	defer os.RemoveAll(repo)
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("hello from the repo"), 0o600); err != nil {
		panic(err)
	}

	// The scripted model reads README.md, writes NOTES.md, tries to write
	// .env (the approver refuses) and then /etc/hosts (outside the root).
	model := &scriptedModel{calls: []toolCall{
		{name: "ReadFile", input: `{"path":"README.md"}`},
		{name: "WriteFile", input: `{"path":"NOTES.md","content":"read the README"}`},
		{name: "WriteFile", input: `{"path":".env","content":"TOKEN=x"}`},
		{name: "WriteFile", input: `{"path":"/etc/hosts","content":"x"}`},
	}}
	approver := gate.ApproverFunc(func(_ context.Context, prompt gate.ApprovalPrompt) (gate.ApprovalAction, error) {
		for _, requirement := range prompt.Unmet {
			if filepath.Base(requirement.Match) == ".env" {
				return gate.ApprovalDeny, nil
			}
		}
		return gate.ApprovalApprove, nil
	})
	agent, err := loop.Define(
		loop.WithName("editor"),
		loop.WithInference(model, fixtureModel()),
		loop.WithTools(readFileDefinition(repo), writeFileDefinition(repo)),
		loop.WithWorkspaceAccess(loop.WorkspaceAccess{Roots: []string{repo}, Approver: approver}),
	)
	if err != nil {
		panic(err)
	}
	if err := runOneTurn(ctx, agent, "Summarize the README into NOTES.md"); err != nil {
		panic(err)
	}
	for _, result := range model.Results() {
		fmt.Println(result)
	}
	notes, err := os.ReadFile(filepath.Join(repo, "NOTES.md"))
	fmt.Printf("NOTES.md: %q %v\n", notes, err)
	// Output:
	// hello from the repo
	// wrote NOTES.md
	// error: permission denied [not authorized]: write .env
	// error: permission denied [out of scope]: write /etc/hosts
	// NOTES.md: "read the README" <nil>
}

// runOneTurn composes a rig around one loop, submits text, and waits for the
// turn to end.
func runOneTurn(ctx context.Context, agent loop.Definition, text string) error {
	store, err := sessionstore.Open(memstore.New())
	if err != nil {
		return err
	}
	harness, err := rig.Define(rig.WithLoops(agent), rig.WithPrimers(string(agent.Name())), rig.WithSessionStore(store))
	if err != nil {
		return err
	}
	live, err := harness.NewSession(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = live.Shutdown(context.Background()) }()
	events, err := live.SubscribeEvents(event.EventFilter{Enduring: event.LoopScope{All: true}})
	if err != nil {
		return err
	}
	defer func() { _ = events.Close() }()
	if _, err := live.Submit(ctx, []content.Block{&content.TextBlock{Text: text}}); err != nil {
		return err
	}
	for delivery := range events.Events() {
		if delivery.Event.EndsTurn() {
			return nil
		}
	}
	return errors.Join(errors.New("event stream ended before the turn"), events.Err())
}

// canonicalTarget resolves path (relative to root) to the canonical path the
// gate judges: symlinks in the deepest existing ancestor are resolved, so a
// not-yet-existing file is named where it would really be created.
func canonicalTarget(root, path string) (string, error) {
	if !filepath.IsAbs(path) {
		path = filepath.Join(root, path)
	}
	path = filepath.Clean(path)
	missing := ""
	for dir := path; ; dir = filepath.Dir(dir) {
		resolved, err := filepath.EvalSymlinks(dir)
		if err == nil {
			return filepath.Join(resolved, missing), nil
		}
		if !errors.Is(err, os.ErrNotExist) || filepath.Dir(dir) == dir {
			return "", err
		}
		missing = filepath.Join(filepath.Base(dir), missing)
	}
}

// underRoot opens an os.Root bound to root and returns the approved target
// relative to it. Executing through the root handle refuses any path, symlink
// or ".." that resolves outside it, including one swapped in after approval:
// the gate decides what may be touched, the tool makes sure that is what is.
func underRoot(root, target string) (*os.Root, string, error) {
	rel, err := filepath.Rel(root, target)
	if err != nil || !filepath.IsLocal(rel) {
		return nil, "", errors.New("path is outside the tool root")
	}
	handle, err := os.OpenRoot(root)
	if err != nil {
		return nil, "", err
	}
	return handle, rel, nil
}

func readFileDefinition(base string) tool.Definition {
	return tool.NewDefinition("ReadFile", 0, func(context.Context, tool.Bindings) ([]tool.InvokableTool, error) {
		root, err := filepath.EvalSymlinks(base)
		if err != nil {
			return nil, err
		}
		return []tool.InvokableTool{fileTool{name: "ReadFile", kind: "filesystem.read", root: root}}, nil
	})
}

func writeFileDefinition(base string) tool.Definition {
	return tool.NewDefinition("WriteFile", 0, func(context.Context, tool.Bindings) ([]tool.InvokableTool, error) {
		root, err := filepath.EvalSymlinks(base)
		if err != nil {
			return nil, err
		}
		return []tool.InvokableTool{fileTool{name: "WriteFile", kind: "filesystem.write", root: root}}, nil
	})
}

// fileTool is a minimal prepared file tool, modelled on the standard tools.
// PrepareCall asks the gate for kind on the canonical target; a write also
// offers the exact path as an "Approve always" candidate. InvokableRun then
// touches only the approved path, through an os.Root bound to root.
type fileTool struct {
	name string
	kind string
	root string
}

type fileArgs struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

func (f fileTool) Info(context.Context) (*tool.ToolInfo, error) {
	return &tool.ToolInfo{Name: f.name, Desc: f.name + " a text file", Schema: json.RawMessage(
		`{"type":"object","properties":{"path":{"type":"string"},"content":{"type":"string"}},"required":["path"]}`)}, nil
}

func (f fileTool) PrepareCall(_ context.Context, id uuid.UUID, argsJSON string) (tool.Request, tool.PreparedArtifact, error) {
	var args fileArgs
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil || args.Path == "" {
		return tool.Request{}, nil, errors.New("a non-empty path is required")
	}
	target, err := canonicalTarget(f.root, args.Path)
	if err != nil {
		return tool.Request{}, nil, err
	}
	verb := strings.ToLower(strings.TrimSuffix(f.name, "File"))
	requirement := tool.Requirement{Kind: f.kind, Scope: target, Match: target, Description: verb + " " + args.Path}
	if f.kind == "filesystem.write" {
		requirement.Candidates = []tool.RuleCandidate{{Kind: f.kind, Match: target, Description: requirement.Description}}
	}
	return tool.Request{ToolName: f.name, ExecutionID: id.String(), Requirements: []tool.Requirement{requirement}},
		tool.TokenArtifact{Token: target}, nil
}

func (f fileTool) InvokableRun(ctx context.Context, argsJSON string) (*tool.ToolResult, error) {
	call, ok := loop.PreparedCallFromContext(ctx)
	artifact, isToken := call.Artifact.(tool.TokenArtifact)
	if !ok || !isToken {
		return tool.TextResult("error: missing prepared call"), nil
	}
	root, rel, err := underRoot(f.root, artifact.Token)
	if err != nil {
		return tool.TextResult("error: " + err.Error()), nil
	}
	defer func() { _ = root.Close() }()
	if f.kind == "filesystem.read" {
		data, err := root.ReadFile(rel)
		if err != nil {
			return tool.TextResult("error: " + err.Error()), nil
		}
		return tool.TextResult(string(data)), nil
	}
	var args fileArgs
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return tool.TextResult("error: " + err.Error()), nil
	}
	if err := root.WriteFile(rel, []byte(args.Content), 0o600); err != nil {
		return tool.TextResult("error: " + err.Error()), nil
	}
	return tool.TextResult("wrote " + rel), nil
}

type toolCall struct{ name, input string }

// scriptedModel issues one scripted tool call per step, records the text of
// every tool result it is sent, then ends the turn.
type scriptedModel struct {
	calls []toolCall

	mu      sync.Mutex
	step    int
	results []string
}

func (*scriptedModel) Invoke(context.Context, inference.Request) (*inference.Response, error) {
	return nil, errors.New("scripted model only streams")
}

func (m *scriptedModel) Stream(_ context.Context, request inference.Request) (*stream.StreamReader[content.Chunk], error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if last := len(request.Messages) - 1; m.step > 0 && last >= 0 {
		if result, ok := request.Messages[last].(*content.ToolResultMessage); ok {
			m.results = append(m.results, blockText(result.Blocks))
		}
	}
	chunks := []content.Chunk{&content.TextChunk{Text: "done"}}
	finish := stream.FinishReasonStop
	if m.step < len(m.calls) {
		call := m.calls[m.step]
		chunks = []content.Chunk{&content.ToolUseChunk{ID: fmt.Sprintf("call-%d", m.step), Name: call.name, InputJSON: call.input}}
		finish = stream.FinishReasonToolUse
	}
	m.step++
	next := 0
	return stream.NewStreamReaderWithResult(func() (content.Chunk, error) {
		if next == len(chunks) {
			return nil, io.EOF
		}
		next++
		return chunks[next-1], nil
	}, nil, func() (stream.StreamResult, bool, error) {
		return stream.StreamResult{FinishReason: finish}, true, nil
	}), nil
}

// Results returns the text of every tool result the model received, in order.
func (m *scriptedModel) Results() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.results...)
}

func blockText(blocks []content.Block) string {
	var text strings.Builder
	for _, block := range blocks {
		if b, ok := block.(*content.TextBlock); ok {
			text.WriteString(b.Text)
		}
	}
	return text.String()
}

func fixtureModel() model.Model {
	return model.Model{Provider: "offline", APIFormat: model.APIFormatOpenAI, BaseURL: "http://localhost", Name: "fixture"}
}
