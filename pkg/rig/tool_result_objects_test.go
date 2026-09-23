package rig

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/looprig/core/content"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/gate"
	"github.com/looprig/harness/pkg/journal"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/session"
	"github.com/looprig/harness/pkg/sessionstore"
	"github.com/looprig/harness/pkg/tool"
	"github.com/looprig/inference"
	"github.com/looprig/inference/stream"
	"github.com/looprig/storage/memstore"
)

// retentionModelBytes is the model preview budget the end-to-end rig declares.
// It is finite on purpose: with an unbounded preview nothing is ever elided and
// no object is ever written.
const retentionModelBytes = 4096

var captureIDPattern = regexp.MustCompile(`read_tool_result capture_id="([0-9a-f-]{36})"`)

// bigOutput is the complete tool result the model sees only a preview of.
func bigOutput() string {
	var b strings.Builder
	for i := range 2000 {
		fmt.Fprintf(&b, "line %05d of the retained build log\n", i)
	}
	return b.String()
}

// retentionLLM drives a turn from what it is sent, so it can act on the capture
// id the loop's marker prints — the same way a real model would:
//
//   - "produce" → call Big;
//   - a tool result whose marker names read_tool_result → call it with that id;
//   - "reread <id> <offset>" → call read_tool_result with those arguments;
//   - anything else → answer "done".
type retentionLLM struct {
	mu    sync.Mutex
	calls int
}

func (*retentionLLM) Invoke(context.Context, inference.Request) (*inference.Response, error) {
	return nil, errors.New("retention llm: Invoke is unused")
}

func (l *retentionLLM) Stream(_ context.Context, request inference.Request) (*stream.StreamReader[content.Chunk], error) {
	l.mu.Lock()
	l.calls++
	id := "use-" + strconv.Itoa(l.calls)
	l.mu.Unlock()
	chunks := []content.Chunk{&content.TextChunk{Text: "done"}}
	last := request.Messages[len(request.Messages)-1]
	text := messageText(last)
	switch {
	case isUserMessage(last) && text == "produce":
		chunks = []content.Chunk{&content.ToolUseChunk{Index: 0, ID: id, Name: "Big", InputJSON: `{}`}}
	case isUserMessage(last) && strings.HasPrefix(text, "reread "):
		fields := strings.Fields(text)
		chunks = []content.Chunk{&content.ToolUseChunk{Index: 0, ID: id, Name: loop.ReadToolResultToolName,
			InputJSON: `{"capture_id":"` + fields[1] + `","offset":` + fields[2] + `}`}}
	case !isUserMessage(last) && captureIDPattern.MatchString(text):
		captureID := captureIDPattern.FindStringSubmatch(text)[1]
		chunks = []content.Chunk{&content.ToolUseChunk{Index: 0, ID: id, Name: loop.ReadToolResultToolName,
			InputJSON: `{"capture_id":"` + captureID + `"}`}}
	}
	index := 0
	return stream.NewStreamReader(func() (content.Chunk, error) {
		if index == len(chunks) {
			return nil, io.EOF
		}
		chunk := chunks[index]
		index++
		return chunk, nil
	}, nil), nil
}

func isUserMessage(message content.Conversation) bool {
	_, ok := message.(*content.UserMessage)
	return ok
}

func messageText(message content.Conversation) string {
	var blocks []content.Block
	switch m := message.(type) {
	case *content.UserMessage:
		blocks = m.Blocks
	case *content.ToolResultMessage:
		blocks = m.Blocks
	case *content.AIMessage:
		blocks = m.Blocks
	}
	var b strings.Builder
	for _, block := range blocks {
		if text, ok := block.(*content.TextBlock); ok {
			b.WriteString(text.Text)
		}
	}
	return b.String()
}

type allowAccessSource struct{}

func (allowAccessSource) AccessVersion() uint16                   { return gate.CurrentAccessVersion }
func (allowAccessSource) AccessFor(string, string) (uint8, error) { return gate.AccessAllow, nil }

type noRuleWriter struct{}

func (noRuleWriter) WriteRules(context.Context, []tool.RuleCandidate) error { return nil }

// retentionTool is a minimal InvokableTool with an allow-listed invocation.
type retentionTool struct {
	name string
	run  func(ctx context.Context, args string) (*tool.ToolResult, error)
}

func (r retentionTool) Info(context.Context) (*tool.ToolInfo, error) {
	return &tool.ToolInfo{Name: r.name, Schema: json.RawMessage(`{"type":"object"}`)}, nil
}

func (r retentionTool) PrepareCall(context.Context, uuid.UUID, string) (tool.Request, tool.PreparedArtifact, error) {
	return tool.Request{ToolName: r.name, Summary: r.name, Requirements: []tool.Requirement{{
		Kind: "tool.invoke", Scope: r.name, Match: r.name, Description: "run " + r.name,
	}}}, nil, nil
}

func (r retentionTool) InvokableRun(ctx context.Context, args string) (*tool.ToolResult, error) {
	return r.run(ctx, args)
}

// pageRecorder keeps every page the test's read_tool_result tool served.
type pageRecorder struct {
	mu    sync.Mutex
	pages []tool.ToolResultPage
	errs  []error
}

func (p *pageRecorder) snapshot() ([]tool.ToolResultPage, []error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]tool.ToolResultPage(nil), p.pages...), append([]error(nil), p.errs...)
}

// readToolResultDefinition is a stand-in for the tools module's
// read_tool_result: it declares tool.RequiresToolResultReader and serves pages
// through the reader harness binds for the calling loop.
func readToolResultDefinition(recorder *pageRecorder) tool.Definition {
	return tool.NewDefinition(loop.ReadToolResultToolName, tool.RequiresToolResultReader, func(_ context.Context, bindings tool.Bindings) ([]tool.InvokableTool, error) {
		reader := bindings.ToolResults
		return []tool.InvokableTool{retentionTool{name: loop.ReadToolResultToolName, run: func(ctx context.Context, args string) (*tool.ToolResult, error) {
			var request struct {
				CaptureID string `json:"capture_id"`
				Offset    uint64 `json:"offset"`
			}
			if err := json.Unmarshal([]byte(args), &request); err != nil {
				return nil, err
			}
			page, err := reader.ReadToolResult(ctx, tool.ToolResultPageRequest{CaptureID: request.CaptureID, Offset: request.Offset})
			recorder.mu.Lock()
			recorder.pages = append(recorder.pages, page)
			recorder.errs = append(recorder.errs, err)
			recorder.mu.Unlock()
			if err != nil {
				return tool.TextResult("error: " + err.Error()), nil
			}
			return tool.TextResult(page.Render()), nil
		}}}, nil
	})
}

func retentionRig(t *testing.T, store *sessionstore.Store, spillBase string, recorder *pageRecorder) *Rig {
	t.Helper()
	evaluator, err := gate.NewInteractiveEvaluator(
		[]gate.AccessBinding{{Kind: "tool.invoke", Source: allowAccessSource{}}},
		nil, loop.GateApprover(), noRuleWriter{}, nil,
	)
	if err != nil {
		t.Fatalf("NewInteractiveEvaluator: %v", err)
	}
	big := bigOutput()
	definition, err := loop.Define(
		loop.WithName("agent"),
		loop.WithInference(&retentionLLM{}, validModel("retention")),
		loop.WithTools(
			tool.NewDefinition("Big", 0, func(context.Context, tool.Bindings) ([]tool.InvokableTool, error) {
				return []tool.InvokableTool{retentionTool{name: "Big", run: func(context.Context, string) (*tool.ToolResult, error) {
					return tool.TextResult(big), nil
				}}}, nil
			}),
			readToolResultDefinition(recorder),
		),
		loop.WithToolLimits(loop.ToolLimits{ResultBytes: retentionModelBytes}),
		loop.WithAccessGate(evaluator),
		loop.WithPolicyRevision("retention-v1"),
	)
	if err != nil {
		t.Fatalf("loop.Define: %v", err)
	}
	r, err := Define(
		WithLoops(definition),
		WithPrimers("agent"),
		WithSessionStore(store),
		WithToolResultObjects(store.ToolResultObjects(), spillBase),
	)
	if err != nil {
		t.Fatalf("Define: %v", err)
	}
	return r
}

func runRetentionTurn(t *testing.T, ctx context.Context, controller session.SessionController, text string) {
	t.Helper()
	sub, err := controller.SubscribeEvents(event.EventFilter{Enduring: event.LoopScope{All: true}})
	if err != nil {
		t.Fatalf("SubscribeEvents: %v", err)
	}
	defer sub.Close()
	if _, err := controller.Submit(ctx, []content.Block{&content.TextBlock{Text: text}}); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	for {
		select {
		case delivery := <-sub.Events():
			if delivery.Event.EndsTurn() {
				if failed, ok := delivery.Event.(event.TurnFailed); ok {
					t.Fatalf("turn %q failed: %v", text, failed.Err)
				}
				return
			}
		case <-ctx.Done():
			t.Fatalf("turn %q timed out", text)
		}
	}
}

func committedCaptures(t *testing.T, store *sessionstore.Store, id uuid.UUID) []event.ToolResultCapture {
	t.Helper()
	replayer, err := store.OpenEventReplayer(id, sessionstore.ReplayRequest{})
	if err != nil {
		t.Fatalf("OpenEventReplayer: %v", err)
	}
	cursor, err := replayer.Open(context.Background(), journal.ReplayRequest{SessionID: id, From: journal.Beginning()})
	if err != nil {
		t.Fatalf("Open replay: %v", err)
	}
	defer func() { _ = cursor.Close() }()
	var captures []event.ToolResultCapture
	for {
		ev, _, err := cursor.Next(context.Background())
		if errors.Is(err, io.EOF) {
			return captures
		}
		if err != nil {
			t.Fatalf("replay: %v", err)
		}
		if done, ok := ev.(event.StepDone); ok {
			captures = append(captures, done.Captures...)
		}
	}
}

// TestToolResultObjectsRoundTripThroughTheStoreAndSurviveRestore is the I2.2
// harness path end to end, over a real harness store:
//
//   - an oversized result is retained under a STORE-ISSUED reference, which the
//     store resolves to the full bytes (defect 1);
//   - the marker names read_tool_result and the capture id, and the model's
//     call pages the retained bytes (defect 2 and the reader binding);
//   - a read page stays under the preview budget, so it is not re-captured;
//   - the committed capture is journal evidence LookupToolResultCapture finds;
//   - after a restore on a fresh rig and store handle, the capture index is
//     rebuilt from the journal and the same capture pages from a later offset.
func TestToolResultObjectsRoundTripThroughTheStoreAndSurviveRestore(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	backend := memstore.New()
	store, err := sessionstore.Open(backend)
	if err != nil {
		t.Fatalf("sessionstore.Open: %v", err)
	}
	recorder := &pageRecorder{}
	live, err := retentionRig(t, store, t.TempDir(), recorder).NewSession(ctx)
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	id := live.SessionID()
	runRetentionTurn(t, ctx, live, "produce")

	captures := committedCaptures(t, store, id)
	if len(captures) != 2 {
		t.Fatalf("committed captures = %d, want 2 (the Big result and the read page)", len(captures))
	}
	big, page := captures[0], captures[1]
	if big.Reference == nil || !strings.HasPrefix(big.Reference.ObjectID, "v1:tool-result:") {
		t.Fatalf("Big capture reference = %+v, want a store-issued tool-result reference", big.Reference)
	}
	if page.Reference != nil {
		t.Fatalf("the read page was retained as an object (%+v): a page must fit the preview budget", page.Reference)
	}
	_, stream, err := store.ToolResultObjects().OpenToolResultObject(ctx, id, *big.Reference)
	if err != nil {
		t.Fatalf("OpenToolResultObject: %v", err)
	}
	full, err := io.ReadAll(stream)
	if closeErr := stream.Close(); err != nil || closeErr != nil {
		t.Fatalf("read retained object: %v / %v", err, closeErr)
	}
	if string(full) != bigOutput() {
		t.Fatalf("retained object is %d bytes, want the full %d-byte result", len(full), len(bigOutput()))
	}
	if _, found, err := store.LookupToolResultCapture(ctx, id, *big.Reference); err != nil || !found {
		t.Fatalf("LookupToolResultCapture = (%v, %v), want the committed evidence", found, err)
	}

	pages, errs := recorder.snapshot()
	if len(pages) != 1 || errs[0] != nil {
		t.Fatalf("read_tool_result served %d pages (errors %v), want 1", len(pages), errs)
	}
	if pages[0].CaptureID != big.ToolExecutionID || pages[0].Offset != 0 {
		t.Fatalf("page = (capture %v, offset %d), want the Big capture from 0", pages[0].CaptureID, pages[0].Offset)
	}
	if !strings.HasPrefix(bigOutput(), string(pages[0].Data)) || len(pages[0].Data) == 0 {
		t.Fatal("the first page is not the retained result's prefix")
	}
	next, more := pages[0].NextOffset()
	if !more {
		t.Fatal("the first page claims to be the last")
	}

	if err := live.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	// A successor: a new store handle over the same backend and a new rig, so no
	// in-memory state of the first session can answer.
	successorStore, err := sessionstore.Open(backend)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	successorRecorder := &pageRecorder{}
	restored, err := retentionRig(t, successorStore, t.TempDir(), successorRecorder).RestoreSession(ctx, id)
	if err != nil {
		t.Fatalf("RestoreSession: %v", err)
	}
	t.Cleanup(func() { _ = restored.Shutdown(context.Background()) })
	runRetentionTurn(t, ctx, restored, "reread "+big.ToolExecutionID.String()+" "+strconv.FormatUint(next, 10))
	pages, errs = successorRecorder.snapshot()
	if len(pages) != 1 || errs[0] != nil {
		t.Fatalf("after restore read_tool_result served %d pages (errors %v), want 1", len(pages), errs)
	}
	want := bigOutput()[next : next+uint64(len(pages[0].Data))]
	if pages[0].Offset != next || string(pages[0].Data) != want || len(want) == 0 {
		t.Fatalf("restored page = (offset %d, %d bytes), want the bytes from %d", pages[0].Offset, len(pages[0].Data), next)
	}

	// The restored session keeps RETAINING readably, and indexes what it
	// retains: a new oversized result gets a store-issued reference and the
	// model's follow-up read of it is served.
	runRetentionTurn(t, ctx, restored, "produce")
	captures = committedCaptures(t, successorStore, id)
	var fresh *event.ToolResultCapture
	for i := range captures {
		if captures[i].Reference != nil && captures[i].ToolExecutionID != big.ToolExecutionID {
			fresh = &captures[i]
		}
	}
	if fresh == nil || !strings.HasPrefix(fresh.Reference.ObjectID, "v1:tool-result:") {
		t.Fatalf("after restore no new store-issued capture was committed: %+v", captures)
	}
	pages, errs = successorRecorder.snapshot()
	if len(pages) != 2 || errs[1] != nil || pages[1].CaptureID != fresh.ToolExecutionID {
		t.Fatalf("after restore the new capture was not readable: pages %d, errors %v", len(pages), errs)
	}
}

func TestWithToolResultObjectsRejectsAnUnusableWiring(t *testing.T) {
	t.Parallel()
	absolute := t.TempDir()
	store := sessionStoreT(t)
	var typedNil *nilToolResultObjectsImpl
	tests := []struct {
		name     string
		options  []Option
		want     DefinitionErrorKind
		wantName string
	}{
		{name: "nil store", options: []Option{WithToolResultObjects(nil, absolute)}, want: DefinitionInvalidToolResultCapture, wantName: "objects"},
		{name: "typed nil store", options: []Option{WithToolResultObjects(typedNil, absolute)}, want: DefinitionInvalidToolResultCapture, wantName: "objects"},
		{name: "relative spill base", options: []Option{WithToolResultObjects(store.ToolResultObjects(), "spills")}, want: DefinitionInvalidToolResultCapture, wantName: "spill_base"},
		{
			name:     "exclusive with the legacy option",
			options:  []Option{WithToolResultCapture(newExternalStore(), absolute), WithToolResultObjects(store.ToolResultObjects(), absolute)},
			want:     DefinitionDuplicateOption,
			wantName: string(keyToolResultCapture),
		},
		{
			name:     "duplicate",
			options:  []Option{WithToolResultObjects(store.ToolResultObjects(), absolute), WithToolResultObjects(store.ToolResultObjects(), absolute)},
			want:     DefinitionDuplicateOption,
			wantName: string(keyToolResultCapture),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := defineWith(t, sessionStoreT(t), tt.options...)
			var definitionErr *DefinitionError
			if !errors.As(err, &definitionErr) || definitionErr.Kind != tt.want || definitionErr.Name != tt.wantName {
				t.Fatalf("Define error = %v, want kind %q name %q", err, tt.want, tt.wantName)
			}
		})
	}
	if _, err := defineWith(t, store, WithToolResultObjects(store.ToolResultObjects(), absolute)); err != nil {
		t.Fatalf("a valid readable wiring was rejected: %v", err)
	}
}

type nilToolResultObjectsImpl struct{ loop.ToolResultObjects }

// TestReaderToolRequiresReadableObjects is "register only when configured": a
// loop declaring the reader requirement is refused unless a readable store is
// wired — including when only the unreadable legacy seam is.
func TestReaderToolRequiresReadableObjects(t *testing.T) {
	t.Parallel()
	definition, err := loop.Define(
		loop.WithName("agent"),
		loop.WithInference(&stubLLM{}, validModel("model")),
		loop.WithTools(readToolResultDefinition(&pageRecorder{})),
	)
	if err != nil {
		t.Fatalf("loop.Define: %v", err)
	}
	store := sessionStoreT(t)
	for _, extra := range [][]Option{nil, {WithToolResultCapture(newExternalStore(), t.TempDir())}} {
		options := append([]Option{WithLoops(definition), WithPrimers("agent"), WithSessionStore(store)}, extra...)
		_, err := Define(options...)
		var definitionErr *DefinitionError
		if !errors.As(err, &definitionErr) || definitionErr.Kind != DefinitionToolResultReaderWithoutObjects {
			t.Fatalf("Define with %d extra options = %v, want %q", len(extra), err, DefinitionToolResultReaderWithoutObjects)
		}
	}
	if _, err := Define(WithLoops(definition), WithPrimers("agent"), WithSessionStore(store), WithToolResultObjects(store.ToolResultObjects(), t.TempDir())); err != nil {
		t.Fatalf("Define with a readable store: %v", err)
	}
}
