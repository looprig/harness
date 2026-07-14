package sessionruntime

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/looprig/core/content"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/internal/hustleruntime"
	"github.com/looprig/harness/internal/loopruntime"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/hustle"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/inference"
)

const validCompactionXML = `<conversation_summary><goal>ship &amp; verify</goal><constraints></constraints><decisions></decisions><state>ready</state><open_items></open_items></conversation_summary>`

func TestCompactionInputWireStrictRoundTrip(t *testing.T) {
	t.Parallel()
	tests := []struct{ name string }{{name: "canonical v1 round trip"}}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			input := validCompactionInput(t)
			raw, err := marshalCompactionInput(input)
			if err != nil {
				t.Fatalf("marshalCompactionInput() error = %v", err)
			}
			if strings.Contains(string(raw), strings.ToUpper(hex.EncodeToString(input.RequestFingerprint[:]))) {
				t.Fatalf("wire fingerprint is not canonical lowercase: %s", raw)
			}
			decoded, err := unmarshalCompactionInput(raw)
			if err != nil {
				t.Fatalf("unmarshalCompactionInput() error = %v", err)
			}
			if decoded.Basis != input.Basis || decoded.Model != input.Model || decoded.RequestFingerprint != input.RequestFingerprint || decoded.MaxSummaryTokens != input.MaxSummaryTokens {
				t.Fatalf("decoded identity = %+v, want %+v", decoded, input)
			}
			if len(decoded.Transcript) != len(input.Transcript) {
				t.Fatalf("decoded transcript length = %d, want %d", len(decoded.Transcript), len(input.Transcript))
			}
		})
	}
}

func TestCompactionInputWireRejectsMalformedValues(t *testing.T) {
	t.Parallel()
	valid, err := marshalCompactionInput(validCompactionInput(t))
	if err != nil {
		t.Fatal(err)
	}
	input := validCompactionInput(t)
	upper := strings.ToUpper(hex.EncodeToString(input.RequestFingerprint[:]))
	tests := []struct {
		name string
		raw  string
	}{
		{name: "empty", raw: ``},
		{name: "unknown field", raw: replaceCompactionJSON(t, valid, "extra", json.RawMessage(`true`))},
		{name: "missing version", raw: removeCompactionJSON(t, valid, "version")},
		{name: "wrong version", raw: replaceCompactionJSON(t, valid, "version", json.RawMessage(`2`))},
		{name: "version wrong type", raw: replaceCompactionJSON(t, valid, "version", json.RawMessage(`"1"`))},
		{name: "missing basis", raw: removeCompactionJSON(t, valid, "basis")},
		{name: "missing model", raw: removeCompactionJSON(t, valid, "model")},
		{name: "model unknown field", raw: replaceCompactionJSON(t, valid, "model", json.RawMessage(`{"provider":"p","model":"m","extra":true}`))},
		{name: "missing fingerprint", raw: removeCompactionJSON(t, valid, "request_fingerprint")},
		{name: "uppercase fingerprint", raw: replaceCompactionJSON(t, valid, "request_fingerprint", json.RawMessage(`"`+upper+`"`))},
		{name: "short fingerprint", raw: replaceCompactionJSON(t, valid, "request_fingerprint", json.RawMessage(`"00"`))},
		{name: "non hex fingerprint", raw: replaceCompactionJSON(t, valid, "request_fingerprint", json.RawMessage(`"`+strings.Repeat("z", 64)+`"`))},
		{name: "missing transcript", raw: removeCompactionJSON(t, valid, "transcript")},
		{name: "null transcript", raw: replaceCompactionJSON(t, valid, "transcript", json.RawMessage(`null`))},
		{name: "empty transcript", raw: replaceCompactionJSON(t, valid, "transcript", json.RawMessage(`[]`))},
		{name: "transcript unknown message field", raw: replaceCompactionJSON(t, valid, "transcript", json.RawMessage(`[{"role":"user","blocks":[],"extra":true}]`))},
		{name: "transcript unknown block field", raw: replaceCompactionJSON(t, valid, "transcript", json.RawMessage(`[{"role":"user","blocks":[{"type":"text","Text":"hello","extra":true}]}]`))},
		{name: "missing budget", raw: removeCompactionJSON(t, valid, "max_summary_tokens")},
		{name: "zero budget", raw: replaceCompactionJSON(t, valid, "max_summary_tokens", json.RawMessage(`0`))},
		{name: "trailing value", raw: string(valid) + `{}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := unmarshalCompactionInput([]byte(tt.raw)); err == nil {
				t.Fatal("unmarshalCompactionInput() error = nil")
			}
		})
	}
}

func TestCompactionTranscriptWireRoundTrip(t *testing.T) {
	t.Parallel()
	tests := []struct{ name string }{{name: "all messages and blocks use strict lower snake wire"}}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			input := validCompactionInput(t)
			blocks := []content.Block{
				&content.TextBlock{Text: "text"},
				&content.ImageBlock{MediaType: content.MediaTypeImagePNG, Source: content.ImageSource{URL: "https://example.test/image.png", Data: []byte{1}}},
				&content.AudioBlock{MediaType: content.MediaTypeAudioWAV, Data: []byte{2}},
				&content.DocumentBlock{MediaType: content.MediaTypeDocumentText, Name: "notes", Data: []byte{3}, Text: "document"},
				&content.ThinkingBlock{Thinking: "thought", Signature: "signature"},
				&content.ToolUseBlock{ID: "call", Name: "tool", Input: json.RawMessage(`{"value":1}`)},
				&content.ToolResultBlock{ToolUseID: "call", Content: []content.Block{&content.TextBlock{Text: "result"}}, IsError: true},
			}
			input.Transcript = content.AgenticMessages{
				&content.SystemMessage{Message: content.Message{Role: content.RoleSystem, Blocks: blocks}},
				&content.AIMessage{Message: content.Message{Role: content.RoleAssistant, Blocks: blocks}, Usage: &content.Usage{InputTokens: 8, OutputTokens: 5, ReasoningTokens: 2}},
				&content.ToolResultMessage{Message: content.Message{Role: content.RoleTool, Blocks: blocks}, ToolUseID: "call", IsError: true},
				&content.UserMessage{Message: content.Message{Role: content.RoleUser, Blocks: blocks}},
			}
			raw, err := marshalCompactionInput(input)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(raw), `"Text"`) || !strings.Contains(string(raw), `"tool_use_id"`) || !strings.Contains(string(raw), `"input_tokens"`) {
				t.Fatalf("transcript wire is not lower snake: %s", raw)
			}
			decoded, err := unmarshalCompactionInput(raw)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(decoded.Transcript, input.Transcript) {
				t.Fatalf("decoded transcript = %#v, want %#v", decoded.Transcript, input.Transcript)
			}
		})
	}
}

func TestValidateCompactionResult(t *testing.T) {
	t.Parallel()
	input := validCompactionInput(t)
	valid := validCompactionOutputJSON(t, input, validCompactionXML)
	other := input
	other.Model.Model = "drifted"
	tests := []struct {
		name       string
		result     hustle.Result
		limit      int
		wantReason loop.InvalidSummaryReason
	}{
		{name: "valid", result: hustle.Result{Output: valid, Usage: &content.Usage{OutputTokens: input.MaxSummaryTokens}}, limit: len(valid)},
		{name: "nil usage", result: hustle.Result{Output: valid}, limit: len(valid), wantReason: loop.InvalidSummaryTokenUsage},
		{name: "zero usage", result: hustle.Result{Output: valid, Usage: &content.Usage{}}, limit: len(valid), wantReason: loop.InvalidSummaryTokenUsage},
		{name: "over token budget", result: hustle.Result{Output: valid, Usage: &content.Usage{OutputTokens: input.MaxSummaryTokens + 1}}, limit: len(valid), wantReason: loop.InvalidSummaryTokenLimit},
		{name: "over byte limit", result: hustle.Result{Output: valid, Usage: &content.Usage{OutputTokens: 1}}, limit: len(valid) - 1, wantReason: loop.InvalidSummaryByteLimit},
		{name: "invalid json", result: hustle.Result{Output: json.RawMessage(`{`), Usage: &content.Usage{OutputTokens: 1}}, limit: len(valid), wantReason: loop.InvalidSummaryWire},
		{name: "identity drift", result: hustle.Result{Output: validCompactionOutputJSON(t, other, validCompactionXML), Usage: &content.Usage{OutputTokens: 1}}, limit: len(valid) + 100, wantReason: loop.InvalidSummaryIdentity},
		{name: "invalid xml", result: hustle.Result{Output: validCompactionOutputJSON(t, input, `<wrong/>`), Usage: &content.Usage{OutputTokens: 1}}, limit: len(valid) + 100, wantReason: loop.InvalidSummaryXMLRoot},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := validateCompactionResult(tt.result, input, tt.limit)
			if tt.wantReason == "" {
				if err != nil || got == nil {
					t.Fatalf("validateCompactionResult() = %v, %v", got, err)
				}
				return
			}
			var invalid *loop.InvalidSummaryError
			if !errors.As(err, &invalid) || invalid.Reason != tt.wantReason {
				t.Fatalf("error = %T %v, want reason %q", err, err, tt.wantReason)
			}
			if got != nil {
				t.Fatalf("output = %+v, want nil", got)
			}
		})
	}
}

func TestCompactionOutputWireRejectsMalformedValues(t *testing.T) {
	t.Parallel()
	input := validCompactionInput(t)
	valid := validCompactionOutputJSON(t, input, validCompactionXML)
	upper := strings.ToUpper(hex.EncodeToString(input.RequestFingerprint[:]))
	tests := []struct {
		name       string
		raw        string
		wantReason loop.InvalidSummaryReason
	}{
		{name: "unknown field", raw: replaceCompactionJSON(t, valid, "extra", json.RawMessage(`true`)), wantReason: loop.InvalidSummaryWire},
		{name: "missing version", raw: removeCompactionJSON(t, valid, "version"), wantReason: loop.InvalidSummaryWire},
		{name: "wrong version", raw: replaceCompactionJSON(t, valid, "version", json.RawMessage(`2`)), wantReason: loop.InvalidSummaryWire},
		{name: "version wrong type", raw: replaceCompactionJSON(t, valid, "version", json.RawMessage(`"1"`)), wantReason: loop.InvalidSummaryWire},
		{name: "missing basis", raw: removeCompactionJSON(t, valid, "basis"), wantReason: loop.InvalidSummaryWire},
		{name: "basis unknown field", raw: replaceCompactionJSON(t, valid, "basis", json.RawMessage(`{"revision":3,"through_event_id":"00000000-0000-4000-8000-000000000001","extra":true}`)), wantReason: loop.InvalidSummaryWire},
		{name: "missing model", raw: removeCompactionJSON(t, valid, "model"), wantReason: loop.InvalidSummaryWire},
		{name: "model unknown field", raw: replaceCompactionJSON(t, valid, "model", json.RawMessage(`{"provider":"provider","model":"model","extra":true}`)), wantReason: loop.InvalidSummaryWire},
		{name: "missing fingerprint", raw: removeCompactionJSON(t, valid, "request_fingerprint"), wantReason: loop.InvalidSummaryWire},
		{name: "uppercase fingerprint", raw: replaceCompactionJSON(t, valid, "request_fingerprint", json.RawMessage(`"`+upper+`"`)), wantReason: loop.InvalidSummaryWire},
		{name: "short fingerprint", raw: replaceCompactionJSON(t, valid, "request_fingerprint", json.RawMessage(`"00"`)), wantReason: loop.InvalidSummaryWire},
		{name: "missing summary", raw: removeCompactionJSON(t, valid, "summary"), wantReason: loop.InvalidSummaryWire},
		{name: "null summary", raw: replaceCompactionJSON(t, valid, "summary", json.RawMessage(`null`)), wantReason: loop.InvalidSummaryWire},
		{name: "summary wrong type", raw: replaceCompactionJSON(t, valid, "summary", json.RawMessage(`42`)), wantReason: loop.InvalidSummaryWire},
		{name: "empty summary", raw: replaceCompactionJSON(t, valid, "summary", json.RawMessage(`""`)), wantReason: loop.InvalidSummaryXMLRoot},
		{name: "trailing value", raw: string(valid) + `{}`, wantReason: loop.InvalidSummaryWire},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := validateCompactionResult(hustle.Result{Output: json.RawMessage(tt.raw), Usage: &content.Usage{OutputTokens: 1}}, input, len(tt.raw)+1)
			var invalid *loop.InvalidSummaryError
			if !errors.As(err, &invalid) || invalid.Reason != tt.wantReason {
				t.Fatalf("error = %T %v, want reason %q", err, err, tt.wantReason)
			}
		})
	}
}

func TestNewCompactionAdapterRejectsInvalidConfiguration(t *testing.T) {
	t.Parallel()
	loopID, _ := uuid.New()
	runner := &compactionRunnerStub{}
	var typedNil *compactionRunnerStub
	tests := []struct {
		name   string
		runner compactionHustleRunner
		mutate func(*hustle.DefinitionDescriptor)
		loopID uuid.UUID
		field  compactionAdapterField
	}{
		{name: "nil runner", loopID: loopID, field: compactionAdapterFieldRunner},
		{name: "typed nil runner", runner: typedNil, loopID: loopID, field: compactionAdapterFieldRunner},
		{name: "invalid descriptor", runner: runner, mutate: func(value *hustle.DefinitionDescriptor) { value.Name = "" }, loopID: loopID, field: compactionAdapterFieldDescriptor},
		{name: "named inference descriptor", runner: runner, mutate: func(value *hustle.DefinitionDescriptor) { value.ModelSource = hustle.ModelSourceNamed }, loopID: loopID, field: compactionAdapterFieldDescriptor},
		{name: "zero loop", runner: runner, field: compactionAdapterFieldLoopID},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			descriptor := testHustleDefinition(t, "conversation.compact").Descriptor()
			if tt.mutate != nil {
				tt.mutate(&descriptor)
			}
			_, err := newCompactionAdapter(tt.runner, descriptor, tt.loopID)
			var invalid *compactionAdapterError
			if !errors.As(err, &invalid) || invalid.Field != tt.field {
				t.Fatalf("error = %T %v, want field %q", err, err, tt.field)
			}
		})
	}
}

func TestCompactionAdapterValidatesBeforeFinalization(t *testing.T) {
	t.Parallel()
	input := validCompactionInput(t)
	loopID, _ := uuid.New()
	tests := []struct {
		name       string
		result     hustle.Result
		runtimeErr error
		wantReason loop.InvalidSummaryReason
	}{
		{name: "valid", result: hustle.Result{Output: validCompactionOutputJSON(t, input, validCompactionXML), Usage: &content.Usage{OutputTokens: 2}}},
		{name: "adapter validation failure", result: hustle.Result{Output: validCompactionOutputJSON(t, input, `<wrong/>`), Usage: &content.Usage{OutputTokens: 2}}, wantReason: loop.InvalidSummaryXMLRoot},
		{name: "generic invalid shape", runtimeErr: &hustleruntime.OutputError{Reason: hustleruntime.OutputFailureInvalidShape}, wantReason: loop.InvalidSummaryOutputShape},
		{name: "generic too large", runtimeErr: &hustleruntime.OutputError{Reason: hustleruntime.OutputFailureTooLarge}, wantReason: loop.InvalidSummaryByteLimit},
		{name: "generic invalid json", runtimeErr: &hustleruntime.OutputError{Reason: hustleruntime.OutputFailureInvalidJSON}, wantReason: loop.InvalidSummaryWire},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			runner := &compactionRunnerStub{result: tt.result, runtimeErr: tt.runtimeErr}
			adapter, err := newCompactionAdapter(runner, testHustleDefinition(t, "conversation.compact").Descriptor(), loopID)
			if err != nil {
				t.Fatal(err)
			}
			var outcome loopruntime.CompactionOutcome
			err = adapter.CompactAndFinalize(context.Background(), input, func(_ context.Context, got loopruntime.CompactionOutcome) error {
				outcome = got
				return nil
			})
			if err != nil {
				t.Fatalf("CompactAndFinalize() error = %v", err)
			}
			if !runner.validatorBeforeFinalizer {
				t.Fatal("runner finalized before validation")
			}
			if runner.request.Cause.LoopID != loopID || runner.request.Name != "conversation.compact" {
				t.Fatalf("request identity = %+v", runner.request)
			}
			if tt.wantReason == "" {
				if outcome.Value == nil || outcome.Err != nil {
					t.Fatalf("outcome = %+v", outcome)
				}
				return
			}
			var invalid *loop.InvalidSummaryError
			if outcome.Value != nil || !errors.As(outcome.Err, &invalid) || invalid.Reason != tt.wantReason {
				t.Fatalf("outcome = %+v, want reason %q", outcome, tt.wantReason)
			}
		})
	}
}

func TestCompactionAdapterStoresOnlyFocusedCapability(t *testing.T) {
	t.Parallel()
	typ := reflect.TypeOf(compactionAdapter{})
	tests := []struct{ name string }{{name: "no session or shutdown capability"}}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for i := 0; i < typ.NumField(); i++ {
				field := typ.Field(i)
				if field.Type == reflect.TypeOf((*Session)(nil)) || strings.Contains(strings.ToLower(field.Name), "shutdown") {
					t.Fatalf("adapter field %s exposes session control", field.Name)
				}
			}
		})
	}
}

type compactionRunnerStub struct {
	request                  hustle.Request
	result                   hustle.Result
	runtimeErr               error
	validatorBeforeFinalizer bool
}

func (s *compactionRunnerStub) RunAndFinalize(ctx context.Context, request hustle.Request, validate hustleruntime.ValidateResult, finalizer hustleruntime.Finalizer) error {
	s.request = request
	var outcome hustle.Outcome
	if s.runtimeErr != nil {
		outcome.Err = s.runtimeErr
	} else if err := validate(ctx, s.result); err != nil {
		outcome.Err = &hustleruntime.OutputError{Cause: err}
	} else {
		outcome.Result = &s.result
	}
	s.validatorBeforeFinalizer = true
	return finalizer(ctx, outcome)
}

func validCompactionInput(t *testing.T) loop.CompactionInput {
	t.Helper()
	eventID, err := uuid.New()
	if err != nil {
		t.Fatal(err)
	}
	var fingerprint [32]byte
	for index := range fingerprint {
		fingerprint[index] = byte(index + 1)
	}
	return loop.CompactionInput{
		Basis:              event.ContextBasis{Revision: 3, ThroughEventID: eventID},
		Model:              inference.ModelKey{Provider: "provider", Model: "model"},
		RequestFingerprint: fingerprint,
		Transcript: content.AgenticMessages{&content.UserMessage{Message: content.Message{
			Role: content.RoleUser, Blocks: []content.Block{&content.TextBlock{Text: "hello"}},
		}}},
		MaxSummaryTokens: 32,
	}
}

func validCompactionOutputJSON(t *testing.T, input loop.CompactionInput, summary string) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(map[string]objectValue{
		"version":             {value: loop.CompactionWireV1},
		"basis":               {value: input.Basis},
		"model":               {value: map[string]string{"provider": string(input.Model.Provider), "model": input.Model.Model}},
		"request_fingerprint": {value: hex.EncodeToString(input.RequestFingerprint[:])},
		"summary":             {value: summary},
	})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// objectValue prevents accidental use of map[string]any outside this test-only
// serialization boundary while still exercising unknown-field order independence.
type objectValue struct{ value interface{} }

func (v objectValue) MarshalJSON() ([]byte, error) { return json.Marshal(v.value) }

func replaceCompactionJSON(t *testing.T, raw []byte, field string, replacement json.RawMessage) string {
	t.Helper()
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil {
		t.Fatal(err)
	}
	object[field] = replacement
	result, err := json.Marshal(object)
	if err != nil {
		t.Fatal(err)
	}
	return string(result)
}

func removeCompactionJSON(t *testing.T, raw []byte, field string) string {
	t.Helper()
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil {
		t.Fatal(err)
	}
	delete(object, field)
	result, err := json.Marshal(object)
	if err != nil {
		t.Fatal(err)
	}
	return string(result)
}
