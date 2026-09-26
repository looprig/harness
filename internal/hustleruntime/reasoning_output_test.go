package hustleruntime

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/looprig/core/content"
	"github.com/looprig/harness/pkg/hustle"
	"github.com/looprig/inference"
	"github.com/looprig/inference/stream"
)

func TestPlainResultReasoningShapes(t *testing.T) {
	text := &content.TextBlock{Text: " {} "}
	thinking := &content.ThinkingBlock{Thinking: "private reasoning"}
	var nilText *content.TextBlock
	var nilThinking *content.ThinkingBlock
	for _, tc := range []struct {
		name   string
		blocks []content.Block
		limit  int
		want   OutputFailureReason
	}{
		{"text", []content.Block{text}, 4, ""},
		{"reasoning-before", []content.Block{thinking, text}, 4, ""},
		{"reasoning-after", []content.Block{text, thinking}, 4, ""},
		{"multiple-reasoning", []content.Block{thinking, text, &content.ThinkingBlock{}}, 4, ""},
		{"no-blocks", nil, 4, OutputFailureInvalidShape},
		{"nil", []content.Block{nil, text}, 4, OutputFailureInvalidShape},
		{"nil-text", []content.Block{thinking, nilText}, 4, OutputFailureInvalidShape},
		{"nil-thinking", []content.Block{nilThinking, text}, 4, OutputFailureInvalidShape},
		{"thinking-only", []content.Block{thinking}, 4, OutputFailureInvalidShape},
		{"multiple-text", []content.Block{thinking, text, text}, 8, OutputFailureInvalidShape},
		{"tool", []content.Block{&content.ToolUseBlock{}, text}, 4, OutputFailureInvalidShape},
		{"image", []content.Block{&content.ImageBlock{}, text}, 4, OutputFailureInvalidShape},
		{"refusal", []content.Block{&content.RefusalBlock{}, text}, 4, OutputFailureInvalidShape},
		{"empty", []content.Block{thinking, &content.TextBlock{}}, 4, OutputFailureEmptyText},
		{"invalid-json", []content.Block{thinking, &content.TextBlock{Text: "no"}}, 4, OutputFailureInvalidJSON},
		{"multiple-json", []content.Block{thinking, &content.TextBlock{Text: "{} {}"}}, 8, OutputFailureInvalidJSON},
		{"oversize", []content.Block{thinking, text}, 3, OutputFailureTooLarge},
	} {
		t.Run(tc.name, func(t *testing.T) {
			usage := &content.Usage{OutputTokens: 42, ReasoningTokens: 17}
			response := &inference.Response{Message: &content.AIMessage{Message: content.Message{Role: content.RoleAssistant, Blocks: tc.blocks}}}
			result, err := extractResult(response, usage, tc.limit)
			if tc.want != "" {
				var outputErr *OutputError
				if !errors.As(err, &outputErr) || outputErr.Reason != tc.want || !outputErr.Valid() || result.Output != nil || result.Usage != nil {
					t.Fatalf("result=%+v error=%v, want %s", result, err, tc.want)
				}
				return
			}
			if err != nil || string(result.Output) != text.Text || result.Usage != usage {
				t.Fatalf("result=%+v error=%v, want exact text and usage", result, err)
			}
		})
	}
}

func TestPlainResultFinishReasons(t *testing.T) {
	for _, tc := range []struct {
		name   string
		finish stream.FinishReason
		tool   bool
		want   stream.FinishReason
	}{
		{"unknown", stream.FinishReasonUnknown, false, ""},
		{"stop", stream.FinishReasonStop, false, ""},
		{"length", stream.FinishReasonLength, false, stream.FinishReasonLength},
		{"filter", stream.FinishReasonContentFilter, false, stream.FinishReasonContentFilter},
		{"tool-finish", stream.FinishReasonToolUse, false, stream.FinishReasonToolUse},
		{"stop-with-tool", stream.FinishReasonStop, true, stream.FinishReasonStop},
		{"future", stream.FinishReason("future"), false, inference.StructuredOutputFinishReasonOther},
	} {
		t.Run(tc.name, func(t *testing.T) {
			blocks := []content.Block{&content.TextBlock{Text: `{}`}}
			if tc.tool {
				blocks = append(blocks, &content.ToolUseBlock{})
			}
			response := &inference.Response{Message: &content.AIMessage{Message: content.Message{Role: content.RoleAssistant, Blocks: blocks}}, FinishReason: tc.finish}
			result, err := extractResult(response, nil, 2)
			if tc.want == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			var outputErr *OutputError
			var finishErr *inference.StructuredOutputFinishError
			if !errors.As(err, &outputErr) || !outputErr.Valid() || !errors.As(err, &finishErr) || finishErr.Reason != tc.want || result.Output != nil {
				t.Fatalf("result=%+v error=%v, want typed finish %s", result, err, tc.want)
			}
		})
	}
}

func TestRunAndFinalizePlainReasoningResult(t *testing.T) {
	usage := &content.Usage{InputTokens: 3, OutputTokens: 42, ReasoningTokens: 17}
	const output = ` { "summary" : "ok" } `
	client := &runtimeTestClient{invoke: func(_ context.Context, request inference.Request) (*inference.Response, error) {
		if request.Output != nil {
			t.Fatal("plain hustle requested native structured output")
		}
		return &inference.Response{Message: &content.AIMessage{Message: content.Message{Role: content.RoleAssistant, Blocks: []content.Block{
			&content.ThinkingBlock{Thinking: "private reasoning"}, &content.TextBlock{Text: output},
		}}}, Usage: usage, FinishReason: stream.FinishReasonStop}, nil
	}}
	definition := runtimeTestBoundDefinition(t, "test.plain-reasoning", hustle.ParticipationBlocking, client, hustle.ModelSourceNamed, nil)
	controller := runtimeTestController(t, definition, &runtimeTestAudit{}, &runtimeTestFaults{}, &runtimeTestActivity{})
	validated := false
	err := controller.RunAndFinalize(context.Background(), runtimeRequest(t, definition.Name()), func(_ context.Context, result hustle.Result) error {
		validated = true
		if string(result.Output) != output || result.Usage == usage || !reflect.DeepEqual(result.Usage, usage) {
			t.Fatalf("result=%+v, want unchanged JSON and cloned complete usage", result)
		}
		return nil
	}, noOpFinalizer)
	if err != nil || !validated || client.invocations.Load() != 1 {
		t.Fatalf("err=%v validated=%v calls=%d", err, validated, client.invocations.Load())
	}
}
