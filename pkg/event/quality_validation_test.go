package event

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/looprig/core/content"
	model "github.com/looprig/inference/model"
)

func TestStepDoneMessageGroupValidation(t *testing.T) {
	t.Parallel()

	var nilAI *content.AIMessage
	var nilTool *content.ToolResultMessage
	ai := aiMsg("answer")
	wrongRoleAI := aiMsg("answer")
	wrongRoleAI.Role = content.RoleUser
	toolResult := &content.ToolResultMessage{Message: content.Message{Role: content.RoleTool}, ToolUseID: "call-1"}
	wrongRoleTool := &content.ToolResultMessage{Message: content.Message{Role: content.RoleAssistant}, ToolUseID: "call-1"}
	system := &content.SystemMessage{Message: content.Message{Role: content.RoleSystem}}
	tests := []struct {
		name         string
		messages     content.AgenticMessages
		wireMessages string
		wantErr      bool
	}{
		{name: "nil group", messages: nil, wantErr: true},
		{name: "null group", messages: nil, wireMessages: "null", wantErr: true},
		{name: "empty group", messages: content.AgenticMessages{}, wantErr: true},
		{name: "nil first message", messages: content.AgenticMessages{nil}, wantErr: true},
		{name: "typed nil AI first", messages: content.AgenticMessages{nilAI}, wantErr: true},
		{name: "AI type with user role", messages: content.AgenticMessages{wrongRoleAI}, wantErr: true},
		{name: "tool first", messages: content.AgenticMessages{toolResult}, wantErr: true},
		{name: "user first", messages: content.AgenticMessages{userMsg("question")}, wantErr: true},
		{name: "second AI", messages: content.AgenticMessages{ai, aiMsg("again")}, wantErr: true},
		{name: "second user", messages: content.AgenticMessages{ai, userMsg("again")}, wantErr: true},
		{name: "second system", messages: content.AgenticMessages{ai, system}, wantErr: true},
		{name: "nil tool", messages: content.AgenticMessages{ai, nil}, wantErr: true},
		{name: "typed nil tool", messages: content.AgenticMessages{ai, nilTool}, wantErr: true},
		{name: "tool type with assistant role", messages: content.AgenticMessages{ai, wrongRoleTool}, wantErr: true},
		{name: "AI only", messages: content.AgenticMessages{ai}},
		{name: "AI followed by tools", messages: content.AgenticMessages{ai, toolResult, toolResult}},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ev := StepDone{Header: fullHeader(), Messages: tt.messages}

			assertStepDoneValidationResult(t, "ValidateEvent", ValidateEvent(ev), tt.wantErr)
			_, marshalErr := MarshalEvent(ev)
			assertStepDoneValidationResult(t, "MarshalEvent", marshalErr, tt.wantErr)
			decoded, decodeErr := UnmarshalEvent(stepDoneWireForTest(t, ev, tt.wireMessages))
			assertStepDoneValidationResult(t, "UnmarshalEvent", decodeErr, tt.wantErr)
			if !tt.wantErr {
				if _, ok := decoded.(StepDone); !ok {
					t.Fatalf("UnmarshalEvent event = %T, want StepDone", decoded)
				}
			}
		})
	}
}

func assertStepDoneValidationResult(t *testing.T, operation string, err error, wantErr bool) {
	t.Helper()
	if !wantErr {
		if err != nil {
			t.Fatalf("%s error = %v, want nil", operation, err)
		}
		return
	}
	var invalid *InvalidEventError
	if !errors.As(err, &invalid) {
		t.Fatalf("%s error = %T %v, want *InvalidEventError", operation, err, err)
	}
	if invalid.Event != "StepDone" || invalid.Field != FieldName("Messages") || invalid.Rule != RuleInvalid {
		t.Fatalf("%s error = %#v, want StepDone/Messages/invalid", operation, invalid)
	}
}

func stepDoneWireForTest(t *testing.T, ev StepDone, override string) []byte {
	t.Helper()
	var messages json.RawMessage
	if override != "" {
		messages = json.RawMessage(override)
	} else if ev.Messages != nil {
		encoded, err := marshalMessages(ev.Messages)
		if err != nil {
			t.Fatalf("marshalMessages: %v", err)
		}
		messages = encoded
	}
	payload, err := json.Marshal(stepDoneWire{Header: ev.Header, Messages: messages})
	if err != nil {
		t.Fatalf("json.Marshal(stepDoneWire): %v", err)
	}
	wire, err := mergeEnvelope("StepDone", payload)
	if err != nil {
		t.Fatalf("mergeEnvelope: %v", err)
	}
	return wire
}

func TestStepDoneUnknownRoleRemainsCodecError(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		messages    string
		wantUnknown bool
	}{
		{name: "valid assistant", messages: `[{"role":"assistant"}]`},
		{name: "unknown nonempty role", messages: `[{"role":"alien"}]`, wantUnknown: true},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			wire := stepDoneWireForTest(t, StepDone{Header: fullHeader()}, tt.messages)
			_, err := UnmarshalEvent(wire)
			if !tt.wantUnknown {
				if err != nil {
					t.Fatalf("UnmarshalEvent error = %v, want nil", err)
				}
				return
			}
			var unknown *UnknownMessageRoleError
			if !errors.As(err, &unknown) || unknown.Role != "alien" {
				t.Fatalf("UnmarshalEvent error = %T %v, want UnknownMessageRoleError for alien", err, err)
			}
			var invalid *InvalidEventError
			if errors.As(err, &invalid) {
				t.Fatalf("UnmarshalEvent error also classified as InvalidEventError: %#v", invalid)
			}
		})
	}
}

func TestMarshalEventValidatesDurableBodies(t *testing.T) {
	t.Parallel()

	invalidRuntime := sampleRuntime()
	invalidRuntime.Key.Provider = ""
	tests := []struct {
		name      string
		event     Event
		wantEvent EventName
		wantField FieldName
	}{
		{name: "LoopStarted runtime", event: LoopStarted{Header: fullHeaderLoop(), Runtime: invalidRuntime}, wantEvent: "LoopStarted", wantField: FieldModelKey},
		{name: "LoopModeChanged runtime", event: LoopModeChanged{Header: fullHeaderLoop(), Runtime: invalidRuntime}, wantEvent: "LoopModeChanged", wantField: FieldModelKey},
		{name: "LoopInferenceChanged runtime", event: LoopInferenceChanged{Header: fullHeaderLoop(), Runtime: invalidRuntime}, wantEvent: "LoopInferenceChanged", wantField: FieldModelKey},
		{name: "TurnDone usage", event: TurnDone{Header: fullHeaderTurn(), Usage: content.Usage{OutputTokens: 1, ReasoningTokens: 2}}, wantEvent: "TurnDone", wantField: FieldUsage},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := MarshalEvent(tt.event)
			var invalid *InvalidEventError
			if !errors.As(err, &invalid) {
				t.Fatalf("MarshalEvent error = %T %v, want *InvalidEventError", err, err)
			}
			if invalid.Event != tt.wantEvent || invalid.Field != tt.wantField || invalid.Rule != RuleInvalid {
				t.Fatalf("MarshalEvent error = %#v, want %s/%s/invalid", invalid, tt.wantEvent, tt.wantField)
			}
		})
	}
}

func TestMarshalEventRejectsOversizedOutput(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		ev   Event
	}{
		{
			name: "TurnDone message exceeds event cap",
			ev: TurnDone{Header: fullHeaderTurn(), Message: &content.AIMessage{Message: content.Message{
				Role:   content.RoleAssistant,
				Blocks: []content.Block{&content.TextBlock{Text: strings.Repeat("x", maxEventBytes)}},
			}}},
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			encoded, err := MarshalEvent(tt.ev)
			if encoded != nil {
				t.Fatalf("MarshalEvent returned %d bytes with an error, want nil output", len(encoded))
			}
			var limit *EventLimitError
			if !errors.As(err, &limit) {
				t.Fatalf("MarshalEvent error = %T %v, want *EventLimitError", err, err)
			}
			if limit.Got <= limit.Max || limit.Max != maxEventBytes {
				t.Fatalf("EventLimitError = %#v, want Got > Max == %d", limit, maxEventBytes)
			}
		})
	}
}

func TestModelRuntimeValidationBoundary(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		runtime ModelRuntime
		wantErr bool
	}{
		{name: "valid", runtime: sampleRuntime()},
		{name: "empty provider", runtime: ModelRuntime{Key: model.ModelKey{Model: "model"}}, wantErr: true},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := validateModelRuntime("LoopStarted", tt.runtime)
			if (err != nil) != tt.wantErr {
				t.Fatalf("validateModelRuntime error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}
