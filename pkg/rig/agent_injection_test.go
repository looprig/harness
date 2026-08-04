package rig

import (
	"context"
	"io"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/looprig/core/content"
	"github.com/looprig/harness/pkg/identity"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/sessionstore"
	"github.com/looprig/inference"
	stream "github.com/looprig/inference/stream"
	"github.com/looprig/storage/memstore"
)

// captureLLM records the tools offered on the FIRST request and returns a one-chunk
// stream so the turn completes. It is the end-to-end observable for what the model
// actually receives (the tool schemas), proving the rig derives + injects the agent
// collaboration tools from the parent definition's delegates.
type captureLLM struct {
	mu    sync.Mutex
	tools []inference.Tool
	got   chan struct{}
	once  sync.Once
	final string
}

func (c *captureLLM) Invoke(context.Context, inference.Request) (*inference.Response, error) {
	return nil, nil
}

func (c *captureLLM) Stream(_ context.Context, req inference.Request) (*stream.StreamReader[content.Chunk], error) {
	c.mu.Lock()
	c.tools = req.Tools
	c.mu.Unlock()
	c.once.Do(func() { close(c.got) })
	i := 0
	chunks := []content.Chunk{&content.TextChunk{Text: c.final}}
	next := func() (content.Chunk, error) {
		if i < len(chunks) {
			ch := chunks[i]
			i++
			return ch, nil
		}
		return nil, io.EOF
	}
	return stream.NewStreamReader(next, nil), nil
}

func (c *captureLLM) waitTools(t *testing.T) []inference.Tool {
	t.Helper()
	select {
	case <-c.got:
	case <-time.After(5 * time.Second):
		t.Fatal("the model was never asked for a turn")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]inference.Tool(nil), c.tools...)
}

func newCaptureLLM() *captureLLM { return &captureLLM{got: make(chan struct{}), final: "done"} }

func findInferenceTool(tools []inference.Tool, name string) (inference.Tool, bool) {
	for _, tl := range tools {
		if tl.Name == name {
			return tl, true
		}
	}
	return inference.Tool{}, false
}

// TestRigInjectsAgentToolsForDelegates proves the rig derives + injects the four agent
// collaboration tools at the session bind site for a delegate-bearing primer, WITHOUT
// the user hand-adding them. The StartAgent catalog is exactly the parent's Delegates().
func TestRigInjectsAgentToolsForDelegates(t *testing.T) {
	t.Parallel()
	store, err := sessionstore.Open(memstore.New())
	if err != nil {
		t.Fatalf("sessionstore.Open: %v", err)
	}
	planner := newCaptureLLM()
	defineLoop := func(name string, client inference.Client, delegates ...identity.AgentName) loop.Definition {
		return mustDefine(
			loop.WithName(identity.AgentName(name)),
			loop.WithInference(client, validModel(name)),
			loop.WithDelegates(delegates...),
			loop.WithDrainTimeout(200*time.Millisecond),
		)
	}
	r, err := Define(
		WithLoops(
			defineLoop("planner", planner, "builder", "reviewer"),
			defineLoop("builder", &stubLLM{}),
			defineLoop("reviewer", &stubLLM{}),
		),
		WithPrimers("planner"),
		WithSessionStore(store),
	)
	if err != nil {
		t.Fatalf("Define: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	s, err := r.NewSession(ctx)
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	t.Cleanup(func() { _ = s.Shutdown(context.Background()) })

	if _, err := s.Submit(ctx, []content.Block{&content.TextBlock{Text: "go"}}); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	tools := planner.waitTools(t)

	wantNames := []string{"ListAgents", "MessageAgent", "StartAgent", "StopAgent"}
	gotNames := toolNames(tools)
	sort.Strings(gotNames)
	if strings.Join(gotNames, "\n") != strings.Join(wantNames, "\n") {
		t.Fatalf("delegate-bearing primer tools = %v, want exactly %v", gotNames, wantNames)
	}
	start, ok := findInferenceTool(tools, "StartAgent")
	if !ok {
		t.Fatalf("delegate-bearing primer did NOT receive StartAgent; tools = %v", gotNames)
	}
	for _, delegate := range []string{"builder", "reviewer"} {
		if !strings.Contains(start.Description, delegate) {
			t.Errorf("StartAgent catalog missing delegate %q; description = %q", delegate, start.Description)
		}
	}
}

// TestRigNoAgentToolsWithoutDelegates proves a loop with NO delegates receives none
// of the agent collaboration tools.
func TestRigNoAgentToolsWithoutDelegates(t *testing.T) {
	t.Parallel()
	store, err := sessionstore.Open(memstore.New())
	if err != nil {
		t.Fatalf("sessionstore.Open: %v", err)
	}
	solo := newCaptureLLM()
	r, err := Define(
		WithLoops(mustDefine(
			loop.WithName("solo"),
			loop.WithInference(solo, validModel("solo")),
			loop.WithDrainTimeout(200*time.Millisecond),
		)),
		WithPrimers("solo"),
		WithSessionStore(store),
	)
	if err != nil {
		t.Fatalf("Define: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	s, err := r.NewSession(ctx)
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	t.Cleanup(func() { _ = s.Shutdown(context.Background()) })

	if _, err := s.Submit(ctx, []content.Block{&content.TextBlock{Text: "go"}}); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	tools := solo.waitTools(t)
	for _, name := range []string{"ListAgents", "MessageAgent", "StartAgent", "StopAgent"} {
		if _, ok := findInferenceTool(tools, name); ok {
			t.Fatalf("no-delegate loop received %s; tools = %v", name, toolNames(tools))
		}
	}
}

func toolNames(tools []inference.Tool) []string {
	names := make([]string, len(tools))
	for i, tl := range tools {
		names[i] = tl.Name
	}
	return names
}
