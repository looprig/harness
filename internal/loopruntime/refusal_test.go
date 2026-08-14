package loopruntime

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/looprig/core/content"
	"github.com/looprig/inference"
)

// refusalText is the wording a provider hands back when it declines. Its
// contents are irrelevant to every assertion here — what matters is that the
// block arrives, and keeps arriving, as a refusal rather than as prose.
const refusalText = "I can't help with that."

// TestIsEmptyAssistantMessageCountsRefusalAsContent proves a refusal-only reply
// is NOT an empty response. A refusal is the model's answer to the request: it
// declined. Classifying it as no content fails a completed turn with
// EmptyResponseError and throws the refusal away, so the caller is told the
// model produced nothing for a request the model actively refused.
//
// An EMPTY refusal counts too. A structured-output refusal can arrive with no
// explanation at all, so the block's presence — never its contents — is the
// signal, exactly as core's RefusalBlock documents.
func TestIsEmptyAssistantMessageCountsRefusalAsContent(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		blocks []content.Block
		want   bool
	}{
		{
			name:   "explained refusal is content",
			blocks: []content.Block{&content.RefusalBlock{Text: refusalText}},
			want:   false,
		},
		{
			name:   "unexplained refusal is content even with empty Text",
			blocks: []content.Block{&content.RefusalBlock{}},
			want:   false,
		},
		{
			name:   "refusal alongside an empty text block is content",
			blocks: []content.Block{&content.TextBlock{}, &content.RefusalBlock{}},
			want:   false,
		},
		{
			name:   "an image-only reply is content",
			blocks: []content.Block{&content.ImageBlock{MediaType: "image/png", Source: content.ImageSource{Data: []byte{1}}}},
			want:   false,
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			aiMsg := &content.AIMessage{Message: content.Message{Role: content.RoleAssistant, Blocks: tt.blocks}}
			if got := isEmptyAssistantMessage(aiMsg, nil); got != tt.want {
				t.Errorf("isEmptyAssistantMessage() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestSanitizeAssistantBlocksKeepsRefusal proves the STORED assistant message
// retains the refusal, including an empty one. The two predicates that decide a
// block's fate must agree: a block isEmptyAssistantMessage counts as content and
// sanitizeAssistantBlocks then drops would fail no turn but would leave the
// history claiming the model answered with nothing.
func TestSanitizeAssistantBlocksKeepsRefusal(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   []content.Block
		want []content.Block
	}{
		{
			name: "explained refusal survives",
			in:   []content.Block{&content.RefusalBlock{Text: refusalText}},
			want: []content.Block{&content.RefusalBlock{Text: refusalText}},
		},
		{
			name: "unexplained refusal survives",
			in:   []content.Block{&content.RefusalBlock{}},
			want: []content.Block{&content.RefusalBlock{}},
		},
		{
			name: "refusal survives beside a dropped empty text block",
			in:   []content.Block{&content.TextBlock{}, &content.RefusalBlock{Text: refusalText}},
			want: []content.Block{&content.RefusalBlock{Text: refusalText}},
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := sanitizeAssistantBlocks(tt.in); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("sanitizeAssistantBlocks() = %#v, want %#v", got, tt.want)
			}
		})
	}
}

// TestRefusalOnlyTurnSurvivesCommitAndRestore is the end-to-end proof. A turn
// whose ONLY content is a streamed refusal has to walk the same three copy
// boundaries a normal reply does — materialization, durable commit, restore seed
// and request rebuild — and stay a refusal at each one.
//
// Every boundary had its own way of losing it: the stream fold never
// accumulated RefusalChunks, the emptiness check classified the resulting turn
// as an empty response, and both block clones erased the variant. Any one of
// them turns "the model declined" into either a hard EmptyResponseError or a
// silent empty answer.
func TestRefusalOnlyTurnSurvivesCommitAndRestore(t *testing.T) {
	t.Parallel()

	client := &recordingLLM{chunks: []content.Chunk{&content.RefusalChunk{Text: refusalText}}}
	l, rec, _ := newLoop(t, client)
	startTurn(t, l, rec, []content.Block{&content.TextBlock{Text: "go"}})

	if terminal := drainToTerminal(t, rec); !isTurnDone(terminal) {
		t.Fatalf("terminal = %#v, want event.TurnDone (a refusal is an answer, not an empty response)", terminal)
	}

	committed := stepDones(rec.events())
	if len(committed) != 1 {
		t.Fatalf("StepDone count = %d, want 1", len(committed))
	}
	if got := findRefusalBlock(t, committed[0].Messages); got.Text != refusalText {
		t.Errorf("committed refusal Text = %q, want %q", got.Text, refusalText)
	}

	// Restore a loop from that committed history and drive one more turn, then
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

	if got := findRefusalBlock(t, replayClient.lastReq().Messages); got.Text != refusalText {
		t.Errorf("replayed refusal Text = %q, want %q", got.Text, refusalText)
	}
}

// TestFlattenToTextNamesARefusalBlock covers the tool-result rendering path. A
// refusal is not tool output — tools do not decline — so one appearing in a
// result is anomalous content whose TEXT must not be spliced into the result the
// model reads back as its tool's answer. It renders as the same visible
// placeholder every other non-text block gets, but the placeholder has to name
// the block: an unnamed "[unsupported unknown]" tells an operator nothing about
// what the tool actually returned.
func TestFlattenToTextNamesARefusalBlock(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		blocks []content.Block
		want   string
	}{
		{
			name:   "a refusal renders as a named placeholder, never as its text",
			blocks: []content.Block{&content.RefusalBlock{Text: refusalText}},
			want:   "[unsupported refusal]",
		},
		{
			name: "a refusal beside text keeps the text and marks the refusal",
			blocks: []content.Block{
				&content.TextBlock{Text: "ran"},
				&content.RefusalBlock{Text: refusalText},
			},
			want: "ran[unsupported refusal]",
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := flattenToText(tt.blocks); got != tt.want {
				t.Errorf("flattenToText() = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestValidateRawToolFrameReportsARefusalAsSuch covers the structured-output
// interception boundary. A refusal there is fail-closed either way, but the
// classification is the whole value of a typed error: "inconsistent_tool_frame"
// sends an operator looking for a codec or accumulator bug, when what actually
// happened is that the model declined to produce the structured output at all.
func TestValidateRawToolFrameReportsARefusalAsSuch(t *testing.T) {
	t.Parallel()

	message := &content.AIMessage{Message: content.Message{
		Role:   content.RoleAssistant,
		Blocks: []content.Block{&content.RefusalBlock{Text: refusalText}},
	}}

	err := validateRawToolFrame(message, nil)
	var conflict *inference.StructuredOutputConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("validateRawToolFrame() error = %T %v, want *inference.StructuredOutputConflictError", err, err)
	}
	if conflict.Feature != "model_refusal" {
		t.Errorf("Feature = %q, want %q", conflict.Feature, "model_refusal")
	}
}

// findRefusalBlock returns the single RefusalBlock in a message graph, failing
// the test when the graph carries none. A graph that instead carries the same
// words in a TextBlock is the silent failure this looks for, so the assertion is
// on the concrete type and not on the text.
func findRefusalBlock(t *testing.T, messages content.AgenticMessages) *content.RefusalBlock {
	t.Helper()
	var found *content.RefusalBlock
	for _, message := range messages {
		ai, ok := message.(*content.AIMessage)
		if !ok {
			continue
		}
		for _, block := range ai.Blocks {
			if refusal, ok := block.(*content.RefusalBlock); ok {
				if found != nil {
					t.Fatalf("want exactly one RefusalBlock, found a second: %#v", refusal)
				}
				found = refusal
			}
		}
	}
	if found == nil {
		t.Fatalf("no RefusalBlock in %#v", messages)
	}
	return found
}
