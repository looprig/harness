package hustleruntime

import (
	"bytes"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"testing"

	"github.com/looprig/core/content"
	"github.com/looprig/inference"
	"github.com/looprig/inference/stream"
)

func TestClassifyToolResponseTerminalAndEvidenceVariants(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		response     *inference.Response
		wantTerminal json.RawMessage
		wantCalls    []evidenceToolCall
	}{
		{
			name:         "terminal text",
			response:     toolResponse(stream.FinishReasonStop, toolResponseText(` { "allowed": true } `)),
			wantTerminal: json.RawMessage(`{"allowed":true}`),
		},
		{
			name: "terminal reserved tool",
			response: toolResponse(stream.FinishReasonToolUse, &content.ToolUseBlock{
				ID: "terminal", Name: inference.StructuredOutputToolName, Input: json.RawMessage(`{"allowed":true}`),
			}),
			wantTerminal: json.RawMessage(`{"allowed":true}`),
		},
		{
			name: "one ordinary call",
			response: toolResponse(stream.FinishReasonToolUse, &content.ToolUseBlock{
				ID: "call-1", Name: "workspace.status", Input: json.RawMessage(`{"path":"."}`),
			}),
			wantCalls: []evidenceToolCall{{id: "call-1", name: "workspace.status", input: json.RawMessage(`{"path":"."}`)}},
		},
		{
			name: "several ordinary calls preserve order",
			response: toolResponse(stream.FinishReasonToolUse,
				&content.ThinkingBlock{Thinking: "private"},
				&content.ToolUseBlock{ID: "call-1", Name: "workspace.status", Input: json.RawMessage(`{}`)},
				&content.ToolUseBlock{ID: "call-2", Name: "workspace.read", Input: json.RawMessage(`{"path":"README.md"}`)},
			),
			wantCalls: []evidenceToolCall{
				{id: "call-1", name: "workspace.status", input: json.RawMessage(`{}`)},
				{id: "call-2", name: "workspace.read", input: json.RawMessage(`{"path":"README.md"}`)},
			},
		},
		{
			name: "unknown finish reason is representation neutral",
			response: toolResponse(stream.FinishReasonUnknown, &content.ToolUseBlock{
				ID: "call-1", Name: "workspace.status", Input: json.RawMessage(`{}`),
			}),
			wantCalls: []evidenceToolCall{{id: "call-1", name: "workspace.status", input: json.RawMessage(`{}`)}},
		},
	}

	known := map[string]struct{}{"workspace.status": {}, "workspace.read": {}}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := classifyToolResponse(tt.response, known, toolResponseLimits{outputBytes: 256, maxCallsPerRound: 16})
			if err != nil {
				t.Fatalf("classifyToolResponse() error = %v", err)
			}
			switch typed := got.(type) {
			case terminalToolResponse:
				if tt.wantTerminal == nil || !bytes.Equal(typed.output, tt.wantTerminal) {
					t.Fatalf("terminal output = %s, want %s", typed.output, tt.wantTerminal)
				}
			case evidenceToolResponse:
				if tt.wantCalls == nil || !equalEvidenceCalls(typed.calls, tt.wantCalls) {
					t.Fatalf("evidence calls = %#v, want %#v", typed.calls, tt.wantCalls)
				}
			default:
				t.Fatalf("response variant = %T, want sealed terminal or evidence variant", got)
			}
		})
	}
}

func TestClassifyToolResponseRejectsInvalidShapesWithoutEcho(t *testing.T) {
	t.Parallel()

	const secret = "never-echo-model-output"
	var nilText *content.TextBlock
	var nilThinking *content.ThinkingBlock
	var nilTool *content.ToolUseBlock
	var nilImage *content.ImageBlock

	tests := []struct {
		name     string
		response *inference.Response
		want     ToolResponseFailureReason
	}{
		{name: "nil response", want: ToolResponseFailureInvalidShape},
		{name: "nil message", response: &inference.Response{FinishReason: stream.FinishReasonStop}, want: ToolResponseFailureInvalidShape},
		{name: "wrong role", response: &inference.Response{Message: &content.AIMessage{Message: content.Message{Role: content.RoleUser}}, FinishReason: stream.FinishReasonStop}, want: ToolResponseFailureInvalidShape},
		{name: "empty blocks", response: toolResponse(stream.FinishReasonStop), want: ToolResponseFailureInvalidShape},
		{name: "nil interface block", response: toolResponse(stream.FinishReasonStop, nil), want: ToolResponseFailureInvalidShape},
		{name: "typed nil text", response: toolResponse(stream.FinishReasonStop, nilText), want: ToolResponseFailureInvalidShape},
		{name: "typed nil thinking", response: toolResponse(stream.FinishReasonStop, nilThinking), want: ToolResponseFailureInvalidShape},
		{name: "typed nil tool", response: toolResponse(stream.FinishReasonToolUse, nilTool), want: ToolResponseFailureInvalidShape},
		{name: "typed nil unsupported", response: toolResponse(stream.FinishReasonStop, nilImage), want: ToolResponseFailureInvalidShape},
		{name: "unsupported block", response: toolResponse(stream.FinishReasonStop, &content.ImageBlock{}), want: ToolResponseFailureInvalidShape},
		{name: "thinking only", response: toolResponse(stream.FinishReasonStop, &content.ThinkingBlock{Thinking: secret}), want: ToolResponseFailureInvalidShape},
		{name: "unknown tool", response: toolResponse(stream.FinishReasonToolUse, &content.ToolUseBlock{ID: "id", Name: secret, Input: json.RawMessage(`{}`)}), want: ToolResponseFailureUnknownTool},
		{name: "malformed arguments", response: toolResponse(stream.FinishReasonToolUse, &content.ToolUseBlock{ID: "id", Name: "workspace.status", Input: json.RawMessage(`{"secret":"` + secret)}), want: ToolResponseFailureMalformedArguments},
		{name: "non-object arguments", response: toolResponse(stream.FinishReasonToolUse, &content.ToolUseBlock{ID: "id", Name: "workspace.status", Input: json.RawMessage(`["` + secret + `"]`)}), want: ToolResponseFailureMalformedArguments},
		{name: "duplicate argument member", response: toolResponse(stream.FinishReasonToolUse, &content.ToolUseBlock{ID: "id", Name: "workspace.status", Input: json.RawMessage(`{"path":"safe","path":"` + secret + `"}`)}), want: ToolResponseFailureMalformedArguments},
		{name: "missing id", response: toolResponse(stream.FinishReasonToolUse, &content.ToolUseBlock{Name: "workspace.status", Input: json.RawMessage(`{}`)}), want: ToolResponseFailureMissingCallID},
		{name: "duplicate id", response: toolResponse(stream.FinishReasonToolUse,
			&content.ToolUseBlock{ID: "same", Name: "workspace.status", Input: json.RawMessage(`{}`)},
			&content.ToolUseBlock{ID: "same", Name: "workspace.read", Input: json.RawMessage(`{}`)},
		), want: ToolResponseFailureDuplicateCallID},
		{name: "mixed text ordinary tool", response: toolResponse(stream.FinishReasonToolUse,
			toolResponseText(`{"secret":"`+secret+`"}`),
			&content.ToolUseBlock{ID: "id", Name: "workspace.status", Input: json.RawMessage(`{}`)},
		), want: ToolResponseFailureMixed},
		{name: "mixed terminal ordinary tool", response: toolResponse(stream.FinishReasonToolUse,
			&content.ToolUseBlock{ID: "terminal", Name: inference.StructuredOutputToolName, Input: json.RawMessage(`{"secret":"` + secret + `"}`)},
			&content.ToolUseBlock{ID: "id", Name: "workspace.status", Input: json.RawMessage(`{}`)},
		), want: ToolResponseFailureMixed},
		{name: "duplicate terminal", response: toolResponse(stream.FinishReasonToolUse,
			&content.ToolUseBlock{ID: "terminal-1", Name: inference.StructuredOutputToolName, Input: json.RawMessage(`{"secret":"` + secret + `"}`)},
			&content.ToolUseBlock{ID: "terminal-2", Name: inference.StructuredOutputToolName, Input: json.RawMessage(`{"allowed":true}`)},
		), want: ToolResponseFailureDuplicateTerminal},
		{name: "empty terminal text", response: toolResponse(stream.FinishReasonStop, toolResponseText("")), want: ToolResponseFailureInvalidTerminal},
		{name: "malformed terminal text", response: toolResponse(stream.FinishReasonStop, toolResponseText(secret)), want: ToolResponseFailureInvalidTerminal},
	}

	known := map[string]struct{}{"workspace.status": {}, "workspace.read": {}}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := classifyToolResponse(tt.response, known, toolResponseLimits{outputBytes: 256, maxCallsPerRound: 16})
			if got != nil {
				t.Fatalf("classifyToolResponse() variant = %T, want nil", got)
			}
			var responseErr *ToolResponseError
			if !errors.As(err, &responseErr) || !responseErr.Valid() || responseErr.Reason != tt.want {
				t.Fatalf("error = %#v, want valid ToolResponseError reason %q", err, tt.want)
			}
			if strings.Contains(err.Error(), secret) {
				t.Fatalf("error %q echoed model-controlled content", err)
			}
		})
	}
}

func TestClassifyToolResponseRejectsFinishReasonContradictions(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		response *inference.Response
	}{
		{name: "ordinary call with stop", response: toolResponse(stream.FinishReasonStop, &content.ToolUseBlock{ID: "id", Name: "workspace.status", Input: json.RawMessage(`{}`)})},
		{name: "terminal tool with stop", response: toolResponse(stream.FinishReasonStop, &content.ToolUseBlock{ID: "terminal", Name: inference.StructuredOutputToolName, Input: json.RawMessage(`{}`)})},
		{name: "terminal text with tool use", response: toolResponse(stream.FinishReasonToolUse, toolResponseText(`{}`))},
		{name: "terminal with length", response: toolResponse(stream.FinishReasonLength, toolResponseText(`{}`))},
		{name: "terminal with content filter", response: toolResponse(stream.FinishReasonContentFilter, toolResponseText(`{}`))},
		{name: "future finish reason", response: toolResponse(stream.FinishReason("provider-secret"), toolResponseText(`{}`))},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := classifyToolResponse(tt.response, map[string]struct{}{"workspace.status": {}}, toolResponseLimits{outputBytes: 256, maxCallsPerRound: 16})
			if got != nil {
				t.Fatalf("variant = %T, want nil", got)
			}
			var responseErr *ToolResponseError
			if !errors.As(err, &responseErr) || responseErr.Reason != ToolResponseFailureFinishReason {
				t.Fatalf("error = %#v, want finish-reason classification", err)
			}
			if strings.Contains(err.Error(), string(tt.response.FinishReason)) {
				t.Fatalf("error %q retained raw finish reason", err)
			}
		})
	}
}

func TestClassifyToolResponseEnforcesExactTerminalOutputByteLimit(t *testing.T) {
	t.Parallel()

	const output = `{"value":"1234"}`
	for _, tt := range []struct {
		name          string
		response      *inference.Response
		limit         int
		wantErr       bool
		wantCompacted json.RawMessage
	}{
		{name: "text exact", response: toolResponse(stream.FinishReasonStop, toolResponseText(output)), limit: len(output), wantCompacted: json.RawMessage(output)},
		{name: "text one over", response: toolResponse(stream.FinishReasonStop, toolResponseText(output)), limit: len(output) - 1, wantErr: true},
		{name: "terminal tool exact", response: toolResponse(stream.FinishReasonToolUse, &content.ToolUseBlock{Name: inference.StructuredOutputToolName, Input: json.RawMessage(output)}), limit: len(output), wantCompacted: json.RawMessage(output)},
		{name: "terminal tool one over", response: toolResponse(stream.FinishReasonToolUse, &content.ToolUseBlock{Name: inference.StructuredOutputToolName, Input: json.RawMessage(output)}), limit: len(output) - 1, wantErr: true},
		{name: "raw whitespace counts before compaction", response: toolResponse(stream.FinishReasonStop, toolResponseText(" "+output+" ")), limit: len(output), wantErr: true},
		{name: "zero", response: toolResponse(stream.FinishReasonStop, toolResponseText(output)), limit: 0, wantErr: true},
		{name: "negative", response: toolResponse(stream.FinishReasonStop, toolResponseText(output)), limit: -1, wantErr: true},
	} {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := classifyToolResponse(tt.response, nil, toolResponseLimits{outputBytes: tt.limit, maxCallsPerRound: 16})
			if tt.wantErr {
				var responseErr *ToolResponseError
				if got != nil || !errors.As(err, &responseErr) || responseErr.Reason != ToolResponseFailureTooLarge {
					t.Fatalf("classifyToolResponse() = (%T,%#v), want too-large error", got, err)
				}
				return
			}
			terminal, ok := got.(terminalToolResponse)
			if err != nil || !ok || !bytes.Equal(terminal.output, tt.wantCompacted) {
				t.Fatalf("classifyToolResponse() = (%#v,%v), want exact terminal output", got, err)
			}
		})
	}
}

func TestClassifyToolResponseBoundsProviderControlledEvidenceFields(t *testing.T) {
	t.Parallel()

	const (
		outputLimit      = 256
		maxBlockCount    = 4096
		maxThinkingBytes = 1 << 20
		maxCallIDBytes   = 1024
		maxToolNameBytes = 64
	)
	exactLengthToolName := strings.Repeat("n", maxToolNameBytes)
	known := map[string]struct{}{"workspace.status": {}, exactLengthToolName: {}}
	validCall := func() *content.ToolUseBlock {
		return &content.ToolUseBlock{
			ID: "call", Name: "workspace.status", Input: json.RawMessage(`{}`),
		}
	}

	exactBlocks := make([]content.Block, maxBlockCount)
	for index := range exactBlocks {
		exactBlocks[index] = &content.ThinkingBlock{}
	}
	exactBlocks[len(exactBlocks)-1] = validCall()
	overBlocks := append(append([]content.Block(nil), exactBlocks...), &content.ThinkingBlock{})

	tests := []struct {
		name     string
		response *inference.Response
		wantErr  bool
	}{
		{
			name:     "block count exact",
			response: toolResponse(stream.FinishReasonToolUse, exactBlocks...),
		},
		{
			name:     "block count one over",
			response: toolResponse(stream.FinishReasonToolUse, overBlocks...),
			wantErr:  true,
		},
		{
			name: "thinking bytes exact",
			response: toolResponse(stream.FinishReasonToolUse,
				&content.ThinkingBlock{Thinking: strings.Repeat("t", maxThinkingBytes)},
				validCall(),
			),
		},
		{
			name: "thinking bytes one over",
			response: toolResponse(stream.FinishReasonToolUse,
				&content.ThinkingBlock{Thinking: strings.Repeat("t", maxThinkingBytes+1)},
				validCall(),
			),
			wantErr: true,
		},
		{
			name: "thinking and signature aggregate exact",
			response: toolResponse(stream.FinishReasonToolUse,
				&content.ThinkingBlock{
					Thinking:  strings.Repeat("t", maxThinkingBytes/2),
					Signature: strings.Repeat("s", maxThinkingBytes/2),
				},
				validCall(),
			),
		},
		{
			name: "thinking and signature aggregate one over",
			response: toolResponse(stream.FinishReasonToolUse,
				&content.ThinkingBlock{
					Thinking:  strings.Repeat("t", maxThinkingBytes/2),
					Signature: strings.Repeat("s", maxThinkingBytes/2+1),
				},
				validCall(),
			),
			wantErr: true,
		},
		{
			name: "call id bytes exact",
			response: toolResponse(stream.FinishReasonToolUse, &content.ToolUseBlock{
				ID: strings.Repeat("i", maxCallIDBytes), Name: "workspace.status", Input: json.RawMessage(`{}`),
			}),
		},
		{
			name: "call id bytes one over",
			response: toolResponse(stream.FinishReasonToolUse, &content.ToolUseBlock{
				ID: strings.Repeat("i", maxCallIDBytes+1), Name: "workspace.status", Input: json.RawMessage(`{}`),
			}),
			wantErr: true,
		},
		{
			name: "tool name bytes exact",
			response: toolResponse(stream.FinishReasonToolUse, &content.ToolUseBlock{
				ID: "call", Name: exactLengthToolName, Input: json.RawMessage(`{}`),
			}),
		},
		{
			name: "tool name bytes one over",
			response: toolResponse(stream.FinishReasonToolUse, &content.ToolUseBlock{
				ID: "call", Name: strings.Repeat("n", maxToolNameBytes+1), Input: json.RawMessage(`{}`),
			}),
			wantErr: true,
		},
		{
			name: "argument bytes exact",
			response: toolResponse(stream.FinishReasonToolUse, &content.ToolUseBlock{
				ID: "call", Name: "workspace.status",
				Input: json.RawMessage(`{"value":"` + strings.Repeat("a", outputLimit-len(`{"value":""}`)) + `"}`),
			}),
		},
		{
			name: "argument bytes one over",
			response: toolResponse(stream.FinishReasonToolUse, &content.ToolUseBlock{
				ID: "call", Name: "workspace.status",
				Input: json.RawMessage(`{"value":"` + strings.Repeat("a", outputLimit-len(`{"value":""}`)+1) + `"}`),
			}),
			wantErr: true,
		},
		{
			name: "aggregate argument bytes exact",
			response: toolResponse(stream.FinishReasonToolUse,
				&content.ToolUseBlock{ID: "a", Name: "workspace.status", Input: json.RawMessage(`{"value":"` + strings.Repeat("a", 112) + `"}`)},
				&content.ToolUseBlock{ID: "b", Name: "workspace.status", Input: json.RawMessage(`{"value":"` + strings.Repeat("b", 120) + `"}`)},
			),
		},
		{
			name: "aggregate argument bytes one over",
			response: toolResponse(stream.FinishReasonToolUse,
				&content.ToolUseBlock{ID: "a", Name: "workspace.status", Input: json.RawMessage(`{"value":"` + strings.Repeat("a", 112) + `"}`)},
				&content.ToolUseBlock{ID: "b", Name: "workspace.status", Input: json.RawMessage(`{"value":"` + strings.Repeat("b", 121) + `"}`)},
			),
			wantErr: true,
		},
	}

	for _, testCase := range tests {
		testCase := testCase
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			got, err := classifyToolResponse(testCase.response, known, toolResponseLimits{outputBytes: outputLimit, maxCallsPerRound: maxBlockCount})
			if testCase.wantErr {
				var responseErr *ToolResponseError
				if got != nil || !errors.As(err, &responseErr) {
					t.Fatalf("classifyToolResponse() = (%T, %v), want bounded failure", got, err)
				}
				if strings.Contains(testCase.name, "one over") && responseErr.Reason != ToolResponseFailureTooLarge {
					t.Fatalf("reason = %q, want %q", responseErr.Reason, ToolResponseFailureTooLarge)
				}
				return
			}
			if err != nil {
				t.Fatalf("classifyToolResponse() error = %v", err)
			}
		})
	}
}

func TestValidEvidenceArgumentsEnforcesDepthMemberAndTokenBounds(t *testing.T) {
	t.Parallel()

	const (
		maxDepth   = 64
		maxMembers = 65536
		maxTokens  = 262144
	)
	nested := func(depth int) json.RawMessage {
		return json.RawMessage(strings.Repeat(`{"v":`, depth) + `0` + strings.Repeat(`}`, depth))
	}
	members := func(count int) json.RawMessage {
		var builder strings.Builder
		builder.WriteByte('{')
		for index := 0; index < count; index++ {
			if index > 0 {
				builder.WriteByte(',')
			}
			builder.WriteString(`"k`)
			builder.WriteString(strconv.Itoa(index))
			builder.WriteString(`":0`)
		}
		builder.WriteByte('}')
		return json.RawMessage(builder.String())
	}
	arrayTokens := func(values int) json.RawMessage {
		return json.RawMessage(`{"v":[` + strings.Repeat(`0,`, values-1) + `0]}`)
	}

	tests := []struct {
		name  string
		input json.RawMessage
		want  bool
	}{
		{name: "depth exact", input: nested(maxDepth), want: true},
		{name: "depth one over", input: nested(maxDepth + 1)},
		{name: "members exact", input: members(maxMembers), want: true},
		{name: "members one over", input: members(maxMembers + 1)},
		{name: "tokens exact", input: arrayTokens(maxTokens - 5), want: true},
		{name: "tokens one over", input: arrayTokens(maxTokens - 4)},
		{name: "duplicate nested member", input: json.RawMessage(`{"outer":{"x":1,"x":2}}`)},
		{name: "non object root", input: json.RawMessage(`[]`)},
		{name: "trailing value", input: json.RawMessage(`{} {}`)},
		{name: "malformed", input: json.RawMessage(`{"x":`)},
	}
	for _, testCase := range tests {
		testCase := testCase
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			if got := validEvidenceArguments(testCase.input); got != testCase.want {
				t.Fatalf("validEvidenceArguments() = %t, want %t", got, testCase.want)
			}
		})
	}
}

func TestPreflightToolResponseEnforcesCallCountBeforeArgumentParsing(t *testing.T) {
	t.Parallel()

	response := toolResponse(stream.FinishReasonToolUse,
		&content.ToolUseBlock{ID: "first", Name: "workspace.status", Input: json.RawMessage(`{}`)},
		&content.ToolUseBlock{
			ID: "second", Name: "workspace.status",
			Input: json.RawMessage(`{"malformed":"provider-secret"`),
		},
	)
	got, err := classifyToolResponse(
		response,
		map[string]struct{}{"workspace.status": {}},
		toolResponseLimits{outputBytes: 256, maxCallsPerRound: 1},
	)
	var evidenceErr *EvidenceError
	if got != nil || !errors.As(err, &evidenceErr) ||
		evidenceErr.Reason != EvidenceFailureCallsPerRoundExceeded {
		t.Fatalf("classifyToolResponse() = (%T, %v), want pre-parse calls-per-round failure", got, err)
	}
}

func TestPreflightToolResponseEnforcesExactAggregateProviderByteLimit(t *testing.T) {
	t.Parallel()

	const callCount = maxProviderResponseBlocks
	metadataBytes := callCount * (maxProviderCallIDBytes + maxProviderToolNameBytes)
	argumentBytes := maxProviderResponseBytes - metadataBytes
	if argumentBytes <= 0 {
		t.Fatal("test contract requires positive argument budget")
	}
	makeResponse := func(extra int) *inference.Response {
		blocks := make([]content.Block, callCount)
		remaining := argumentBytes + extra
		for index := range blocks {
			size := remaining / (callCount - index)
			remaining -= size
			blocks[index] = &content.ToolUseBlock{
				ID:    strings.Repeat("i", maxProviderCallIDBytes),
				Name:  strings.Repeat("n", maxProviderToolNameBytes),
				Input: json.RawMessage(strings.Repeat(" ", size)),
			}
		}
		return toolResponse(stream.FinishReasonToolUse, blocks...)
	}
	limits := toolResponseLimits{
		outputBytes:      maxProviderResponseBytes,
		maxCallsPerRound: callCount,
	}
	if err := preflightToolResponse(makeResponse(0), limits); err != nil {
		t.Fatalf("exact aggregate provider bytes rejected: %v", err)
	}
	var responseErr *ToolResponseError
	if err := preflightToolResponse(makeResponse(1), limits); !errors.As(err, &responseErr) ||
		responseErr.Reason != ToolResponseFailureTooLarge {
		t.Fatalf("one-over aggregate provider bytes error = %v, want too large", err)
	}
}

func TestClassifyToolResponseOwnsReturnedBytes(t *testing.T) {
	t.Parallel()

	terminalInput := json.RawMessage(`{"allowed":true}`)
	terminalResponse := toolResponse(stream.FinishReasonToolUse, &content.ToolUseBlock{
		ID: "terminal", Name: inference.StructuredOutputToolName, Input: terminalInput,
	})
	gotTerminal, err := classifyToolResponse(terminalResponse, nil, toolResponseLimits{outputBytes: len(terminalInput), maxCallsPerRound: 1})
	if err != nil {
		t.Fatalf("classify terminal: %v", err)
	}
	terminalInput[2] = 'X'
	if terminal := gotTerminal.(terminalToolResponse); !bytes.Equal(terminal.output, json.RawMessage(`{"allowed":true}`)) {
		t.Fatalf("terminal output aliases provider input: %s", terminal.output)
	}

	callInput := json.RawMessage(`{"path":"."}`)
	evidenceResponse := toolResponse(stream.FinishReasonToolUse, &content.ToolUseBlock{
		ID: "call", Name: "workspace.status", Input: callInput,
	})
	gotEvidence, err := classifyToolResponse(evidenceResponse, map[string]struct{}{"workspace.status": {}}, toolResponseLimits{outputBytes: 256, maxCallsPerRound: 1})
	if err != nil {
		t.Fatalf("classify evidence: %v", err)
	}
	callInput[2] = 'X'
	if evidence := gotEvidence.(evidenceToolResponse); !bytes.Equal(evidence.calls[0].input, json.RawMessage(`{"path":"."}`)) {
		t.Fatalf("evidence input aliases provider input: %s", evidence.calls[0].input)
	}
}

func toolResponse(finish stream.FinishReason, blocks ...content.Block) *inference.Response {
	return &inference.Response{
		Message:      &content.AIMessage{Message: content.Message{Role: content.RoleAssistant, Blocks: blocks}},
		FinishReason: finish,
	}
}

func toolResponseText(text string) content.Block {
	return &content.TextBlock{Text: text}
}

func equalEvidenceCalls(got, want []evidenceToolCall) bool {
	if len(got) != len(want) {
		return false
	}
	for index := range got {
		if got[index].id != want[index].id || got[index].name != want[index].name ||
			!bytes.Equal(got[index].input, want[index].input) {
			return false
		}
	}
	return true
}
