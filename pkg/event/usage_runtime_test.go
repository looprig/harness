package event_test

import (
	"bytes"
	"errors"
	"reflect"
	"testing"

	"github.com/looprig/core/content"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/identity"
	"github.com/looprig/inference"
)

func TestUsageBearingEventsRoundTrip(t *testing.T) {
	t.Parallel()
	sessionID, loopID, turnID, stepID := vID(t), vID(t), vID(t), vID(t)
	usage := content.Usage{InputTokens: 11, OutputTokens: 7, CacheReadTokens: 3, CacheCreationTokens: 2, ReasoningTokens: 5}
	ai := &content.AIMessage{
		Message: content.Message{Role: content.RoleAssistant, Blocks: []content.Block{&content.TextBlock{Text: "answer"}}},
		Usage:   &usage,
	}
	tests := []struct {
		name string
		ev   event.Event
	}{
		{
			name: "step done preserves AI usage through tagged messages",
			ev: event.StepDone{
				Header:   event.Header{Coordinates: identity.Coordinates{SessionID: sessionID, LoopID: loopID, TurnID: turnID, StepID: stepID}, EventID: vID(t)},
				Messages: content.AgenticMessages{ai, &content.ToolResultMessage{Message: content.Message{Role: content.RoleTool}, ToolUseID: "call-1"}},
			},
		},
		{
			name: "turn done preserves final AI and checked turn usage",
			ev: event.TurnDone{
				Header:  event.Header{Coordinates: identity.Coordinates{SessionID: sessionID, LoopID: loopID, TurnID: turnID}, EventID: vID(t)},
				Message: ai,
				Usage:   usage,
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			first, err := event.MarshalEvent(tt.ev)
			if err != nil {
				t.Fatalf("MarshalEvent() error = %v", err)
			}
			got, err := event.UnmarshalEvent(first)
			if err != nil {
				t.Fatalf("UnmarshalEvent() error = %v", err)
			}
			if !reflect.DeepEqual(got, tt.ev) {
				t.Errorf("round trip = %#v, want %#v", got, tt.ev)
			}
			second, err := event.MarshalEvent(got)
			if err != nil {
				t.Fatalf("MarshalEvent(round trip) error = %v", err)
			}
			if !bytes.Equal(second, first) {
				t.Errorf("persist/replay/persist changed bytes\nfirst:  %s\nsecond: %s", first, second)
			}
		})
	}
}

func TestModelRuntimeLifecycleRoundTrip(t *testing.T) {
	t.Parallel()
	sessionID, loopID := vID(t), vID(t)
	header := event.Header{Coordinates: identity.Coordinates{SessionID: sessionID, LoopID: loopID}, EventID: vID(t)}
	runtime := event.ModelRuntime{
		Key:    inference.ModelKey{Provider: "openrouter", Model: "anthropic/claude-sonnet-4"},
		Limits: inference.ContextLimits{WindowTokens: 200_000, MaxInputTokens: 180_000, MaxOutputTokens: 20_000},
		Effort: inference.EffortHigh,
	}
	tests := []struct {
		name string
		ev   event.Event
	}{
		{name: "loop started", ev: event.LoopStarted{Header: header, InitialMode: "plan", Runtime: runtime}},
		{name: "inference changed", ev: event.LoopInferenceChanged{Header: header, Runtime: runtime}},
		{name: "mode changed", ev: event.LoopModeChanged{Header: header, PreviousMode: "plan", Mode: "build", Runtime: runtime}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			data, err := event.MarshalEvent(tt.ev)
			if err != nil {
				t.Fatalf("MarshalEvent() error = %v", err)
			}
			got, err := event.UnmarshalEvent(data)
			if err != nil {
				t.Fatalf("UnmarshalEvent() error = %v", err)
			}
			if !reflect.DeepEqual(got, tt.ev) {
				t.Errorf("round trip = %#v, want %#v", got, tt.ev)
			}
		})
	}
}

func TestModelRuntimeValidation(t *testing.T) {
	t.Parallel()
	sessionID, loopID := vID(t), vID(t)
	header := event.Header{Coordinates: identity.Coordinates{SessionID: sessionID, LoopID: loopID}, EventID: vID(t)}
	valid := event.ModelRuntime{Key: inference.ModelKey{Provider: "provider", Model: "model"}, Limits: inference.ContextLimits{WindowTokens: 100}, Effort: inference.EffortLow}
	tests := []struct {
		name      string
		runtime   event.ModelRuntime
		wantField event.FieldName
	}{
		{name: "valid runtime", runtime: valid},
		{name: "missing provider", runtime: event.ModelRuntime{Key: inference.ModelKey{Model: "model"}}, wantField: event.FieldModelKey},
		{name: "missing model", runtime: event.ModelRuntime{Key: inference.ModelKey{Provider: "provider"}}, wantField: event.FieldModelKey},
		{name: "limit exceeds window", runtime: event.ModelRuntime{Key: valid.Key, Limits: inference.ContextLimits{WindowTokens: 10, MaxInputTokens: 11}}, wantField: event.FieldContextLimits},
		{name: "invalid effort", runtime: event.ModelRuntime{Key: valid.Key, Effort: inference.Effort("extreme")}, wantField: event.FieldEffort},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			events := []event.Event{
				event.LoopStarted{Header: header, Runtime: tt.runtime},
				event.LoopInferenceChanged{Header: header, Runtime: tt.runtime},
				event.LoopModeChanged{Header: header, Runtime: tt.runtime},
			}
			for _, ev := range events {
				err := event.ValidateEvent(ev)
				if tt.wantField == "" {
					if err != nil {
						t.Errorf("ValidateEvent(%T) error = %v", ev, err)
					}
					continue
				}
				var invalid *event.InvalidEventError
				if !errors.As(err, &invalid) {
					t.Errorf("ValidateEvent(%T) error = %T %v, want *InvalidEventError", ev, err, err)
					continue
				}
				if invalid.Field != tt.wantField || invalid.Rule != event.RuleInvalid {
					t.Errorf("ValidateEvent(%T) = %+v, want field=%s rule=%s", ev, invalid, tt.wantField, event.RuleInvalid)
				}
			}
		})
	}
}

func TestLifecycleDecodeAcceptsLegacyMissingRuntime(t *testing.T) {
	t.Parallel()
	sessionID, loopID, eventID := vID(t), vID(t), vID(t)
	prefix := `,"v":1,"session_id":"` + sessionID.String() + `","loop_id":"` + loopID.String() + `","event_id":"` + eventID.String() + `"}`
	tests := []struct {
		name     string
		typeName string
		check    func(event.Event) event.ModelRuntime
	}{
		{name: "loop started", typeName: "LoopStarted", check: func(ev event.Event) event.ModelRuntime { return ev.(event.LoopStarted).Runtime }},
		{name: "inference changed", typeName: "LoopInferenceChanged", check: func(ev event.Event) event.ModelRuntime { return ev.(event.LoopInferenceChanged).Runtime }},
		{name: "mode changed", typeName: "LoopModeChanged", check: func(ev event.Event) event.ModelRuntime { return ev.(event.LoopModeChanged).Runtime }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := event.UnmarshalEvent([]byte(`{"type":"` + tt.typeName + `"` + prefix))
			if err != nil {
				t.Fatalf("UnmarshalEvent() error = %v", err)
			}
			if runtime := tt.check(got); runtime != (event.ModelRuntime{}) {
				t.Errorf("legacy runtime = %+v, want zero fallback", runtime)
			}
		})
	}
}
