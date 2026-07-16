package loop

import (
	"errors"
	"testing"
	"time"

	"github.com/looprig/core/content"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/event"
	contextcount "github.com/looprig/inference/contextcount"
	model "github.com/looprig/inference/model"
)

func TestContextObservationPolicyValidation(t *testing.T) {
	t.Parallel()
	exact := exactCounterCapability()
	heuristic := heuristicCounterCapability()
	valid := ContextObservationPolicy{ReservedOutput: 32, CountTimeout: 37*time.Millisecond + time.Nanosecond}
	tests := []struct {
		name       string
		policy     ContextObservationPolicy
		capability contextcount.CounterCapability
		wantField  ContextObservationPolicyField
	}{
		{name: "exact counter", policy: valid, capability: exact},
		{name: "heuristic with margin", policy: ContextObservationPolicy{ReservedOutput: 32, SafetyMargin: 8, CountTimeout: valid.CountTimeout}, capability: heuristic},
		{name: "zero reservation", policy: ContextObservationPolicy{CountTimeout: valid.CountTimeout}, capability: exact, wantField: ContextObservationFieldReservedOutput},
		{name: "zero timeout", policy: ContextObservationPolicy{ReservedOutput: 32}, capability: exact, wantField: ContextObservationFieldCountTimeout},
		{name: "negative timeout", policy: ContextObservationPolicy{ReservedOutput: 32, CountTimeout: -time.Nanosecond}, capability: exact, wantField: ContextObservationFieldCountTimeout},
		{name: "heuristic without margin", policy: valid, capability: heuristic, wantField: ContextObservationFieldSafetyMargin},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := tt.policy.Validate(tt.capability)
			if tt.wantField == "" {
				if err != nil {
					t.Fatalf("Validate() error = %v", err)
				}
				return
			}
			var target *ContextObservationPolicyError
			if !errors.As(err, &target) || target.Field != tt.wantField {
				t.Fatalf("Validate() error = %T %v, want field %q", err, err, tt.wantField)
			}
		})
	}
}

func TestDefinitionContextPolicyOwnership(t *testing.T) {
	t.Parallel()
	counter := &policyCounter{capability: exactCounterCapability()}
	base := []Option{WithName("agent"), WithInference(&fakeLLM{}, testModel()), WithContextCounter(counter), WithInferenceCapability(localInferenceCapability())}
	observation := ContextObservationPolicy{ReservedOutput: 32, CountTimeout: 37*time.Millisecond + time.Nanosecond}
	tests := []struct {
		name string
		opts []Option
		kind DefinitionErrorKind
	}{
		{name: "observation owns admission", opts: append(append([]Option(nil), base...), WithContextObservation(observation))},
		{name: "counter requires policy", opts: append([]Option(nil), base...), kind: DefinitionMissingContextPolicy},
		{name: "observation requires counter", opts: []Option{WithName("agent"), WithInference(&fakeLLM{}, testModel()), WithContextObservation(observation)}, kind: DefinitionMissingContextCounter},
		{name: "observation and compaction conflict", opts: append(append(append([]Option(nil), base...), WithContextObservation(observation)), WithCompaction(manualCompactionPolicy())), kind: DefinitionConflictingContextPolicy},
		{name: "duplicate observation uses canonical duplicate kind", opts: append(append(append([]Option(nil), base...), WithContextObservation(observation)), WithContextObservation(observation)), kind: DefinitionDuplicateOption},
		{name: "invalid observation", opts: append(append([]Option(nil), base...), WithContextObservation(ContextObservationPolicy{})), kind: DefinitionInvalidContextObservation},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			definition, err := Define(tt.opts...)
			if tt.kind == "" {
				if err != nil {
					t.Fatalf("Define() error = %v", err)
				}
				got, ok := definition.ContextObservationPolicy()
				if !ok || got != observation {
					t.Fatalf("ContextObservationPolicy() = %+v, %v; want %+v, true", got, ok, observation)
				}
				return
			}
			var target *DefinitionError
			if !errors.As(err, &target) || target.Kind != tt.kind {
				t.Fatalf("Define() error = %T %v, want kind %q", err, err, tt.kind)
			}
		})
	}
}

func TestContextObservationPolicyFingerprint(t *testing.T) {
	t.Parallel()
	counter := &policyCounter{capability: exactCounterCapability()}
	define := func(policy ContextObservationPolicy) Definition {
		value, err := Define(WithName("agent"), WithInference(&fakeLLM{}, testModel()), WithContextCounter(counter), WithInferenceCapability(localInferenceCapability()), WithContextObservation(policy))
		if err != nil {
			t.Fatalf("Define() error = %v", err)
		}
		return value
	}
	base := ContextObservationPolicy{ReservedOutput: 32, CountTimeout: 37*time.Millisecond + time.Nanosecond}
	tests := []struct {
		name   string
		mutate func(*ContextObservationPolicy)
	}{
		{name: "reservation", mutate: func(value *ContextObservationPolicy) { value.ReservedOutput++ }},
		{name: "margin", mutate: func(value *ContextObservationPolicy) { value.SafetyMargin++ }},
		{name: "timeout", mutate: func(value *ContextObservationPolicy) { value.CountTimeout++ }},
	}
	want := define(base).PolicyRevision()
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			changed := base
			tt.mutate(&changed)
			if got := define(changed).PolicyRevision(); got == want {
				t.Fatal("PolicyRevision() ignored context observation change")
			}
		})
	}
}

func TestContextLimitErrorCarriesMeasurement(t *testing.T) {
	t.Parallel()
	measurement := event.ContextMeasurement{
		Basis: event.ContextBasis{Revision: 1, ThroughEventID: uuid.UUID{1}}, Model: model.ModelKey{Provider: "test", Model: "small"},
		RequestFingerprint: [32]byte{1}, InputTokens: content.TokenCount(90), InputLimit: content.TokenCount(90), Quality: contextcount.CountQualityExactLocal,
	}
	tests := []struct {
		name        string
		measurement event.ContextMeasurement
	}{
		{name: "at limit", measurement: measurement},
		{name: "over limit", measurement: func() event.ContextMeasurement { value := measurement; value.InputTokens++; return value }()},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := &ContextLimitError{Measurement: tt.measurement}
			if err.Measurement != tt.measurement || err.Error() == "" {
				t.Fatalf("ContextLimitError = %+v, want measurement %+v and non-empty text", err, tt.measurement)
			}
		})
	}
}
