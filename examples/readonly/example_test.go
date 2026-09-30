package readonly_test

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
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/rig"
	"github.com/looprig/harness/pkg/sessionstore"
	"github.com/looprig/harness/pkg/tool"
	"github.com/looprig/inference"
	"github.com/looprig/inference/model"
	"github.com/looprig/inference/stream"
	"github.com/looprig/storage/memstore"
)

// Example_readOnlyTools gives an agent read-only file access with one option.
// Without an access gate every tool call fails closed; loop.WithReadOnlyAccess
// lets reads inside the named roots run and denies everything else.
//
// A real composition registers the standard read tools from
// github.com/looprig/tools instead of the small readFile tool below:
//
//	loop.WithTools(tools.GlobDefinition(guard), tools.GrepDefinition(guard), tools.ReadFileDefinition(guard)),
//	loop.WithReadOnlyAccess(repoRoot),
func Example_readOnlyTools() {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	repo, err := os.MkdirTemp("", "readonly-example-")
	if err != nil {
		panic(err)
	}
	defer os.RemoveAll(repo)
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("hello from the repo"), 0o600); err != nil {
		panic(err)
	}

	// The scripted model asks to read README.md, then /etc/hosts.
	model := &scriptedModel{calls: []toolCall{
		{name: "ReadFile", input: `{"path":"README.md"}`},
		{name: "ReadFile", input: `{"path":"/etc/hosts"}`},
	}}
	agent, err := loop.Define(
		loop.WithName("reader"),
		loop.WithInference(model, fixtureModel()),
		loop.WithTools(readFileDefinition(repo)),
		loop.WithReadOnlyAccess(repo),
	)
	if err != nil {
		panic(err)
	}
	if err := runOneTurn(ctx, agent, "Read the README"); err != nil {
		panic(err)
	}
	for _, result := range model.Results() {
		fmt.Println(result)
	}
	// Output:
	// hello from the repo
	// error: permission denied [out of scope]: read /etc/hosts
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

// readFileDefinition is a minimal prepared read tool. Like the standard tools,
// it resolves the path first and asks the access gate for a filesystem.read
// of that canonical path; it runs only if the gate approves.
func readFileDefinition(base string) tool.Definition {
	return tool.NewDefinition("ReadFile", 0, func(context.Context, tool.Bindings) ([]tool.InvokableTool, error) {
		return []tool.InvokableTool{readFile{base: base}}, nil
	})
}

type readFile struct{ base string }

func (readFile) Info(context.Context) (*tool.ToolInfo, error) {
	return &tool.ToolInfo{Name: "ReadFile", Desc: "Read a text file", Schema: json.RawMessage(
		`{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}`)}, nil
}

func (r readFile) PrepareCall(_ context.Context, id uuid.UUID, argsJSON string) (tool.Request, tool.PreparedArtifact, error) {
	var args struct{ Path string }
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil || args.Path == "" {
		return tool.Request{}, nil, errors.New("a non-empty path is required")
	}
	path := args.Path
	if !filepath.IsAbs(path) {
		path = filepath.Join(r.base, path)
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return tool.Request{}, nil, err
	}
	return tool.Request{
		ToolName:    "ReadFile",
		ExecutionID: id.String(),
		Requirements: []tool.Requirement{{
			Kind: "filesystem.read", Scope: resolved, Match: resolved, Description: "read " + args.Path,
		}},
	}, tool.TokenArtifact{Token: resolved}, nil
}

func (readFile) InvokableRun(ctx context.Context, _ string) (*tool.ToolResult, error) {
	call, ok := loop.PreparedCallFromContext(ctx)
	artifact, isToken := call.Artifact.(tool.TokenArtifact)
	if !ok || !isToken {
		return tool.TextResult("error: missing prepared call"), nil
	}
	data, err := os.ReadFile(artifact.Token)
	if err != nil {
		return tool.TextResult("error: " + err.Error()), nil
	}
	return tool.TextResult(string(data)), nil
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
