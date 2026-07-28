package hook

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
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

func TestOutcomeValidity(t *testing.T) {
	t.Parallel()

	tests := []struct {
		outcome Outcome
		valid   bool
	}{
		{Outcome(0), false},
		{OutcomeCompleted, true},
		{OutcomeDenied, true},
		{OutcomeFailed, true},
		{OutcomeCanceled, true},
		{OutcomeCanceled + 1, false},
	}
	for _, test := range tests {
		if got := test.outcome.Valid(); got != test.valid {
			t.Errorf("Outcome(%d).Valid() = %v, want %v", test.outcome, got, test.valid)
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
		{name: "non-ASCII code", code: "policy.bloqué", reason: "reason"},
		{name: "invalid UTF-8 reason", code: "code", reason: string([]byte{0xff})},
		{name: "uppercase code", code: "Policy.blocked", reason: "reason"},
		{name: "numeric prefix code", code: "1policy", reason: "reason"},
		{name: "punctuation prefix code", code: ".policy", reason: "reason"},
		{name: "invalid code punctuation", code: "policy/blocked", reason: "reason"},
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
			var malformed *Denial
			if errors.As(err, &malformed) {
				t.Fatal("invalid denial was exposed as an intentional *Denial")
			}
		})
	}
}

func TestAsDenialRevalidatesConstructedValues(t *testing.T) {
	t.Parallel()

	validErr := fmt.Errorf(
		"wrapped guard result: %w",
		Deny("policy.blocked-1", "operation is not permitted — retry later"),
	)
	denial, ok := AsDenial(validErr)
	if !ok {
		t.Fatalf("AsDenial(Deny()) = (_, false), error type %T", validErr)
	}
	if denial.Code != "policy.blocked-1" || denial.Reason != "operation is not permitted — retry later" {
		t.Fatalf("AsDenial(Deny()) = %#v", denial)
	}
	if _, ok := AsDenial(Deny(
		"a"+strings.Repeat("z", 63),
		strings.Repeat("r", 1024),
	)); !ok {
		t.Fatal("AsDenial rejected maximum-size valid fields")
	}

	tests := []struct {
		name   string
		denial *Denial
	}{
		{name: "blank", denial: &Denial{}},
		{name: "uppercase code", denial: &Denial{Code: "Policy.blocked", Reason: "reason"}},
		{name: "numeric prefix", denial: &Denial{Code: "1policy", Reason: "reason"}},
		{name: "invalid punctuation", denial: &Denial{Code: "policy/blocked", Reason: "reason"}},
		{name: "oversized code", denial: &Denial{Code: strings.Repeat("c", 65), Reason: "reason"}},
		{name: "invalid UTF-8 code", denial: &Denial{Code: string([]byte{0xff}), Reason: "reason"}},
		{name: "non-ASCII code", denial: &Denial{Code: "policy.bloqué", Reason: "reason"}},
		{name: "control in code", denial: &Denial{Code: "policy\x00blocked", Reason: "reason"}},
		{name: "blank reason", denial: &Denial{Code: "policy.blocked", Reason: " \t"}},
		{name: "oversized reason", denial: &Denial{Code: "policy.blocked", Reason: strings.Repeat("r", 1025)}},
		{name: "invalid UTF-8 reason", denial: &Denial{Code: "policy.blocked", Reason: string([]byte{0xff})}},
		{name: "control in reason", denial: &Denial{Code: "policy.blocked", Reason: "bad\nreason"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			if got, ok := AsDenial(test.denial); ok || got != nil {
				t.Fatalf("AsDenial(%#v) = (%#v, %v), want (nil, false)", test.denial, got, ok)
			}
		})
	}

	if got, ok := AsDenial(errors.New("ordinary")); ok || got != nil {
		t.Fatalf("AsDenial(ordinary) = (%#v, %v), want (nil, false)", got, ok)
	}
}

func TestAsDenialReturnsIndependentCopy(t *testing.T) {
	t.Parallel()

	original := &Denial{Code: "policy.blocked", Reason: "original reason"}
	classified, ok := AsDenial(original)
	if !ok {
		t.Fatal("AsDenial(valid direct denial) = (_, false)")
	}

	original.Code = "policy.changed"
	original.Reason = "changed reason"
	if classified.Code != "policy.blocked" || classified.Reason != "original reason" {
		t.Fatalf("classified denial aliases caller-owned value: %#v", classified)
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
	topP := 0.75
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
			TopP:        &topP,
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
	*clonedInference.Request.Model.Sampling.TopP = 0.1
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

	if temperature != 0.25 || topP != 0.75 || maxTokens != 64 || request.Model.Sampling.Stop[0] != "END" {
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

	emptyGateAnswer := CloneCall(Call{
		Operation: OperationGateWait,
		GateWait:  &GateWaitData{Answer: &gate.Answer{Values: map[string]string{}}},
	})
	if emptyGateAnswer.GateWait.Answer.Values == nil {
		t.Fatal("CloneCall converted non-nil empty gate answer map to nil")
	}

	emptyNestedBlocks := CloneCall(Call{
		Operation: OperationToolExecution,
		ToolExecution: &ToolExecutionData{Result: &tool.ToolResult{
			Content: []content.Block{&content.ToolResultBlock{Content: []content.Block{}}},
		}},
	})
	nested := emptyNestedBlocks.ToolExecution.Result.Content[0].(*content.ToolResultBlock)
	if nested.Content == nil {
		t.Fatal("CloneCall converted non-nil empty nested block slice to nil")
	}
}

func TestCloneConversationVariants(t *testing.T) {
	t.Parallel()

	messages := content.AgenticMessages{
		&content.UserMessage{Message: content.Message{
			Role: content.RoleUser, Blocks: []content.Block{&content.TextBlock{Text: "user"}},
		}},
		&content.AIMessage{
			Message: content.Message{
				Role: content.RoleAssistant, Blocks: []content.Block{&content.TextBlock{Text: "assistant"}},
			},
			Usage: &content.Usage{InputTokens: 1},
		},
		&content.SystemMessage{Message: content.Message{
			Role: content.RoleSystem, Blocks: []content.Block{&content.TextBlock{Text: "system"}},
		}},
		&content.ToolResultMessage{
			Message: content.Message{
				Role: content.RoleTool, Blocks: []content.Block{&content.TextBlock{Text: "tool"}},
			},
			ToolUseID: "call", IsError: true,
		},
	}

	cloned := cloneMessages(messages)
	for index := range messages {
		if reflect.ValueOf(messages[index]).Pointer() == reflect.ValueOf(cloned[index]).Pointer() {
			t.Errorf("conversation %d retained its source pointer", index)
		}
		cloneConversationBlocks(cloned[index])[0].(*content.TextBlock).Text = "changed"
		if got := cloneConversationBlocks(messages[index])[0].(*content.TextBlock).Text; got == "changed" {
			t.Errorf("conversation %d aliases blocks", index)
		}
	}
	cloned[1].(*content.AIMessage).Usage.InputTokens = 99
	if messages[1].(*content.AIMessage).Usage.InputTokens != 1 {
		t.Fatal("AI conversation clone aliases usage")
	}
}

func TestCloneBlockVariants(t *testing.T) {
	t.Parallel()

	blocks := []content.Block{
		&content.TextBlock{Text: "text"},
		&content.ImageBlock{
			MediaType: "image/png", Source: content.ImageSource{URL: "https://example.invalid", Data: []byte{1}},
		},
		&content.AudioBlock{MediaType: "audio/wav", Data: []byte{2}},
		&content.DocumentBlock{MediaType: "text/plain", Name: "doc", Data: []byte{3}, Text: "document"},
		&content.ThinkingBlock{Thinking: "thought", Signature: "signature"},
		&content.ToolUseBlock{ID: "call", Name: "tool", Input: json.RawMessage(`{"x":1}`)},
		&content.ToolResultBlock{
			ToolUseID: "call", Content: []content.Block{&content.TextBlock{Text: "result"}}, IsError: true,
		},
	}

	cloned := cloneBlocks(blocks)
	for index := range blocks {
		if reflect.ValueOf(blocks[index]).Pointer() == reflect.ValueOf(cloned[index]).Pointer() {
			t.Errorf("block %d retained its source pointer", index)
		}
	}
	cloned[1].(*content.ImageBlock).Source.Data[0] = 9
	cloned[2].(*content.AudioBlock).Data[0] = 9
	cloned[3].(*content.DocumentBlock).Data[0] = 9
	cloned[5].(*content.ToolUseBlock).Input[0] = '['
	cloned[6].(*content.ToolResultBlock).Content[0].(*content.TextBlock).Text = "changed"
	if blocks[1].(*content.ImageBlock).Source.Data[0] != 1 ||
		blocks[2].(*content.AudioBlock).Data[0] != 2 ||
		blocks[3].(*content.DocumentBlock).Data[0] != 3 ||
		string(blocks[5].(*content.ToolUseBlock).Input) != `{"x":1}` ||
		blocks[6].(*content.ToolResultBlock).Content[0].(*content.TextBlock).Text != "result" {
		t.Fatal("block clone aliases reference-backed data")
	}
}

func TestClonePreservesTypedNilVariants(t *testing.T) {
	t.Parallel()

	conversations := []content.Conversation{
		(*content.UserMessage)(nil),
		(*content.AIMessage)(nil),
		(*content.SystemMessage)(nil),
		(*content.ToolResultMessage)(nil),
	}
	for index, conversation := range conversations {
		cloned := cloneConversation(conversation)
		if reflect.TypeOf(cloned) != reflect.TypeOf(conversation) || !reflect.ValueOf(cloned).IsNil() {
			t.Errorf("typed-nil conversation %d cloned as %#v", index, cloned)
		}
	}

	blocks := []content.Block{
		(*content.TextBlock)(nil),
		(*content.ImageBlock)(nil),
		(*content.AudioBlock)(nil),
		(*content.DocumentBlock)(nil),
		(*content.ThinkingBlock)(nil),
		(*content.ToolUseBlock)(nil),
		(*content.ToolResultBlock)(nil),
	}
	for index, block := range blocks {
		cloned := cloneBlock(block)
		if reflect.TypeOf(cloned) != reflect.TypeOf(block) || !reflect.ValueOf(cloned).IsNil() {
			t.Errorf("typed-nil block %d cloned as %#v", index, cloned)
		}
	}
}

func TestCloneRejectsUnknownSealedVariants(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		kind CloneErrorKind
		run  func()
	}{
		{name: "conversation", kind: CloneUnknownConversation, run: func() { cloneConversation(nil) }},
		{name: "block", kind: CloneUnknownBlock, run: func() { cloneBlock(nil) }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			defer func() {
				recovered := recover()
				cloneErr, ok := recovered.(*CloneError)
				if !ok {
					t.Fatalf("panic = %T(%v), want *CloneError", recovered, recovered)
				}
				if cloneErr.Kind != test.kind {
					t.Fatalf("CloneError.Kind = %q, want %q", cloneErr.Kind, test.kind)
				}
			}()
			test.run()
			t.Fatal("clone returned silently for an unknown sealed variant")
		})
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

func cloneConversationBlocks(message content.Conversation) []content.Block {
	switch typed := message.(type) {
	case *content.UserMessage:
		return typed.Blocks
	case *content.AIMessage:
		return typed.Blocks
	case *content.SystemMessage:
		return typed.Blocks
	case *content.ToolResultMessage:
		return typed.Blocks
	default:
		panic(fmt.Sprintf("unexpected test conversation %T", message))
	}
}
