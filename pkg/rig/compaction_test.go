package rig

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/looprig/harness/pkg/hustle"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/sessionstore"
	"github.com/looprig/inference"
	"github.com/looprig/storage/memstore"
)

type rigContextCounter struct{ capability inference.CounterCapability }

func (*rigContextCounter) CountContext(_ context.Context, _ inference.Request) (inference.ContextCount, error) {
	panic("unused")
}
func (c *rigContextCounter) CounterCapability() inference.CounterCapability { return c.capability }

type compactionFingerprintConfig struct {
	counter   inference.CounterCapability
	inference inference.InferenceCapability
	policy    loop.CompactionPolicy
}

func neutralFingerprintConfig() compactionFingerprintConfig {
	return compactionFingerprintConfig{
		counter:   inference.CounterCapability{Transport: inference.CounterTransportLocal, Retention: inference.RetentionNone, TokenizerRev: "v1", Quality: inference.CountQualityExactLocal},
		inference: inference.InferenceCapability{Transport: inference.InferenceTransportLocal, Retention: inference.RetentionNone},
		policy:    loop.CompactionPolicy{ReservedOutput: 10, MaxSummaryTokens: 5, CountTimeout: time.Millisecond, Hustle: "context.compact"},
	}
}

func endpointFingerprintConfig() compactionFingerprintConfig {
	identity := inference.SecurityIdentity{1}
	return compactionFingerprintConfig{
		counter: inference.CounterCapability{
			Provider: "provider", Transport: inference.CounterTransportSeparateEndpoint,
			SecurityIdentity: identity, Retention: inference.RetentionNone,
			TokenizerRev: "v1", Quality: inference.CountQualityExactProvider,
		},
		inference: inference.InferenceCapability{
			Provider: "provider", Transport: inference.InferenceTransportTLS,
			SecurityIdentity: identity, Retention: inference.RetentionLogged,
		},
		policy: loop.CompactionPolicy{SafetyMargin: 1, ReservedOutput: 10, MaxSummaryTokens: 5, CountTimeout: time.Millisecond, Hustle: "context.compact"},
	}
}

func automaticFingerprintConfig() compactionFingerprintConfig {
	config := neutralFingerprintConfig()
	config.policy = loop.CompactionPolicy{
		Automatic: true, CounterPolicy: loop.CounterPolicyRequireExact,
		CompactAt: 8_000, RearmBelow: 6_000, ReservedOutput: 10,
		SafetyMargin: 1, MaxSummaryTokens: 5, CountTimeout: time.Millisecond,
		Hustle: "context.compact",
	}
	return config
}

func compactionLoopDefinitionFromConfig(t *testing.T, config compactionFingerprintConfig) loop.Definition {
	t.Helper()
	counter := &rigContextCounter{capability: config.counter}
	definition, err := loop.Define(
		loop.WithName("agent"), loop.WithInference(&stubLLM{}, validModel("model")),
		loop.WithContextCounter(counter), loop.WithInferenceCapability(config.inference),
		loop.WithCompaction(config.policy),
	)
	if err != nil {
		t.Fatalf("loop.Define: %v", err)
	}
	return definition
}

func compactionLoopDefinition(t *testing.T, hustleName hustle.Name) loop.Definition {
	t.Helper()
	config := neutralFingerprintConfig()
	config.policy.Hustle = hustleName
	return compactionLoopDefinitionFromConfig(t, config)
}

func compactionRigOptions(t *testing.T, definition loop.Definition, definitions ...hustle.Definition) []Option {
	t.Helper()
	store, err := sessionstore.Open(memstore.New())
	if err != nil {
		t.Fatal(err)
	}
	options := []Option{WithLoops(definition), WithPrimers("agent"), WithSessionStore(store)}
	if len(definitions) > 0 {
		options = append(options, WithHustles(definitions...), WithHustleLimits(validHustleLimits()))
	}
	return options
}

func TestDefineValidatesCompactionHustleAfterRegistrationFreeze(t *testing.T) {
	t.Parallel()
	valid := validRigHustle(t, "context.compact")
	background := validRigHustle(t, "context.compact", hustle.WithName("context.compact"), hustle.WithParticipation(hustle.ParticipationBackground), hustle.WithTimeout(time.Second), hustle.WithLimits(hustle.Limits{InputBytes: 1024, OutputBytes: 512}), hustle.WithCurrentLoopModel(), hustle.WithSystemPrompt("p", "v1"), hustle.WithPolicyRevision("v1"))
	named := validRigHustle(t, "context.compact", hustle.WithName("context.compact"), hustle.WithParticipation(hustle.ParticipationBlocking), hustle.WithTimeout(time.Second), hustle.WithLimits(hustle.Limits{InputBytes: 1024, OutputBytes: 512}), hustle.WithNamedInference(&stubLLM{}, validModel("named")), hustle.WithSystemPrompt("p", "v1"), hustle.WithPolicyRevision("v1"))
	tests := []struct {
		name        string
		definitions []hustle.Definition
		kind        DefinitionErrorKind
	}{
		{name: "valid blocking current loop", definitions: []hustle.Definition{valid}},
		{name: "missing", kind: DefinitionMissingCompactionHustle},
		{name: "background", definitions: []hustle.Definition{background}, kind: DefinitionIncompatibleCompactionHustle},
		{name: "named", definitions: []hustle.Definition{named}, kind: DefinitionIncompatibleCompactionHustle},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			definition := compactionLoopDefinition(t, "context.compact")
			rig, err := Define(compactionRigOptions(t, definition, tt.definitions...)...)
			if tt.kind == "" {
				if err != nil || rig == nil {
					t.Fatalf("Define() = %v, %v", rig, err)
				}
				return
			}
			if rig != nil {
				t.Fatal("invalid definition returned partial rig")
			}
			var target *DefinitionError
			if !errors.As(err, &target) || target.Kind != tt.kind || target.Name != "context.compact" {
				t.Fatalf("error = %T %v", err, err)
			}
		})
	}
}

func TestCompactionTopologyFingerprintSensitivity(t *testing.T) {
	t.Parallel()
	providerConfig := func() compactionFingerprintConfig {
		config := neutralFingerprintConfig()
		config.counter.Provider = "provider"
		config.inference.Provider = "provider"
		return config
	}
	tlsConfig := func() compactionFingerprintConfig {
		config := neutralFingerprintConfig()
		config.inference = inference.InferenceCapability{Provider: "provider", Transport: inference.InferenceTransportTLS, SecurityIdentity: inference.SecurityIdentity{1}, Retention: inference.RetentionNone}
		return config
	}
	tests := []struct {
		name   string
		base   func() compactionFingerprintConfig
		mutate func(*compactionFingerprintConfig)
	}{
		{name: "counter provider", base: providerConfig, mutate: func(c *compactionFingerprintConfig) { c.counter.Provider = "" }},
		{name: "counter transport", base: endpointFingerprintConfig, mutate: func(c *compactionFingerprintConfig) { c.counter.Transport = inference.CounterTransportSameEndpoint }},
		{name: "counter security identity", base: endpointFingerprintConfig, mutate: func(c *compactionFingerprintConfig) { c.counter.SecurityIdentity = inference.SecurityIdentity{2} }},
		{name: "counter retention", base: endpointFingerprintConfig, mutate: func(c *compactionFingerprintConfig) { c.counter.Retention = inference.RetentionEphemeral }},
		{name: "counter tokenizer revision", base: endpointFingerprintConfig, mutate: func(c *compactionFingerprintConfig) { c.counter.TokenizerRev = "v2" }},
		{name: "counter quality", base: endpointFingerprintConfig, mutate: func(c *compactionFingerprintConfig) { c.counter.Quality = inference.CountQualityExactLocal }},
		{name: "inference provider", base: neutralFingerprintConfig, mutate: func(c *compactionFingerprintConfig) { c.inference.Provider = "other" }},
		{name: "inference transport", base: tlsConfig, mutate: func(c *compactionFingerprintConfig) { c.inference.Transport = inference.InferenceTransportAttestedTLS }},
		{name: "inference security identity", base: tlsConfig, mutate: func(c *compactionFingerprintConfig) { c.inference.SecurityIdentity = inference.SecurityIdentity{2} }},
		{name: "inference retention", base: tlsConfig, mutate: func(c *compactionFingerprintConfig) { c.inference.Retention = inference.RetentionEphemeral }},
		{name: "policy automatic", base: automaticFingerprintConfig, mutate: func(c *compactionFingerprintConfig) { c.policy.Automatic = false }},
		{name: "policy counter policy", base: automaticFingerprintConfig, mutate: func(c *compactionFingerprintConfig) { c.policy.CounterPolicy = loop.CounterPolicyAllowConservative }},
		{name: "policy compact threshold", base: automaticFingerprintConfig, mutate: func(c *compactionFingerprintConfig) { c.policy.CompactAt++ }},
		{name: "policy rearm threshold", base: automaticFingerprintConfig, mutate: func(c *compactionFingerprintConfig) { c.policy.RearmBelow++ }},
		{name: "policy reserved output", base: automaticFingerprintConfig, mutate: func(c *compactionFingerprintConfig) { c.policy.ReservedOutput++ }},
		{name: "policy safety margin", base: automaticFingerprintConfig, mutate: func(c *compactionFingerprintConfig) { c.policy.SafetyMargin++ }},
		{name: "policy summary budget", base: automaticFingerprintConfig, mutate: func(c *compactionFingerprintConfig) { c.policy.MaxSummaryTokens++ }},
		{name: "policy count timeout", base: automaticFingerprintConfig, mutate: func(c *compactionFingerprintConfig) { c.policy.CountTimeout++ }},
		{name: "policy hustle", base: automaticFingerprintConfig, mutate: func(c *compactionFingerprintConfig) { c.policy.Hustle = "context.compact.v2" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			baseConfig := tt.base()
			changedConfig := tt.base()
			tt.mutate(&changedConfig)
			base := compactionLoopDefinitionFromConfig(t, baseConfig)
			changed := compactionLoopDefinitionFromConfig(t, changedConfig)
			if changed.PolicyRevision() == base.PolicyRevision() {
				t.Fatal("loop policy revision ignored independent mutation")
			}
			baseRevision := topologyRevision([]loop.Definition{base}, []string{"agent"}, "agent")
			if got := topologyRevision([]loop.Definition{changed}, []string{"agent"}, "agent"); got == baseRevision {
				t.Fatal("topology ignored independent mutation")
			}
			if got := topologyRevision([]loop.Definition{base}, []string{"agent"}, "agent"); got != baseRevision {
				t.Fatal("topology is unstable")
			}
		})
	}
}
