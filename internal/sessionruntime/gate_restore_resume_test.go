package sessionruntime

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/looprig/core/content"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/event"
	gatedomain "github.com/looprig/harness/pkg/gate"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/sessionstore"
	"github.com/looprig/harness/pkg/tool"
	"github.com/looprig/inference"
	stream "github.com/looprig/inference/stream"
)

// This file drives a session PARKED at a gate across a crash-equivalent release
// (AbandonResidency) and a restore on a fresh runtime — the Host failover shape —
// and asserts the product contract: the gate comes back OPEN, an answer given to
// the successor reaches the waiting tool call, and the turn continues with the
// model seeing that tool result. Nothing is re-asked and nothing runs twice.

// resumeScriptLLM answers the first request with one tool call and every later
// request with plain text, recording every request it is sent.
type resumeScriptLLM struct {
	toolName string
	toolArgs string
	// continueOnly makes EVERY request answer with text: the restored runtime's
	// model must never be asked to re-issue the call.
	continueOnly bool

	mu       sync.Mutex
	requests []inference.Request
}

func (l *resumeScriptLLM) Invoke(context.Context, inference.Request) (*inference.Response, error) {
	return nil, errors.New("resumeScriptLLM.Invoke not used")
}

func (l *resumeScriptLLM) Stream(_ context.Context, req inference.Request) (*stream.StreamReader[content.Chunk], error) {
	l.mu.Lock()
	first := len(l.requests) == 0
	l.requests = append(l.requests, req)
	l.mu.Unlock()
	chunks := []content.Chunk{textChunk("continued")}
	if first && !l.continueOnly {
		args := l.toolArgs
		if args == "" {
			args = `{}`
		}
		chunks = []content.Chunk{
			textChunk("let me check"),
			&content.ToolUseChunk{Index: 1, ID: "call-1", Name: l.toolName, InputJSON: args},
		}
	}
	i := 0
	return stream.NewStreamReader(func() (content.Chunk, error) {
		if i < len(chunks) {
			c := chunks[i]
			i++
			return c, nil
		}
		return nil, io.EOF
	}, nil), nil
}

func (l *resumeScriptLLM) snapshot() []inference.Request {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]inference.Request(nil), l.requests...)
}

// askTool asks the user one question and returns the answer. It declares itself
// replay-safe up to its question, which is what lets a restored session re-run it
// to deliver an answer.
type askTool struct {
	replaySafe bool
	runs       atomic.Int64
	answers    chan string
}

func newAskTool(replaySafe bool) *askTool {
	return &askTool{replaySafe: replaySafe, answers: make(chan string, 4)}
}

func (a *askTool) Info(context.Context) (*tool.ToolInfo, error) {
	return &tool.ToolInfo{Name: "Ask", Desc: "ask", Schema: []byte(`{"type":"object"}`)}, nil
}

func (a *askTool) PrepareCall(context.Context, uuid.UUID, string) (tool.Request, tool.PreparedArtifact, error) {
	return tool.Request{ToolName: "Ask"}, nil, nil
}

func (a *askTool) InvokableRun(ctx context.Context, _ string) (*tool.ToolResult, error) {
	a.runs.Add(1)
	answer, err := loop.RequestUserInput(ctx, "Which color?", nil)
	if err != nil {
		return nil, err
	}
	a.answers <- answer
	return tool.TextResult("the user said " + answer), nil
}

func (a *askTool) UserInputReplaySafe() bool { return a.replaySafe }

func askDefinition(client inference.Client, tl *askTool) loop.Definition {
	evaluator, err := gatedomain.NewInteractiveEvaluator(
		[]gatedomain.AccessBinding{{Kind: "tool.invoke", Source: gatedAccessSource{}}},
		nil, loop.GateApprover(), &orderedRuleWriter{seq: &atomic.Int64{}}, nil)
	if err != nil {
		panic(err)
	}
	return mustDefine(
		loop.WithName("agent"),
		loop.WithInference(client, validModel("base")),
		loop.WithSystem("base"),
		loop.WithTools(tool.NewDefinition("Ask", 0, func(context.Context, tool.Bindings) ([]tool.InvokableTool, error) {
			return []tool.InvokableTool{tl}, nil
		})),
		loop.WithAccessGate(evaluator),
		loop.WithPolicyRevision("gate-resume"),
		loop.WithDrainTimeout(200*time.Millisecond),
	)
}

func permissionDefinition(t *testing.T, client inference.Client, tl *gatedE2ETool) loop.Definition {
	t.Helper()
	evaluator, err := gatedomain.NewInteractiveEvaluator(
		[]gatedomain.AccessBinding{{Kind: "tool.invoke", Source: gatedAccessSource{}}},
		nil, loop.GateApprover(), &orderedRuleWriter{seq: &atomic.Int64{}}, nil)
	if err != nil {
		t.Fatalf("NewInteractiveEvaluator: %v", err)
	}
	return mustDefine(
		loop.WithName("agent"),
		loop.WithInference(client, validModel("base")),
		loop.WithSystem("base"),
		loop.WithTools(tool.NewDefinition("Gated", 0, func(context.Context, tool.Bindings) ([]tool.InvokableTool, error) {
			return []tool.InvokableTool{tl}, nil
		})),
		loop.WithAccessGate(evaluator),
		loop.WithPolicyRevision("gate-resume"),
		loop.WithDrainTimeout(200*time.Millisecond),
	)
}

// awaitOpenGate polls the directory until exactly one gate of kind is open.
func awaitOpenGate(t *testing.T, s *Session, kind gatedomain.Kind) gatedomain.Gate {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		for _, g := range s.ListGates(context.Background()) {
			if g.Kind == kind {
				return g
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("no %s gate opened", kind)
	return gatedomain.Gate{}
}

// parkAndAbandon runs the original runtime to its gate and releases it
// crash-equivalently, returning the open gate.
func parkAndAbandon(t *testing.T, store *sessionstore.Store, def loop.Definition, kind gatedomain.Kind) (uuid.UUID, gatedomain.Gate) {
	t.Helper()
	lifecycle, err := newTestLifecycle(def, store)
	if err != nil {
		t.Fatal(err)
	}
	s, err := lifecycle.NewSession(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Submit(context.Background(), []content.Block{&content.TextBlock{Text: "go"}}); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	g := awaitOpenGate(t, s, kind)
	if err := s.AbandonResidency(context.Background()); err != nil {
		t.Fatalf("AbandonResidency: %v", err)
	}
	return s.SessionID(), g
}

func restoreParked(t *testing.T, store *sessionstore.Store, def loop.Definition, sessionID uuid.UUID) *Session {
	t.Helper()
	lifecycle, err := newTestLifecycle(def, store)
	if err != nil {
		t.Fatal(err)
	}
	restored, err := lifecycle.RestoreSession(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("RestoreSession: %v", err)
	}
	t.Cleanup(func() { _ = restored.Shutdown(context.Background()) })
	return restored
}

// awaitTurnTerminal waits for the next loop turn terminal on sub.
func awaitTurnTerminal(t *testing.T, sub event.Subscription) event.Event {
	t.Helper()
	timeout := time.After(15 * time.Second)
	for {
		select {
		case d, ok := <-sub.Events():
			if !ok {
				t.Fatal("subscription closed before a turn terminal")
			}
			switch d.Event.(type) {
			case event.TurnDone, event.TurnFailed, event.TurnInterrupted:
				return d.Event
			}
		case <-timeout:
			t.Fatal("no turn terminal after the gate answer")
		}
	}
}

// lastToolResult returns the final message of req if it is a tool result.
func lastToolResult(req inference.Request) (*content.ToolResultMessage, bool) {
	if len(req.Messages) == 0 {
		return nil, false
	}
	trm, ok := req.Messages[len(req.Messages)-1].(*content.ToolResultMessage)
	return trm, ok
}

func messageText(blocks []content.Block) string {
	var sb strings.Builder
	for _, b := range blocks {
		if tb, ok := b.(*content.TextBlock); ok {
			sb.WriteString(tb.Text)
		}
	}
	return sb.String()
}

// assertContinuationRequest checks the restored model call: the pending assistant
// step with its tool call is replayed verbatim, followed by the answer as the
// matching tool result.
func assertContinuationRequest(t *testing.T, req inference.Request, wantResult string) {
	t.Helper()
	trm, ok := lastToolResult(req)
	if !ok {
		t.Fatalf("continuation request does not end with a tool result: %#v", req.Messages)
	}
	if trm.ToolUseID != "call-1" {
		t.Fatalf("tool result answers %q, want call-1", trm.ToolUseID)
	}
	if got := messageText(trm.Blocks); !strings.Contains(got, wantResult) {
		t.Fatalf("tool result text = %q, want it to contain %q", got, wantResult)
	}
	ai, ok := req.Messages[len(req.Messages)-2].(*content.AIMessage)
	if !ok {
		t.Fatalf("message before the tool result is %T, want the pending assistant step", req.Messages[len(req.Messages)-2])
	}
	calls := 0
	for _, b := range ai.Blocks {
		if tu, ok := b.(*content.ToolUseBlock); ok && tu.ID == "call-1" {
			calls++
		}
	}
	if calls != 1 {
		t.Fatalf("pending assistant step carries %d call-1 tool uses, want 1", calls)
	}
	if got := messageText(ai.Blocks); got != "let me check" {
		t.Fatalf("pending assistant text = %q, want the original step's text", got)
	}
}

func countEvents[T event.Event](events []event.Event) int {
	n := 0
	for _, ev := range events {
		if _, ok := ev.(T); ok {
			n++
		}
	}
	return n
}

func TestRestoredAskUserGateResumesTheTurnWithTheAnswer(t *testing.T) {
	t.Parallel()
	store := newRestoreStore(t)
	original := newAskTool(true)
	sessionID, parked := parkAndAbandon(t, store,
		askDefinition(&resumeScriptLLM{toolName: "Ask"}, original), gatedomain.KindAskUser)

	successorLLM := &resumeScriptLLM{toolName: "Ask", continueOnly: true}
	successor := newAskTool(true)
	restored := restoreParked(t, store, askDefinition(successorLLM, successor), sessionID)

	open := restored.ListGates(context.Background())
	if len(open) != 1 || open[0].ID != parked.ID {
		t.Fatalf("restored gates = %+v, want the parked ask_user gate %v open", open, parked.ID)
	}
	sub, err := restored.SubscribeEvents(event.EventFilter{Enduring: event.LoopScope{All: true}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sub.Close() }()
	// Answer at once: the answer may reach the loop before the resumed call has
	// re-entered its question, and it must not be dropped.
	if err := restored.RespondGate(context.Background(), gatedomain.GateResponse{
		GateID: parked.ID, Action: "answer",
		Values: map[string]json.RawMessage{"answer": json.RawMessage(`"blue"`)},
		Source: gatedomain.ResponseSource{Kind: gatedomain.ResponseFromUser},
	}); err != nil {
		t.Fatalf("RespondGate on the restored ask_user gate: %v", err)
	}
	terminal := awaitTurnTerminal(t, sub)
	if _, ok := terminal.(event.TurnDone); !ok {
		t.Fatalf("resumed turn ended %T, want TurnDone", terminal)
	}
	select {
	case got := <-successor.answers:
		if got != "blue" {
			t.Fatalf("the waiting tool call received %q, want blue", got)
		}
	default:
		t.Fatal("the answer never reached the waiting tool call")
	}
	requests := successorLLM.snapshot()
	if len(requests) != 1 {
		t.Fatalf("successor model requests = %d, want exactly the one continuation", len(requests))
	}
	assertContinuationRequest(t, requests[0], "the user said blue")

	events := replayAllSessionEvents(t, store, sessionID)
	if n := countEvents[event.TurnInterrupted](events); n != 0 {
		t.Fatalf("durable TurnInterrupted = %d, want 0: the parked turn resumes, it is not closed", n)
	}
	if n := countEvents[event.TurnStarted](events); n != 1 {
		t.Fatalf("durable TurnStarted = %d, want 1: the resumed turn is the original turn", n)
	}
	for _, ev := range events {
		if resolved, ok := ev.(event.GateResolved); ok && resolved.Reason == gatedomain.CloseRestoreUnavailable {
			t.Fatal("the ask_user gate was closed restore_unavailable")
		}
	}
}

func TestRestoredPermissionGateRunsTheApprovedToolOnce(t *testing.T) {
	t.Parallel()
	store := newRestoreStore(t)
	originalTool := &gatedE2ETool{seq: &atomic.Int64{}}
	sessionID, parked := parkAndAbandon(t, store,
		permissionDefinition(t, &resumeScriptLLM{toolName: "Gated"}, originalTool), gatedomain.KindPermission)

	successorLLM := &resumeScriptLLM{toolName: "Gated", continueOnly: true}
	successorTool := &gatedE2ETool{seq: &atomic.Int64{}}
	restored := restoreParked(t, store, permissionDefinition(t, successorLLM, successorTool), sessionID)
	sub, err := restored.SubscribeEvents(event.EventFilter{Enduring: event.LoopScope{All: true}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sub.Close() }()
	if err := restored.RespondGate(context.Background(), gatedomain.GateResponse{
		GateID: parked.ID, Action: string(gatedomain.ApprovalApprove),
		Source: gatedomain.ResponseSource{Kind: gatedomain.ResponseFromUser},
	}); err != nil {
		t.Fatalf("RespondGate on the restored permission gate: %v", err)
	}
	terminal := awaitTurnTerminal(t, sub)
	if _, ok := terminal.(event.TurnDone); !ok {
		t.Fatalf("resumed turn ended %T, want TurnDone", terminal)
	}
	if runs, _ := originalTool.snapshot(); runs != 0 {
		t.Fatalf("the original runtime ran the gated tool %d times before approval", runs)
	}
	if runs, _ := successorTool.snapshot(); runs != 1 {
		t.Fatalf("the approved tool ran %d times on the successor, want exactly 1", runs)
	}
	requests := successorLLM.snapshot()
	if len(requests) != 1 {
		t.Fatalf("successor model requests = %d, want exactly the one continuation", len(requests))
	}
	assertContinuationRequest(t, requests[0], "ran")
	if gates := restored.ListGates(context.Background()); len(gates) != 0 {
		t.Fatalf("gates still open after the resumed turn: %+v", gates)
	}
}

func TestRestoredPermissionDenialReachesTheModelAndRunsNothing(t *testing.T) {
	t.Parallel()
	store := newRestoreStore(t)
	sessionID, parked := parkAndAbandon(t, store,
		permissionDefinition(t, &resumeScriptLLM{toolName: "Gated"}, &gatedE2ETool{seq: &atomic.Int64{}}), gatedomain.KindPermission)

	successorLLM := &resumeScriptLLM{toolName: "Gated", continueOnly: true}
	successorTool := &gatedE2ETool{seq: &atomic.Int64{}}
	restored := restoreParked(t, store, permissionDefinition(t, successorLLM, successorTool), sessionID)
	sub, err := restored.SubscribeEvents(event.EventFilter{Enduring: event.LoopScope{All: true}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sub.Close() }()
	if err := restored.RespondGate(context.Background(), gatedomain.GateResponse{
		GateID: parked.ID, Action: string(gatedomain.ApprovalDeny),
		Source: gatedomain.ResponseSource{Kind: gatedomain.ResponseFromUser},
	}); err != nil {
		t.Fatalf("RespondGate: %v", err)
	}
	if _, ok := awaitTurnTerminal(t, sub).(event.TurnDone); !ok {
		t.Fatal("resumed turn did not finish")
	}
	if runs, _ := successorTool.snapshot(); runs != 0 {
		t.Fatalf("a denied tool ran %d times", runs)
	}
	requests := successorLLM.snapshot()
	if len(requests) != 1 {
		t.Fatalf("successor model requests = %d, want 1", len(requests))
	}
	trm, ok := lastToolResult(requests[0])
	if !ok || !trm.IsError || trm.ToolUseID != "call-1" {
		t.Fatalf("continuation does not carry the denial as call-1's error result: %#v", requests[0].Messages)
	}
}

// TestRepeatedFailoverWhileParkedStillResumes: a successor that is itself lost
// before the answer arrives leaves the gate exactly as it found it, so a third
// runtime resumes the same question.
func TestRepeatedFailoverWhileParkedStillResumes(t *testing.T) {
	t.Parallel()
	store := newRestoreStore(t)
	sessionID, parked := parkAndAbandon(t, store,
		askDefinition(&resumeScriptLLM{toolName: "Ask"}, newAskTool(true)), gatedomain.KindAskUser)

	second := func() *Session {
		lifecycle, err := newTestLifecycle(askDefinition(&resumeScriptLLM{toolName: "Ask", continueOnly: true}, newAskTool(true)), store)
		if err != nil {
			t.Fatal(err)
		}
		s, err := lifecycle.RestoreSession(context.Background(), sessionID)
		if err != nil {
			t.Fatalf("second RestoreSession: %v", err)
		}
		return s
	}()
	awaitOpenGate(t, second, gatedomain.KindAskUser)
	if err := second.AbandonResidency(context.Background()); err != nil {
		t.Fatalf("second AbandonResidency: %v", err)
	}

	thirdLLM := &resumeScriptLLM{toolName: "Ask", continueOnly: true}
	third := restoreParked(t, store, askDefinition(thirdLLM, newAskTool(true)), sessionID)
	sub, err := third.SubscribeEvents(event.EventFilter{Enduring: event.LoopScope{All: true}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sub.Close() }()
	if err := third.RespondGate(context.Background(), gatedomain.GateResponse{
		GateID: parked.ID, Action: "answer",
		Values: map[string]json.RawMessage{"answer": json.RawMessage(`"green"`)},
		Source: gatedomain.ResponseSource{Kind: gatedomain.ResponseFromUser},
	}); err != nil {
		t.Fatalf("RespondGate on the third runtime: %v", err)
	}
	if _, ok := awaitTurnTerminal(t, sub).(event.TurnDone); !ok {
		t.Fatal("third runtime's resumed turn did not finish")
	}
	requests := thirdLLM.snapshot()
	if len(requests) != 1 {
		t.Fatalf("third runtime model requests = %d, want 1", len(requests))
	}
	assertContinuationRequest(t, requests[0], "the user said green")
	events := replayAllSessionEvents(t, store, sessionID)
	if n := countEvents[event.TurnInterrupted](events); n != 0 {
		t.Fatalf("durable TurnInterrupted = %d, want 0", n)
	}
}

// TestAskUserFromAnUndeclaredToolIsStillClosedAtRestore: re-running a tool to
// deliver an answer is only sound for a tool that declares the run up to its
// question replay-safe. Any other tool keeps the pre-v0.39 closure.
func TestAskUserFromAnUndeclaredToolIsStillClosedAtRestore(t *testing.T) {
	t.Parallel()
	store := newRestoreStore(t)
	sessionID, parked := parkAndAbandon(t, store,
		askDefinition(&resumeScriptLLM{toolName: "Ask"}, newAskTool(false)), gatedomain.KindAskUser)
	restored := restoreParked(t, store,
		askDefinition(&resumeScriptLLM{toolName: "Ask", continueOnly: true}, newAskTool(false)), sessionID)
	if open := restored.ListGates(context.Background()); len(open) != 0 {
		t.Fatalf("restored gates = %+v, want none: an undeclared tool cannot be re-run", open)
	}
	closed := false
	for _, ev := range replayAllSessionEvents(t, store, sessionID) {
		if resolved, ok := ev.(event.GateResolved); ok && resolved.GateID == parked.ID && resolved.Reason == gatedomain.CloseRestoreUnavailable {
			closed = true
		}
	}
	if !closed {
		t.Fatal("the undeclared ask_user gate was not closed restore_unavailable")
	}
}

// TestResumedTurnIsLiveWorkAndReopensNothing: while the resumed turn waits for its
// answer the session is NOT idle (it would otherwise be reported quiescent, and
// drained or checkpointed, mid-turn); the resumed call re-announces nothing already
// durable; and the session goes idle once the turn finishes.
func TestResumedTurnIsLiveWorkAndReopensNothing(t *testing.T) {
	t.Parallel()
	store := newRestoreStore(t)
	sessionID, parked := parkAndAbandon(t, store,
		askDefinition(&resumeScriptLLM{toolName: "Ask"}, newAskTool(true)), gatedomain.KindAskUser)
	restored := restoreParked(t, store,
		askDefinition(&resumeScriptLLM{toolName: "Ask", continueOnly: true}, newAskTool(true)), sessionID)
	if restored.hub.IsIdle() {
		t.Fatal("the session reports idle while its resumed turn waits for an answer")
	}
	before := replayAllSessionEvents(t, store, sessionID)
	sub, err := restored.SubscribeEvents(event.EventFilter{Enduring: event.LoopScope{All: true}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sub.Close() }()
	if err := restored.RespondGate(context.Background(), gatedomain.GateResponse{
		GateID: parked.ID, Action: "answer",
		Values: map[string]json.RawMessage{"answer": json.RawMessage(`"red"`)},
		Source: gatedomain.ResponseSource{Kind: gatedomain.ResponseFromUser},
	}); err != nil {
		t.Fatal(err)
	}
	awaitTurnTerminal(t, sub)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := restored.WaitIdle(ctx); err != nil {
		t.Fatalf("WaitIdle after the resumed turn: %v", err)
	}
	events := replayAllSessionEvents(t, store, sessionID)
	if got, want := countEvents[event.UserInputRequested](events), countEvents[event.UserInputRequested](before); got != want {
		t.Fatalf("durable UserInputRequested went %d -> %d: the resumed call must not ask again", want, got)
	}
	if n := countEvents[event.GateOpened](events); n != 1 {
		t.Fatalf("durable GateOpened = %d, want 1", n)
	}
}

// blockingGatedTool runs until released, so a test can lose the runtime after the
// approval is durable but before the step commits.
type blockingGatedTool struct {
	gatedE2ETool
	entered chan struct{}
}

func (b *blockingGatedTool) InvokableRun(ctx context.Context, args string) (*tool.ToolResult, error) {
	_, _ = b.gatedE2ETool.InvokableRun(ctx, args)
	close(b.entered)
	<-ctx.Done()
	return nil, ctx.Err()
}

// TestApprovedButUncommittedStepIsNeverRunAgain is the exactly-once edge: the
// approval is durable (GateResolved) and the tool has run, but the runtime is lost
// before the step commits. The next restore finds no open gate, so it resumes
// nothing — the turn is interrupted and the tool is not run a second time.
func TestApprovedButUncommittedStepIsNeverRunAgain(t *testing.T) {
	t.Parallel()
	store := newRestoreStore(t)
	sessionID, parked := parkAndAbandon(t, store,
		permissionDefinition(t, &resumeScriptLLM{toolName: "Gated"}, &gatedE2ETool{seq: &atomic.Int64{}}), gatedomain.KindPermission)

	blocking := &blockingGatedTool{gatedE2ETool: gatedE2ETool{seq: &atomic.Int64{}}, entered: make(chan struct{})}
	evaluator, err := gatedomain.NewInteractiveEvaluator(
		[]gatedomain.AccessBinding{{Kind: "tool.invoke", Source: gatedAccessSource{}}},
		nil, loop.GateApprover(), &orderedRuleWriter{seq: &atomic.Int64{}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	def := mustDefine(
		loop.WithName("agent"),
		loop.WithInference(&resumeScriptLLM{toolName: "Gated", continueOnly: true}, validModel("base")),
		loop.WithSystem("base"),
		loop.WithTools(tool.NewDefinition("Gated", 0, func(context.Context, tool.Bindings) ([]tool.InvokableTool, error) {
			return []tool.InvokableTool{blocking}, nil
		})),
		loop.WithAccessGate(evaluator),
		loop.WithPolicyRevision("gate-resume"),
		loop.WithDrainTimeout(200*time.Millisecond),
	)
	lifecycle, err := newTestLifecycle(def, store)
	if err != nil {
		t.Fatal(err)
	}
	second, err := lifecycle.RestoreSession(context.Background(), sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if err := second.RespondGate(context.Background(), gatedomain.GateResponse{
		GateID: parked.ID, Action: string(gatedomain.ApprovalApprove),
		Source: gatedomain.ResponseSource{Kind: gatedomain.ResponseFromUser},
	}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-blocking.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the approved tool never ran on the successor")
	}
	if err := second.AbandonResidency(context.Background()); err != nil {
		t.Fatal(err)
	}

	thirdTool := &gatedE2ETool{seq: &atomic.Int64{}}
	thirdLLM := &resumeScriptLLM{toolName: "Gated", continueOnly: true}
	third := restoreParked(t, store, permissionDefinition(t, thirdLLM, thirdTool), sessionID)
	if gates := third.ListGates(context.Background()); len(gates) != 0 {
		t.Fatalf("an answered gate reopened: %+v", gates)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := third.WaitIdle(ctx); err != nil {
		t.Fatalf("WaitIdle: %v", err)
	}
	if runs, _ := thirdTool.snapshot(); runs != 0 {
		t.Fatalf("the approved tool ran again after the second failover (%d runs)", runs)
	}
	if n := len(thirdLLM.snapshot()); n != 0 {
		t.Fatalf("the third runtime called the model %d times for a turn it could not resume", n)
	}
	if n := countEvents[event.TurnInterrupted](replayAllSessionEvents(t, store, sessionID)); n != 1 {
		t.Fatalf("durable TurnInterrupted = %d, want 1: an unresumable turn is closed as before", n)
	}
}

// changedRequestTool prepares a request that differs from the one the parked gate
// shows, as a tool upgraded between the two runtimes might.
type changedRequestTool struct{ gatedE2ETool }

func (c *changedRequestTool) PrepareCall(ctx context.Context, id uuid.UUID, args string) (tool.Request, tool.PreparedArtifact, error) {
	request, artifact, err := c.gatedE2ETool.PrepareCall(ctx, id, args)
	request.Summary = "do a DIFFERENT gated thing"
	return request, artifact, err
}

// TestRestoredPermissionGateIsNotReusedForADifferentRequest: an answer is bound to
// what its gate showed. If the successor's re-evaluation asks for something else,
// the stale gate is closed and a fresh one opened; the tool does not run on the
// strength of the old gate.
func TestRestoredPermissionGateIsNotReusedForADifferentRequest(t *testing.T) {
	t.Parallel()
	store := newRestoreStore(t)
	sessionID, parked := parkAndAbandon(t, store,
		permissionDefinition(t, &resumeScriptLLM{toolName: "Gated"}, &gatedE2ETool{seq: &atomic.Int64{}}), gatedomain.KindPermission)

	changed := &changedRequestTool{gatedE2ETool: gatedE2ETool{seq: &atomic.Int64{}}}
	evaluator, err := gatedomain.NewInteractiveEvaluator(
		[]gatedomain.AccessBinding{{Kind: "tool.invoke", Source: gatedAccessSource{}}},
		nil, loop.GateApprover(), &orderedRuleWriter{seq: &atomic.Int64{}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	def := mustDefine(
		loop.WithName("agent"),
		loop.WithInference(&resumeScriptLLM{toolName: "Gated", continueOnly: true}, validModel("base")),
		loop.WithSystem("base"),
		loop.WithTools(tool.NewDefinition("Gated", 0, func(context.Context, tool.Bindings) ([]tool.InvokableTool, error) {
			return []tool.InvokableTool{changed}, nil
		})),
		loop.WithAccessGate(evaluator),
		loop.WithPolicyRevision("gate-resume"),
		loop.WithDrainTimeout(200*time.Millisecond),
	)
	restored := restoreParked(t, store, def, sessionID)
	deadline := time.Now().Add(10 * time.Second)
	var fresh []gatedomain.Gate
	for time.Now().Before(deadline) {
		fresh = restored.ListGates(context.Background())
		if len(fresh) == 1 && fresh[0].ID != parked.ID {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if len(fresh) != 1 || fresh[0].ID == parked.ID {
		t.Fatalf("gates = %+v, want the stale gate replaced by one fresh gate", fresh)
	}
	if !strings.Contains(fresh[0].Prompt.Body, "DIFFERENT") {
		t.Fatalf("fresh gate body = %q, want the re-evaluated request", fresh[0].Prompt.Body)
	}
	err = restored.RespondGate(context.Background(), gatedomain.GateResponse{
		GateID: parked.ID, Action: string(gatedomain.ApprovalApprove),
		Source: gatedomain.ResponseSource{Kind: gatedomain.ResponseFromUser},
	})
	var gateErr *GateError
	if !errors.As(err, &gateErr) || gateErr.Kind != GateNotFound {
		t.Fatalf("answering the stale gate = %v, want GateNotFound", err)
	}
	if runs, _ := changed.snapshot(); runs != 0 {
		t.Fatalf("the tool ran %d times on the stale approval", runs)
	}
	var abandoned bool
	for _, ev := range replayAllSessionEvents(t, store, sessionID) {
		if resolved, ok := ev.(event.GateResolved); ok && resolved.GateID == parked.ID && resolved.Reason == gatedomain.CloseAbandoned {
			abandoned = true
		}
	}
	if !abandoned {
		t.Fatal("the stale gate was not durably closed")
	}
}

// echoTool is an ordinary, effectful sibling call.
type echoTool struct{ runs atomic.Int64 }

func (e *echoTool) Info(context.Context) (*tool.ToolInfo, error) {
	return &tool.ToolInfo{Name: "Echo", Desc: "echo", Schema: []byte(`{"type":"object"}`)}, nil
}

func (e *echoTool) PrepareCall(context.Context, uuid.UUID, string) (tool.Request, tool.PreparedArtifact, error) {
	return tool.Request{ToolName: "Echo"}, nil, nil
}

func (e *echoTool) InvokableRun(context.Context, string) (*tool.ToolResult, error) {
	e.runs.Add(1)
	return tool.TextResult("echoed"), nil
}

// twoCallLLM issues Echo and Ask in one step, then continues with text.
type twoCallLLM struct{ resumeScriptLLM }

func (l *twoCallLLM) Stream(ctx context.Context, req inference.Request) (*stream.StreamReader[content.Chunk], error) {
	l.mu.Lock()
	first := len(l.requests) == 0
	l.requests = append(l.requests, req)
	l.mu.Unlock()
	chunks := []content.Chunk{textChunk("continued")}
	if first && !l.continueOnly {
		chunks = []content.Chunk{
			&content.ToolUseChunk{Index: 0, ID: "call-0", Name: "Echo", InputJSON: `{}`},
			&content.ToolUseChunk{Index: 1, ID: "call-1", Name: "Ask", InputJSON: `{}`},
		}
	}
	i := 0
	return stream.NewStreamReader(func() (content.Chunk, error) {
		if i < len(chunks) {
			c := chunks[i]
			i++
			return c, nil
		}
		return nil, io.EOF
	}, nil), nil
}

func twoToolDefinition(client inference.Client, ask *askTool, echo *echoTool) loop.Definition {
	evaluator, err := gatedomain.NewInteractiveEvaluator(
		[]gatedomain.AccessBinding{{Kind: "tool.invoke", Source: gatedAccessSource{}}},
		nil, loop.GateApprover(), &orderedRuleWriter{seq: &atomic.Int64{}}, nil)
	if err != nil {
		panic(err)
	}
	return mustDefine(
		loop.WithName("agent"),
		loop.WithInference(client, validModel("base")),
		loop.WithSystem("base"),
		loop.WithTools(
			tool.NewDefinition("Ask", 0, func(context.Context, tool.Bindings) ([]tool.InvokableTool, error) {
				return []tool.InvokableTool{ask}, nil
			}),
			tool.NewDefinition("Echo", 0, func(context.Context, tool.Bindings) ([]tool.InvokableTool, error) {
				return []tool.InvokableTool{echo}, nil
			}),
		),
		loop.WithAccessGate(evaluator),
		loop.WithPolicyRevision("gate-resume"),
		loop.WithDrainTimeout(200*time.Millisecond),
	)
}

// TestResumedAskUserStepDoesNotRerunItsSiblings: a user-input gate parks a step
// mid-execution, so a sibling call may already have taken effect on the lost
// runtime. The successor re-runs only the asking call and reports the sibling's
// outcome as unknown instead of running it again.
func TestResumedAskUserStepDoesNotRerunItsSiblings(t *testing.T) {
	t.Parallel()
	store := newRestoreStore(t)
	originalEcho := &echoTool{}
	sessionID, parked := parkAndAbandon(t, store,
		twoToolDefinition(&twoCallLLM{}, newAskTool(true), originalEcho), gatedomain.KindAskUser)

	successorEcho := &echoTool{}
	successorLLM := &twoCallLLM{resumeScriptLLM: resumeScriptLLM{continueOnly: true}}
	restored := restoreParked(t, store, twoToolDefinition(successorLLM, newAskTool(true), successorEcho), sessionID)
	sub, err := restored.SubscribeEvents(event.EventFilter{Enduring: event.LoopScope{All: true}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sub.Close() }()
	if err := restored.RespondGate(context.Background(), gatedomain.GateResponse{
		GateID: parked.ID, Action: "answer",
		Values: map[string]json.RawMessage{"answer": json.RawMessage(`"yes"`)},
		Source: gatedomain.ResponseSource{Kind: gatedomain.ResponseFromUser},
	}); err != nil {
		t.Fatal(err)
	}
	if _, ok := awaitTurnTerminal(t, sub).(event.TurnDone); !ok {
		t.Fatal("resumed turn did not finish")
	}
	if n := successorEcho.runs.Load(); n != 0 {
		t.Fatalf("the sibling call ran %d times on the successor", n)
	}
	requests := successorLLM.snapshot()
	if len(requests) != 1 {
		t.Fatalf("successor requests = %d, want 1", len(requests))
	}
	msgs := requests[0].Messages
	if len(msgs) < 2 {
		t.Fatalf("continuation too short: %#v", msgs)
	}
	echoResult, ok1 := msgs[len(msgs)-2].(*content.ToolResultMessage)
	askResult, ok2 := msgs[len(msgs)-1].(*content.ToolResultMessage)
	if !ok1 || !ok2 {
		t.Fatalf("continuation does not end with the step's two tool results: %#v", msgs)
	}
	if echoResult.ToolUseID != "call-0" || !echoResult.IsError || !strings.Contains(messageText(echoResult.Blocks), "outcome is unknown") {
		t.Fatalf("sibling result = %+v, want call-0's unknown-outcome error", echoResult)
	}
	if askResult.ToolUseID != "call-1" || !strings.Contains(messageText(askResult.Blocks), "the user said yes") {
		t.Fatalf("asking call's result = %+v, want the answer", askResult)
	}
}

// echoThenGatedLLM issues Echo and Gated in one step, then continues with text.
type echoThenGatedLLM struct{ resumeScriptLLM }

func (l *echoThenGatedLLM) Stream(ctx context.Context, req inference.Request) (*stream.StreamReader[content.Chunk], error) {
	l.mu.Lock()
	first := len(l.requests) == 0
	l.requests = append(l.requests, req)
	l.mu.Unlock()
	chunks := []content.Chunk{textChunk("continued")}
	if first && !l.continueOnly {
		chunks = []content.Chunk{
			&content.ToolUseChunk{Index: 0, ID: "call-0", Name: "Echo", InputJSON: `{}`},
			&content.ToolUseChunk{Index: 1, ID: "call-1", Name: "Gated", InputJSON: `{}`},
		}
	}
	i := 0
	return stream.NewStreamReader(func() (content.Chunk, error) {
		if i < len(chunks) {
			c := chunks[i]
			i++
			return c, nil
		}
		return nil, io.EOF
	}, nil), nil
}

func echoGatedDefinition(t *testing.T, client inference.Client, echo *echoTool, gated *gatedE2ETool) loop.Definition {
	t.Helper()
	evaluator, err := gatedomain.NewInteractiveEvaluator(
		[]gatedomain.AccessBinding{{Kind: "tool.invoke", Source: gatedAccessSource{}}},
		nil, loop.GateApprover(), &orderedRuleWriter{seq: &atomic.Int64{}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return mustDefine(
		loop.WithName("agent"),
		loop.WithInference(client, validModel("base")),
		loop.WithSystem("base"),
		loop.WithTools(
			tool.NewDefinition("Echo", 0, func(context.Context, tool.Bindings) ([]tool.InvokableTool, error) {
				return []tool.InvokableTool{echo}, nil
			}),
			tool.NewDefinition("Gated", 0, func(context.Context, tool.Bindings) ([]tool.InvokableTool, error) {
				return []tool.InvokableTool{gated}, nil
			}),
		),
		loop.WithAccessGate(evaluator),
		loop.WithPolicyRevision("gate-resume"),
		loop.WithDrainTimeout(200*time.Millisecond),
	)
}

// TestResumedPermissionStepRunsTheWholeBatch: a step parked at a permission gate
// executed NOTHING (access is resolved for the whole batch before any call runs),
// so its sibling calls are run on the successor — once — rather than reported as
// unknown.
func TestResumedPermissionStepRunsTheWholeBatch(t *testing.T) {
	t.Parallel()
	store := newRestoreStore(t)
	originalEcho := &echoTool{}
	sessionID, parked := parkAndAbandon(t, store,
		echoGatedDefinition(t, &echoThenGatedLLM{}, originalEcho, &gatedE2ETool{seq: &atomic.Int64{}}), gatedomain.KindPermission)
	if n := originalEcho.runs.Load(); n != 0 {
		t.Fatalf("a sibling ran %d times before its batch's access resolved", n)
	}

	successorEcho := &echoTool{}
	successorGated := &gatedE2ETool{seq: &atomic.Int64{}}
	successorLLM := &echoThenGatedLLM{resumeScriptLLM: resumeScriptLLM{continueOnly: true}}
	restored := restoreParked(t, store, echoGatedDefinition(t, successorLLM, successorEcho, successorGated), sessionID)
	sub, err := restored.SubscribeEvents(event.EventFilter{Enduring: event.LoopScope{All: true}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sub.Close() }()
	if err := restored.RespondGate(context.Background(), gatedomain.GateResponse{
		GateID: parked.ID, Action: string(gatedomain.ApprovalApprove),
		Source: gatedomain.ResponseSource{Kind: gatedomain.ResponseFromUser},
	}); err != nil {
		t.Fatal(err)
	}
	if _, ok := awaitTurnTerminal(t, sub).(event.TurnDone); !ok {
		t.Fatal("resumed turn did not finish")
	}
	if n := successorEcho.runs.Load(); n != 1 {
		t.Fatalf("sibling ran %d times on the successor, want 1", n)
	}
	if runs, _ := successorGated.snapshot(); runs != 1 {
		t.Fatalf("approved call ran %d times, want 1", runs)
	}
	requests := successorLLM.snapshot()
	if len(requests) != 1 {
		t.Fatalf("successor requests = %d, want 1", len(requests))
	}
	msgs := requests[0].Messages
	echoResult, ok := msgs[len(msgs)-2].(*content.ToolResultMessage)
	if !ok || echoResult.ToolUseID != "call-0" || echoResult.IsError || messageText(echoResult.Blocks) != "echoed" {
		t.Fatalf("sibling result = %#v, want call-0's real result", msgs[len(msgs)-2])
	}
}
