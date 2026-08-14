package loopruntime

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/looprig/core/content"
	"github.com/looprig/harness/pkg/event"
)

// anthropicFormat is the codec dialect label an Anthropic thinking signature is
// tagged with. Replay is legal only against the dialect that issued the state,
// so this exact string is what ReplayableAs must still answer true for on the
// far side of a commit and a restore.
const anthropicFormat = "anthropic"

// signedThinkingChunk is a complete Anthropic reasoning block as it arrives on
// the stream: the visible reasoning plus the opaque signature the provider
// requires back, verbatim, on the next request of the conversation.
func signedThinkingChunk() content.Chunk {
	return &content.ThinkingChunk{
		Index:               0,
		Thinking:            "weighing the options",
		Signature:           "sig-abc",
		ProviderState:       json.RawMessage(`{"signature":"sig-abc"}`),
		ProviderStateFormat: anthropicFormat,
	}
}

// TestThinkingProviderStateSurvivesCommitAndRestore is the end-to-end proof for
// the whole provider-state bundle. Every codec in the inference layer carefully
// preserves reasoning continuation state, and none of that is observable unless
// Harness carries the state across its own three copy boundaries. The test
// walks a streamed assistant turn through all of them in sequence:
//
//  1. materialization — the streamed ThinkingChunk becomes a ThinkingBlock;
//  2. durable commit — the block reaches the consumer-visible StepDone;
//  3. restore seed + request build — a loop rebuilt from that committed history
//     puts the block back on the wire for the next turn.
//
// ReplayableAs must still be true at the end. Any single dropped copy along the
// way turns provider-side reasoning replay into dead code silently: nothing
// errors, the model simply never receives its own signature back.
func TestThinkingProviderStateSurvivesCommitAndRestore(t *testing.T) {
	t.Parallel()

	// 1 + 2: run a live turn and read the committed step group off the fan-in.
	client := &recordingLLM{chunks: []content.Chunk{signedThinkingChunk(), textChunk("done")}}
	l, rec, _ := newLoop(t, client)
	startTurn(t, l, rec, []content.Block{&content.TextBlock{Text: "go"}})
	drainToTerminal(t, rec)

	committed := stepDones(rec.events())
	if len(committed) != 1 {
		t.Fatalf("StepDone count = %d, want 1", len(committed))
	}
	commitBlock := findThinkingBlock(t, committed[0].Messages)
	if !commitBlock.ReplayableAs(anthropicFormat) {
		t.Fatalf("committed thinking block is not replayable: %#v", commitBlock)
	}

	// 3: restore a loop from that committed history and drive one more turn, then
	// read the request the provider would actually have received.
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	replayClient := &recordingLLM{chunks: []content.Chunk{textChunk("ok")}}
	restoredRec := &recordingPublisher{}
	seed := content.AgenticMessages{seededUser("go")}
	seed = append(seed, committed[0].Messages...)

	restored, err := newRestoredWithConfig(ctx, mustID(t), mustID(t), restoredRec,
		runtimeConfig{Client: replayClient, Model: testModel(), DrainTimeout: 200 * time.Millisecond},
		RestoredState{Msgs: seed, TurnIndex: 1})
	if err != nil {
		t.Fatalf("NewRestored: %v", err)
	}
	startTurn(t, restored, restoredRec, []content.Block{&content.TextBlock{Text: "next"}})
	drainToTerminal(t, restoredRec)

	requestBlock := findThinkingBlock(t, replayClient.lastReq().Messages)
	if !requestBlock.ReplayableAs(anthropicFormat) {
		t.Fatalf("restored request's thinking block is not replayable: %#v", requestBlock)
	}
	if string(requestBlock.ProviderState) != `{"signature":"sig-abc"}` {
		t.Errorf("ProviderState = %s, want %s", requestBlock.ProviderState, `{"signature":"sig-abc"}`)
	}
	if requestBlock.Signature != "sig-abc" {
		t.Errorf("Signature = %q, want %q", requestBlock.Signature, "sig-abc")
	}
}

// findThinkingBlock returns the single ThinkingBlock in a message graph, failing
// the test when the graph carries none (the symptom of a copy that dropped it).
func findThinkingBlock(t *testing.T, messages content.AgenticMessages) *content.ThinkingBlock {
	t.Helper()
	var found *content.ThinkingBlock
	for _, message := range messages {
		ai, ok := message.(*content.AIMessage)
		if !ok {
			continue
		}
		for _, block := range ai.Blocks {
			if thinking, ok := block.(*content.ThinkingBlock); ok {
				if found != nil {
					t.Fatalf("want exactly one ThinkingBlock, found a second: %#v", thinking)
				}
				found = thinking
			}
		}
	}
	if found == nil {
		t.Fatalf("no ThinkingBlock in %#v", messages)
	}
	return found
}

// TestRedactedThinkingOnlyTurnSucceeds proves a reply whose ONLY content is a
// redacted reasoning block completes the turn instead of failing it. The block
// looks empty (no text, no visible reasoning) but is the sole carrier of the
// continuation state, so treating it as no content both fails a valid turn with
// EmptyResponseError and throws away the state.
func TestRedactedThinkingOnlyTurnSucceeds(t *testing.T) {
	t.Parallel()

	redacted := &content.ThinkingChunk{
		Index:               0,
		ProviderState:       json.RawMessage(`{"data":"redacted-blob"}`),
		ProviderStateFormat: anthropicFormat,
	}
	client := &recordingLLM{chunks: []content.Chunk{redacted}}
	l, rec, _ := newLoop(t, client)
	startTurn(t, l, rec, []content.Block{&content.TextBlock{Text: "go"}})

	if terminal := drainToTerminal(t, rec); !isTurnDone(terminal) {
		t.Fatalf("terminal = %#v, want event.TurnDone", terminal)
	}
	block := findThinkingBlock(t, stepDones(rec.events())[0].Messages)
	if !block.ReplayableAs(anthropicFormat) {
		t.Errorf("committed redacted block is not replayable: %#v", block)
	}
}

func isTurnDone(ev event.Event) bool {
	_, ok := ev.(event.TurnDone)
	return ok
}
