package hustleruntime

import (
	"errors"
	"strings"
	"testing"

	"github.com/looprig/core/content"
	"github.com/looprig/inference"
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
