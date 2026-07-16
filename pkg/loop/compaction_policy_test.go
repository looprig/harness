package loop

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/inference"
	contextcount "github.com/looprig/inference/contextcount"
	model "github.com/looprig/inference/model"
)

type policyCounter struct {
	capability contextcount.CounterCapability
	countCalls int
}

func (c *policyCounter) CountContext(context.Context, inference.Request) (contextcount.ContextCount, error) {
	c.countCalls++
	panic("definition validation performed I/O")
}

func (c *policyCounter) CounterCapability() contextcount.CounterCapability { return c.capability }

func exactCounterCapability() contextcount.CounterCapability {
	return contextcount.CounterCapability{Transport: contextcount.CounterTransportLocal, Retention: contextcount.RetentionNone, TokenizerRev: "exact-v1", Quality: contextcount.CountQualityExactLocal}
}

func heuristicCounterCapability() contextcount.CounterCapability {
	return contextcount.CounterCapability{Transport: contextcount.CounterTransportLocal, Retention: contextcount.RetentionNone, TokenizerRev: "estimate-v1", Quality: contextcount.CountQualityHeuristicEstimate}
}

func localInferenceCapability() contextcount.InferenceCapability {
	return contextcount.InferenceCapability{Transport: contextcount.InferenceTransportLocal, Retention: contextcount.RetentionNone}
}

func manualCompactionPolicy() CompactionPolicy {
	return CompactionPolicy{ReservedOutput: 10, MaxSummaryTokens: 5, CountTimeout: 17 * time.Millisecond, Hustle: "context.compact"}
}

func automaticCompactionPolicy() CompactionPolicy {
	return CompactionPolicy{Automatic: true, CounterPolicy: CounterPolicyAllowConservative, CompactAt: 8_000, RearmBelow: 6_000, ReservedOutput: 10, SafetyMargin: 2, MaxSummaryTokens: 5, CountTimeout: 17 * time.Millisecond, Hustle: "context.compact"}
}

func contextDefinitionOptions(counter contextcount.ContextCounter, capability contextcount.InferenceCapability, policy CompactionPolicy) []Option {
	return []Option{WithName("agent"), WithInference(&fakeLLM{}, testModel()), WithContextCounter(counter), WithInferenceCapability(capability), WithCompaction(policy)}
}

func TestCompactionPolicyValidation(t *testing.T) {
	t.Parallel()
	exact := exactCounterCapability()
	heuristic := heuristicCounterCapability()
	tests := []struct {
		name       string
		policy     CompactionPolicy
		capability contextcount.CounterCapability
		wantErr    bool
	}{
		{name: "manual exact", policy: manualCompactionPolicy(), capability: exact},
		{name: "manual heuristic requires margin", policy: manualCompactionPolicy(), capability: heuristic, wantErr: true},
		{name: "manual heuristic with margin", policy: func() CompactionPolicy { value := manualCompactionPolicy(); value.SafetyMargin = 1; return value }(), capability: heuristic},
		{name: "automatic conservative heuristic", policy: automaticCompactionPolicy(), capability: heuristic},
		{name: "automatic require exact rejects heuristic", policy: func() CompactionPolicy {
			value := automaticCompactionPolicy()
			value.CounterPolicy = CounterPolicyRequireExact
			return value
		}(), capability: heuristic, wantErr: true},
		{name: "automatic require exact accepts provider", policy: func() CompactionPolicy {
			value := automaticCompactionPolicy()
			value.CounterPolicy = CounterPolicyRequireExact
			return value
		}(), capability: func() contextcount.CounterCapability {
			value := exact
			value.Quality = contextcount.CountQualityExactProvider
			return value
		}()},
		{name: "automatic unknown counter policy", policy: func() CompactionPolicy {
			value := automaticCompactionPolicy()
			value.CounterPolicy = CounterPolicyUnknown
			return value
		}(), capability: heuristic, wantErr: true},
		{name: "automatic zero rearm", policy: func() CompactionPolicy { value := automaticCompactionPolicy(); value.RearmBelow = 0; return value }(), capability: heuristic, wantErr: true},
		{name: "automatic rearm equals compact", policy: func() CompactionPolicy {
			value := automaticCompactionPolicy()
			value.RearmBelow = value.CompactAt
			return value
		}(), capability: heuristic, wantErr: true},
		{name: "automatic compact full scale", policy: func() CompactionPolicy {
			value := automaticCompactionPolicy()
			value.CompactAt = event.FullScaleBasisPoints
			return value
		}(), capability: heuristic, wantErr: true},
		{name: "zero reserved output", policy: func() CompactionPolicy { value := manualCompactionPolicy(); value.ReservedOutput = 0; return value }(), capability: exact, wantErr: true},
		{name: "zero summary budget", policy: func() CompactionPolicy { value := manualCompactionPolicy(); value.MaxSummaryTokens = 0; return value }(), capability: exact, wantErr: true},
		{name: "zero count timeout", policy: func() CompactionPolicy { value := manualCompactionPolicy(); value.CountTimeout = 0; return value }(), capability: exact, wantErr: true},
		{name: "negative count timeout", policy: func() CompactionPolicy { value := manualCompactionPolicy(); value.CountTimeout = -1; return value }(), capability: exact, wantErr: true},
		{name: "blank hustle", policy: func() CompactionPolicy { value := manualCompactionPolicy(); value.Hustle = ""; return value }(), capability: exact, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := tt.policy.Validate(tt.capability)
			if (err != nil) != tt.wantErr {
				t.Fatalf("Validate() error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				var target *CompactionPolicyError
				if !errors.As(err, &target) {
					t.Fatalf("error = %T, want *CompactionPolicyError", err)
				}
			}
		})
	}
}

func TestDefinitionContextOptionsAndCapabilityValidation(t *testing.T) {
	t.Parallel()
	validCounter := &policyCounter{capability: exactCounterCapability()}
	separate := exactCounterCapability()
	separate.Provider = "other"
	separate.Transport = contextcount.CounterTransportSeparateEndpoint
	separate.SecurityIdentity = contextcount.SecurityIdentity{1}
	tests := []struct {
		name string
		opts []Option
		kind DefinitionErrorKind
	}{
		{name: "counter without inference capability", opts: []Option{WithName("agent"), WithInference(&fakeLLM{}, testModel()), WithContextCounter(validCounter)}, kind: DefinitionMissingInferenceCapability},
		{name: "capability without counter", opts: []Option{WithName("agent"), WithInference(&fakeLLM{}, testModel()), WithInferenceCapability(localInferenceCapability())}, kind: DefinitionMissingContextCounter},
		{name: "typed nil counter", opts: contextDefinitionOptions((*policyCounter)(nil), localInferenceCapability(), manualCompactionPolicy()), kind: DefinitionInvalidContextCounter},
		{name: "invalid counter metadata", opts: contextDefinitionOptions(&policyCounter{}, localInferenceCapability(), manualCompactionPolicy()), kind: DefinitionInvalidContextCounter},
		{name: "invalid inference metadata", opts: contextDefinitionOptions(validCounter, contextcount.InferenceCapability{}, manualCompactionPolicy()), kind: DefinitionInvalidInferenceCapability},
		{name: "incompatible counter", opts: contextDefinitionOptions(&policyCounter{capability: separate}, localInferenceCapability(), manualCompactionPolicy()), kind: DefinitionIncompatibleContextCounter},
		{name: "compaction missing counter and capability", opts: []Option{WithName("agent"), WithInference(&fakeLLM{}, testModel()), WithCompaction(manualCompactionPolicy())}, kind: DefinitionMissingContextCounter},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := Define(tt.opts...)
			var target *DefinitionError
			if !errors.As(err, &target) || target.Kind != tt.kind {
				t.Fatalf("Define() error = %T %v, want %q", err, err, tt.kind)
			}
		})
	}
	if validCounter.countCalls != 0 {
		t.Fatalf("definition validation count calls = %d, want zero", validCounter.countCalls)
	}
}

func TestDefinitionContextOptionsSingletonAndTimeoutPreserved(t *testing.T) {
	t.Parallel()
	counter := &policyCounter{capability: exactCounterCapability()}
	policy := manualCompactionPolicy()
	policy.CountTimeout = 37*time.Second + 19*time.Nanosecond
	tests := []struct {
		name  string
		extra Option
	}{
		{name: "counter", extra: WithContextCounter(counter)},
		{name: "inference capability", extra: WithInferenceCapability(localInferenceCapability())},
		{name: "compaction", extra: WithCompaction(policy)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			opts := contextDefinitionOptions(counter, localInferenceCapability(), policy)
			opts = append(opts, tt.extra)
			_, err := Define(opts...)
			var target *DefinitionError
			if !errors.As(err, &target) || target.Kind != DefinitionDuplicateOption {
				t.Fatalf("error = %T %v", err, err)
			}
		})
	}
	definition, err := Define(contextDefinitionOptions(counter, localInferenceCapability(), policy)...)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := definition.CompactionPolicy()
	if !ok || got.CountTimeout != policy.CountTimeout || got.CountTimeout == 2*time.Second {
		t.Fatalf("CountTimeout = %v, ok=%v; want %v", got.CountTimeout, ok, policy.CountTimeout)
	}
}

func TestContextTransportBinding(t *testing.T) {
	t.Parallel()
	counter := &policyCounter{capability: exactCounterCapability()}
	base := testModel()
	changed := base.Clone()
	changed.Name = "other"
	changed.Limits = model.ContextLimits{WindowTokens: 200}
	changed.Caps.Tools = !changed.Caps.Tools
	effort := model.EffortHigh
	changed.Sampling.Effort = effort
	tests := []struct {
		name      string
		candidate model.Model
		wantErr   bool
	}{
		{name: "request shape changes allowed", candidate: changed},
		{name: "provider change rejected", candidate: func() model.Model { value := changed; value.Provider = "other"; return value }(), wantErr: true},
		{name: "api format change rejected", candidate: func() model.Model { value := changed; value.APIFormat = model.APIFormatAnthropic; return value }(), wantErr: true},
		{name: "base url change rejected", candidate: func() model.Model { value := changed; value.BaseURL = "http://localhost:9999"; return value }(), wantErr: true},
	}
	definition, err := Define(contextDefinitionOptions(counter, localInferenceCapability(), manualCompactionPolicy())...)
	if err != nil {
		t.Fatal(err)
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := definition.ValidateContextModel(tt.candidate)
			if (err != nil) != tt.wantErr {
				t.Fatalf("ValidateContextModel() error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				var target *ContextTransportBindingError
				if !errors.As(err, &target) {
					t.Fatalf("error = %T", err)
				}
			}
		})
	}
	badMode := changed
	badMode.Provider = "other"
	_, err = Define(append(contextDefinitionOptions(counter, localInferenceCapability(), manualCompactionPolicy()), WithModes(Mode{Name: "bad", Model: badMode}), WithInitialMode("bad"))...)
	var definitionErr *DefinitionError
	if !errors.As(err, &definitionErr) || definitionErr.Kind != DefinitionInvalidModeBinding {
		t.Fatalf("mode error = %T %v", err, err)
	}
}

func TestRequestFingerprintSensitivity(t *testing.T) {
	t.Parallel()
	temperature := 0.5
	input := RequestFingerprintInput{
		SystemRevision: "system-v1", ToolPolicyRevision: "tools-v1", RuntimeContextRevision: "runtime-v1",
		Model:             func() model.Model { value := testModel(); value.Sampling.Temperature = &temperature; return value }(),
		Basis:             event.ContextBasis{Revision: 1, ThroughEventID: uuid.UUID{1}},
		CounterCapability: exactCounterCapability(), InferenceCapability: localInferenceCapability(),
	}
	base, err := RequestFingerprint(input)
	if err != nil || base == ([32]byte{}) {
		t.Fatalf("RequestFingerprint() = %x, %v", base, err)
	}
	mutations := []struct {
		name   string
		mutate func(*RequestFingerprintInput)
	}{
		{name: "system", mutate: func(v *RequestFingerprintInput) { v.SystemRevision = "system-v2" }},
		{name: "tools", mutate: func(v *RequestFingerprintInput) { v.ToolPolicyRevision = "tools-v2" }},
		{name: "model", mutate: func(v *RequestFingerprintInput) { v.Model.Name = "other" }},
		{name: "sampling", mutate: func(v *RequestFingerprintInput) { n := 0.7; v.Model.Sampling.Temperature = &n }},
		{name: "basis revision", mutate: func(v *RequestFingerprintInput) { v.Basis.Revision++ }},
		{name: "basis event", mutate: func(v *RequestFingerprintInput) { v.Basis.ThroughEventID = uuid.UUID{2} }},
		{name: "runtime context", mutate: func(v *RequestFingerprintInput) { v.RuntimeContextRevision = "runtime-v2" }},
		{name: "counter provider", mutate: func(v *RequestFingerprintInput) { v.CounterCapability.Provider = "local-owner" }},
		{name: "counter tokenizer", mutate: func(v *RequestFingerprintInput) { v.CounterCapability.TokenizerRev = "exact-v2" }},
		{name: "inference retention", mutate: func(v *RequestFingerprintInput) { v.InferenceCapability.Retention = contextcount.RetentionUnknown }},
	}
	for _, tt := range mutations {
		t.Run(tt.name, func(t *testing.T) {
			copy := input
			copy.Model = input.Model.Clone()
			tt.mutate(&copy)
			got, fingerprintErr := RequestFingerprint(copy)
			if fingerprintErr != nil {
				t.Fatalf("RequestFingerprint() error = %v", fingerprintErr)
			}
			if got == base {
				t.Fatal("fingerprint did not change")
			}
		})
	}
	again, err := RequestFingerprint(input)
	if err != nil || again != base {
		t.Fatalf("fingerprint unstable: %x != %x, err=%v", again, base, err)
	}
}
