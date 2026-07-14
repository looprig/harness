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

func compactionLoopDefinition(t *testing.T, hustleName hustle.Name) loop.Definition {
	t.Helper()
	counter := &rigContextCounter{capability: inference.CounterCapability{Transport: inference.CounterTransportLocal, Retention: inference.RetentionNone, TokenizerRev: "v1", Quality: inference.CountQualityExactLocal}}
	definition, err := loop.Define(
		loop.WithName("agent"), loop.WithInference(&stubLLM{}, validModel("model")),
		loop.WithContextCounter(counter), loop.WithInferenceCapability(inference.InferenceCapability{Transport: inference.InferenceTransportLocal, Retention: inference.RetentionNone}),
		loop.WithCompaction(loop.CompactionPolicy{ReservedOutput: 10, MaxSummaryTokens: 5, CountTimeout: time.Millisecond, Hustle: hustleName}),
	)
	if err != nil {
		t.Fatalf("loop.Define: %v", err)
	}
	return definition
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
	base := compactionLoopDefinition(t, "context.compact")
	changed := func() loop.Definition {
		counter := &rigContextCounter{capability: inference.CounterCapability{Transport: inference.CounterTransportLocal, Retention: inference.RetentionNone, TokenizerRev: "v2", Quality: inference.CountQualityExactLocal}}
		definition, err := loop.Define(loop.WithName("agent"), loop.WithInference(&stubLLM{}, validModel("model")), loop.WithContextCounter(counter), loop.WithInferenceCapability(inference.InferenceCapability{Transport: inference.InferenceTransportLocal, Retention: inference.RetentionNone}), loop.WithCompaction(loop.CompactionPolicy{ReservedOutput: 11, MaxSummaryTokens: 5, CountTimeout: time.Millisecond, Hustle: "context.compact"}))
		if err != nil {
			t.Fatal(err)
		}
		return definition
	}()
	baseRevision := topologyRevision([]loop.Definition{base}, []string{"agent"}, "agent")
	if got := topologyRevision([]loop.Definition{changed}, []string{"agent"}, "agent"); got == baseRevision {
		t.Fatal("topology ignored context policy/capability change")
	}
	if got := topologyRevision([]loop.Definition{base}, []string{"agent"}, "agent"); got != baseRevision {
		t.Fatal("topology is unstable")
	}
}
