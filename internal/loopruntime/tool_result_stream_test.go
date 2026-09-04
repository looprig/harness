package loopruntime

import (
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/looprig/core/content"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/gate"
	"github.com/looprig/harness/pkg/tool"
	"github.com/looprig/inference"
)

// capturingRunTool is a fakeRunTool that ALSO implements
// tool.CapturingInvokableTool: it streams raw bytes into the sink and returns a
// separate bounded ToolResult, which is the whole point of the capability. It
// records which entry point ran so a test can tell the streaming path from the
// fallback rather than inferring it from the result.
type capturingRunTool struct {
	*fakeRunTool

	raw      string
	preview  string
	runErr   error
	empty    bool
	panicMsg string

	mu       sync.Mutex
	captured int
	plain    int
}

func newCapturingRunTool(name, raw, preview string) *capturingRunTool {
	return &capturingRunTool{fakeRunTool: &fakeRunTool{name: name, output: preview}, raw: raw, preview: preview}
}

func (c *capturingRunTool) InvokableRun(ctx context.Context, argsJSON string) (*tool.ToolResult, error) {
	c.mu.Lock()
	c.plain++
	c.mu.Unlock()
	return c.fakeRunTool.InvokableRun(ctx, argsJSON)
}

func (c *capturingRunTool) InvokableRunCaptured(ctx context.Context, argsJSON string, sink tool.ResultCaptureSink) (*tool.ToolResult, error) {
	c.mu.Lock()
	c.captured++
	c.mu.Unlock()
	// Written in pieces, which is what a real streaming producer does and what
	// makes "the ceiling applies to the running total" reachable from here.
	for offset := 0; offset < len(c.raw); offset += 7 {
		end := min(offset+7, len(c.raw))
		if _, err := io.WriteString(sink, c.raw[offset:end]); err != nil {
			return nil, err
		}
	}
	if c.panicMsg != "" {
		panic(c.panicMsg)
	}
	if c.runErr != nil {
		return nil, c.runErr
	}
	if c.empty {
		return &tool.ToolResult{}, nil
	}
	return tool.TextResult(c.preview), nil
}

func (c *capturingRunTool) counts() (captured, plain int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.captured, c.plain
}

// captureRuntime builds the BatchRuntime a loop with retention configured
// produces, backed by a real session spill directory.
func captureRuntime(t *testing.T, ceiling int) (BatchRuntime, *captureSpillDirectory) {
	t.Helper()
	session, err := uuid.New()
	if err != nil {
		t.Fatalf("uuid.New: %v", err)
	}
	dir, err := newCaptureSpillDirectory(t.TempDir(), session)
	if err != nil {
		t.Fatalf("newCaptureSpillDirectory: %v", err)
	}
	t.Cleanup(func() { _ = dir.Release() })
	return BatchRuntime{
		GateRegistrations: make(chan gateRegistration),
		IDGen:             uuid.New,
		Emit:              func(event.Event) {},
		captureSinks:      spillCaptureSinks(dir, ceiling),
	}, dir
}

// TestRunBatchPrefersTheStreamingCapability covers the preference in both
// directions inside ONE batch, which is also the concurrent-tools case: a
// capturing tool must reach InvokableRunCaptured and carry its streamed spill on
// the result, while a materialized tool in the same batch reaches InvokableRun
// and carries no capture. Each capturing call must get its OWN spill, keyed by
// its own ToolExecutionID.
func TestRunBatchPrefersTheStreamingCapability(t *testing.T) {
	t.Parallel()
	streaming := newCapturingRunTool("Stream", strings.Repeat("R", 300), "preview")
	plain := &fakeRunTool{name: "Plain", output: "plain output"}
	ts := ToolSet{Access: autoApproveGate{}, Registry: []tool.InvokableTool{streaming, plain}}
	runtime, dir := captureRuntime(t, 4096)
	calls := []content.ToolUseBlock{call(t, "Stream", `{}`), call(t, "Plain", `{}`), call(t, "Stream", `{}`)}
	results := RunBatch(context.Background(), calls, resolveToolSetCaps(ts), runtime)
	if len(results) != 3 {
		t.Fatalf("results = %d, want 3", len(results))
	}
	captured, plainRuns := streaming.counts()
	if captured != 2 || plainRuns != 0 {
		t.Fatalf("streaming tool entry points: captured=%d plain=%d, want 2/0", captured, plainRuns)
	}
	if results[1].capture != nil {
		t.Fatal("a materialized tool's result carries a capture sink")
	}
	paths := map[string]bool{}
	for _, i := range []int{0, 2} {
		sink := results[i].capture
		if sink == nil {
			t.Fatalf("result %d carries no capture sink", i)
		}
		if got := sink.capturedBytes(); got != 300 {
			t.Errorf("result %d capturedBytes = %d, want 300", i, got)
		}
		backing, ok := sink.backing.(*fileBacking)
		if !ok {
			t.Fatalf("result %d backing = %T, want a file backing", i, sink.backing)
		}
		if paths[backing.path] {
			t.Errorf("two concurrent calls share the spill path %q", backing.path)
		}
		paths[backing.path] = true
		if want := spillPathFor(dir, results[i].ToolExecutionID); backing.path != want {
			t.Errorf("result %d spill path = %q, want %q (keyed by ToolExecutionID)", i, backing.path, want)
		}
	}
	if resultText(results[0]) != "preview" {
		t.Fatalf("committed content = %q, want the tool's bounded preview", resultText(results[0]))
	}
}

func spillPathFor(dir *captureSpillDirectory, id uuid.UUID) string {
	path, err := dir.spillPath(id)
	if err != nil {
		panic(err)
	}
	return path
}

// TestRunBatchFallsBackWhenRetentionIsNotConfigured pins the other half of the
// preference: with no capture sink factory — a loop with no object store — a
// capturing tool takes the ordinary InvokableRun path and nothing is spilled.
// Without this the capability would silently change behaviour for every existing
// composition.
func TestRunBatchFallsBackWhenRetentionIsNotConfigured(t *testing.T) {
	t.Parallel()
	streaming := newCapturingRunTool("Stream", strings.Repeat("R", 300), "preview")
	ts := resolveToolSetCaps(ToolSet{Access: autoApproveGate{}, Registry: []tool.InvokableTool{streaming}})
	results := runBatchNoGate(context.Background(), []content.ToolUseBlock{call(t, "Stream", `{}`)}, ts, func(event.Event) {})
	captured, plain := streaming.counts()
	if captured != 0 || plain != 1 {
		t.Fatalf("entry points: captured=%d plain=%d, want 0/1", captured, plain)
	}
	if results[0].capture != nil {
		t.Fatal("a spill was opened with no retention configured")
	}
}

// TestRunBatchReleasesTheSpillOfACallThatProducedNoResult covers every way a
// capturing call can end without a retainable result — an error, an empty
// result, and a panic. Each still opened a spill, and each must leave nothing on
// disk: the retention pipeline never sees these results, so the runner is the
// only layer that can release them.
func TestRunBatchReleasesTheSpillOfACallThatProducedNoResult(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		arrange func(*capturingRunTool)
	}{
		{name: "tool error", arrange: func(c *capturingRunTool) { c.runErr = errors.New("boom") }},
		{name: "empty result", arrange: func(c *capturingRunTool) { c.empty = true }},
		{name: "tool panic", arrange: func(c *capturingRunTool) { c.panicMsg = "kaboom" }},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			streaming := newCapturingRunTool("Stream", "raw bytes", "preview")
			tt.arrange(streaming)
			ts := resolveToolSetCaps(ToolSet{Access: autoApproveGate{}, Registry: []tool.InvokableTool{streaming}})
			runtime, dir := captureRuntime(t, 4096)
			results := RunBatch(context.Background(), []content.ToolUseBlock{call(t, "Stream", `{}`)}, ts, runtime)
			if results[0].capture != nil {
				t.Fatalf("a result with no retainable content carries a capture sink")
			}
			entries, err := os.ReadDir(dir.root)
			if err != nil {
				t.Fatalf("read spill root: %v", err)
			}
			if len(entries) != 0 {
				t.Fatalf("spill root holds %d files after a call that produced no result", len(entries))
			}
		})
	}
}

// TestRunBatchAppliesMiddlewareToTheStreamingPath holds the capability to the
// same contract the fallback has: a middleware wraps tool execution, and a
// streaming call that skipped the chain would silently drop every consumer's
// middleware for exactly the tools that produce the most output.
func TestRunBatchAppliesMiddlewareToTheStreamingPath(t *testing.T) {
	t.Parallel()
	streaming := newCapturingRunTool("Stream", "raw", "preview")
	var wrapped int
	middleware := func(ctx context.Context, tl tool.InvokableTool, argsJSON string, next tool.ToolExecuteFunc) (*tool.ToolResult, error) {
		wrapped++
		return next(ctx, argsJSON)
	}
	ts := resolveToolSetCaps(ToolSet{Access: autoApproveGate{}, Registry: []tool.InvokableTool{streaming}, Middlewares: []tool.ToolMiddleware{middleware}})
	runtime, _ := captureRuntime(t, 4096)
	results := RunBatch(context.Background(), []content.ToolUseBlock{call(t, "Stream", `{}`)}, ts, runtime)
	if wrapped != 1 {
		t.Fatalf("middleware invocations = %d, want 1", wrapped)
	}
	captured, plain := streaming.counts()
	if captured != 1 || plain != 0 {
		t.Fatalf("entry points: captured=%d plain=%d, want 1/0", captured, plain)
	}
	if results[0].capture == nil {
		t.Fatal("the streamed capture was lost through the middleware chain")
	}
}

// TestRunBatchSurvivesASpillThatCannotBeOpened pins the fail-forward rule: the
// tool still runs and still returns its result, and the unusable sink carries the
// open failure so retention — not the tool — is what fails. A runner that refused
// the call would turn a loop-side storage problem into a tool error.
func TestRunBatchSurvivesASpillThatCannotBeOpened(t *testing.T) {
	t.Parallel()
	streaming := newCapturingRunTool("Stream", "raw bytes", "preview")
	ts := resolveToolSetCaps(ToolSet{Access: autoApproveGate{}, Registry: []tool.InvokableTool{streaming}})
	runtime, dir := captureRuntime(t, 4096)
	if err := dir.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}
	results := RunBatch(context.Background(), []content.ToolUseBlock{call(t, "Stream", `{}`)}, ts, runtime)
	if resultText(results[0]) != "preview" {
		t.Fatalf("result = %q, want the tool's own result: a spill failure is not a tool failure", resultText(results[0]))
	}
	if results[0].IsError {
		t.Fatal("a spill failure was reported to the model as a tool error")
	}
	sink := results[0].capture
	if sink == nil || sink.spillErr() == nil {
		t.Fatalf("capture = %+v, want a sink carrying the open failure", sink)
	}
	if sink.offeredBytes() != uint64(len("raw bytes")) {
		t.Fatalf("offeredBytes = %d, want %d: a failed spill still counts the producer's bytes", sink.offeredBytes(), len("raw bytes"))
	}
}

// --- turn-level spill release on the paths that discard a batch (H5.3 fix round) ---

// turnCaptureFixture wires a turn for the streaming path: a real session spill
// directory, a store so retention is configured at all, and the toolset the
// caller supplies. It returns the spill root so a test can assert what is left on
// disk.
func turnCaptureFixture(t *testing.T, ts ToolSet, client inference.Client, gateReg chan<- gateRegistration) (turnConfig, turnState, *turnRecorder, *captureSpillDirectory) {
	t.Helper()
	session, err := uuid.New()
	if err != nil {
		t.Fatalf("uuid.New: %v", err)
	}
	dir, err := newCaptureSpillDirectory(t.TempDir(), session)
	if err != nil {
		t.Fatalf("newCaptureSpillDirectory: %v", err)
	}
	t.Cleanup(func() { _ = dir.Release() })
	cfg, state, rec := newTurnFixture([]content.Block{&content.TextBlock{Text: "go"}}, nil, ts, client, gateReg)
	cfg.toolResultObjects = newFakeObjectStore()
	cfg.toolResultSpills = dir
	return cfg, state, rec, dir
}

func requireEmptySpillRoot(t *testing.T, dir *captureSpillDirectory, when string) {
	t.Helper()
	entries, err := os.ReadDir(dir.root)
	if err != nil {
		t.Fatalf("read spill root: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("spill root holds %d files after %s; nothing downstream will ever remove them", len(entries), when)
	}
}

// TestRunTurnReleasesSpillsWhenTheBatchIsCancelled is the reader for the release
// on turn.go's cancellation path. TestToolResultRetentionCancellationReleasesEveryStreamedSpill
// sounds like it covers this and does not: it proves the property inside
// retainToolResults, which is exactly the function this path never reaches — the
// turn returns TurnInterrupted before retention is called at all.
func TestRunTurnReleasesSpillsWhenTheBatchIsCancelled(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	// The tool cancels the turn from inside its own streaming call, AFTER it has
	// written to the sink: the spill therefore exists on disk at the moment the
	// turn decides to discard the batch.
	streaming := &turnStreamingTool{name: "Streamer", raw: strings.Repeat("C", 2048), preview: "preview", onCaptured: cancel}
	ts := agenticToolSet([]tool.InvokableTool{streaming}, 25, 100)
	client := &scriptedLLM{scripts: [][]content.Chunk{{toolUseChunk(0, "id-stream", "Streamer", `{}`)}}}
	cfg, state, _, dir := turnCaptureFixture(t, ts, client, noGateReg())

	terminal := runTurn(ctx, cfg, state)
	if _, ok := terminal.(event.TurnInterrupted); !ok {
		t.Fatalf("terminal = %T, want TurnInterrupted", terminal)
	}
	if streaming.capturedRuns() != 1 {
		t.Fatalf("streaming runs = %d, want 1: the spill under test was never written", streaming.capturedRuns())
	}
	requireEmptySpillRoot(t, dir, "a cancelled batch")
}

// TestReviewCaptureFailureCannotLeaveASpillIsGuaranteedByRunBatch measures what
// actually protects turn.go's review-capture-failure path, which is NOT the
// releaseCaptures call on that line.
//
// The gates asked for a reader for that call. There cannot be one, and the reason
// is a mechanism rather than a gap: RunBatch resolves access SEQUENTIALLY before
// any execution, and a *reviewContextCaptureError returns collectResults(rs)
// immediately — before the emit-Started phase and before any call runs — precisely
// so a non-gated call ahead of the failing one cannot execute while the turn fails
// closed. No call executes, so no result can carry a capture sink, so the release
// is a no-op by construction and deleting it is an EQUIVALENT mutation.
//
// What is worth pinning is the premise, which is what would change if RunBatch
// ever grew an execute-before-review path: with a batch mixing a gated call and a
// non-gated CAPTURING call over an over-bound conversation, no tool runs, no
// result carries a capture, and the session spill root is untouched.
func TestReviewCaptureFailureCannotLeaveASpillIsGuaranteedByRunBatch(t *testing.T) {
	t.Parallel()
	gated := &fakeRunTool{name: "T", output: "must not run"}
	gated.prepareFn = func(executionID uuid.UUID, _ string) (tool.Request, tool.PreparedArtifact, error) {
		return commandRequest(executionID, "git status", false), nil, nil
	}
	streaming := &turnStreamingTool{name: "Streamer", raw: strings.Repeat("R", 2048), preview: "preview"}
	ts := resolveToolSetCaps(ToolSet{
		Access:   interactiveEvaluator(t, gate.AccessGated, &recordingRuleWriter{}, &recordingIssuer{}),
		Registry: []tool.InvokableTool{gated, streaming},
	})
	client := &scriptedLLM{scripts: [][]content.Chunk{{
		toolUseChunk(0, "gated", "T", `{}`),
		toolUseChunk(1, "streamed", "Streamer", `{}`),
	}}}
	gateReg := make(chan gateRegistration, 1)
	cfg, state, rec, dir := turnCaptureFixture(t, ts, client, gateReg)
	cfg.base = content.AgenticMessages{
		reviewUserMessage(strings.Repeat("x", gate.MaxReviewContextEntryInputBytes+1)),
	}
	cfg.reviewContext = &reviewContextConfiguration{
		Metadata: reviewContextMetadata{
			WorkspaceRoot:      "/workspace",
			WorkingDirectory:   "/workspace",
			SecurityCeiling:    "workspace-write; unmet-requirements=true",
			GatePolicyRevision: "gate-policy-v1",
		},
		Policy: testReviewContextPolicy(),
	}

	terminal := runTurn(context.Background(), cfg, state)
	failed, ok := terminal.(event.TurnFailed)
	if !ok {
		t.Fatalf("terminal = %T, want TurnFailed on the review-capture failure", terminal)
	}
	var captureErr *reviewContextCaptureError
	if !errors.As(failed.Err, &captureErr) {
		t.Fatalf("TurnFailed.Err = %v, want *reviewContextCaptureError", failed.Err)
	}
	if got := streaming.capturedRuns(); got != 0 {
		t.Fatalf("streaming runs = %d, want 0: RunBatch must abort the whole batch before the execute phase", got)
	}
	// The phase boundary itself, which is the premise the equivalence rests on:
	// RunBatch emits a ToolCallStarted for EVERY requested call immediately before
	// the execute phase, so zero Started events is the observable form of "the
	// batch was abandoned before any call could open a spill".
	for _, ev := range rec.events() {
		if _, started := ev.(event.ToolCallStarted); started {
			t.Fatal("a ToolCallStarted was emitted, so the batch reached its execute phase and turn.go's release is no longer a no-op")
		}
	}
	requireEmptySpillRoot(t, dir, "a turn that failed on review capture")
}
