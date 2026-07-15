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

func TestLifecycleDecodeMigratesLegacyRuntime(t *testing.T) {
	t.Parallel()
	sessionID, loopID, eventID := vID(t), vID(t), vID(t)
	prefix := `,"v":1,"session_id":"` + sessionID.String() + `","loop_id":"` + loopID.String() + `","event_id":"` + eventID.String() + `"`
	legacyModel := `,"model":{"Provider":"openai","APIFormat":"responses","BaseURL":"https://api.openai.com","Name":"gpt-legacy","Origin":0,"Caps":{"AcceptsImages":false,"MaxContext":128000,"Tools":true,"Thinking":true},"Sampling":{}},"effort":"high"`
	wantMigrated := event.ModelRuntime{
		Key:    inference.ModelKey{Provider: "openai", Model: "gpt-legacy"},
		Limits: inference.ContextLimits{WindowTokens: 128_000},
		Effort: inference.EffortHigh,
	}
	tests := []struct {
		name        string
		wire        string
		wantRuntime event.ModelRuntime
		wantErr     bool
		wantLegacy  bool
	}{
		{
			name:        "old inference model and effort migrate to runtime",
			wire:        `{"type":"LoopInferenceChanged"` + prefix + legacyModel + `}`,
			wantRuntime: wantMigrated,
		},
		{
			name: "old loop started remains an explicit definition fallback",
			wire: `{"type":"LoopStarted"` + prefix + `}`,
		},
		{
			name: "old mode change remains an explicit selected-mode fallback",
			wire: `{"type":"LoopModeChanged"` + prefix + `,"previous_mode":"plan","mode":"build"}`,
		},
		{
			name:       "inference change missing both old model and current runtime is rejected",
			wire:       `{"type":"LoopInferenceChanged"` + prefix + `}`,
			wantErr:    true,
			wantLegacy: true,
		},
		{
			name:       "negative legacy max context is rejected without unsigned wrap",
			wire:       `{"type":"LoopInferenceChanged"` + prefix + `,"model":{"Provider":"legacy","Name":"model","Caps":{"MaxContext":-1}}}`,
			wantErr:    true,
			wantLegacy: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := event.UnmarshalEvent([]byte(tt.wire))
			if (err != nil) != tt.wantErr {
				t.Fatalf("UnmarshalEvent() error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				var legacy *event.LegacyRuntimeMigrationError
				if tt.wantLegacy && !errors.As(err, &legacy) {
					t.Errorf("UnmarshalEvent() error = %T %v, want *LegacyRuntimeMigrationError", err, err)
				}
				return
			}
			var runtime event.ModelRuntime
			switch e := got.(type) {
			case event.LoopStarted:
				runtime = e.Runtime
			case event.LoopInferenceChanged:
				runtime = e.Runtime
			case event.LoopModeChanged:
				runtime = e.Runtime
			default:
				t.Fatalf("UnmarshalEvent() = %T, want lifecycle event", got)
			}
			if runtime != tt.wantRuntime {
				t.Errorf("legacy runtime = %+v, want %+v", runtime, tt.wantRuntime)
			}
			if _, ok := got.(event.LoopInferenceChanged); ok {
				encoded, marshalErr := event.MarshalEvent(got)
				if marshalErr != nil {
					t.Fatalf("MarshalEvent(migrated) error = %v", marshalErr)
				}
				if bytes.Contains(encoded, []byte(`"model"`)) {
					t.Errorf("MarshalEvent(migrated) retained legacy model field: %s", encoded)
				}
			}
		})
	}
}
