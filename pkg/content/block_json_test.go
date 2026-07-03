package content_test

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/looprig/harness/pkg/content"
)

// TestBlockCodecRoundTrip verifies that every concrete Block variant survives a
// MarshalBlock -> UnmarshalBlock round trip with deep equality. Empty
// ToolResult.Content uses nil (not []Block{}) because JSON normalizes empty
// slices to nil on the way back — an empty-non-nil slice would not round-trip
// equal, which is correct, expected behavior.
func TestBlockCodecRoundTrip(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   content.Block
	}{
		{
			name: "text block",
			in:   &content.TextBlock{Text: "hello world"},
		},
		{
			name: "thinking block with signature",
			in:   &content.ThinkingBlock{Thinking: "I think...", Signature: "sig_123"},
		},
		{
			name: "image block with URL source",
			in:   &content.ImageBlock{MediaType: content.MediaTypeImagePNG, Source: content.ImageSource{URL: "https://example.com/a.png"}},
		},
		{
			name: "image block with inline data source",
			in:   &content.ImageBlock{MediaType: content.MediaTypeImageJPEG, Source: content.ImageSource{Data: []byte{0xFF, 0xD8, 0xFF}}},
		},
		{
			name: "audio block",
			in:   &content.AudioBlock{MediaType: content.MediaTypeAudioMPEG, Data: []byte{0x49, 0x44, 0x33}},
		},
		{
			name: "document block with text",
			in:   &content.DocumentBlock{MediaType: content.MediaTypeDocumentMarkdown, Name: "readme.md", Text: "# Title"},
		},
		{
			name: "document block with binary data",
			in:   &content.DocumentBlock{MediaType: content.MediaTypeDocumentPDF, Name: "report.pdf", Data: []byte{0x25, 0x50, 0x44, 0x46}},
		},
		{
			name: "tool_use block",
			in:   &content.ToolUseBlock{ID: "tu_1", Name: "search", Input: json.RawMessage(`{"q":"go"}`)},
		},
		{
			name: "empty tool_result block with nil content",
			in:   &content.ToolResultBlock{ToolUseID: "tu_1", Content: nil, IsError: true},
		},
		{
			name: "nested tool_result block with content",
			in: &content.ToolResultBlock{
				ToolUseID: "tu_2",
				Content: []content.Block{
					&content.TextBlock{Text: "part1"},
					&content.ImageBlock{MediaType: content.MediaTypeImagePNG, Source: content.ImageSource{URL: "https://example.com/x.png"}},
				},
				IsError: false,
			},
		},
		{
			// Locks the recursion invariant: a tool_result whose content holds
			// another tool_result holding a text block must survive the round
			// trip two levels deep.
			name: "doubly nested tool_result block",
			in: &content.ToolResultBlock{
				ToolUseID: "tu_outer",
				Content: []content.Block{
					&content.ToolResultBlock{
						ToolUseID: "tu_inner",
						Content: []content.Block{
							&content.TextBlock{Text: "deep"},
						},
					},
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			data, err := content.MarshalBlock(tt.in)
			if err != nil {
				t.Fatalf("MarshalBlock() error = %v", err)
			}
			got, err := content.UnmarshalBlock(data)
			if err != nil {
				t.Fatalf("UnmarshalBlock() error = %v", err)
			}
			if !reflect.DeepEqual(got, tt.in) {
				t.Errorf("round trip = %#v, want %#v", got, tt.in)
			}
		})
	}
}

// TestUnmarshalBlockUnknownTag verifies an unknown wire tag fails secure with a
// typed *UnknownBlockTypeError carrying the offending tag.
func TestUnmarshalBlockUnknownTag(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		data     string
		wantType content.BlockType
	}{
		{name: "unknown video tag", data: `{"type":"video","url":"x"}`, wantType: "video"},
		{name: "empty tag", data: `{"type":"","Text":"x"}`, wantType: ""},
		{name: "missing tag", data: `{"Text":"x"}`, wantType: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := content.UnmarshalBlock([]byte(tt.data))
			var ube *content.UnknownBlockTypeError
			if !errors.As(err, &ube) {
				t.Fatalf("UnmarshalBlock() error = %v, want *UnknownBlockTypeError", err)
			}
			if ube.Type != tt.wantType {
				t.Errorf("UnknownBlockTypeError.Type = %q, want %q", ube.Type, tt.wantType)
			}
		})
	}
}

// TestUnmarshalBlockMalformed verifies invalid JSON yields a typed *BlockDecodeError.
func TestUnmarshalBlockMalformed(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		data string
	}{
		{name: "not json", data: `{not json`},
		{name: "truncated", data: `{"type":`},
		{name: "array not object", data: `[1,2,3]`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := content.UnmarshalBlock([]byte(tt.data))
			var bde *content.BlockDecodeError
			if !errors.As(err, &bde) {
				t.Fatalf("UnmarshalBlock() error = %v, want *BlockDecodeError", err)
			}
		})
	}
}

// TestUnmarshalBlocksElementCap verifies a JSON array exceeding the per-slice
// element cap fails secure with a typed *BlockLimitError.
func TestUnmarshalBlocksElementCap(t *testing.T) {
	t.Parallel()

	// Build an oversized array of valid text blocks: maxBlocksPerSlice + 1.
	const over = 10_000 + 1
	raws := make([]json.RawMessage, over)
	for i := range raws {
		raws[i] = json.RawMessage(`{"type":"text","Text":"x"}`)
	}
	data, err := json.Marshal(raws)
	if err != nil {
		t.Fatalf("setup marshal error = %v", err)
	}

	_, err = content.UnmarshalBlocks(data)
	var ble *content.BlockLimitError
	if !errors.As(err, &ble) {
		t.Fatalf("UnmarshalBlocks() error = %v, want *BlockLimitError", err)
	}
	if ble.Limit != "slice_count" {
		t.Errorf("BlockLimitError.Limit = %q, want %q", ble.Limit, "slice_count")
	}
	if ble.Got != over {
		t.Errorf("BlockLimitError.Got = %d, want %d", ble.Got, over)
	}
}

// TestUnmarshalBlockByteCap verifies a single serialized block exceeding the
// per-block byte cap fails secure with a typed *BlockLimitError before any
// decode work. The cap (maxBlockBytes) is unexported, so the oversized input is
// constructed directly: 8<<20 is 8 MiB and +64 clears the JSON wrapper.
func TestUnmarshalBlockByteCap(t *testing.T) {
	t.Parallel()

	oversized := []byte(`{"type":"text","text":"` + strings.Repeat("a", 8<<20+64) + `"}`)

	_, err := content.UnmarshalBlock(oversized)
	var ble *content.BlockLimitError
	if !errors.As(err, &ble) {
		t.Fatalf("UnmarshalBlock() error = %v, want *BlockLimitError", err)
	}
	if ble.Limit != "block_bytes" {
		t.Errorf("BlockLimitError.Limit = %q, want %q", ble.Limit, "block_bytes")
	}
	if ble.Got != len(oversized) {
		t.Errorf("BlockLimitError.Got = %d, want %d", ble.Got, len(oversized))
	}
}

// TestMarshalBlockNil pins the fail-secure contract for nil block payloads and
// asserts neither path panics:
//   - a nil Block interface has no concrete type, so blockTag fails closed with
//     *UnknownBlockTypeError;
//   - a typed-nil pointer (*TextBlock)(nil) matches the text arm in blockTag but
//     marshals to JSON null, which MarshalBlock rejects with *NilBlockError
//     rather than emit an empty {"type":"text"} block.
func TestMarshalBlockNil(t *testing.T) {
	t.Parallel()

	t.Run("nil interface yields UnknownBlockTypeError", func(t *testing.T) {
		t.Parallel()
		_, err := content.MarshalBlock(nil)
		var ube *content.UnknownBlockTypeError
		if !errors.As(err, &ube) {
			t.Fatalf("MarshalBlock(nil) error = %v, want *UnknownBlockTypeError", err)
		}
	})

	t.Run("typed-nil yields NilBlockError", func(t *testing.T) {
		t.Parallel()
		var b content.Block = (*content.TextBlock)(nil)
		_, err := content.MarshalBlock(b)
		var nbe *content.NilBlockError
		if !errors.As(err, &nbe) {
			t.Fatalf("MarshalBlock((*TextBlock)(nil)) error = %v, want *NilBlockError", err)
		}
		if nbe.Type != content.TypeText {
			t.Errorf("NilBlockError.Type = %q, want %q", nbe.Type, content.TypeText)
		}
	})
}

// TestPayloadsHaveNoTypeKey guards MarshalBlock's flat-merge invariant: no concrete
// Block payload may serialize a top-level "type" key, or the injected codec tag
// (block_json.go) would silently collide with a real field.
func TestPayloadsHaveNoTypeKey(t *testing.T) {
	t.Parallel()

	payloads := []content.Block{
		&content.TextBlock{Text: "x"},
		&content.ImageBlock{MediaType: content.MediaTypeImagePNG, Source: content.ImageSource{URL: "u"}},
		&content.AudioBlock{MediaType: content.MediaTypeAudioMPEG, Data: []byte{1}},
		&content.DocumentBlock{MediaType: content.MediaTypeDocumentPDF, Name: "n", Data: []byte{1}},
		&content.ThinkingBlock{Thinking: "t", Signature: "s"},
		&content.ToolUseBlock{ID: "i", Name: "n", Input: json.RawMessage(`{}`)},
		&content.ToolResultBlock{ToolUseID: "i", Content: []content.Block{&content.TextBlock{Text: "x"}}},
	}

	for _, p := range payloads {
		p := p
		t.Run(reflect.TypeOf(p).String(), func(t *testing.T) {
			t.Parallel()
			raw, err := json.Marshal(p)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			var fields map[string]json.RawMessage
			if err := json.Unmarshal(raw, &fields); err != nil {
				t.Fatalf("unmarshal to map: %v", err)
			}
			if _, ok := fields["type"]; ok {
				t.Errorf("%s serializes a top-level \"type\" key, colliding with the codec tag", reflect.TypeOf(p))
			}
		})
	}
}
