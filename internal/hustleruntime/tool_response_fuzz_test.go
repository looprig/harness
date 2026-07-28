package hustleruntime

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/looprig/core/content"
	"github.com/looprig/inference"
	"github.com/looprig/inference/stream"
)

func FuzzClassifyToolResponse(f *testing.F) {
	f.Add(uint8(0), uint8(0), `{"allowed":true}`, "call-1", "workspace.status", int32(64))
	f.Add(uint8(1), uint8(2), `{"path":"README.md"}`, "call-1", "workspace.read", int32(256))
	f.Add(uint8(2), uint8(1), `not-json`, "", "unknown-secret", int32(1))
	f.Add(uint8(3), uint8(3), `{"nested":{"value":1}}`, "duplicate", "workspace.status", int32(-1))

	f.Fuzz(func(t *testing.T, shape, finish uint8, raw, id, name string, limit int32) {
		response := fuzzToolResponse(shape, finish, raw, id, name)
		got, err := classifyToolResponse(
			response,
			map[string]struct{}{"workspace.status": {}, "workspace.read": {}},
			int(limit),
		)
		if err != nil {
			if got != nil {
				t.Fatalf("error returned with non-nil variant %T", got)
			}
			var responseErr *ToolResponseError
			if !errors.As(err, &responseErr) || !responseErr.Valid() {
				t.Fatalf("error = %#v, want valid bounded ToolResponseError", err)
			}
			return
		}

		switch typed := got.(type) {
		case terminalToolResponse:
			if len(typed.output) == 0 || len(typed.output) > int(limit) || !json.Valid(typed.output) {
				t.Fatalf("invalid terminal output: length=%d limit=%d", len(typed.output), limit)
			}
			typed.output[0] ^= 0xff
		case evidenceToolResponse:
			if len(typed.calls) == 0 {
				t.Fatal("empty evidence-call variant")
			}
			for _, call := range typed.calls {
				if call.id == "" || (call.name != "workspace.status" && call.name != "workspace.read") ||
					len(call.input) == 0 || !json.Valid(call.input) {
					t.Fatalf("invalid evidence call metadata")
				}
			}
			typed.calls[0].input[0] ^= 0xff
		default:
			t.Fatalf("successful response variant = %T", got)
		}
	})
}

func fuzzToolResponse(shape, finish uint8, raw, id, name string) *inference.Response {
	reason := []stream.FinishReason{
		stream.FinishReasonUnknown,
		stream.FinishReasonStop,
		stream.FinishReasonToolUse,
		stream.FinishReasonLength,
		stream.FinishReasonContentFilter,
		stream.FinishReason("future"),
	}[int(finish)%6]

	input := json.RawMessage(raw)
	var blocks []content.Block
	switch shape % 10 {
	case 0:
		blocks = []content.Block{&content.TextBlock{Text: raw}}
	case 1:
		blocks = []content.Block{&content.ToolUseBlock{ID: id, Name: name, Input: input}}
	case 2:
		blocks = []content.Block{&content.ToolUseBlock{ID: id, Name: inference.StructuredOutputToolName, Input: input}}
	case 3:
		blocks = []content.Block{
			&content.ToolUseBlock{ID: id, Name: name, Input: input},
			&content.ToolUseBlock{ID: id, Name: "workspace.read", Input: input},
		}
	case 4:
		blocks = []content.Block{&content.TextBlock{Text: raw}, &content.ToolUseBlock{ID: id, Name: name, Input: input}}
	case 5:
		var block *content.ToolUseBlock
		blocks = []content.Block{block}
	case 6:
		blocks = []content.Block{nil}
	case 7:
		blocks = []content.Block{&content.ImageBlock{}}
	case 8:
		blocks = []content.Block{&content.ThinkingBlock{Thinking: raw}}
	default:
		return nil
	}
	return toolResponse(reason, blocks...)
}
