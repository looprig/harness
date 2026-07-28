package rig

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/looprig/core/content"
	"github.com/looprig/harness/pkg/gate"
	"github.com/looprig/harness/pkg/hustle"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/tool"
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
	evidence      *hustle.EvidenceToolPolicy
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
	if spec.evidence != nil {
		options = append(options, hustle.WithEvidenceTools(spec.evidence.Clone()))
	}
	definition, err := hustle.Define(options...)
	if err != nil {
		t.Fatalf("hustle.Define: %v", err)
	}
	return definition
}

type rigPermissionClassifier struct {
	name       hustle.Name
	revision   string
	definition hustle.Definition
}

func (c rigPermissionClassifier) Name() hustle.Name             { return c.name }
func (c rigPermissionClassifier) Revision() string              { return c.revision }
func (c rigPermissionClassifier) Definition() hustle.Definition { return c.definition }
func (rigPermissionClassifier) Applies(gate.PermissionReviewSubject) bool {
	return true
}
func (rigPermissionClassifier) MarshalInput(gate.PermissionReviewSubject) (json.RawMessage, error) {
	return json.RawMessage(`{}`), nil
}
func (rigPermissionClassifier) ValidateResult(gate.PermissionReviewSubject, hustle.Result) (gate.PermissionAssessment, error) {
	return gate.PermissionAssessment{}, nil
}

func rigEvidencePolicy(name string) hustle.EvidenceToolPolicy {
	return hustle.EvidenceToolPolicy{
		Revision: "evidence-policy-v1",
		Limits: hustle.ToolLoopLimits{
			MaxRounds: 2, MaxCalls: 3, MaxCallsPerRound: 2,
			MaxResultBytes: 1024, MaxEvidenceBytes: 2048,
		},
		Definitions: []tool.Definition{
			tool.NewEvidenceDefinition(
				"definition-"+name, tool.RequiresWorkspaceRead,
				[]tool.ToolInfo{{
					Name: name, Desc: "Read " + name + " evidence",
					Schema: json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`),
				}}, nil,
			),
		},
	}
}

func defineRigPermissionClassifier(t *testing.T, name hustle.Name, evidence hustle.EvidenceToolPolicy) gate.PermissionClassifier {
	t.Helper()
	return defineRigPermissionClassifierWith(t, name, "classifier-"+string(name)+"-v1", evidence, rigHustleOutput())
}

func defineRigPermissionClassifierWith(
	t *testing.T,
	name hustle.Name,
	revision string,
	evidence hustle.EvidenceToolPolicy,
	output *inference.OutputSchema,
) gate.PermissionClassifier {
	t.Helper()
	spec := defaultRigHustleSpec()
	spec.name = name
	spec.modelSource = hustle.ModelSourceNamed
	spec.policyRev = revision
	spec.evidence = &evidence
	spec.output = output
	definition := defineRigHustle(t, spec)
	return rigPermissionClassifier{name: name, revision: spec.policyRev, definition: definition}
}

func rigPermissionClassifierSet(t *testing.T, classifiers ...gate.PermissionClassifier) gate.PermissionClassifierSet {
	t.Helper()
	set, err := gate.NewPermissionClassifierSet(classifiers...)
	if err != nil {
		t.Fatalf("gate.NewPermissionClassifierSet: %v", err)
	}
	return set
}

func TestPermissionReviewTopologyFingerprintSensitivity(t *testing.T) {
	t.Parallel()
	loopDefinition := mustDefine(loop.WithName("agent"), loop.WithInference(&stubLLM{}, validModel("loop-model")))
	limits := validHustleLimits()
	baseEvidence := rigEvidencePolicy("status")
	baseClassifier := defineRigPermissionClassifier(t, "alpha", baseEvidence)
	secondClassifier := defineRigPermissionClassifier(t, "zulu", rigEvidencePolicy("diff"))
	baseSet := rigPermissionClassifierSet(t, baseClassifier, secondClassifier)
	revision := func(set gate.PermissionClassifierSet, reviewPolicyRevision string) string {
		projection, err := permissionReviewFingerprintFrom(set, reviewPolicyRevision)
		if err != nil {
			t.Fatalf("permissionReviewFingerprintFrom: %v", err)
		}
		return topologyRevisionWithHustlesAndPermissionReview(
			[]loop.Definition{loopDefinition}, []string{"agent"}, "agent",
			nil, limits, projection,
		)
	}
	base := revision(baseSet, "review-policy-v1")

	tests := []struct {
		name   string
		set    gate.PermissionClassifierSet
		policy string
	}{
		{name: "review policy revision", set: baseSet, policy: "review-policy-v2"},
		{name: "classifier order", set: rigPermissionClassifierSet(t, secondClassifier, baseClassifier), policy: "review-policy-v1"},
		{name: "classifier revision", set: rigPermissionClassifierSet(t,
			defineRigPermissionClassifierWith(t, "alpha", "classifier-alpha-v2", baseEvidence, rigHustleOutput()),
			secondClassifier), policy: "review-policy-v1"},
		{name: "evidence policy revision", set: rigPermissionClassifierSet(t,
			defineRigPermissionClassifier(t, "alpha", func() hustle.EvidenceToolPolicy {
				value := baseEvidence.Clone()
				value.Revision = "evidence-policy-v2"
				return value
			}()), secondClassifier), policy: "review-policy-v1"},
		{name: "produced tool metadata", set: rigPermissionClassifierSet(t,
			defineRigPermissionClassifier(t, "alpha", rigEvidencePolicy("changed")), secondClassifier), policy: "review-policy-v1"},
		{name: "static tool description", set: rigPermissionClassifierSet(t,
			defineRigPermissionClassifier(t, "alpha", func() hustle.EvidenceToolPolicy {
				value := baseEvidence.Clone()
				value.Definitions[0] = tool.NewEvidenceDefinition(
					"definition-status", tool.RequiresWorkspaceRead,
					[]tool.ToolInfo{{
						Name: "status", Desc: "Changed static evidence description",
						Schema: json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`),
					}}, nil,
				)
				return value
			}()), secondClassifier), policy: "review-policy-v1"},
		{name: "static tool schema", set: rigPermissionClassifierSet(t,
			defineRigPermissionClassifier(t, "alpha", func() hustle.EvidenceToolPolicy {
				value := baseEvidence.Clone()
				value.Definitions[0] = tool.NewEvidenceDefinition(
					"definition-status", tool.RequiresWorkspaceRead,
					[]tool.ToolInfo{{
						Name: "status", Desc: "Read status evidence",
						Schema: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}},"required":["path"],"additionalProperties":false}`),
					}}, nil,
				)
				return value
			}()), secondClassifier), policy: "review-policy-v1"},
		{name: "definition requirements metadata", set: rigPermissionClassifierSet(t,
			defineRigPermissionClassifier(t, "alpha", func() hustle.EvidenceToolPolicy {
				value := baseEvidence.Clone()
				value.Definitions[0] = tool.NewEvidenceDefinition(
					"definition-status", 0, value.Definitions[0].ToolInfos(), nil,
				)
				return value
			}()), secondClassifier), policy: "review-policy-v1"},
		{name: "output schema digest", set: rigPermissionClassifierSet(t,
			defineRigPermissionClassifierWith(t, "alpha", "classifier-alpha-v1", baseEvidence, func() *inference.OutputSchema {
				value := rigHustleOutput().Clone()
				value.Schema = json.RawMessage(`{"type":"object","properties":{"decision":{"type":"string"}},"required":["decision"],"additionalProperties":false}`)
				return &value
			}()), secondClassifier), policy: "review-policy-v1"},
		{name: "output description digest", set: rigPermissionClassifierSet(t,
			defineRigPermissionClassifierWith(t, "alpha", "classifier-alpha-v1", baseEvidence, func() *inference.OutputSchema {
				value := rigHustleOutput().Clone()
				value.Description = "Changed classifier output contract"
				return &value
			}()), secondClassifier), policy: "review-policy-v1"},
	}
	for _, testCase := range tests {
		testCase := testCase
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			if got := revision(testCase.set, testCase.policy); got == base {
				t.Fatalf("topology revision unchanged: %q", got)
			}
		})
	}

	for _, mutation := range []struct {
		name   string
		mutate func(*hustle.ToolLoopLimits)
	}{
		{name: "max rounds", mutate: func(v *hustle.ToolLoopLimits) { v.MaxRounds++ }},
		{name: "max calls", mutate: func(v *hustle.ToolLoopLimits) { v.MaxCalls++ }},
		{name: "max calls per round", mutate: func(v *hustle.ToolLoopLimits) { v.MaxCallsPerRound++ }},
		{name: "max result bytes", mutate: func(v *hustle.ToolLoopLimits) { v.MaxResultBytes++ }},
		{name: "max evidence bytes", mutate: func(v *hustle.ToolLoopLimits) { v.MaxEvidenceBytes++ }},
	} {
		mutation := mutation
		t.Run(mutation.name, func(t *testing.T) {
			t.Parallel()
			evidence := baseEvidence.Clone()
			mutation.mutate(&evidence.Limits)
			changed := rigPermissionClassifierSet(t,
				defineRigPermissionClassifier(t, "alpha", evidence), secondClassifier)
			if got := revision(changed, "review-policy-v1"); got == base {
				t.Fatalf("topology revision unchanged: %q", got)
			}
		})
	}
}

func TestClassifierEvidenceProjectionDomainsAreVersioned(t *testing.T) {
	t.Parallel()
	for name, testCase := range map[string]struct{ got, want string }{
		"hustle topology": {
			hustleTopologyDigestDomain,
			"looprig/rig/hustle-topology/v1",
		},
		"permission review": {
			permissionReviewTopologyDigestDomain,
			"looprig/rig/permission-review-topology/v1",
		},
		"permission classifier": {
			permissionClassifierProjectionDigestDomain,
			"looprig/rig/permission-classifier-projection/v1",
		},
	} {
		if testCase.got != testCase.want {
			t.Fatalf("%s domain = %q, want %q", name, testCase.got, testCase.want)
		}
	}
}

func TestPermissionReviewFingerprintDomainsIndependentlyChangeIdentity(t *testing.T) {
	t.Parallel()
	classifier := defineRigPermissionClassifier(t, "alpha", rigEvidencePolicy("status"))
	review, err := permissionReviewFingerprintFrom(
		rigPermissionClassifierSet(t, classifier),
		"review-policy-v1",
	)
	if err != nil {
		t.Fatalf("permissionReviewFingerprintFrom: %v", err)
	}

	currentMaterial := canonicalPermissionReviewMaterialWithDomains(
		"base",
		*review,
		permissionReviewTopologyDigestDomain,
		permissionClassifierProjectionDigestDomain,
	)
	if production := canonicalPermissionReviewMaterial("base", *review); !bytes.Equal(production, currentMaterial) {
		t.Fatal("production permission review material does not use both current domains")
	}
	current := hexSHA256Bytes(currentMaterial)
	changedOuter := hexSHA256Bytes(canonicalPermissionReviewMaterialWithDomains(
		"base",
		*review,
		"looprig/rig/permission-review-topology/v2",
		permissionClassifierProjectionDigestDomain,
	))
	changedClassifier := hexSHA256Bytes(canonicalPermissionReviewMaterialWithDomains(
		"base",
		*review,
		permissionReviewTopologyDigestDomain,
		"looprig/rig/permission-classifier-projection/v2",
	))
	removedClassifier := hexSHA256Bytes(canonicalPermissionReviewMaterialWithDomains(
		"base",
		*review,
		permissionReviewTopologyDigestDomain,
		"",
	))
	substitutedClassifier := hexSHA256Bytes(canonicalPermissionReviewMaterialWithDomains(
		"base",
		*review,
		permissionReviewTopologyDigestDomain,
		hustleTopologyDigestDomain,
	))

	for name, changed := range map[string]string{
		"outer domain":                  changedOuter,
		"classifier domain":             changedClassifier,
		"removed classifier domain":     removedClassifier,
		"substituted classifier domain": substitutedClassifier,
	} {
		if changed == current {
			t.Fatalf("%s did not change permission review identity: %q", name, changed)
		}
	}
}

func TestPermissionReviewFingerprintPreservesLegacyIdentityWhenDisabled(t *testing.T) {
	t.Parallel()
	definition := mustDefine(loop.WithName("agent"), loop.WithInference(&stubLLM{}, validModel("loop-model")))
	legacy := topologyRevisionWithHustles(
		[]loop.Definition{definition}, []string{"agent"}, "agent", nil, HustleLimits{},
	)
	got := topologyRevisionWithHustlesAndPermissionReview(
		[]loop.Definition{definition}, []string{"agent"}, "agent", nil, HustleLimits{}, nil,
	)
	if got != legacy {
		t.Fatalf("disabled permission review changed legacy identity: %q != %q", got, legacy)
	}
	fields := ConfigFingerprintFields{AgentKind: "legacy"}
	legacyFingerprint := frozenFingerprintWithHustles(
		fields, []loop.Definition{definition}, []string{"agent"}, "agent", nil, HustleLimits{},
	)
	gotFingerprint := frozenFingerprintWithPermissionReview(
		fields, []loop.Definition{definition}, []string{"agent"}, "agent", nil, HustleLimits{}, nil,
	)
	if gotFingerprint != legacyFingerprint {
		t.Fatalf("disabled review changed frozen fingerprint: %#v != %#v", gotFingerprint, legacyFingerprint)
	}
	legacyManifest := frozenManifestWithHustles(
		fields, []loop.Definition{definition}, []string{"agent"}, "agent", nil, HustleLimits{},
	)
	gotManifest := frozenManifestWithPermissionReview(
		fields, []loop.Definition{definition}, []string{"agent"}, "agent", nil, HustleLimits{}, nil,
	)
	if !reflect.DeepEqual(gotManifest, legacyManifest) {
		t.Fatalf("disabled review changed frozen manifest: %#v != %#v", gotManifest, legacyManifest)
	}
}

func TestPermissionReviewFingerprintMaterialIsDeterministicAndSecretFree(t *testing.T) {
	t.Parallel()
	evidence := rigEvidencePolicy("status")
	classifier := defineRigPermissionClassifier(t, "alpha", evidence)
	set := rigPermissionClassifierSet(t, classifier)
	left, err := permissionReviewFingerprintFrom(set, "review-policy-v1")
	if err != nil {
		t.Fatalf("permissionReviewFingerprintFrom: %v", err)
	}
	right, err := permissionReviewFingerprintFrom(set, "review-policy-v1")
	if err != nil {
		t.Fatalf("permissionReviewFingerprintFrom: %v", err)
	}
	leftMaterial := canonicalPermissionReviewMaterial("base", *left)
	rightMaterial := canonicalPermissionReviewMaterial("base", *right)
	if !bytes.Equal(leftMaterial, rightMaterial) {
		t.Fatal("identical permission review configuration encoded nondeterministically")
	}
	for _, forbidden := range [][]byte{
		[]byte("raw prompt alpha"),
		[]byte("Return the classifier result"),
		[]byte(`"properties":{"allowed"`),
		[]byte("credential-a"),
	} {
		if bytes.Contains(leftMaterial, forbidden) {
			t.Fatalf("fingerprint material exposed forbidden raw input %q", forbidden)
		}
	}
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
			// The assembled manifest must route TopologyRev through the SAME
			// hustle-aware revision the fingerprint uses, so a stamped manifest
			// and the legacy fingerprint never show phantom topology drift.
			frozenManifest := frozenManifestWithHustles(
				ConfigFingerprintFields{}, []loop.Definition{loopDefinition},
				[]string{"agent"}, "agent", []hustle.Definition{definition}, limits,
			)
			if frozenManifest.TopologyRev != frozenFingerprint.TopologyRev {
				t.Fatalf("manifest TopologyRev = %q, fingerprint = %q", frozenManifest.TopologyRev, frozenFingerprint.TopologyRev)
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

func TestHustleTopologyFingerprintIncludesStructuredOutputRevision(t *testing.T) {
	t.Parallel()
	definition := defineRigHustle(t, defaultRigHustleSpec())
	descriptor := definition.Descriptor()
	if descriptor.StructuredOutputRevision != inference.StructuredOutputRevision {
		t.Fatalf("StructuredOutputRevision = %q, want %q", descriptor.StructuredOutputRevision, inference.StructuredOutputRevision)
	}

	future := descriptor
	future.StructuredOutputRevision = "structured-output/future"
	encoded, err := json.Marshal(future)
	if err != nil {
		t.Fatalf("json.Marshal(future descriptor): %v", err)
	}
	futurePolicyRevision := fmt.Sprintf("%x", sha256.Sum256(encoded))
	limits := validHustleLimits()
	legacy := topologyRevision([]loop.Definition{mustDefine(loop.WithName("agent"), loop.WithInference(&stubLLM{}, validModel("loop-model")))}, []string{"agent"}, "agent")
	currentTopology := hexSHA256Bytes(canonicalHustleTopologyMaterial(legacy, []hustleTopologyRow{{Name: definition.Name(), PolicyRevision: definition.PolicyRevision()}}, limits))
	futureTopology := hexSHA256Bytes(canonicalHustleTopologyMaterial(legacy, []hustleTopologyRow{{Name: definition.Name(), PolicyRevision: futurePolicyRevision}}, limits))
	if currentTopology == futureTopology {
		t.Fatal("hustle topology fingerprint ignored StructuredOutputRevision drift")
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
