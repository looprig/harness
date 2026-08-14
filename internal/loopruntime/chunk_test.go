package loopruntime

import (
	"bytes"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/looprig/core/content"
	"github.com/looprig/core/content/blocktest"
	"github.com/looprig/harness/pkg/event"
)

// TestChunkProcessorEmitThenAccumulate proves the chunk layer's ordering
// contract: process(chunk) emits the live TokenDelta for the chunk BEFORE
// folding it into the blockState. The emit callback inspects the blockState at
// emit time; if accumulation already happened the block would be visible, which
// would violate the "emit then accumulate" order.
func TestChunkProcessorEmitThenAccumulate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		chunk       content.Chunk
		wantText    string
		wantThink   string
		wantTool    bool
		wantRefusal string
		wantImage   bool
	}{
		{
			name:     "text chunk emits TokenDelta then folds into text",
			chunk:    &content.TextChunk{Text: "hello"},
			wantText: "hello",
		},
		{
			name:      "thinking chunk emits TokenDelta then folds into thinking",
			chunk:     &content.ThinkingChunk{Thinking: "reasoning"},
			wantThink: "reasoning",
		},
		{
			name:     "tool-use chunk emits TokenDelta then folds into tool uses",
			chunk:    &content.ToolUseChunk{Index: 0, ID: "id-1", Name: "Echo", InputJSON: `{}`},
			wantTool: true,
		},
		{
			name:        "refusal chunk emits TokenDelta then folds into the refusal",
			chunk:       &content.RefusalChunk{Text: "I can't help with that."},
			wantRefusal: "I can't help with that.",
		},
		{
			name:      "image chunk emits TokenDelta then folds into images",
			chunk:     &content.ImageChunk{Index: 0, MediaType: "image/png", Source: content.ImageSource{Data: []byte{0x89, 'P'}}},
			wantImage: true,
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var st blockState

			// emitted records whether emit ran, and emptyAtEmit captures whether the
			// blockState was still empty when emit fired (proving emit precedes fold).
			var emitted bool
			var emptyAtEmit bool
			const turnIndex event.TurnIndex = 7
			emit := func(ev event.Event) {
				td, ok := ev.(event.TokenDelta)
				if !ok {
					t.Errorf("emit got %T, want event.TokenDelta", ev)
					return
				}
				if td.Chunk != tt.chunk {
					t.Errorf("TokenDelta.Chunk = %#v, want %#v", td.Chunk, tt.chunk)
				}
				if td.TurnIndex != turnIndex {
					t.Errorf("TokenDelta.TurnIndex = %d, want %d", td.TurnIndex, turnIndex)
				}
				emitted = true
				msg := st.AIMessage()
				emptyAtEmit = len(msg.Blocks) == 0
			}

			p := newChunkProcessor(emit, chunkState{blocks: &st})
			p.process(tt.chunk, turnIndex)

			if !emitted {
				t.Fatal("process did not emit a TokenDelta")
			}
			if !emptyAtEmit {
				t.Error("blockState was already updated at emit time; want emit BEFORE accumulate")
			}

			// After process, the chunk must be folded into the blockState.
			msg := st.AIMessage()
			var gotText, gotThink, gotRefusal string
			var gotTool, gotImage bool
			for _, b := range msg.Blocks {
				switch v := b.(type) {
				case *content.TextBlock:
					gotText = v.Text
				case *content.ThinkingBlock:
					gotThink = v.Thinking
				case *content.ToolUseBlock:
					gotTool = true
				case *content.RefusalBlock:
					gotRefusal = v.Text
				case *content.ImageBlock:
					gotImage = true
				}
			}
			if gotText != tt.wantText {
				t.Errorf("folded text = %q, want %q", gotText, tt.wantText)
			}
			if gotThink != tt.wantThink {
				t.Errorf("folded thinking = %q, want %q", gotThink, tt.wantThink)
			}
			if gotTool != tt.wantTool {
				t.Errorf("folded tool-use present = %v, want %v", gotTool, tt.wantTool)
			}
			if gotRefusal != tt.wantRefusal {
				t.Errorf("folded refusal = %q, want %q", gotRefusal, tt.wantRefusal)
			}
			if gotImage != tt.wantImage {
				t.Errorf("folded image present = %v, want %v", gotImage, tt.wantImage)
			}
		})
	}
}

// TestChunkProcessorAccumulatesEverySealedChunkVariant is the anti-drift guard
// on the streaming fold. process dispatches on the concrete chunk type, and a
// Go type switch is not exhaustive: a variant added to core compiles fine here
// and is simply never folded. The failure is silent and asymmetric — the live
// TokenDelta still goes out, so a consumer watches content arrive that the
// materialized message, the committed history, and the next request all lack.
//
// The fixtures come from core/content/blocktest, which fails on its own if it does
// not cover every variant core declares, so a new chunk type reaches this test
// automatically instead of waiting for someone to remember it.
func TestChunkProcessorAccumulatesEverySealedChunkVariant(t *testing.T) {
	t.Parallel()

	for _, chunk := range blocktest.Chunks(t) {
		t.Run(fmt.Sprintf("%T", chunk), func(t *testing.T) {
			t.Parallel()
			var st blockState
			p := newChunkProcessor(func(event.Event) {}, chunkState{blocks: &st})

			p.process(chunk, 0)

			if blocks := st.AIMessage().Blocks; len(blocks) == 0 {
				t.Fatalf("process(%T) folded into no block; every chunk variant needs an arm in chunkProcessor.process", chunk)
			}
		})
	}
}

// TestChunkProcessorDoesNotRetainCallerBytes pins the ownership contract the
// loop relies on when it hands a provider's chunk to the fold. A codec is free
// to reuse its read buffer between deltas, so a fold that retained the caller's
// slice would let the NEXT delta rewrite bytes already accumulated.
//
// Image data is where this is unrecoverable: text mangled mid-stream is visibly
// wrong, but image bytes overwritten in place produce a file no decoder can
// recover and no validation in this package can detect.
func TestChunkProcessorDoesNotRetainCallerBytes(t *testing.T) {
	t.Parallel()

	imageData := []byte{0x89, 0x50, 0x4e}
	providerState := json.RawMessage(`{"signature":"abc"}`)

	var st blockState
	p := newChunkProcessor(func(event.Event) {}, chunkState{blocks: &st})
	p.process(&content.ImageChunk{Index: 0, MediaType: "image/png", Source: content.ImageSource{Data: imageData}}, 0)
	p.process(&content.ThinkingChunk{Index: 0, Thinking: "why", ProviderState: providerState, ProviderStateFormat: "anthropic"}, 0)

	// The provider reuses both buffers for its next read.
	for index := range imageData {
		imageData[index] = 0
	}
	for index := range providerState {
		providerState[index] = 'x'
	}

	for _, block := range st.AIMessage().Blocks {
		switch typed := block.(type) {
		case *content.ImageBlock:
			if !bytes.Equal(typed.Source.Data, []byte{0x89, 0x50, 0x4e}) {
				t.Errorf("accumulated image data = %v, want the bytes as delivered; the fold aliases the caller's slice", typed.Source.Data)
			}
		case *content.ThinkingBlock:
			if string(typed.ProviderState) != `{"signature":"abc"}` {
				t.Errorf("accumulated provider state = %s, want the bytes as delivered; the fold aliases the caller's slice", typed.ProviderState)
			}
		}
	}
}

// TestChunkProcessorFoldsWithNoOpEmit proves accumulation is independent of
// emission: a chunk is still folded into the blockState even when the emit
// callback does nothing with it.
func TestChunkProcessorFoldsWithNoOpEmit(t *testing.T) {
	t.Parallel()

	var st blockState
	noop := func(event.Event) {}
	p := newChunkProcessor(noop, chunkState{blocks: &st})

	p.process(&content.TextChunk{Text: "a"}, 0)
	p.process(&content.TextChunk{Text: "b"}, 0)
	p.process(&content.ToolUseChunk{Index: 0, ID: "id", Name: "Echo", InputJSON: `{}`}, 0)

	msg := st.AIMessage()
	if len(msg.Blocks) != 2 {
		t.Fatalf("AIMessage blocks = %d, want 2 (text + tool_use)", len(msg.Blocks))
	}
	tb, ok := msg.Blocks[0].(*content.TextBlock)
	if !ok {
		t.Fatalf("block[0] = %T, want *content.TextBlock", msg.Blocks[0])
	}
	if tb.Text != "ab" {
		t.Errorf("folded text = %q, want %q (folds even with no-op emit)", tb.Text, "ab")
	}
	if len(st.ToolUses()) != 1 {
		t.Errorf("ToolUses() len = %d, want 1 (folds even with no-op emit)", len(st.ToolUses()))
	}
}
