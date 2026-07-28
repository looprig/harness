package hustle

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/looprig/core/content"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/tool"
	"github.com/looprig/inference"
	model "github.com/looprig/inference/model"
	stream "github.com/looprig/inference/stream"
)

type testClient struct{ identity string }

func (*testClient) Invoke(context.Context, inference.Request) (*inference.Response, error) {
	return nil, nil
}

func (*testClient) Stream(context.Context, inference.Request) (*stream.StreamReader[content.Chunk], error) {
	return nil, nil
}

type testResolver struct {
	wantID  uuid.UUID
	binding InferenceBinding
	err     error
	calls   int
}

func (r *testResolver) ResolveHustleModel(_ context.Context, id uuid.UUID) (InferenceBinding, error) {
	r.calls++
	if id != r.wantID {
		return InferenceBinding{}, &testResolveCause{message: "unexpected loop id"}
	}
	if r.err != nil {
		return InferenceBinding{}, r.err
	}
	return r.binding, nil
}

type testResolveCause struct{ message string }

func (e *testResolveCause) Error() string { return e.message }

func validModel(name string) model.Model {
	temperature := 0.25
	topP := 0.9
	maxTokens := 321
	return model.Model{
		Provider:  "test-provider",
		APIFormat: "test-format",
		BaseURL:   "https://models.example.invalid",
		Name:      name,
		Sampling: model.Sampling{
			Temperature: &temperature,
			TopP:        &topP,
			MaxTokens:   &maxTokens,
			Stop:        []string{"END"},
			Effort:      model.EffortMedium,
		},
	}
}

func zeroInferenceModel() model.Model { return model.Model{} }

func invalidInferenceEffort() model.Effort { return model.Effort("bogus") }

func validNamedOptions(client inference.Client, model model.Model) []Option {
	return []Option{
		WithName("conversation-compaction"),
		WithParticipation(ParticipationBlocking),
		WithTimeout(2*time.Second + time.Nanosecond),
		WithLimits(Limits{InputBytes: 4096, OutputBytes: 2048}),
		WithNamedInference(client, model),
		WithSystemPrompt("Summarize the conversation.", "prompt-v1"),
		WithPolicyRevision("parser-v1"),
	}
}

func validCurrentOptions() []Option {
	return []Option{
		WithName("current-model-job"),
		WithParticipation(ParticipationBackground),
		WithTimeout(3 * time.Second),
		WithLimits(Limits{InputBytes: 1024, OutputBytes: 512}),
		WithCurrentLoopModel(),
		WithSystemPrompt("Classify the input.", "prompt-v2"),
		WithPolicyRevision("classifier-v1"),
	}
}

func validOutputSchema() inference.OutputSchema {
	return inference.OutputSchema{
		Name:        "classifier_result",
		Description: "SECRET output guidance",
		Schema: json.RawMessage(`{
			"type":"object",
			"properties":{"verdict":{"type":"string","enum":["allow","deny"]}},
			"required":["verdict"],
			"additionalProperties":false
		}`),
		Strict: true,
	}
}

func validEvidenceToolPolicy() EvidenceToolPolicy {
	return EvidenceToolPolicy{
		Revision: "evidence-policy-v1",
		Limits: ToolLoopLimits{
			MaxRounds:        3,
			MaxCalls:         6,
			MaxCallsPerRound: 2,
			MaxResultBytes:   4096,
			MaxEvidenceBytes: 8192,
		},
		Definitions: []tool.Definition{
			testEvidenceDefinition("workspace-status", tool.RequiresWorkspaceRead, []string{"workspace-status"}, nil),
			testEvidenceDefinition("git-evidence", tool.RequiresWorkspaceRead, []string{"git-diff", "git-status"}, nil),
		},
	}
}

func testEvidenceDefinition(name string, requirements tool.Requirements, names []string, factory tool.EvidenceFactory) tool.Definition {
	infos := make([]tool.ToolInfo, len(names))
	for i, produced := range names {
		infos[i] = tool.ToolInfo{
			Name:   produced,
			Desc:   "Read-only evidence for " + produced,
			Schema: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}},"required":["path"],"additionalProperties":false}`),
		}
	}
	return tool.NewEvidenceDefinition(name, requirements, infos, factory)
}

func TestEvidenceToolPolicyRejectsDefinitionsWithoutFrozenToolInfos(t *testing.T) {
	t.Parallel()
	policy := validEvidenceToolPolicy()
	policy.Definitions[0] = tool.NewDefinition("workspace-status", 0, nil)
	_, err := Define(append(validEvidenceOptionsWithoutPolicy(), WithEvidenceTools(policy))...)
	if err == nil {
		t.Fatal("Define() error = nil, want ordinary definition rejected")
	}
}

func TestEvidenceToolCatalogIdentityIncludesStaticModelFacingMetadataAndLoopPolicy(t *testing.T) {
	t.Parallel()
	define := func(policy EvidenceToolPolicy) Definition {
		definition, err := Define(append(validEvidenceOptionsWithoutPolicy(), WithEvidenceTools(policy))...)
		if err != nil {
			t.Fatalf("Define(): %v", err)
		}
		return definition
	}
	base := validEvidenceToolPolicy()
	baseDefinition := define(base)
	baseDescriptor := baseDefinition.Descriptor()

	tests := []struct {
		name   string
		mutate func(*EvidenceToolPolicy)
	}{
		{name: "static description", mutate: func(policy *EvidenceToolPolicy) {
			policy.Definitions[0] = tool.NewEvidenceDefinition(
				"workspace-status", tool.RequiresWorkspaceRead,
				[]tool.ToolInfo{{
					Name: "workspace-status", Desc: "Different read-only evidence",
					Schema: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}},"required":["path"],"additionalProperties":false}`),
				}}, nil,
			)
		}},
		{name: "static schema", mutate: func(policy *EvidenceToolPolicy) {
			policy.Definitions[0] = tool.NewEvidenceDefinition(
				"workspace-status", tool.RequiresWorkspaceRead,
				[]tool.ToolInfo{{
					Name: "workspace-status", Desc: "Read-only evidence for workspace-status",
					Schema: json.RawMessage(`{"type":"object","properties":{"path":{"type":"integer"}},"required":["path"],"additionalProperties":false}`),
				}}, nil,
			)
		}},
		{name: "loop limit", mutate: func(policy *EvidenceToolPolicy) { policy.Limits.MaxRounds++ }},
	}
	for _, testCase := range tests {
		testCase := testCase
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			changed := base.Clone()
			testCase.mutate(&changed)
			got := define(changed)
			if got.Descriptor().EvidenceToolDefinitionsSHA256 == baseDescriptor.EvidenceToolDefinitionsSHA256 {
				t.Fatal("evidence definition catalog digest did not change")
			}
			if got.PolicyRevision() == baseDefinition.PolicyRevision() {
				t.Fatal("hustle policy digest did not change")
			}
		})
	}
}

func TestEvidenceIdentityDomainsAreVersioned(t *testing.T) {
	t.Parallel()
	for name, testCase := range map[string]struct{ got, want string }{
		"policy":         {evidencePolicyDigestDomain, "looprig/hustle/evidence-policy/v1"},
		"definitions":    {evidenceDefinitionCatalogDigestDomain, "looprig/hustle/evidence-definition-catalog/v1"},
		"produced names": {evidenceProducedNamesDigestDomain, "looprig/hustle/evidence-produced-names/v1"},
		"bound tool":     {boundEvidenceToolDigestDomain, "looprig/hustle/bound-evidence-tool/v1"},
	} {
		if testCase.got != testCase.want {
			t.Fatalf("%s domain = %q, want %q", name, testCase.got, testCase.want)
		}
	}
}

func validEvidenceOptions() []Option {
	options := validNamedOptions(&testClient{}, validModel("evidence"))
	options = replaceOption(options, 1, WithParticipation(ParticipationBlocking))
	options = append(options, WithOutputSchema(validOutputSchema()), WithEvidenceTools(validEvidenceToolPolicy()))
	return options
}

func TestEvidenceToolsRequireCompleteValidPolicy(t *testing.T) {
	t.Parallel()
	base := validEvidenceToolPolicy()
	typedNil := reflect.Zero(reflect.TypeOf(base.Definitions[0])).Interface().(tool.Definition)
	tests := []struct {
		name   string
		mutate func(*EvidenceToolPolicy)
	}{
		{name: "missing revision", mutate: func(p *EvidenceToolPolicy) { p.Revision = "" }},
		{name: "blank revision", mutate: func(p *EvidenceToolPolicy) { p.Revision = " \t" }},
		{name: "leading whitespace revision", mutate: func(p *EvidenceToolPolicy) { p.Revision = " evidence-policy-v1" }},
		{name: "trailing whitespace revision", mutate: func(p *EvidenceToolPolicy) { p.Revision = "evidence-policy-v1 " }},
		{name: "nul revision", mutate: func(p *EvidenceToolPolicy) { p.Revision = "evidence\x00policy" }},
		{name: "invalid utf8 revision", mutate: func(p *EvidenceToolPolicy) { p.Revision = "policy-\xff" }},
		{name: "overlong revision", mutate: func(p *EvidenceToolPolicy) { p.Revision = strings.Repeat("r", MaxEvidenceToolPolicyRevisionBytes+1) }},
		{name: "zero rounds", mutate: func(p *EvidenceToolPolicy) { p.Limits.MaxRounds = 0 }},
		{name: "zero calls", mutate: func(p *EvidenceToolPolicy) { p.Limits.MaxCalls = 0 }},
		{name: "zero calls per round", mutate: func(p *EvidenceToolPolicy) { p.Limits.MaxCallsPerRound = 0 }},
		{name: "zero result bytes", mutate: func(p *EvidenceToolPolicy) { p.Limits.MaxResultBytes = 0 }},
		{name: "zero evidence bytes", mutate: func(p *EvidenceToolPolicy) { p.Limits.MaxEvidenceBytes = 0 }},
		{name: "excessive rounds", mutate: func(p *EvidenceToolPolicy) { p.Limits.MaxRounds = maxToolLoopCount + 1 }},
		{name: "excessive calls", mutate: func(p *EvidenceToolPolicy) { p.Limits.MaxCalls = maxToolLoopCount + 1 }},
		{name: "excessive result bytes", mutate: func(p *EvidenceToolPolicy) {
			p.Limits.MaxResultBytes = maxPayloadBytes + 1
			p.Limits.MaxEvidenceBytes = maxPayloadBytes + 1
		}},
		{name: "excessive evidence bytes", mutate: func(p *EvidenceToolPolicy) { p.Limits.MaxEvidenceBytes = maxPayloadBytes + 1 }},
		{name: "calls per round exceeds total", mutate: func(p *EvidenceToolPolicy) { p.Limits.MaxCallsPerRound = p.Limits.MaxCalls + 1 }},
		{name: "result exceeds evidence", mutate: func(p *EvidenceToolPolicy) { p.Limits.MaxResultBytes = p.Limits.MaxEvidenceBytes + 1 }},
		{name: "no definitions", mutate: func(p *EvidenceToolPolicy) { p.Definitions = nil }},
		{name: "nil definition", mutate: func(p *EvidenceToolPolicy) { p.Definitions[0] = nil }},
		{name: "typed nil definition", mutate: func(p *EvidenceToolPolicy) { p.Definitions[0] = typedNil }},
		{name: "blank definition name", mutate: func(p *EvidenceToolPolicy) {
			p.Definitions[0] = tool.NewDefinition(" ", tool.RequiresWorkspace, nil)
		}},
		{name: "noncanonical definition name", mutate: func(p *EvidenceToolPolicy) {
			p.Definitions[0] = tool.NewDefinition(" workspace-status ", tool.RequiresWorkspace, nil)
		}},
		{name: "duplicate definition name", mutate: func(p *EvidenceToolPolicy) {
			p.Definitions[1] = tool.NewDefinition("workspace-status", tool.RequiresWorkspace, nil)
		}},
		{name: "no produced names", mutate: func(p *EvidenceToolPolicy) {
			p.Definitions[0] = tool.NewBundleDefinition("workspace-status", nil, tool.RequiresWorkspace, nil)
		}},
		{name: "blank produced name", mutate: func(p *EvidenceToolPolicy) {
			p.Definitions[0] = tool.NewBundleDefinition("workspace-status", []string{" "}, tool.RequiresWorkspace, nil)
		}},
		{name: "noncanonical produced name", mutate: func(p *EvidenceToolPolicy) {
			p.Definitions[0] = tool.NewBundleDefinition("workspace-status", []string{" status "}, tool.RequiresWorkspace, nil)
		}},
		{name: "duplicate produced name in definition", mutate: func(p *EvidenceToolPolicy) {
			p.Definitions[0] = tool.NewBundleDefinition("workspace-status", []string{"status", "status"}, tool.RequiresWorkspace, nil)
		}},
		{name: "duplicate produced name across definitions", mutate: func(p *EvidenceToolPolicy) {
			p.Definitions[1] = tool.NewBundleDefinition("git-evidence", []string{"workspace-status"}, tool.RequiresWorkspace, nil)
		}},
		{name: "delegate controller requirement", mutate: func(p *EvidenceToolPolicy) {
			p.Definitions[0] = tool.NewDefinition("delegate", tool.RequiresDelegateController, nil)
		}},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			policy := base.Clone()
			testCase.mutate(&policy)
			_, err := Define(append(validEvidenceOptionsWithoutPolicy(), WithEvidenceTools(policy))...)
			var definitionErr *DefinitionError
			if !errors.As(err, &definitionErr) || definitionErr.Kind != DefinitionInvalidEvidenceTools {
				t.Fatalf("Define() error = %T %v, want invalid evidence tools", err, err)
			}
			if strings.Contains(err.Error(), policy.Revision) && policy.Revision != "" {
				t.Fatalf("error echoed evidence policy revision: %v", err)
			}
		})
	}
}

func TestEvidenceToolPolicyAcceptsExactBoundariesAndCurrentLoopModel(t *testing.T) {
	t.Parallel()
	policy := validEvidenceToolPolicy()
	policy.Revision = strings.Repeat("r", MaxEvidenceToolPolicyRevisionBytes)
	policy.Limits = ToolLoopLimits{
		MaxRounds:        maxToolLoopCount,
		MaxCalls:         maxToolLoopCount,
		MaxCallsPerRound: maxToolLoopCount,
		MaxResultBytes:   maxPayloadBytes,
		MaxEvidenceBytes: maxPayloadBytes,
	}
	options := replaceOption(validCurrentOptions(), 1, WithParticipation(ParticipationBlocking))
	options = append(options, WithOutputSchema(validOutputSchema()), WithEvidenceTools(policy))
	if _, err := Define(options...); err != nil {
		t.Fatalf("Define(exact evidence boundaries with current-loop model) error = %v", err)
	}
}

func TestEvidenceToolPolicyCatalogBoundaries(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		definitions []tool.Definition
		wantErr     bool
	}{
		{
			name:        "exact definition limit",
			definitions: evidenceDefinitions(MaxEvidenceToolDefinitions, 1),
		},
		{
			name:        "definition limit plus one",
			definitions: evidenceDefinitions(MaxEvidenceToolDefinitions+1, 1),
			wantErr:     true,
		},
		{
			name:        "exact aggregate produced-name limit",
			definitions: evidenceDefinitions(MaxEvidenceToolDefinitions, MaxEvidenceProducedToolNames/MaxEvidenceToolDefinitions),
		},
		{
			name:        "aggregate produced-name limit plus one",
			definitions: evidenceDefinitionsWithProducedCount(MaxEvidenceToolDefinitions, MaxEvidenceProducedToolNames+1),
			wantErr:     true,
		},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			policy := validEvidenceToolPolicy()
			policy.Definitions = testCase.definitions
			_, err := Define(append(validEvidenceOptionsWithoutPolicy(), WithEvidenceTools(policy))...)
			if (err != nil) != testCase.wantErr {
				t.Fatalf("Define() error = %v, wantErr %v", err, testCase.wantErr)
			}
			if testCase.wantErr {
				var definitionErr *DefinitionError
				if !errors.As(err, &definitionErr) || definitionErr.Kind != DefinitionInvalidEvidenceTools || len(err.Error()) > 256 {
					t.Fatalf("Define() error = %T %v, want bounded invalid evidence tools", err, err)
				}
			}
		})
	}
}

func TestEvidenceToolPolicyNameByteBoundaries(t *testing.T) {
	t.Parallel()
	exactASCII := strings.Repeat("a", MaxEvidenceToolNameBytes)
	exactUTF8 := strings.Repeat("é", MaxEvidenceToolNameBytes/2)
	overUTF8 := exactUTF8 + "a"
	tests := []struct {
		name           string
		definitionName string
		producedName   string
		wantErr        bool
	}{
		{name: "exact ASCII definition and produced name", definitionName: exactASCII, producedName: exactASCII},
		{name: "nonportable UTF-8 tool name", definitionName: exactUTF8, producedName: exactUTF8, wantErr: true},
		{name: "definition name one byte over", definitionName: overUTF8, producedName: "produced", wantErr: true},
		{name: "produced name one byte over", definitionName: "definition", producedName: overUTF8, wantErr: true},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			policy := validEvidenceToolPolicy()
			policy.Definitions = []tool.Definition{testEvidenceDefinition(
				testCase.definitionName, tool.RequiresWorkspaceRead, []string{testCase.producedName}, nil,
			)}
			_, err := Define(append(validEvidenceOptionsWithoutPolicy(), WithEvidenceTools(policy))...)
			if (err != nil) != testCase.wantErr {
				t.Fatalf("Define() error = %v, wantErr %v", err, testCase.wantErr)
			}
		})
	}
}

func evidenceDefinitions(definitionCount, producedPerDefinition int) []tool.Definition {
	definitions := make([]tool.Definition, definitionCount)
	for definitionIndex := range definitionCount {
		produced := make([]string, producedPerDefinition)
		for producedIndex := range producedPerDefinition {
			produced[producedIndex] = fmt.Sprintf("tool-%03d-%03d", definitionIndex, producedIndex)
		}
		definitions[definitionIndex] = testEvidenceDefinition(
			fmt.Sprintf("definition-%03d", definitionIndex),
			tool.RequiresWorkspaceRead,
			produced,
			nil,
		)
	}
	return definitions
}

func evidenceDefinitionsWithProducedCount(definitionCount, producedCount int) []tool.Definition {
	definitions := make([]tool.Definition, 0, definitionCount)
	remaining := producedCount
	for definitionIndex := range definitionCount {
		count := remaining / (definitionCount - definitionIndex)
		produced := make([]string, count)
		for producedIndex := range count {
			produced[producedIndex] = fmt.Sprintf("tool-%03d-%03d", definitionIndex, producedIndex)
		}
		definitions = append(definitions, tool.NewBundleDefinition(
			fmt.Sprintf("definition-%03d", definitionIndex),
			produced,
			tool.RequiresWorkspace,
			nil,
		))
		remaining -= count
	}
	return definitions
}

func TestZeroEvidenceToolPolicyPreservesToollessIdentity(t *testing.T) {
	t.Parallel()
	without, err := Define(validCurrentOptions()...)
	if err != nil {
		t.Fatal(err)
	}
	withZero, err := Define(append(validCurrentOptions(), WithEvidenceTools(EvidenceToolPolicy{}))...)
	if err != nil {
		t.Fatalf("Define(zero evidence policy) error = %v", err)
	}
	if withZero.Descriptor() != without.Descriptor() || withZero.PolicyRevision() != without.PolicyRevision() {
		t.Fatalf("zero evidence policy changed tool-less identity:\n%#v\n%#v", without.Descriptor(), withZero.Descriptor())
	}
	if policy, ok := withZero.EvidenceToolPolicy(); ok || policy.Revision != "" || policy.Limits != (ToolLoopLimits{}) || policy.Definitions != nil {
		t.Fatalf("EvidenceToolPolicy() = %#v, %v, want zero,false", policy, ok)
	}
}

func validEvidenceOptionsWithoutPolicy() []Option {
	options := validNamedOptions(&testClient{}, validModel("evidence"))
	options = replaceOption(options, 1, WithParticipation(ParticipationBlocking))
	return append(options, WithOutputSchema(validOutputSchema()))
}

func TestEvidenceToolsRequireStructuredBlockingDefinition(t *testing.T) {
	t.Parallel()
	policy := validEvidenceToolPolicy()
	tests := []struct {
		name string
		opts []Option
	}{
		{name: "missing structured output", opts: append(validNamedOptions(&testClient{}, validModel("evidence")), WithEvidenceTools(policy))},
		{name: "background participation", opts: append(validCurrentOptions(), WithOutputSchema(validOutputSchema()), WithEvidenceTools(policy))},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			_, err := Define(testCase.opts...)
			var definitionErr *DefinitionError
			if !errors.As(err, &definitionErr) || definitionErr.Kind != DefinitionInvalidEvidenceTools {
				t.Fatalf("Define() error = %T %v, want invalid evidence tools", err, err)
			}
		})
	}
}

func TestEvidenceToolsDuplicateOptionRejected(t *testing.T) {
	t.Parallel()
	policy := validEvidenceToolPolicy()
	_, err := Define(append(validEvidenceOptions(), WithEvidenceTools(policy))...)
	var definitionErr *DefinitionError
	if !errors.As(err, &definitionErr) || definitionErr.Kind != DefinitionDuplicateOption || definitionErr.Field != "evidence_tools" {
		t.Fatalf("Define() error = %T %v, want duplicate evidence_tools option", err, err)
	}
}

func TestEvidenceToolPolicyIsImmutable(t *testing.T) {
	t.Parallel()
	policy := validEvidenceToolPolicy()
	wantFirst := policy.Definitions[0]
	option := WithEvidenceTools(policy)
	policy.Revision = "mutated"
	policy.Limits.MaxCalls++
	policy.Definitions[0] = policy.Definitions[1]
	policy.Definitions = append(policy.Definitions, policy.Definitions[1])

	definition, err := Define(append(validEvidenceOptionsWithoutPolicy(), option)...)
	if err != nil {
		t.Fatalf("Define() error = %v", err)
	}
	got, ok := definition.EvidenceToolPolicy()
	if !ok || got.Revision != "evidence-policy-v1" || got.Limits != validEvidenceToolPolicy().Limits || len(got.Definitions) != 2 || got.Definitions[0] != wantFirst {
		t.Fatalf("EvidenceToolPolicy() = %#v, %v", got, ok)
	}
	got.Definitions[0] = got.Definitions[1]
	again, ok := definition.EvidenceToolPolicy()
	if !ok || again.Definitions[0] != wantFirst {
		t.Fatal("EvidenceToolPolicy accessor exposed mutable definition slice")
	}
}

func TestEvidenceToolsDescriptorIsSecretFreeAndIdentityComplete(t *testing.T) {
	t.Parallel()
	define := func(t *testing.T, policy EvidenceToolPolicy) Definition {
		t.Helper()
		definition, err := Define(append(validEvidenceOptionsWithoutPolicy(), WithEvidenceTools(policy))...)
		if err != nil {
			t.Fatalf("Define() error = %v", err)
		}
		return definition
	}
	basePolicy := validEvidenceToolPolicy()
	base := define(t, basePolicy)
	descriptor := base.Descriptor()
	if descriptor.EvidenceToolPolicyRevision != basePolicy.Revision ||
		descriptor.EvidenceToolDefinitionCount != len(basePolicy.Definitions) ||
		descriptor.EvidenceToolLimits != basePolicy.Limits ||
		descriptor.EvidenceToolDefinitionsSHA256 == ([sha256.Size]byte{}) ||
		descriptor.EvidenceProducedToolNamesSHA256 == ([sha256.Size]byte{}) ||
		!descriptor.StructuredOutputWithTools {
		t.Fatalf("evidence descriptor incomplete: %#v", descriptor)
	}
	encoded, err := json.Marshal(descriptor)
	if err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{"workspace-status", "git-evidence", "git-diff", "git-status"} {
		if bytes.Contains(encoded, []byte(raw)) {
			t.Fatalf("descriptor leaked raw evidence metadata %q: %s", raw, encoded)
		}
	}

	mutations := []struct {
		name   string
		mutate func(*EvidenceToolPolicy)
	}{
		{name: "revision", mutate: func(p *EvidenceToolPolicy) { p.Revision = "evidence-policy-v2" }},
		{name: "rounds", mutate: func(p *EvidenceToolPolicy) { p.Limits.MaxRounds++ }},
		{name: "calls", mutate: func(p *EvidenceToolPolicy) { p.Limits.MaxCalls++ }},
		{name: "calls per round", mutate: func(p *EvidenceToolPolicy) { p.Limits.MaxCallsPerRound++ }},
		{name: "result bytes", mutate: func(p *EvidenceToolPolicy) { p.Limits.MaxResultBytes++ }},
		{name: "evidence bytes", mutate: func(p *EvidenceToolPolicy) { p.Limits.MaxEvidenceBytes++ }},
		{name: "definition name", mutate: func(p *EvidenceToolPolicy) {
			p.Definitions[0] = testEvidenceDefinition("workspace-tree", tool.RequiresWorkspaceRead, []string{"workspace-status"}, nil)
		}},
		{name: "definition requirements", mutate: func(p *EvidenceToolPolicy) {
			p.Definitions[0] = testEvidenceDefinition("workspace-status", 0, []string{"workspace-status"}, nil)
		}},
		{name: "produced name", mutate: func(p *EvidenceToolPolicy) {
			p.Definitions[1] = testEvidenceDefinition("git-evidence", tool.RequiresWorkspaceRead, []string{"git-diff", "git-log"}, nil)
		}},
		{name: "produced name order", mutate: func(p *EvidenceToolPolicy) {
			p.Definitions[1] = testEvidenceDefinition("git-evidence", tool.RequiresWorkspaceRead, []string{"git-status", "git-diff"}, nil)
		}},
		{name: "definition order", mutate: func(p *EvidenceToolPolicy) {
			p.Definitions[0], p.Definitions[1] = p.Definitions[1], p.Definitions[0]
		}},
	}
	for _, testCase := range mutations {
		t.Run(testCase.name, func(t *testing.T) {
			policy := basePolicy.Clone()
			testCase.mutate(&policy)
			changed := define(t, policy)
			if changed.PolicyRevision() == base.PolicyRevision() {
				t.Fatal("behavioral mutation did not change policy identity")
			}
		})
	}
}

func TestDefineValidDefinitions(t *testing.T) {
	t.Parallel()
	client := &testClient{identity: "named"}
	tests := []struct {
		name          string
		opts          []Option
		wantName      Name
		wantSource    ModelSource
		wantPart      Participation
		wantTimeout   time.Duration
		wantNamedKey  model.ModelKey
		wantPromptRev string
	}{
		{
			name: "named model", opts: validNamedOptions(client, validModel("named-model")),
			wantName: "conversation-compaction", wantSource: ModelSourceNamed,
			wantPart: ParticipationBlocking, wantTimeout: 2*time.Second + time.Nanosecond,
			wantNamedKey:  model.ModelKey{Provider: "test-provider", Model: "named-model"},
			wantPromptRev: "prompt-v1",
		},
		{
			name: "current loop model", opts: validCurrentOptions(),
			wantName: "current-model-job", wantSource: ModelSourceCurrentLoop,
			wantPart: ParticipationBackground, wantTimeout: 3 * time.Second,
			wantPromptRev: "prompt-v2",
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			definition, err := Define(tt.opts...)
			if err != nil {
				t.Fatalf("Define() error = %v", err)
			}
			descriptor := definition.Descriptor()
			if definition.Name() != tt.wantName || definition.Participation() != tt.wantPart || definition.Timeout() != tt.wantTimeout {
				t.Fatalf("definition accessors = (%q,%d,%s), want (%q,%d,%s)", definition.Name(), definition.Participation(), definition.Timeout(), tt.wantName, tt.wantPart, tt.wantTimeout)
			}
			if descriptor.ModelSource != tt.wantSource || descriptor.NamedModelKey != tt.wantNamedKey || descriptor.PromptRevision != tt.wantPromptRev {
				t.Fatalf("Descriptor() = %#v, want source %d, key %#v, prompt revision %q", descriptor, tt.wantSource, tt.wantNamedKey, tt.wantPromptRev)
			}
			if descriptor.TimeoutNanos != int64(tt.wantTimeout) || definition.PolicyRevision() == "" {
				t.Fatalf("timeout/policy = (%d,%q), want (%d,non-empty)", descriptor.TimeoutNanos, definition.PolicyRevision(), int64(tt.wantTimeout))
			}
		})
	}
}

func TestDefineValidation(t *testing.T) {
	t.Parallel()
	client := &testClient{}
	model := validModel("model")
	typedNilClient := (*testClient)(nil)
	tests := []struct {
		name  string
		opts  []Option
		kind  DefinitionErrorKind
		field string
	}{
		{name: "no options", opts: nil, kind: DefinitionMissingName},
		{name: "nil option", opts: append(validNamedOptions(client, model), nil), kind: DefinitionNilOption},
		{name: "duplicate name", opts: append(validNamedOptions(client, model), WithName("other")), kind: DefinitionDuplicateOption},
		{name: "duplicate participation", opts: append(validNamedOptions(client, model), WithParticipation(ParticipationBackground)), kind: DefinitionDuplicateOption},
		{name: "duplicate timeout", opts: append(validNamedOptions(client, model), WithTimeout(time.Second)), kind: DefinitionDuplicateOption},
		{name: "duplicate limits", opts: append(validNamedOptions(client, model), WithLimits(Limits{InputBytes: 1, OutputBytes: 1})), kind: DefinitionDuplicateOption},
		{name: "duplicate named source", opts: append(validNamedOptions(client, model), WithNamedInference(client, model)), kind: DefinitionDuplicateOption},
		{name: "model source collision", opts: append(validNamedOptions(client, model), WithCurrentLoopModel()), kind: DefinitionDuplicateOption},
		{name: "duplicate system prompt", opts: append(validNamedOptions(client, model), WithSystemPrompt("other", "prompt-v2")), kind: DefinitionDuplicateOption},
		{name: "duplicate policy revision", opts: append(validNamedOptions(client, model), WithPolicyRevision("other")), kind: DefinitionDuplicateOption},
		{name: "duplicate output schema", opts: append(append(validNamedOptions(client, model), WithOutputSchema(validOutputSchema())), WithOutputSchema(validOutputSchema())), kind: DefinitionDuplicateOption},
		{name: "invalid output schema", opts: append(validNamedOptions(client, model), WithOutputSchema(inference.OutputSchema{Name: "invalid", Schema: json.RawMessage(`{"type":"array"}`)})), kind: DefinitionInvalidOutputSchema},
		{name: "blank name", opts: replaceOption(validNamedOptions(client, model), 0, WithName(" \t")), kind: DefinitionMissingName},
		{name: "reserved name", opts: replaceOption(validNamedOptions(client, model), 0, WithName("_looprig.internal")), kind: DefinitionReservedName},
		{name: "long name accepted", opts: replaceOption(validNamedOptions(client, model), 0, WithName(Name(strings.Repeat("n", 129))))},
		{name: "missing participation", opts: withoutOption(validNamedOptions(client, model), 1), kind: DefinitionInvalidParticipation},
		{name: "unknown participation", opts: replaceOption(validNamedOptions(client, model), 1, WithParticipation(Participation(99))), kind: DefinitionInvalidParticipation},
		{name: "missing model source", opts: withoutOption(validNamedOptions(client, model), 4), kind: DefinitionMissingModelSource},
		{name: "nil named client", opts: replaceOption(validNamedOptions(client, model), 4, WithNamedInference(nil, model)), kind: DefinitionInvalidClient},
		{name: "typed nil named client", opts: replaceOption(validNamedOptions(client, model), 4, WithNamedInference(typedNilClient, model)), kind: DefinitionInvalidClient},
		{name: "invalid named model", opts: replaceOption(validNamedOptions(client, model), 4, WithNamedInference(client, zeroInferenceModel())), kind: DefinitionInvalidModel},
		{name: "model missing durable provider", opts: replaceOption(validNamedOptions(client, model), 4, WithNamedInference(client, modelWithoutProvider(model))), kind: DefinitionInvalidModel},
		{name: "invalid named model effort", opts: replaceOption(validNamedOptions(client, model), 4, WithNamedInference(client, modelWithEffort(model, invalidInferenceEffort()))), kind: DefinitionInvalidModel},
		{name: "named nan temperature", opts: replaceOption(validNamedOptions(client, model), 4, WithNamedInference(client, modelWithTemperature(model, math.NaN()))), kind: DefinitionInvalidModel, field: "model.sampling.temperature"},
		{name: "named positive infinity temperature", opts: replaceOption(validNamedOptions(client, model), 4, WithNamedInference(client, modelWithTemperature(model, math.Inf(1)))), kind: DefinitionInvalidModel, field: "model.sampling.temperature"},
		{name: "named negative infinity temperature", opts: replaceOption(validNamedOptions(client, model), 4, WithNamedInference(client, modelWithTemperature(model, math.Inf(-1)))), kind: DefinitionInvalidModel, field: "model.sampling.temperature"},
		{name: "named nan top p", opts: replaceOption(validNamedOptions(client, model), 4, WithNamedInference(client, modelWithTopP(model, math.NaN()))), kind: DefinitionInvalidModel, field: "model.sampling.top_p"},
		{name: "named positive infinity top p", opts: replaceOption(validNamedOptions(client, model), 4, WithNamedInference(client, modelWithTopP(model, math.Inf(1)))), kind: DefinitionInvalidModel, field: "model.sampling.top_p"},
		{name: "named negative infinity top p", opts: replaceOption(validNamedOptions(client, model), 4, WithNamedInference(client, modelWithTopP(model, math.Inf(-1)))), kind: DefinitionInvalidModel, field: "model.sampling.top_p"},
		{name: "zero timeout", opts: replaceOption(validNamedOptions(client, model), 2, WithTimeout(0)), kind: DefinitionInvalidTimeout},
		{name: "negative timeout", opts: replaceOption(validNamedOptions(client, model), 2, WithTimeout(-time.Nanosecond)), kind: DefinitionInvalidTimeout},
		{name: "long timeout accepted", opts: replaceOption(validNamedOptions(client, model), 2, WithTimeout(24*time.Hour+time.Nanosecond))},
		{name: "zero input limit", opts: replaceOption(validNamedOptions(client, model), 3, WithLimits(Limits{InputBytes: 0, OutputBytes: 1})), kind: DefinitionInvalidLimits},
		{name: "negative output limit", opts: replaceOption(validNamedOptions(client, model), 3, WithLimits(Limits{InputBytes: 1, OutputBytes: -1})), kind: DefinitionInvalidLimits},
		{name: "excessive input limit", opts: replaceOption(validNamedOptions(client, model), 3, WithLimits(Limits{InputBytes: maxPayloadBytes + 1, OutputBytes: 1})), kind: DefinitionInvalidLimits},
		{name: "excessive output limit", opts: replaceOption(validNamedOptions(client, model), 3, WithLimits(Limits{InputBytes: 1, OutputBytes: maxPayloadBytes + 1})), kind: DefinitionInvalidLimits},
		{name: "blank system prompt", opts: replaceOption(validNamedOptions(client, model), 5, WithSystemPrompt(" \n", "prompt-v1")), kind: DefinitionInvalidSystemPrompt},
		{name: "long system prompt accepted", opts: replaceOption(validNamedOptions(client, model), 5, WithSystemPrompt(strings.Repeat("p", 256*1024+1), "prompt-v1"))},
		{name: "blank prompt revision", opts: replaceOption(validNamedOptions(client, model), 5, WithSystemPrompt("prompt", " \t")), kind: DefinitionInvalidPromptRevision},
		{name: "long prompt revision accepted", opts: replaceOption(validNamedOptions(client, model), 5, WithSystemPrompt("prompt", strings.Repeat("r", 257)))},
		{name: "missing policy revision", opts: withoutOption(validNamedOptions(client, model), 6), kind: DefinitionMissingPolicyRevision},
		{name: "blank policy revision", opts: replaceOption(validNamedOptions(client, model), 6, WithPolicyRevision("")), kind: DefinitionInvalidPolicyRevision},
		{name: "long policy revision accepted", opts: replaceOption(validNamedOptions(client, model), 6, WithPolicyRevision(strings.Repeat("r", 257)))},
		{name: "minimum boundaries", opts: []Option{WithName("n"), WithParticipation(ParticipationBlocking), WithTimeout(time.Nanosecond), WithLimits(Limits{InputBytes: 1, OutputBytes: 1}), WithNamedInference(client, model), WithSystemPrompt("p", "r"), WithPolicyRevision("r")}},
		{name: "maximum payload boundaries", opts: []Option{WithName("payload-boundary"), WithParticipation(ParticipationBackground), WithTimeout(time.Second), WithLimits(Limits{InputBytes: maxPayloadBytes, OutputBytes: maxPayloadBytes}), WithCurrentLoopModel(), WithSystemPrompt("p", "r"), WithPolicyRevision("r")}},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := Define(tt.opts...)
			if tt.kind == "" {
				if err != nil {
					t.Fatalf("Define() boundary error = %v", err)
				}
				return
			}
			var definitionErr *DefinitionError
			if !errors.As(err, &definitionErr) || definitionErr.Kind != tt.kind {
				t.Fatalf("Define() error = %T %v, want *DefinitionError kind %q", err, err, tt.kind)
			}
			if tt.field != "" && (definitionErr.Field != tt.field || definitionErr.Cause != nil) {
				t.Fatalf("Define() error field/cause = (%q,%v), want (%q,nil)", definitionErr.Field, definitionErr.Cause, tt.field)
			}
		})
	}
}

func TestOutputSchemaOptionIsImmutable(t *testing.T) {
	t.Parallel()
	input := validOutputSchema()
	want := input.Clone()
	option := WithOutputSchema(input)
	input.Name = "mutated"
	input.Description = "mutated"
	input.Schema[0] = '['
	input.Strict = false

	definition, err := Define(append(validCurrentOptions(), option)...)
	if err != nil {
		t.Fatalf("Define() error = %v", err)
	}
	bound, err := definition.Bind(context.Background(), Bindings{Models: &testResolver{}})
	if err != nil {
		t.Fatalf("Bind() error = %v", err)
	}
	first, ok := bound.OutputSchema()
	if !ok || first == nil {
		t.Fatal("OutputSchema() = absent, want configured output")
	}
	if first.Name != want.Name || first.Description != want.Description || first.Strict != want.Strict || !bytes.Equal(first.Schema, want.Schema) {
		t.Fatalf("OutputSchema() = %#v, want frozen %#v", first, want)
	}
	first.Name = "accessor-mutated"
	first.Description = "accessor-mutated"
	first.Schema[0] = '['
	first.Strict = false
	second, ok := bound.OutputSchema()
	if !ok || second == nil || second.Name != want.Name || second.Description != want.Description || second.Strict != want.Strict || !bytes.Equal(second.Schema, want.Schema) {
		t.Fatalf("second OutputSchema() = %#v, want independent clone %#v", second, want)
	}

	other, err := Define(append(validCurrentOptions(), option)...)
	if err != nil {
		t.Fatalf("Define(reused option) error = %v", err)
	}
	otherBound, err := other.Bind(context.Background(), Bindings{Models: &testResolver{}})
	if err != nil {
		t.Fatalf("Bind(reused option) error = %v", err)
	}
	third, ok := otherBound.OutputSchema()
	if !ok || third == nil || !bytes.Equal(third.Schema, want.Schema) {
		t.Fatalf("reused option OutputSchema() = %#v, want frozen %#v", third, want)
	}
}

func TestOutputSchemaAbsentPreservesLegacyIdentity(t *testing.T) {
	t.Parallel()
	definition, err := Define(validCurrentOptions()...)
	if err != nil {
		t.Fatalf("Define() error = %v", err)
	}
	bound, err := definition.Bind(context.Background(), Bindings{Models: &testResolver{}})
	if err != nil {
		t.Fatalf("Bind() error = %v", err)
	}
	if output, ok := bound.OutputSchema(); ok || output != nil {
		t.Fatalf("OutputSchema() = (%#v,%v), want (nil,false)", output, ok)
	}
	encoded, err := json.Marshal(definition.Descriptor())
	if err != nil {
		t.Fatalf("json.Marshal(Descriptor()) error = %v", err)
	}
	const wantDescriptor = `{"Name":"current-model-job","Participation":2,"ModelSource":1,"NamedModelKey":{"Provider":"","Model":""},"NamedModelPolicyRevision":"","PromptRevision":"prompt-v2","PromptSHA256":[96,237,206,114,218,60,219,9,3,59,125,91,20,95,2,115,56,120,242,198,248,226,184,42,138,16,158,155,28,35,152,108],"PolicyRevision":"classifier-v1","TimeoutNanos":3000000000,"Limits":{"InputBytes":1024,"OutputBytes":512}}`
	const wantPolicyRevision = "a68d812281998bcf7a364da8b7bdd3c25ce95a42c844e1391dad82d7931e3a42"
	if string(encoded) != wantDescriptor || definition.PolicyRevision() != wantPolicyRevision {
		t.Fatalf("tool-less fixture drifted:\n%s\n%s", encoded, definition.PolicyRevision())
	}
	for _, key := range []string{"OutputSchemaName", "OutputSchemaSHA256", "StructuredOutputRevision"} {
		if bytes.Contains(encoded, []byte(key)) {
			t.Fatalf("absent output changed legacy descriptor JSON with %q: %s", key, encoded)
		}
	}
}

func TestOutputSchemaValidationErrorDoesNotRetainSchema(t *testing.T) {
	t.Parallel()
	const secret = "raw-schema-secret"
	output := inference.OutputSchema{
		Name:   "classifier_result",
		Schema: json.RawMessage(`{"type":"object","properties":{},"required":[],"additionalProperties":false,"` + secret + `":true}`),
	}
	_, err := Define(append(validCurrentOptions(), WithOutputSchema(output))...)
	var definitionErr *DefinitionError
	if !errors.As(err, &definitionErr) || definitionErr.Kind != DefinitionInvalidOutputSchema || definitionErr.Field != "output_schema" {
		t.Fatalf("Define() error = %T %v, want invalid output schema DefinitionError", err, err)
	}
	if strings.Contains(err.Error(), secret) || strings.Contains(fmt.Sprint(definitionErr.Cause), secret) {
		t.Fatalf("validation error retained raw schema: %v / %v", err, definitionErr.Cause)
	}
}

func TestOutputSchemaDescriptorIdentity(t *testing.T) {
	t.Parallel()
	baseOutput := validOutputSchema()
	define := func(t *testing.T, output inference.OutputSchema) Definition {
		t.Helper()
		definition, err := Define(append(validCurrentOptions(), WithOutputSchema(output))...)
		if err != nil {
			t.Fatalf("Define() error = %v", err)
		}
		return definition
	}
	base := define(t, baseOutput)
	descriptor := base.Descriptor()
	if descriptor.OutputSchemaName != baseOutput.Name || descriptor.StructuredOutputRevision != inference.StructuredOutputRevision {
		t.Fatalf("output descriptor identity = (%q,%q), want (%q,%q)", descriptor.OutputSchemaName, descriptor.StructuredOutputRevision, baseOutput.Name, inference.StructuredOutputRevision)
	}
	if descriptor.OutputSchemaSHA256 == ([sha256.Size]byte{}) {
		t.Fatal("OutputSchemaSHA256 is zero")
	}
	wantOutputDigest := sha256.Sum256([]byte(`{"description":"SECRET output guidance","schema":{"type":"object","properties":{"verdict":{"type":"string","enum":["allow","deny"]}},"required":["verdict"],"additionalProperties":false},"strict":true}`))
	if descriptor.OutputSchemaSHA256 != wantOutputDigest {
		t.Fatalf("OutputSchemaSHA256 = %x, want canonical behavioral digest %x", descriptor.OutputSchemaSHA256, wantOutputDigest)
	}
	encoded, err := json.Marshal(descriptor)
	if err != nil {
		t.Fatalf("json.Marshal(Descriptor()) error = %v", err)
	}
	for _, secret := range []string{string(baseOutput.Schema), baseOutput.Description, "SECRET"} {
		if bytes.Contains(encoded, []byte(secret)) {
			t.Fatalf("descriptor leaked output policy %q: %s", secret, encoded)
		}
	}

	whitespace := baseOutput.Clone()
	whitespace.Schema = json.RawMessage(`{"type":"object","properties":{"verdict":{"type":"string","enum":["allow","deny"]}},"required":["verdict"],"additionalProperties":false}`)
	if got := define(t, whitespace); got.PolicyRevision() != base.PolicyRevision() || got.Descriptor().OutputSchemaSHA256 != descriptor.OutputSchemaSHA256 {
		t.Fatal("insignificant schema whitespace changed output identity")
	}
	changedName := baseOutput.Clone()
	changedName.Name = "classifier_result_v2"
	changedSchema := baseOutput.Clone()
	changedSchema.Schema = json.RawMessage(`{"type":"object","properties":{"verdict":{"type":"boolean"}},"required":["verdict"],"additionalProperties":false}`)
	changedDescription := baseOutput.Clone()
	changedDescription.Description = "different guidance"
	changedStrict := baseOutput.Clone()
	changedStrict.Strict = false
	for _, testCase := range []struct {
		name   string
		output inference.OutputSchema
	}{
		{name: "name", output: changedName},
		{name: "schema", output: changedSchema},
		{name: "description", output: changedDescription},
		{name: "strict", output: changedStrict},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			if got := define(t, testCase.output).PolicyRevision(); got == base.PolicyRevision() {
				t.Fatalf("PolicyRevision() unchanged: %q", got)
			}
		})
	}
	revisedDescriptor := descriptor
	revisedDescriptor.StructuredOutputRevision = "structured-output/v2"
	revisedDigest, err := digestDescriptorPolicy(revisedDescriptor)
	if err != nil {
		t.Fatalf("digestDescriptorPolicy(revised) error = %v", err)
	}
	if revisedDigest == base.PolicyRevision() {
		t.Fatalf("structured output revision drift retained policy revision %q", revisedDigest)
	}
}

func TestBoundDefinitionAccessors(t *testing.T) {
	t.Parallel()
	client := &testClient{}
	tests := []struct {
		name string
		opts []Option
	}{
		{name: "named", opts: validNamedOptions(client, validModel("named"))},
		{name: "current loop", opts: validCurrentOptions()},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			definition, err := Define(tt.opts...)
			if err != nil {
				t.Fatalf("Define() error = %v", err)
			}
			bindings := Bindings{}
			if definition.Descriptor().ModelSource == ModelSourceCurrentLoop {
				bindings.Models = &testResolver{}
			}
			bound, err := definition.Bind(context.Background(), bindings)
			if err != nil {
				t.Fatalf("Bind() error = %v", err)
			}
			if bound.Name() != definition.Name() || bound.Participation() != definition.Participation() || bound.Timeout() != definition.Timeout() || bound.Limits() != definition.Limits() || bound.Descriptor() != definition.Descriptor() {
				t.Fatalf("bound accessors differ from definition")
			}
			if strings.TrimSpace(bound.SystemPrompt()) == "" {
				t.Fatal("SystemPrompt() is blank")
			}
		})
	}
}

func TestDefinitionDescriptorIdentity(t *testing.T) {
	t.Parallel()
	client := &testClient{identity: "client-secret-identity"}
	baseModel := validModel("model")
	baseOptions := validNamedOptions(client, baseModel)
	base, err := Define(baseOptions...)
	if err != nil {
		t.Fatalf("Define(base) error = %v", err)
	}
	tests := []struct {
		name   string
		opts   []Option
		same   bool
		assert func(*testing.T, Definition)
	}{
		{name: "identical definition", opts: validNamedOptions(client, validModel("model")), same: true},
		{name: "client identity excluded", opts: validNamedOptions(&testClient{identity: "other-secret"}, validModel("model")), same: true},
		{name: "model source", opts: replaceOption(validNamedOptions(client, baseModel), 4, WithCurrentLoopModel())},
		{name: "model name", opts: validNamedOptions(client, validModel("other-model"))},
		{name: "model base URL", opts: validNamedOptions(client, modelWithBaseURL(baseModel, "https://other.example.invalid"))},
		{name: "model temperature", opts: validNamedOptions(client, modelWithTemperature(baseModel, 0.75))},
		{name: "model top p", opts: validNamedOptions(client, modelWithTopP(baseModel, 0.75))},
		{name: "model max tokens", opts: validNamedOptions(client, modelWithMaxTokens(baseModel, 654))},
		{name: "model stop", opts: validNamedOptions(client, modelWithStop(baseModel, []string{"STOP"}))},
		{name: "model effort", opts: validNamedOptions(client, modelWithEffort(baseModel, model.EffortLow))},
		{name: "prompt bytes", opts: replaceOption(baseOptions, 5, WithSystemPrompt("Different prompt.", "prompt-v1"))},
		{name: "prompt revision", opts: replaceOption(baseOptions, 5, WithSystemPrompt("Summarize the conversation.", "prompt-v2"))},
		{name: "participation", opts: replaceOption(baseOptions, 1, WithParticipation(ParticipationBackground))},
		{name: "exact nanosecond timeout", opts: replaceOption(baseOptions, 2, WithTimeout(2*time.Second+2*time.Nanosecond))},
		{name: "input limit", opts: replaceOption(baseOptions, 3, WithLimits(Limits{InputBytes: 4097, OutputBytes: 2048}))},
		{name: "output limit", opts: replaceOption(baseOptions, 3, WithLimits(Limits{InputBytes: 4096, OutputBytes: 2049}))},
		{name: "opaque policy", opts: replaceOption(baseOptions, 6, WithPolicyRevision("parser-v2"))},
		{name: "prompt digest", opts: baseOptions, same: true, assert: assertPromptDigest},
		{name: "secret-free descriptor", opts: baseOptions, same: true, assert: assertSecretFreeDescriptor},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			definition, defineErr := Define(tt.opts...)
			if defineErr != nil {
				t.Fatalf("Define() error = %v", defineErr)
			}
			if gotSame := definition.PolicyRevision() == base.PolicyRevision(); gotSame != tt.same {
				t.Fatalf("same policy = %v, want %v\nbase=%s\ngot =%s", gotSame, tt.same, base.PolicyRevision(), definition.PolicyRevision())
			}
			firstRevision := definition.PolicyRevision()
			secondRevision := definition.PolicyRevision()
			if firstRevision != secondRevision {
				t.Fatal("PolicyRevision() is unstable")
			}
			if tt.assert != nil {
				tt.assert(t, definition)
			}
		})
	}
}

func TestDefinitionDefensiveCopies(t *testing.T) {
	t.Parallel()
	client := &testClient{}
	model := validModel("frozen")
	originalTemperature := *model.Sampling.Temperature
	originalTopP := *model.Sampling.TopP
	originalMaxTokens := *model.Sampling.MaxTokens
	originalStop := model.Sampling.Stop[0]
	definition, err := Define(validNamedOptions(client, model)...)
	if err != nil {
		t.Fatalf("Define() error = %v", err)
	}
	*model.Sampling.Temperature = 0.99
	*model.Sampling.TopP = 0.01
	*model.Sampling.MaxTokens = 999
	model.Sampling.Stop[0] = "MUTATED"
	bound, err := definition.Bind(context.Background(), Bindings{})
	if err != nil {
		t.Fatalf("Bind() error = %v", err)
	}
	tests := []struct {
		name   string
		mutate func(InferenceBinding)
	}{
		{name: "temperature pointer", mutate: func(binding InferenceBinding) { *binding.Model.Sampling.Temperature = 0.88 }},
		{name: "top p pointer", mutate: func(binding InferenceBinding) { *binding.Model.Sampling.TopP = 0.88 }},
		{name: "max tokens pointer", mutate: func(binding InferenceBinding) { *binding.Model.Sampling.MaxTokens = 777 }},
		{name: "stop slice", mutate: func(binding InferenceBinding) { binding.Model.Sampling.Stop[0] = "CHANGED" }},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			first, resolveErr := bound.ResolveInference(context.Background(), uuid.UUID{})
			if resolveErr != nil {
				t.Fatalf("ResolveInference(first) error = %v", resolveErr)
			}
			tt.mutate(first)
			second, resolveErr := bound.ResolveInference(context.Background(), uuid.UUID{})
			if resolveErr != nil {
				t.Fatalf("ResolveInference(second) error = %v", resolveErr)
			}
			if *second.Model.Sampling.Temperature != originalTemperature || *second.Model.Sampling.TopP != originalTopP ||
				*second.Model.Sampling.MaxTokens != originalMaxTokens || second.Model.Sampling.Stop[0] != originalStop {
				t.Fatalf("resolved model mutated: temperature=%v top_p=%v max_tokens=%v stop=%q", *second.Model.Sampling.Temperature, *second.Model.Sampling.TopP, *second.Model.Sampling.MaxTokens, second.Model.Sampling.Stop[0])
			}
		})
	}
}

func TestBindValidation(t *testing.T) {
	t.Parallel()
	current, err := Define(validCurrentOptions()...)
	if err != nil {
		t.Fatalf("Define(current) error = %v", err)
	}
	named, err := Define(validNamedOptions(&testClient{}, validModel("named"))...)
	if err != nil {
		t.Fatalf("Define(named) error = %v", err)
	}
	var zero Definition
	var typedNilResolver *testResolver
	tests := []struct {
		name       string
		definition Definition
		ctx        context.Context
		bindings   Bindings
		kind       BindErrorKind
		wantErr    bool
	}{
		{name: "named needs no resolver", definition: named, ctx: context.Background()},
		{name: "current with resolver", definition: current, ctx: context.Background(), bindings: Bindings{Models: &testResolver{}}},
		{name: "zero definition", definition: zero, ctx: context.Background(), kind: BindInvalidDefinition, wantErr: true},
		{name: "nil context", definition: current, kind: BindInvalidContext, wantErr: true},
		{name: "current missing resolver", definition: current, ctx: context.Background(), kind: BindMissingModelResolver, wantErr: true},
		{name: "current typed nil resolver", definition: current, ctx: context.Background(), bindings: Bindings{Models: typedNilResolver}, kind: BindMissingModelResolver, wantErr: true},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, bindErr := tt.definition.Bind(tt.ctx, tt.bindings)
			if !tt.wantErr {
				if bindErr != nil {
					t.Fatalf("Bind() error = %v", bindErr)
				}
				return
			}
			var typed *BindError
			if !errors.As(bindErr, &typed) || typed.Kind != tt.kind {
				t.Fatalf("Bind() error = %T %v, want kind %q", bindErr, bindErr, tt.kind)
			}
		})
	}
}

func TestResolveInference(t *testing.T) {
	t.Parallel()
	loopID := uuid.MustParse("11111111-1111-4111-8111-111111111111")
	client := &testClient{identity: "resolved"}
	resolverCause := &testResolveCause{message: "loop exited"}
	tests := []struct {
		name      string
		resolver  *testResolver
		ctx       context.Context
		loopID    uuid.UUID
		kind      ResolveErrorKind
		wantErr   bool
		wantCause error
		noCause   bool
	}{
		{name: "exact loop id delegated", resolver: &testResolver{wantID: loopID, binding: InferenceBinding{Client: client, Model: validModel("live")}}, ctx: context.Background(), loopID: loopID},
		{name: "nil context", resolver: &testResolver{wantID: loopID}, loopID: loopID, kind: ResolveInvalidContext, wantErr: true},
		{name: "zero cause loop id", resolver: &testResolver{}, ctx: context.Background(), kind: ResolveInvalidLoopID, wantErr: true},
		{name: "resolver failure preserved", resolver: &testResolver{wantID: loopID, err: resolverCause}, ctx: context.Background(), loopID: loopID, kind: ResolveModelFailed, wantErr: true, wantCause: resolverCause},
		{name: "nil resolved client", resolver: &testResolver{wantID: loopID, binding: InferenceBinding{Model: validModel("live")}}, ctx: context.Background(), loopID: loopID, kind: ResolveInvalidBinding, wantErr: true},
		{name: "invalid resolved model", resolver: &testResolver{wantID: loopID, binding: InferenceBinding{Client: client}}, ctx: context.Background(), loopID: loopID, kind: ResolveInvalidBinding, wantErr: true},
		{name: "invalid resolved model effort", resolver: &testResolver{wantID: loopID, binding: InferenceBinding{Client: client, Model: modelWithEffort(validModel("live"), model.Effort("bogus"))}}, ctx: context.Background(), loopID: loopID, kind: ResolveInvalidBinding, wantErr: true},
		{name: "current nan temperature", resolver: &testResolver{wantID: loopID, binding: InferenceBinding{Client: client, Model: modelWithTemperature(validModel("live"), math.NaN())}}, ctx: context.Background(), loopID: loopID, kind: ResolveInvalidBinding, wantErr: true, noCause: true},
		{name: "current positive infinity temperature", resolver: &testResolver{wantID: loopID, binding: InferenceBinding{Client: client, Model: modelWithTemperature(validModel("live"), math.Inf(1))}}, ctx: context.Background(), loopID: loopID, kind: ResolveInvalidBinding, wantErr: true, noCause: true},
		{name: "current negative infinity temperature", resolver: &testResolver{wantID: loopID, binding: InferenceBinding{Client: client, Model: modelWithTemperature(validModel("live"), math.Inf(-1))}}, ctx: context.Background(), loopID: loopID, kind: ResolveInvalidBinding, wantErr: true, noCause: true},
		{name: "current nan top p", resolver: &testResolver{wantID: loopID, binding: InferenceBinding{Client: client, Model: modelWithTopP(validModel("live"), math.NaN())}}, ctx: context.Background(), loopID: loopID, kind: ResolveInvalidBinding, wantErr: true, noCause: true},
		{name: "current positive infinity top p", resolver: &testResolver{wantID: loopID, binding: InferenceBinding{Client: client, Model: modelWithTopP(validModel("live"), math.Inf(1))}}, ctx: context.Background(), loopID: loopID, kind: ResolveInvalidBinding, wantErr: true, noCause: true},
		{name: "current negative infinity top p", resolver: &testResolver{wantID: loopID, binding: InferenceBinding{Client: client, Model: modelWithTopP(validModel("live"), math.Inf(-1))}}, ctx: context.Background(), loopID: loopID, kind: ResolveInvalidBinding, wantErr: true, noCause: true},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			definition, defineErr := Define(validCurrentOptions()...)
			if defineErr != nil {
				t.Fatalf("Define() error = %v", defineErr)
			}
			bound, bindErr := definition.Bind(context.Background(), Bindings{Models: tt.resolver})
			if bindErr != nil {
				t.Fatalf("Bind() error = %v", bindErr)
			}
			binding, resolveErr := bound.ResolveInference(tt.ctx, tt.loopID)
			if !tt.wantErr {
				if resolveErr != nil || binding.Client != client || binding.Model.Name != "live" || tt.resolver.calls != 1 {
					t.Fatalf("ResolveInference() = (%#v,%v), calls=%d", binding, resolveErr, tt.resolver.calls)
				}
				return
			}
			var typed *ResolveError
			if !errors.As(resolveErr, &typed) || typed.Kind != tt.kind {
				t.Fatalf("ResolveInference() error = %T %v, want kind %q", resolveErr, resolveErr, tt.kind)
			}
			if tt.wantCause != nil && !errors.Is(resolveErr, tt.wantCause) {
				t.Fatalf("ResolveInference() error = %v, want wrapped cause %v", resolveErr, tt.wantCause)
			}
			if tt.noCause && typed.Cause != nil {
				t.Fatalf("ResolveInference() cause = %v, want nil", typed.Cause)
			}
		})
	}
}

func TestResolveNamedInferenceFrozen(t *testing.T) {
	t.Parallel()
	client := &testClient{identity: "frozen"}
	definition, err := Define(validNamedOptions(client, validModel("named"))...)
	if err != nil {
		t.Fatalf("Define() error = %v", err)
	}
	resolver := &testResolver{err: &testResolveCause{message: "must not be called"}}
	bound, err := definition.Bind(context.Background(), Bindings{Models: resolver})
	if err != nil {
		t.Fatalf("Bind() error = %v", err)
	}
	tests := []struct {
		name    string
		ctx     context.Context
		loopID  uuid.UUID
		kind    ResolveErrorKind
		wantErr bool
	}{
		{name: "zero loop id allowed", ctx: context.Background(), loopID: uuid.UUID{}},
		{name: "nonzero loop id ignored", ctx: context.Background(), loopID: uuid.MustParse("22222222-2222-4222-8222-222222222222")},
		{name: "nil context rejected", kind: ResolveInvalidContext, wantErr: true},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			binding, resolveErr := bound.ResolveInference(tt.ctx, tt.loopID)
			if tt.wantErr {
				var typed *ResolveError
				if !errors.As(resolveErr, &typed) || typed.Kind != tt.kind {
					t.Fatalf("ResolveInference() error = %T %v, want kind %q", resolveErr, resolveErr, tt.kind)
				}
				return
			}
			if resolveErr != nil || binding.Client != client || binding.Model.Name != "named" {
				t.Fatalf("ResolveInference() = (%#v,%v)", binding, resolveErr)
			}
			if resolver.calls != 0 {
				t.Fatalf("named ResolveInference called resolver %d times", resolver.calls)
			}
		})
	}
}

func replaceOption(options []Option, index int, option Option) []Option {
	copyOf := append([]Option(nil), options...)
	copyOf[index] = option
	return copyOf
}

func withoutOption(options []Option, index int) []Option {
	copyOf := append([]Option(nil), options...)
	return append(copyOf[:index], copyOf[index+1:]...)
}

func modelWithoutProvider(model model.Model) model.Model {
	model.Provider = ""
	return model
}

func modelWithTemperature(model model.Model, value float64) model.Model {
	model.Sampling.Temperature = &value
	return model
}

func modelWithBaseURL(model model.Model, value string) model.Model {
	model.BaseURL = value
	return model
}

func modelWithTopP(model model.Model, value float64) model.Model {
	model.Sampling.TopP = &value
	return model
}

func modelWithMaxTokens(model model.Model, value int) model.Model {
	model.Sampling.MaxTokens = &value
	return model
}

func modelWithStop(model model.Model, value []string) model.Model {
	model.Sampling.Stop = value
	return model
}

func modelWithEffort(model model.Model, effort model.Effort) model.Model {
	model.Sampling.Effort = effort
	return model
}

func assertPromptDigest(t *testing.T, definition Definition) {
	t.Helper()
	const want = "3345c2bf4ecc9b601e29aaccef25275b3aeb5c9a1d42e0536fc57661a2230de0"
	descriptor := definition.Descriptor()
	if got := hex.EncodeToString(descriptor.PromptSHA256[:]); got != want {
		t.Fatalf("PromptSHA256 = %s, want %s", got, want)
	}
}

func assertSecretFreeDescriptor(t *testing.T, definition Definition) {
	t.Helper()
	descriptor := definition.Descriptor()
	encoded, err := json.Marshal(descriptor)
	if err != nil {
		t.Fatalf("json.Marshal(Descriptor()) error = %v", err)
	}
	for _, secret := range []string{"Summarize the conversation.", "client-secret-identity", "models.example.invalid"} {
		if strings.Contains(string(encoded), secret) || strings.Contains(definition.PolicyRevision(), secret) {
			t.Fatalf("descriptor or policy leaked %q: %s / %s", secret, encoded, definition.PolicyRevision())
		}
	}
	wantFields := []string{
		"Name", "Participation", "ModelSource", "NamedModelKey", "NamedModelPolicyRevision",
		"PromptRevision", "PromptSHA256", "OutputSchemaName", "OutputSchemaSHA256",
		"StructuredOutputRevision", "PolicyRevision", "TimeoutNanos", "Limits",
		"EvidenceToolPolicyRevision", "EvidenceToolDefinitionsSHA256",
		"EvidenceProducedToolNamesSHA256", "EvidenceToolLimits",
		"EvidenceToolDefinitionCount", "StructuredOutputWithTools",
	}
	typeOf := reflect.TypeOf(descriptor)
	if typeOf.NumField() != len(wantFields) {
		t.Fatalf("DefinitionDescriptor has %d fields, want exactly %d", typeOf.NumField(), len(wantFields))
	}
	for index, want := range wantFields {
		if typeOf.Field(index).Name != want {
			t.Fatalf("DefinitionDescriptor field[%d] = %q, want %q", index, typeOf.Field(index).Name, want)
		}
	}
}
