package hook

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/looprig/core/content"
	"github.com/looprig/harness/pkg/gate"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/tool"
	"github.com/looprig/inference"
	"github.com/looprig/inference/model"
	"github.com/looprig/inference/stream"
)

func TestOperationValidityAndGuardability(t *testing.T) {
	t.Parallel()

	tests := []struct {
		operation Operation
		valid     bool
		guardable bool
	}{
		{Operation(0), false, false},
		{OperationTurn, true, true},
		{OperationStep, true, false},
		{OperationInference, true, true},
		{OperationCompaction, true, true},
		{OperationToolCall, true, true},
		{OperationGateWait, true, false},
		{OperationToolExecution, true, false},
		{OperationJournalAppend, true, false},
		{OperationJournalAppend + 1, false, false},
	}

	for _, test := range tests {
		if got := test.operation.Valid(); got != test.valid {
			t.Errorf("Operation(%d).Valid() = %v, want %v", test.operation, got, test.valid)
		}
		if got := test.operation.Guardable(); got != test.guardable {
			t.Errorf("Operation(%d).Guardable() = %v, want %v", test.operation, got, test.guardable)
		}
	}
}

func TestValidateSet(t *testing.T) {
	t.Parallel()

	noopGuard := func(context.Context, Call) error { return nil }
	noopAround := func(ctx context.Context, _ Call) (context.Context, FinishFunc) {
		return ctx, func(Result) {}
	}

	if err := ValidateSet(Set{}); err != nil {
		t.Fatalf("ValidateSet(empty) = %v, want nil", err)
	}
	if err := ValidateSet(Set{
		PolicyRevision: "policy-v1",
		Guards: []Guard{
			{Operation: OperationTurn, Check: noopGuard},
			{Operation: OperationInference, Check: noopGuard},
			{Operation: OperationCompaction, Check: noopGuard},
			{Operation: OperationToolCall, Check: noopGuard},
		},
		Around: []Around{
			{Operation: OperationTurn, Begin: noopAround},
			{Operation: OperationStep, Begin: noopAround},
			{Operation: OperationInference, Begin: noopAround},
			{Operation: OperationCompaction, Begin: noopAround},
			{Operation: OperationToolCall, Begin: noopAround},
			{Operation: OperationGateWait, Begin: noopAround},
			{Operation: OperationToolExecution, Begin: noopAround},
			{Operation: OperationJournalAppend, Begin: noopAround},
		},
	}); err != nil {
		t.Fatalf("ValidateSet(valid) = %v, want nil", err)
	}

	tests := []struct {
		name string
		set  Set
		kind ConfigErrorKind
	}{
		{
			name: "guard unknown operation",
			set:  Set{PolicyRevision: "v1", Guards: []Guard{{Operation: Operation(99), Check: noopGuard}}},
			kind: ConfigUnknownOperation,
		},
		{
			name: "guard non-guardable operation",
			set:  Set{PolicyRevision: "v1", Guards: []Guard{{Operation: OperationStep, Check: noopGuard}}},
			kind: ConfigOperationNotGuardable,
		},
		{
			name: "nil guard",
			set:  Set{PolicyRevision: "v1", Guards: []Guard{{Operation: OperationTurn}}},
			kind: ConfigNilGuard,
		},
		{
			name: "around unknown operation",
			set:  Set{Around: []Around{{Operation: Operation(99), Begin: noopAround}}},
			kind: ConfigUnknownOperation,
		},
		{
			name: "nil around",
			set:  Set{Around: []Around{{Operation: OperationStep}}},
			kind: ConfigNilAround,
		},
		{
			name: "guard without revision",
			set:  Set{Guards: []Guard{{Operation: OperationTurn, Check: noopGuard}}},
			kind: ConfigMissingPolicyRevision,
		},
		{
			name: "guard with blank revision",
			set:  Set{PolicyRevision: " \t", Guards: []Guard{{Operation: OperationTurn, Check: noopGuard}}},
			kind: ConfigMissingPolicyRevision,
		},
		{
			name: "revision without guard",
			set:  Set{PolicyRevision: "v1", Around: []Around{{Operation: OperationTurn, Begin: noopAround}}},
			kind: ConfigUnexpectedPolicyRevision,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			err := ValidateSet(test.set)
			var configErr *ConfigError
			if !errors.As(err, &configErr) {
				t.Fatalf("ValidateSet() error = %T, want *ConfigError", err)
			}
			if configErr.Kind != test.kind {
				t.Fatalf("ConfigError.Kind = %q, want %q", configErr.Kind, test.kind)
			}
		})
	}
}

func TestDeny(t *testing.T) {
	t.Parallel()

	err := Deny("policy.blocked", "operation is not permitted")
	var denial *Denial
	if !errors.As(err, &denial) {
		t.Fatalf("Deny(valid) = %T, want *Denial", err)
	}
	if denial.Code != "policy.blocked" || denial.Reason != "operation is not permitted" {
		t.Fatalf("Denial = %#v", denial)
	}

	tests := []struct {
		name   string
		code   string
		reason string
	}{
		{name: "blank code", code: "", reason: "reason"},
		{name: "whitespace code", code: " \t", reason: "reason"},
		{name: "blank reason", code: "code", reason: ""},
		{name: "whitespace reason", code: "code", reason: "\n\t"},
		{name: "control in code", code: "bad\x00code", reason: "reason"},
		{name: "control in reason", code: "code", reason: "bad\nreason"},
		{name: "invalid UTF-8 code", code: string([]byte{0xff}), reason: "reason"},
		{name: "invalid UTF-8 reason", code: "code", reason: string([]byte{0xff})},
		{name: "oversized code", code: strings.Repeat("c", 65), reason: "reason"},
		{name: "oversized reason", code: "code", reason: strings.Repeat("r", 1025)},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			err := Deny(test.code, test.reason)
			var configErr *ConfigError
			if !errors.As(err, &configErr) {
				t.Fatalf("Deny() error = %T, want *ConfigError", err)
			}
			if configErr.Kind != ConfigInvalidDenial {
				t.Fatalf("ConfigError.Kind = %q, want %q", configErr.Kind, ConfigInvalidDenial)
			}
			if errors.As(err, &denial) {
				t.Fatal("invalid denial was exposed as an intentional *Denial")
			}
		})
	}
}

func TestValidateCallRequiresOneMatchingPayload(t *testing.T) {
	t.Parallel()

	valid := []Call{
		{Operation: OperationTurn, Turn: &TurnData{}},
		{Operation: OperationStep, Step: &StepData{}},
		{Operation: OperationInference, Inference: &InferenceData{}},
		{Operation: OperationCompaction, Compaction: &CompactionData{}},
		{Operation: OperationToolCall, ToolCall: &ToolCallData{}},
		{Operation: OperationGateWait, GateWait: &GateWaitData{}},
		{Operation: OperationToolExecution, ToolExecution: &ToolExecutionData{}},
		{Operation: OperationJournalAppend, JournalAppend: &JournalAppendData{}},
	}
	for _, call := range valid {
		if err := ValidateCall(call); err != nil {
			t.Errorf("ValidateCall(%d) = %v, want nil", call.Operation, err)
		}
	}

	tests := []struct {
		name string
		call Call
		kind CallErrorKind
	}{
		{name: "unknown operation", call: Call{Operation: 99, Turn: &TurnData{}}, kind: CallUnknownOperation},
		{name: "no payload", call: Call{Operation: OperationTurn}, kind: CallInvalidPayload},
		{name: "mismatched payload", call: Call{Operation: OperationTurn, Step: &StepData{}}, kind: CallInvalidPayload},
		{
			name: "matching plus extra payload",
			call: Call{Operation: OperationTurn, Turn: &TurnData{}, Step: &StepData{}},
			kind: CallInvalidPayload,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			err := ValidateCall(test.call)
			var callErr *CallError
			if !errors.As(err, &callErr) {
				t.Fatalf("ValidateCall() error = %T, want *CallError", err)
			}
			if callErr.Kind != test.kind {
				t.Fatalf("CallError.Kind = %q, want %q", callErr.Kind, test.kind)
			}
			var configErr *ConfigError
			if errors.As(err, &configErr) {
				t.Fatal("ValidateCall() exposed malformed runtime data as *ConfigError")
			}
		})
	}
}

func TestCloneCallOwnsReferenceBackedData(t *testing.T) {
	t.Parallel()

	temperature := 0.25
	maxTokens := 64
	user := &content.UserMessage{Message: content.Message{
		Role: content.RoleUser,
		Blocks: []content.Block{&content.ToolResultBlock{
			ToolUseID: "nested",
			Content: []content.Block{&content.ImageBlock{
				MediaType: "image/png",
				Source:    content.ImageSource{URL: "https://example.invalid/image", Data: []byte{1, 2, 3}},
			}},
		}},
	}}
	ai := &content.AIMessage{
		Message: content.Message{
			Role:   content.RoleAssistant,
			Blocks: []content.Block{&content.ToolUseBlock{ID: "call", Name: "tool", Input: json.RawMessage(`{"x":1}`)}},
		},
		Usage: &content.Usage{InputTokens: 2, OutputTokens: 3},
	}
	request := &inference.Request{
		Model: model.Model{Sampling: model.Sampling{
			Temperature: &temperature,
			MaxTokens:   &maxTokens,
			Stop:        []string{"END"},
		}},
		Messages: []content.Conversation{user},
		Tools: []inference.Tool{{
			Name:   "tool",
			Schema: json.RawMessage(`{"type":"object"}`),
		}},
		Output: &inference.OutputSchema{
			Name:   "answer",
			Schema: json.RawMessage(`{"type":"object"}`),
		},
		Override: &model.Sampling{
			Temperature: &temperature,
			Stop:        []string{"STOP"},
		},
	}
	compactionInput := &loop.CompactionInput{Transcript: []content.Conversation{user}}
	compactionOutput := &loop.CompactionOutput{Summary: user}
	toolResult := &tool.ToolResult{Content: []content.Block{&content.DocumentBlock{
		MediaType: "text/plain",
		Name:      "result.txt",
		Data:      []byte("payload"),
		Text:      "payload",
	}}}
	gateAnswer := &gate.Answer{Values: map[string]string{"answer": "yes"}}

	calls := []Call{
		{Operation: OperationTurn, Turn: &TurnData{Input: user}},
		{Operation: OperationInference, Inference: &InferenceData{
			Request: request, AIMessage: ai,
			StreamResult: &stream.StreamResult{Usage: &content.Usage{InputTokens: 5}, Model: "model"},
		}},
		{Operation: OperationCompaction, Compaction: &CompactionData{
			Input: compactionInput, Output: compactionOutput,
		}},
		{Operation: OperationToolCall, ToolCall: &ToolCallData{ArgsJSON: json.RawMessage(`{"raw":true}`)}},
		{Operation: OperationGateWait, GateWait: &GateWaitData{Answer: gateAnswer}},
		{Operation: OperationToolExecution, ToolExecution: &ToolExecutionData{
			ArgsJSON: json.RawMessage(`{"exec":true}`), Result: toolResult,
		}},
		{Operation: OperationStep, Step: &StepData{}},
		{Operation: OperationJournalAppend, JournalAppend: &JournalAppendData{}},
	}

	clones := make([]Call, len(calls))
	for index := range calls {
		clones[index] = CloneCall(calls[index])
		if err := ValidateCall(clones[index]); err != nil {
			t.Fatalf("CloneCall(%d) invalid: %v", calls[index].Operation, err)
		}
	}

	clonedTurnImage := clones[0].Turn.Input.Blocks[0].(*content.ToolResultBlock).Content[0].(*content.ImageBlock)
	clonedTurnImage.Source.Data[0] = 9
	if got := user.Blocks[0].(*content.ToolResultBlock).Content[0].(*content.ImageBlock).Source.Data[0]; got != 1 {
		t.Fatalf("turn clone aliases message block data: source byte = %d", got)
	}

	clonedInference := clones[1].Inference
	*clonedInference.Request.Model.Sampling.Temperature = 0.9
	*clonedInference.Request.Model.Sampling.MaxTokens = 512
	clonedInference.Request.Model.Sampling.Stop[0] = "CHANGED"
	clonedInference.Request.Messages[0].(*content.UserMessage).Blocks[0] = &content.TextBlock{Text: "changed"}
	clonedInference.Request.Tools[0].Schema[0] = '['
	clonedInference.Request.Output.Schema[0] = '['
	*clonedInference.Request.Override.Temperature = 0.8
	clonedInference.Request.Override.Stop[0] = "CHANGED"
	clonedInference.AIMessage.Blocks[0].(*content.ToolUseBlock).Input[0] = '['
	clonedInference.AIMessage.Usage.InputTokens = 99
	clonedInference.StreamResult.Usage.InputTokens = 99

	if temperature != 0.25 || maxTokens != 64 || request.Model.Sampling.Stop[0] != "END" {
		t.Fatal("inference clone aliases model sampling")
	}
	if _, ok := request.Messages[0].(*content.UserMessage).Blocks[0].(*content.ToolResultBlock); !ok {
		t.Fatal("inference clone aliases request messages")
	}
	if string(request.Tools[0].Schema) != `{"type":"object"}` ||
		string(request.Output.Schema) != `{"type":"object"}` ||
		request.Override.Stop[0] != "STOP" {
		t.Fatal("inference clone aliases schema or override data")
	}
	if string(ai.Blocks[0].(*content.ToolUseBlock).Input) != `{"x":1}` ||
		ai.Usage.InputTokens != 2 ||
		calls[1].Inference.StreamResult.Usage.InputTokens != 5 {
		t.Fatal("inference clone aliases terminal data")
	}

	clones[2].Compaction.Input.Transcript[0].(*content.UserMessage).Blocks[0] = &content.TextBlock{Text: "changed"}
	clones[2].Compaction.Output.Summary.Blocks[0] = &content.TextBlock{Text: "changed"}
	if _, ok := compactionInput.Transcript[0].(*content.UserMessage).Blocks[0].(*content.ToolResultBlock); !ok {
		t.Fatal("compaction clone aliases transcript")
	}
	if _, ok := compactionOutput.Summary.Blocks[0].(*content.ToolResultBlock); !ok {
		t.Fatal("compaction clone aliases summary")
	}

	clones[3].ToolCall.ArgsJSON[0] = '['
	if string(calls[3].ToolCall.ArgsJSON) != `{"raw":true}` {
		t.Fatal("tool-call clone aliases JSON")
	}
	clones[4].GateWait.Answer.Values["answer"] = "changed"
	if gateAnswer.Values["answer"] != "yes" {
		t.Fatal("gate-wait clone aliases answer values")
	}
	clones[5].ToolExecution.ArgsJSON[0] = '['
	clones[5].ToolExecution.Result.Content[0].(*content.DocumentBlock).Data[0] = 'X'
	if string(calls[5].ToolExecution.ArgsJSON) != `{"exec":true}` ||
		string(toolResult.Content[0].(*content.DocumentBlock).Data) != "payload" {
		t.Fatal("tool-execution clone aliases JSON or result blocks")
	}
}

func TestCloneCallPreservesNilAndEmptySlices(t *testing.T) {
	t.Parallel()

	nilCall := CloneCall(Call{
		Operation: OperationToolCall,
		ToolCall:  &ToolCallData{ArgsJSON: nil},
	})
	if nilCall.ToolCall.ArgsJSON != nil {
		t.Fatal("CloneCall converted nil JSON to non-nil")
	}

	emptyCall := CloneCall(Call{
		Operation: OperationToolCall,
		ToolCall:  &ToolCallData{ArgsJSON: json.RawMessage{}},
	})
	if emptyCall.ToolCall.ArgsJSON == nil || len(emptyCall.ToolCall.ArgsJSON) != 0 {
		t.Fatal("CloneCall converted non-nil empty JSON to nil")
	}

	emptyInference := CloneCall(Call{
		Operation: OperationInference,
		Inference: &InferenceData{Request: &inference.Request{
			Model:    model.Model{Sampling: model.Sampling{Stop: []string{}}},
			Messages: content.AgenticMessages{},
			Tools:    []inference.Tool{},
			Override: &model.Sampling{Stop: []string{}},
		}},
	})
	if emptyInference.Inference.Request.Model.Sampling.Stop == nil {
		t.Fatal("CloneCall converted non-nil empty model stop sequence to nil")
	}
	if emptyInference.Inference.Request.Override.Stop == nil {
		t.Fatal("CloneCall converted non-nil empty override stop sequence to nil")
	}
	if emptyInference.Inference.Request.Messages == nil || emptyInference.Inference.Request.Tools == nil {
		t.Fatal("CloneCall converted non-nil empty inference slices to nil")
	}
}

func TestCloneResultClonesCallAndRetainsError(t *testing.T) {
	t.Parallel()

	sentinel := errors.New("sentinel")
	result := Result{
		Call: Call{
			Operation: OperationToolExecution,
			ToolExecution: &ToolExecutionData{
				ArgsJSON: json.RawMessage(`{"x":1}`),
				Result:   tool.TextResult("original"),
			},
		},
		EndedAt: time.Unix(123, 456),
		Outcome: OutcomeFailed,
		Err:     sentinel,
	}

	cloned := CloneResult(result)
	cloned.ToolExecution.ArgsJSON[0] = '['
	cloned.ToolExecution.Result.Content[0].(*content.TextBlock).Text = "changed"

	if string(result.ToolExecution.ArgsJSON) != `{"x":1}` ||
		result.ToolExecution.Result.Content[0].(*content.TextBlock).Text != "original" {
		t.Fatal("CloneResult aliases its embedded Call")
	}
	if cloned.Err != sentinel {
		t.Fatal("CloneResult cloned or replaced Err")
	}
	if !cloned.EndedAt.Equal(result.EndedAt) || cloned.Outcome != OutcomeFailed {
		t.Fatal("CloneResult did not preserve terminal scalars")
	}
}
