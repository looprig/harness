package rig

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/looprig/core/content"
	"github.com/looprig/harness/pkg/hustle"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/inference"
	model "github.com/looprig/inference/model"
	stream "github.com/looprig/inference/stream"
)

type credentialedHustleClient struct{ credential string }

func (*credentialedHustleClient) Invoke(context.Context, inference.Request) (*inference.Response, error) {
	return nil, nil
}

func (*credentialedHustleClient) Stream(context.Context, inference.Request) (*stream.StreamReader[content.Chunk], error) {
	return nil, nil
}

type rigHustleSpec struct {
	name          hustle.Name
	participation hustle.Participation
	modelSource   hustle.ModelSource
	client        inference.Client
	model         model.Model
	prompt        string
	promptRev     string
	policyRev     string
	timeout       time.Duration
	limits        hustle.Limits
	output        *inference.OutputSchema
}

func rigHustleOutput() *inference.OutputSchema {
	return &inference.OutputSchema{
		Name:        "classifier_result",
		Description: "Return the classifier result",
		Schema:      json.RawMessage(`{"type":"object","properties":{"allowed":{"type":"boolean"}},"required":["allowed"],"additionalProperties":false}`),
		Strict:      true,
	}
}

func defaultRigHustleSpec() rigHustleSpec {
	return rigHustleSpec{
		name: "compact", participation: hustle.ParticipationBlocking,
		modelSource: hustle.ModelSourceCurrentLoop,
		client:      &credentialedHustleClient{credential: "credential-a"}, model: validModel("named-model"),
		prompt: "raw prompt alpha", promptRev: "prompt-v1", policyRev: "policy-v1",
		timeout: time.Second, limits: hustle.Limits{InputBytes: 1024, OutputBytes: 512},
		output: rigHustleOutput(),
	}
}

func defineRigHustle(t *testing.T, spec rigHustleSpec) hustle.Definition {
	t.Helper()
	options := []hustle.Option{
		hustle.WithName(spec.name), hustle.WithParticipation(spec.participation),
		hustle.WithTimeout(spec.timeout), hustle.WithLimits(spec.limits),
		hustle.WithSystemPrompt(spec.prompt, spec.promptRev), hustle.WithPolicyRevision(spec.policyRev),
	}
	if spec.modelSource == hustle.ModelSourceNamed {
		options = append(options, hustle.WithNamedInference(spec.client, spec.model))
	} else {
		options = append(options, hustle.WithCurrentLoopModel())
	}
	if spec.output != nil {
		options = append(options, hustle.WithOutputSchema(*spec.output))
	}
	definition, err := hustle.Define(options...)
	if err != nil {
		t.Fatalf("hustle.Define: %v", err)
	}
	return definition
}

func TestHustleTopologyFingerprintDeterministic(t *testing.T) {
	t.Parallel()
	loopDefinition := mustDefine(loop.WithName("agent"), loop.WithInference(&stubLLM{}, validModel("loop-model")))
	firstSpec := defaultRigHustleSpec()
	firstSpec.name = "alpha"
	secondSpec := defaultRigHustleSpec()
	secondSpec.name = "zulu"
	first, second := defineRigHustle(t, firstSpec), defineRigHustle(t, secondSpec)
	limits := validHustleLimits()
	tests := []struct {
		name  string
		left  []hustle.Definition
		right []hustle.Definition
	}{
		{name: "same order", left: []hustle.Definition{first, second}, right: []hustle.Definition{first, second}},
		{name: "registration order independent", left: []hustle.Definition{first, second}, right: []hustle.Definition{second, first}},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			left := topologyRevisionWithHustles([]loop.Definition{loopDefinition}, []string{"agent"}, "agent", tt.left, limits)
			right := topologyRevisionWithHustles([]loop.Definition{loopDefinition}, []string{"agent"}, "agent", tt.right, limits)
			if left != right {
				t.Fatalf("topology revisions differ: %q != %q", left, right)
			}
		})
	}
}

func TestHustleTopologyFingerprintSensitivityAndExclusions(t *testing.T) {
	t.Parallel()
	loopDefinition := mustDefine(loop.WithName("agent"), loop.WithInference(&stubLLM{}, validModel("loop-model")))
	baseSpec := defaultRigHustleSpec()
	baseDefinition := defineRigHustle(t, baseSpec)
	baseLimits := validHustleLimits()
	revision := func(definition hustle.Definition, limits HustleLimits) string {
		return topologyRevisionWithHustles([]loop.Definition{loopDefinition}, []string{"agent"}, "agent", []hustle.Definition{definition}, limits)
	}
	base := revision(baseDefinition, baseLimits)

	namedSource := baseSpec
	namedSource.modelSource = hustle.ModelSourceNamed
	namedSourceDefinition := defineRigHustle(t, namedSource)
	tests := []struct {
		name       string
		definition hustle.Definition
		limits     HustleLimits
		wantEqual  bool
	}{
		{name: "name", definition: defineRigHustle(t, func() rigHustleSpec { value := baseSpec; value.name = "other"; return value }()), limits: baseLimits},
		{name: "participation", definition: defineRigHustle(t, func() rigHustleSpec {
			value := baseSpec
			value.participation = hustle.ParticipationBackground
			return value
		}()), limits: baseLimits},
		{name: "model source", definition: namedSourceDefinition, limits: baseLimits},
		{name: "named model policy", definition: defineRigHustle(t, func() rigHustleSpec { value := namedSource; value.model = validModel("other-model"); return value }()), limits: baseLimits},
		{name: "prompt revision", definition: defineRigHustle(t, func() rigHustleSpec { value := baseSpec; value.promptRev = "prompt-v2"; return value }()), limits: baseLimits},
		{name: "raw prompt behavior digest", definition: defineRigHustle(t, func() rigHustleSpec { value := baseSpec; value.prompt = "raw prompt beta"; return value }()), limits: baseLimits},
		{name: "policy revision", definition: defineRigHustle(t, func() rigHustleSpec { value := baseSpec; value.policyRev = "policy-v2"; return value }()), limits: baseLimits},
		{name: "output schema name", definition: defineRigHustle(t, func() rigHustleSpec {
			value := baseSpec
			output := value.output.Clone()
			output.Name = "classifier_result_v2"
			value.output = &output
			return value
		}()), limits: baseLimits},
		{name: "output schema", definition: defineRigHustle(t, func() rigHustleSpec {
			value := baseSpec
			output := value.output.Clone()
			output.Schema = json.RawMessage(`{"type":"object","properties":{"verdict":{"type":"string"}},"required":["verdict"],"additionalProperties":false}`)
			value.output = &output
			return value
		}()), limits: baseLimits},
		{name: "output description", definition: defineRigHustle(t, func() rigHustleSpec {
			value := baseSpec
			output := value.output.Clone()
			output.Description = "Changed behavior"
			value.output = &output
			return value
		}()), limits: baseLimits},
		{name: "output strictness", definition: defineRigHustle(t, func() rigHustleSpec {
			value := baseSpec
			output := value.output.Clone()
			output.Strict = false
			value.output = &output
			return value
		}()), limits: baseLimits},
		{name: "output absent", definition: defineRigHustle(t, func() rigHustleSpec { value := baseSpec; value.output = nil; return value }()), limits: baseLimits},
		{name: "timeout", definition: defineRigHustle(t, func() rigHustleSpec { value := baseSpec; value.timeout++; return value }()), limits: baseLimits},
		{name: "input bytes", definition: defineRigHustle(t, func() rigHustleSpec { value := baseSpec; value.limits.InputBytes++; return value }()), limits: baseLimits},
		{name: "output bytes", definition: defineRigHustle(t, func() rigHustleSpec { value := baseSpec; value.limits.OutputBytes++; return value }()), limits: baseLimits},
		{name: "blocking concurrent", definition: baseDefinition, limits: func() HustleLimits { value := baseLimits; value.BlockingConcurrent++; return value }()},
		{name: "blocking queued", definition: baseDefinition, limits: func() HustleLimits { value := baseLimits; value.BlockingQueued++; return value }()},
		{name: "background concurrent", definition: baseDefinition, limits: func() HustleLimits { value := baseLimits; value.BackgroundConcurrent++; return value }()},
		{name: "background queued", definition: baseDefinition, limits: func() HustleLimits { value := baseLimits; value.BackgroundQueued++; return value }()},
		{name: "audit timeout", definition: baseDefinition, limits: func() HustleLimits { value := baseLimits; value.AuditTimeout++; return value }()},
		{name: "finalization timeout", definition: baseDefinition, limits: func() HustleLimits { value := baseLimits; value.FinalizationTimeout++; return value }()},
		{name: "worker drain timeout", definition: baseDefinition, limits: func() HustleLimits { value := baseLimits; value.WorkerDrainTimeout++; return value }()},
		{name: "named client identity and credentials excluded", definition: defineRigHustle(t, func() rigHustleSpec {
			value := namedSource
			value.client = &credentialedHustleClient{credential: "different-secret"}
			return value
		}()), limits: baseLimits, wantEqual: true},
		{name: "current-loop resolved live model excluded", definition: defineRigHustle(t, func() rigHustleSpec { value := baseSpec; value.model = validModel("changed-live-model"); return value }()), limits: baseLimits, wantEqual: true},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			gotBase := base
			if tt.name == "named client identity and credentials excluded" {
				gotBase = revision(namedSourceDefinition, baseLimits)
			}
			got := revision(tt.definition, tt.limits)
			if (got == gotBase) != tt.wantEqual {
				t.Fatalf("revision equality = %v, want %v", got == gotBase, tt.wantEqual)
			}
		})
	}
}

func TestHustleBoundAndFrozenTopologyFingerprintEquivalent(t *testing.T) {
	t.Parallel()
	loopDefinition := mustDefine(loop.WithName("agent"), loop.WithInference(&stubLLM{}, validModel("loop-model")))
	bound := bindFingerprintDefinition(loopDefinition)
	definition := defineRigHustle(t, defaultRigHustleSpec())
	limits := validHustleLimits()
	tests := []struct {
		name string
	}{
		{name: "registered hustle and limits"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			boundFingerprint := fingerprintWithTopologyAndHustles(
				bound, ConfigFingerprintFields{}, []loop.Definition{loopDefinition},
				[]string{"agent"}, "agent", []hustle.Definition{definition}, limits,
			)
			frozenFingerprint := frozenFingerprintWithHustles(
				ConfigFingerprintFields{}, []loop.Definition{loopDefinition},
				[]string{"agent"}, "agent", []hustle.Definition{definition}, limits,
			)
			if boundFingerprint.TopologyRev != frozenFingerprint.TopologyRev {
				t.Fatalf("bound TopologyRev = %q, frozen = %q", boundFingerprint.TopologyRev, frozenFingerprint.TopologyRev)
			}
		})
	}
}

func TestHustleBoundTopologyFingerprintSensitivity(t *testing.T) {
	t.Parallel()
	loopDefinition := mustDefine(loop.WithName("agent"), loop.WithInference(&stubLLM{}, validModel("loop-model")))
	bound := bindFingerprintDefinition(loopDefinition)
	baseSpec := defaultRigHustleSpec()
	baseDefinition := defineRigHustle(t, baseSpec)
	baseLimits := validHustleLimits()
	revision := func(definition hustle.Definition, limits HustleLimits) string {
		return fingerprintWithTopologyAndHustles(
			bound, ConfigFingerprintFields{}, []loop.Definition{loopDefinition},
			[]string{"agent"}, "agent", []hustle.Definition{definition}, limits,
		).TopologyRev
	}
	baseRevision := revision(baseDefinition, baseLimits)
	tests := []struct {
		name       string
		definition hustle.Definition
		limits     HustleLimits
	}{
		{name: "hustle policy", definition: defineRigHustle(t, func() rigHustleSpec { value := baseSpec; value.policyRev = "policy-v2"; return value }()), limits: baseLimits},
		{name: "lane limit", definition: baseDefinition, limits: func() HustleLimits { value := baseLimits; value.BackgroundQueued++; return value }()},
		{name: "cleanup limit", definition: baseDefinition, limits: func() HustleLimits { value := baseLimits; value.WorkerDrainTimeout++; return value }()},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := revision(tt.definition, tt.limits); got == baseRevision {
				t.Fatalf("bound topology revision unchanged: %q", got)
			}
		})
	}
}

func TestNoHustleTopologyFingerprintPreservesLegacyMaterial(t *testing.T) {
	t.Parallel()
	definition := mustDefine(loop.WithName("agent"), loop.WithInference(&stubLLM{}, validModel("loop-model")))
	tests := []struct {
		name       string
		definition loop.Definition
		primers    []string
		active     string
	}{
		{name: "single active primer", definition: definition, primers: []string{"agent"}, active: "agent"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			legacyMaterial := "loop:" + string(tt.definition.Name()) + "\npolicy:" + tt.definition.PolicyRevision() + "\nprimer:agent\nactive:agent"
			want := hexSHA256(legacyMaterial)
			if got := topologyRevision([]loop.Definition{tt.definition}, tt.primers, tt.active); got != want {
				t.Fatalf("topologyRevision() = %q, want legacy %q", got, want)
			}
		})
	}
}

func TestHustleTopologyCanonicalEncodingIsInjective(t *testing.T) {
	t.Parallel()
	limits := validHustleLimits()
	tests := []struct {
		name  string
		left  []hustleTopologyRow
		right []hustleTopologyRow
	}{
		{
			name:  "newline policy tag cannot move from name to policy",
			left:  []hustleTopologyRow{{Name: "alpha\npolicy:beta", PolicyRevision: "gamma"}},
			right: []hustleTopologyRow{{Name: "alpha", PolicyRevision: "beta\npolicy:gamma"}},
		},
		{
			name:  "embedded row tags cannot manufacture another definition",
			left:  []hustleTopologyRow{{Name: "alpha", PolicyRevision: "beta\nhustle:charlie\npolicy:delta"}},
			right: []hustleTopologyRow{{Name: "alpha", PolicyRevision: "beta"}, {Name: "charlie", PolicyRevision: "delta"}},
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			left := canonicalHustleTopologyMaterial("legacy-topology-hash", tt.left, limits)
			right := canonicalHustleTopologyMaterial("legacy-topology-hash", tt.right, limits)
			if string(left) == string(right) {
				t.Fatalf("distinct hustle rows encoded identically: %q", left)
			}
		})
	}
}

func TestHustleTopologyCanonicalEncodingDeterministic(t *testing.T) {
	t.Parallel()
	limits := validHustleLimits()
	first := hustleTopologyRow{Name: "alpha\npolicy:embedded", PolicyRevision: "first"}
	second := hustleTopologyRow{Name: "zulu", PolicyRevision: "second"}
	tests := []struct {
		name  string
		left  []hustleTopologyRow
		right []hustleTopologyRow
	}{
		{name: "row order independent", left: []hustleTopologyRow{first, second}, right: []hustleTopologyRow{second, first}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			left := canonicalHustleTopologyMaterial("legacy-topology-hash", tt.left, limits)
			right := canonicalHustleTopologyMaterial("legacy-topology-hash", tt.right, limits)
			if string(left) != string(right) {
				t.Fatalf("canonical encodings differ: %q != %q", left, right)
			}
		})
	}
}
