package sessionruntime

import (
	"encoding/json"
	"fmt"
	"reflect"
	"testing"

	"github.com/looprig/core/content"
	"github.com/looprig/core/content/blocktest"
	"github.com/looprig/harness/pkg/loop"
)

// TestCompactionBlockWireRoundTripPreservesEveryExportedField is the compaction
// codec's anti-drift guard. The wire structs enumerate block fields by hand and
// the decoder runs with DisallowUnknownFields, so a field missing from the wire
// types is not merely dropped — once the producer starts emitting it, the
// payload becomes a HARD decode error. Reflection-populated fixtures make a new
// core field fail here at the source rather than at a live compaction.
func TestCompactionBlockWireRoundTripPreservesEveryExportedField(t *testing.T) {
	t.Parallel()

	for _, want := range blocktest.Blocks(t) {
		t.Run(fmt.Sprintf("%T", want), func(t *testing.T) {
			t.Parallel()
			raw, err := encodeCompactionBlock(want, 0)
			if err != nil {
				t.Fatalf("encodeCompactionBlock() error = %v", err)
			}
			got, err := decodeCompactionBlock(raw, 0)
			if err != nil {
				t.Fatalf("decodeCompactionBlock(%s) error = %v", raw, err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("round trip through %s = %#v, want %#v", raw, got, want)
			}
		})
	}
}

// TestCloneCompactionInputPreservesEveryExportedField covers the adapter's own
// defensive copy, which sits between the session's live transcript and the
// compaction hustle. Its per-variant switch dropped ToolUseBlock.ProviderState
// while preserving ThinkingBlock.ProviderState — an asymmetry no field-named
// test would notice — and it now delegates to content.CloneBlocks, where the
// arms are maintained beside the union. What remains to check here is that the
// adapter really routes through that copy and hands back memory of its own.
func TestCloneCompactionInputPreservesEveryExportedField(t *testing.T) {
	t.Parallel()

	blocks := blocktest.Blocks(t)
	input := loop.CompactionInput{
		Transcript: content.AgenticMessages{
			&content.AIMessage{Message: content.Message{Role: content.RoleAssistant, Blocks: blocks}},
		},
	}

	cloned := cloneCompactionInput(input)

	if !reflect.DeepEqual(cloned, input) {
		t.Fatalf("cloneCompactionInput() dropped or altered a field:\n got %#v\nwant %#v", cloned, input)
	}
	got := cloned.Transcript[0].(*content.AIMessage)
	for index, want := range blocks {
		t.Run(fmt.Sprintf("%T", want), func(t *testing.T) {
			blocktest.AssertIndependent(t, want, got.Blocks[index])
		})
	}
}

// TestDecodeCompactionBlockAcceptsPayloadsWithoutProviderState pins the
// migration contract for transcripts persisted before provider state was on the
// wire: the new fields are optional on decode, so an older payload that simply
// omits them decodes to a block with no provider state rather than failing the
// strict decoder. Encoding stays symmetric — a block with no provider state
// emits no provider-state keys, so payload bytes are unchanged for the
// overwhelmingly common case.
func TestDecodeCompactionBlockAcceptsPayloadsWithoutProviderState(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		raw  string
		want content.Block
	}{
		{
			name: "legacy thinking block",
			raw:  `{"type":"thinking","thinking":"reasoning","signature":"sig"}`,
			want: &content.ThinkingBlock{Thinking: "reasoning", Signature: "sig"},
		},
		{
			name: "legacy tool use block",
			raw:  `{"type":"tool_use","id":"call","name":"tool","input":{"value":1}}`,
			want: &content.ToolUseBlock{ID: "call", Name: "tool", Input: json.RawMessage(`{"value":1}`)},
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := decodeCompactionBlock(json.RawMessage(tt.raw), 0)
			if err != nil {
				t.Fatalf("decodeCompactionBlock() error = %v", err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("decodeCompactionBlock() = %#v, want %#v", got, tt.want)
			}
		})
	}
}

// TestRefusalSurvivesTheCompactionWireAsItsOwnTag pins the half of the refusal
// contract that a field-by-field round trip cannot see. A RefusalBlock and a
// TextBlock carry identical payloads, so a round trip through an encoder that
// tagged refusals as text would still compare equal on Text while quietly
// restoring the block as ordinary assistant prose — the model's decision to
// decline erased. The tag on the wire, and the type on the far side, are the
// assertions.
func TestRefusalSurvivesTheCompactionWireAsItsOwnTag(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		block *content.RefusalBlock
		want  string
	}{
		{
			name:  "explained refusal",
			block: &content.RefusalBlock{Text: "I can't help with that."},
			want:  `{"type":"refusal","text":"I can't help with that."}`,
		},
		{
			// A provider may refuse with no explanation at all; the block's
			// presence is the signal, so an empty Text must still round trip.
			name:  "unexplained refusal",
			block: &content.RefusalBlock{},
			want:  `{"type":"refusal","text":""}`,
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			raw, err := encodeCompactionBlock(tt.block, 0)
			if err != nil {
				t.Fatalf("encodeCompactionBlock() error = %v", err)
			}
			if string(raw) != tt.want {
				t.Fatalf("encodeCompactionBlock() = %s, want %s", raw, tt.want)
			}
			got, err := decodeCompactionBlock(raw, 0)
			if err != nil {
				t.Fatalf("decodeCompactionBlock(%s) error = %v", raw, err)
			}
			if _, ok := got.(*content.RefusalBlock); !ok {
				t.Fatalf("decodeCompactionBlock(%s) = %T, want *content.RefusalBlock", raw, got)
			}
			if !reflect.DeepEqual(got, tt.block) {
				t.Fatalf("decodeCompactionBlock(%s) = %#v, want %#v", raw, got, tt.block)
			}
		})
	}
}

// TestDecodeCompactionRefusalBlockRejectsTruncatedPayloads proves the strict
// decoder does not manufacture an unexplained refusal out of a payload that
// merely lost its text, which would be indistinguishable from a real one.
func TestDecodeCompactionRefusalBlockRejectsTruncatedPayloads(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		raw  string
	}{
		{name: "missing text", raw: `{"type":"refusal"}`},
		{name: "unknown field", raw: `{"type":"refusal","text":"no","reason":"policy"}`},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if _, err := decodeCompactionBlock(json.RawMessage(tt.raw), 0); err == nil {
				t.Fatalf("decodeCompactionBlock(%s) error = nil, want a wire error", tt.raw)
			}
		})
	}
}

// TestEncodeCompactionBlockOmitsAbsentProviderState proves the payload bytes are
// unchanged for blocks without provider state, so adding the fields does not
// churn every persisted compaction transcript.
func TestEncodeCompactionBlockOmitsAbsentProviderState(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		block content.Block
		want  string
	}{
		{
			name:  "thinking",
			block: &content.ThinkingBlock{Thinking: "reasoning", Signature: "sig"},
			want:  `{"type":"thinking","thinking":"reasoning","signature":"sig"}`,
		},
		{
			name:  "tool use",
			block: &content.ToolUseBlock{ID: "call", Name: "tool", Input: json.RawMessage(`{"value":1}`)},
			want:  `{"type":"tool_use","id":"call","name":"tool","input":{"value":1}}`,
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			raw, err := encodeCompactionBlock(tt.block, 0)
			if err != nil {
				t.Fatalf("encodeCompactionBlock() error = %v", err)
			}
			if string(raw) != tt.want {
				t.Fatalf("encodeCompactionBlock() = %s, want %s", raw, tt.want)
			}
		})
	}
}
