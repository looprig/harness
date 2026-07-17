package hustleruntime

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/looprig/core/content"
	"github.com/looprig/inference"
	"github.com/looprig/inference/stream"
)

func TestExtractResultReportsBoundedReason(t *testing.T) {
	t.Parallel()
	valid := func(text string) *inference.Response {
		return &inference.Response{Message: &content.AIMessage{Message: content.Message{
			Role: content.RoleAssistant, Blocks: []content.Block{&content.TextBlock{Text: text}},
		}}}
	}
	tests := []struct {
		name       string
		response   *inference.Response
		limit      int
		wantReason OutputFailureReason
		wantOK     bool
	}{
		{name: "nil response", limit: 64, wantReason: OutputFailureInvalidShape},
		{name: "nil message", response: &inference.Response{}, limit: 64, wantReason: OutputFailureInvalidShape},
		{name: "wrong role", response: &inference.Response{Message: &content.AIMessage{Message: content.Message{Role: content.RoleUser, Blocks: []content.Block{&content.TextBlock{Text: `{}`}}}}}, limit: 64, wantReason: OutputFailureInvalidShape},
		{name: "zero blocks", response: &inference.Response{Message: &content.AIMessage{Message: content.Message{Role: content.RoleAssistant}}}, limit: 64, wantReason: OutputFailureInvalidShape},
		{name: "multiple blocks", response: &inference.Response{Message: &content.AIMessage{Message: content.Message{Role: content.RoleAssistant, Blocks: []content.Block{&content.TextBlock{Text: `{}`}, &content.TextBlock{Text: `{}`}}}}}, limit: 64, wantReason: OutputFailureInvalidShape},
		{name: "non text", response: &inference.Response{Message: &content.AIMessage{Message: content.Message{Role: content.RoleAssistant, Blocks: []content.Block{&content.ThinkingBlock{Thinking: `{}`}}}}}, limit: 64, wantReason: OutputFailureInvalidShape},
		{name: "typed nil text", response: &inference.Response{Message: &content.AIMessage{Message: content.Message{Role: content.RoleAssistant, Blocks: []content.Block{(*content.TextBlock)(nil)}}}}, limit: 64, wantReason: OutputFailureInvalidShape},
		{name: "empty text before JSON", response: valid(""), limit: 64, wantReason: OutputFailureEmptyText},
		{name: "too large before JSON", response: valid("not-json"), limit: 3, wantReason: OutputFailureTooLarge},
		{name: "invalid JSON", response: valid("not-json"), limit: 64, wantReason: OutputFailureInvalidJSON},
		{name: "valid", response: valid(`{"ok":true}`), limit: 64, wantOK: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := extractResult(tt.response, nil, tt.limit)
			if tt.wantOK {
				if err != nil || string(result.Output) != `{"ok":true}` {
					t.Fatalf("extractResult() = %s, %v", result.Output, err)
				}
				return
			}
			var outputErr *OutputError
			if !errors.As(err, &outputErr) || outputErr.Reason != tt.wantReason {
				t.Fatalf("extractResult() error = %T %v, want reason %q", err, err, tt.wantReason)
			}
			if strings.Contains(err.Error(), "not-json") {
				t.Fatalf("error leaked raw output: %v", err)
			}
		})
	}
}

func TestOutputFailureReasonValid(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		value OutputFailureReason
		want  bool
	}{
		{name: "unknown"},
		{name: "invalid shape", value: OutputFailureInvalidShape, want: true},
		{name: "empty text", value: OutputFailureEmptyText, want: true},
		{name: "too large", value: OutputFailureTooLarge, want: true},
		{name: "invalid json", value: OutputFailureInvalidJSON, want: true},
		{name: "future", value: "future"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.value.Valid(); got != tt.want {
				t.Fatalf("Valid() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestOutputErrorValid(t *testing.T) {
	t.Parallel()
	cause := errors.New("domain validation failed")
	tests := []struct {
		name  string
		value OutputError
		want  bool
	}{
		{name: "reason only", value: OutputError{Reason: OutputFailureInvalidShape}, want: true},
		{name: "cause only", value: OutputError{Cause: cause}, want: true},
		{name: "neither"},
		{name: "both", value: OutputError{Reason: OutputFailureInvalidJSON, Cause: cause}},
		{name: "unknown reason", value: OutputError{Reason: "future"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.value.Valid(); got != tt.want {
				t.Fatalf("Valid() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestExtractStructuredResultUsesNativeTextOnly(t *testing.T) {
	t.Parallel()
	text := func(value string) content.Block { return &content.TextBlock{Text: value} }
	thinking := func() content.Block { return &content.ThinkingBlock{Thinking: "private"} }
	terminal := func() content.Block {
		return &content.ToolUseBlock{Name: inference.StructuredOutputToolName, Input: json.RawMessage(`{"summary":"secret-value"}`)}
	}
	ordinary := func() content.Block { return &content.ToolUseBlock{Name: "ordinary", Input: json.RawMessage(`{}`)} }
	response := func(finish stream.FinishReason, blocks ...content.Block) *inference.Response {
		return &inference.Response{Message: &content.AIMessage{Message: content.Message{Role: content.RoleAssistant, Blocks: blocks}}, FinishReason: finish}
	}
	tests := []struct {
		name       string
		response   *inference.Response
		limit      int
		want       string
		wantReason OutputFailureReason
		wantCause  string
	}{
		{name: "text fragments and thinking", response: response(stream.FinishReasonStop, thinking(), text(` {"summary":`), text(` "ok"} `)), limit: 64, want: `{"summary":"ok"}`},
		{name: "terminal fallback forbidden on tool use", response: response(stream.FinishReasonToolUse, thinking(), terminal()), limit: 64, wantCause: "finish"},
		{name: "terminal fallback forbidden on unknown", response: response(stream.FinishReasonUnknown, terminal()), limit: 64, wantReason: OutputFailureInvalidShape},
		{name: "ordinary tool forbidden", response: response(stream.FinishReasonUnknown, ordinary()), limit: 64, wantCause: "malformed"},
		{name: "mixed text and terminal forbidden", response: response(stream.FinishReasonUnknown, text(`{"summary":"ok"}`), terminal()), limit: 64, wantCause: "malformed"},
		{name: "thinking only forbidden", response: response(stream.FinishReasonUnknown, thinking()), limit: 64, wantCause: "malformed"},
		{name: "harness byte bound retained", response: response(stream.FinishReasonStop, text(`{"summary":"ok"}`)), limit: len(`{"summary":"ok"}`) - 1, wantReason: OutputFailureTooLarge},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := extractStructuredResult(tt.response, nil, tt.limit)
			if tt.want != "" {
				if err != nil || string(result.Output) != tt.want {
					t.Fatalf("extractStructuredResult() = %s,%v, want %s,nil", result.Output, err, tt.want)
				}
				return
			}
			var outputErr *OutputError
			if !errors.As(err, &outputErr) || !outputErr.Valid() {
				t.Fatalf("error = %T %v, want valid OutputError", err, err)
			}
			if tt.wantReason != "" && outputErr.Reason != tt.wantReason {
				t.Fatalf("reason = %q, want %q", outputErr.Reason, tt.wantReason)
			}
			switch tt.wantCause {
			case "finish":
				var target *inference.StructuredOutputFinishError
				if !errors.As(err, &target) {
					t.Fatalf("error = %T %v, want StructuredOutputFinishError", err, err)
				}
			case "malformed":
				var target *inference.MalformedStructuredOutputError
				if !errors.As(err, &target) {
					t.Fatalf("error = %T %v, want MalformedStructuredOutputError", err, err)
				}
			}
			if strings.Contains(err.Error(), "secret-value") {
				t.Fatalf("error leaked raw output: %v", err)
			}
		})
	}
}

func TestExtractStructuredResultEnforcesRawSemanticOutputLimit(t *testing.T) {
	t.Parallel()
	text := func(value string) content.Block { return &content.TextBlock{Text: value} }
	thinking := func(value string) content.Block { return &content.ThinkingBlock{Thinking: value} }
	terminal := func() content.Block {
		return &content.ToolUseBlock{Name: inference.StructuredOutputToolName, Input: json.RawMessage(`{"summary":"ok"}`)}
	}
	response := func(finish stream.FinishReason, blocks ...content.Block) *inference.Response {
		return &inference.Response{Message: &content.AIMessage{Message: content.Message{Role: content.RoleAssistant, Blocks: blocks}}, FinishReason: finish}
	}
	compact := `{"summary":"ok"}`
	padded := "   " + compact + "   "
	first := `   {"summary":`
	second := `"ok"}   `
	tests := []struct {
		name          string
		response      *inference.Response
		limit         int
		want          string
		wantReason    OutputFailureReason
		wantFinish    stream.FinishReason
		wantMalformed bool
	}{
		{name: "padding exceeds raw limit", response: response(stream.FinishReasonStop, text(padded)), limit: len(compact), wantReason: OutputFailureTooLarge},
		{name: "fragment sum exceeds raw limit", response: response(stream.FinishReasonUnknown, text(first), text(second)), limit: len(first) + len(second) - 1, wantReason: OutputFailureTooLarge},
		{name: "exact raw boundary", response: response(stream.FinishReasonStop, text(padded)), limit: len(padded), want: compact},
		{name: "thinking excluded from budget", response: response(stream.FinishReasonStop, thinking(strings.Repeat("private", 128)), text(compact)), limit: len(compact), want: compact},
		{name: "length precedes oversized", response: response(stream.FinishReasonLength, text(padded)), limit: len(compact), wantFinish: stream.FinishReasonLength},
		{name: "content filter precedes oversized", response: response(stream.FinishReasonContentFilter, text(padded)), limit: len(compact), wantFinish: stream.FinishReasonContentFilter},
		{name: "tool use precedes oversized", response: response(stream.FinishReasonToolUse, text(padded)), limit: len(compact), wantFinish: stream.FinishReasonToolUse},
		{name: "future reason precedes oversized", response: response(stream.FinishReason("future"), text(padded)), limit: len(compact), wantFinish: inference.StructuredOutputFinishReasonOther},
		{name: "malformed JSON precedes oversized", response: response(stream.FinishReasonStop, text(strings.Repeat("not-json", 16))), limit: 1, wantMalformed: true},
		{name: "ambiguous representation precedes oversized", response: response(stream.FinishReasonUnknown, text(padded), terminal()), limit: 1, wantMalformed: true},
		{name: "zero limit fails closed", response: response(stream.FinishReasonStop, text(compact)), limit: 0, wantReason: OutputFailureTooLarge},
		{name: "negative limit fails closed", response: response(stream.FinishReasonStop, text(compact)), limit: -1, wantReason: OutputFailureTooLarge},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			result, err := extractStructuredResult(tt.response, nil, tt.limit)
			if tt.want != "" {
				if err != nil || string(result.Output) != tt.want {
					t.Fatalf("extractStructuredResult() = %s,%v, want %s,nil", result.Output, err, tt.want)
				}
				return
			}
			var outputErr *OutputError
			if !errors.As(err, &outputErr) || !outputErr.Valid() {
				t.Fatalf("error = %T %v, want valid OutputError", err, err)
			}
			if tt.wantReason != "" && outputErr.Reason != tt.wantReason {
				t.Fatalf("reason = %q, want %q", outputErr.Reason, tt.wantReason)
			}
			if tt.wantFinish != "" {
				var finishErr *inference.StructuredOutputFinishError
				if !errors.As(err, &finishErr) || finishErr.Reason != tt.wantFinish {
					t.Fatalf("error = %T %v, want finish %q", err, err, tt.wantFinish)
				}
			}
			if tt.wantMalformed {
				var malformedErr *inference.MalformedStructuredOutputError
				if !errors.As(err, &malformedErr) {
					t.Fatalf("error = %T %v, want MalformedStructuredOutputError", err, err)
				}
			}
			if strings.Contains(err.Error(), compact) {
				t.Fatalf("error leaked raw output: %v", err)
			}
		})
	}
}
