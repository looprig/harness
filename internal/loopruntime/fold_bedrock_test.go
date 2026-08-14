package loopruntime

import (
	"context"
	"testing"
	"time"

	"github.com/looprig/core/content"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/tool"
	contextcount "github.com/looprig/inference/contextcount"
	model "github.com/looprig/inference/model"
)

// bedrockTestModel is a valid Bedrock Converse model descriptor with room for the
// small transcripts these tests build. It is what makes the loop's context counter
// run the real bedrockconverse request encoder.
func bedrockTestModel() model.Model {
	m := model.Model{
		Provider:  model.ProviderName("bedrock"),
		APIFormat: model.APIFormatBedrockConverse,
		BaseURL:   "https://bedrock-runtime.us-east-1.amazonaws.com",
		Name:      "anthropic.claude-sonnet-4-20250514-v1:0",
		Limits:    testContextLimits{WindowTokens: 200000, MaxInputTokens: 180000, MaxOutputTokens: 8000},
	}
	m.Caps.Tools = true
	return m
}

// newBedrockFoldLoop builds a fold-capable loop whose context counter is the real
// bundled estimator — the same component production uses to measure a candidate
// request, and therefore the same component that encodes the transcript into the
// Bedrock Converse wire shape BEFORE any HTTP call. A projection this loop cannot
// encode fails the turn locally, exactly as it does in production.
func newBedrockFoldLoop(t *testing.T, client *scriptedLLM, ts ToolSet) (*Loop, *recordingPublisher) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	sessionID, err := uuid.New()
	if err != nil {
		t.Fatalf("uuid.New: %v", err)
	}
	loopID, err := uuid.New()
	if err != nil {
		t.Fatalf("uuid.New: %v", err)
	}
	counter := contextcount.NewEstimator()
	rec := &recordingPublisher{}
	l, err := newWithConfig(ctx, sessionID, loopID, Provenance{}, rec, runtimeConfig{
		Client:              client,
		Model:               bedrockTestModel(),
		Tools:               ts,
		DrainTimeout:        500 * time.Millisecond,
		ContextCounter:      counter,
		CounterCapability:   counter.CounterCapability(),
		InferenceCapability: contextTestInferenceCapability(),
		ContextObservation:  &loop.ContextObservationPolicy{ReservedOutput: 1000, CountTimeout: time.Second},
	})
	if err != nil {
		t.Fatalf("newWithConfig: %v", err)
	}
	return l, rec
}

// TestFoldedInputEncodesForBedrockConverse drives the REAL fold path against the REAL
// Bedrock Converse encoder.
//
// The user types while a tool call is in flight; foldPending commits that message into
// history immediately after the tool results, and the mandatory continuation request
// is then measured by the loop's context counter, which encodes it for Bedrock. Before
// the projector learned to separate the two turns this measurement failed with a
// conversation collision, so the turn died as event.TurnFailed with the message already
// committed — and every later turn re-projected the same history and failed the same
// way, which is what a user experiences as a permanently wedged session.
//
// The second turn is the recovery half: it re-projects the stored transcript that
// contains the tool-result -> user adjacency. A transcript already wedged by the old
// projector therefore recovers on its next turn with no history surgery.
func TestFoldedInputEncodesForBedrockConverse(t *testing.T) {
	t.Parallel()

	bt := newBlockingTool()
	ts := agenticToolSet([]tool.InvokableTool{bt}, 25, 100)
	client := &scriptedLLM{scripts: [][]content.Chunk{
		{toolUseChunk(0, "call-1", "Block", `{}`)}, // turn 1 step 0: the turn parks in the tool
		{textChunk("stopping")},                    // turn 1 step 1: continuation carrying the folded input
		{textChunk("second turn answer")},          // turn 2: re-projects the folded transcript
	}}
	l, rec := newBedrockFoldLoop(t, client, ts)

	startTurn(t, l, rec, textBlocks("read the file"))
	<-bt.started

	foldedID := mustID(t)
	if d := submitUserInputBlocks(t, l, rec, foldedID, textBlocks("actually, stop")); !isQueued(d) {
		t.Fatalf("queued submit outcome = %T, want event.InputQueued", d)
	}
	close(bt.release)

	terminal := drainToTerminal(t, rec)
	if failed, ok := terminal.(event.TurnFailed); ok {
		t.Fatalf("turn 1 failed after folding queued input: %v", failed.Err)
	}
	if _, ok := terminal.(event.TurnDone); !ok {
		t.Fatalf("turn 1 terminal = %T, want event.TurnDone", terminal)
	}

	// The folded message is committed history now. A later turn re-projects it, so
	// the fix has to hold for every subsequent request, not just the continuation.
	next := terminalIndex(rec, 0)
	startTurn(t, l, rec, textBlocks("anything else?"))
	terminal = awaitTerminalAfter(t, rec, next)
	if failed, ok := terminal.(event.TurnFailed); ok {
		t.Fatalf("turn 2 failed re-projecting the folded transcript: %v", failed.Err)
	}
	if _, ok := terminal.(event.TurnDone); !ok {
		t.Fatalf("turn 2 terminal = %T, want event.TurnDone", terminal)
	}

	// Turn 2's request carries the whole transcript, including the tool-result ->
	// folded-user adjacency that used to be unencodable.
	reqs := waitForRequests(t, client, 3)
	last := reqs[len(reqs)-1].Messages
	var sawToolResultThenUser bool
	for i := 1; i < len(last); i++ {
		if _, ok := last[i-1].(*content.ToolResultMessage); !ok {
			continue
		}
		if _, ok := last[i].(*content.UserMessage); ok {
			sawToolResultThenUser = true
		}
	}
	if !sawToolResultThenUser {
		t.Fatal("turn 2 request has no tool-result -> user adjacency; the test is no longer exercising the defect")
	}
}

// isQueued reports whether a submit outcome is event.InputQueued.
func isQueued(e event.Event) bool {
	_, ok := e.(event.InputQueued)
	return ok
}
