package hustle

import (
	"errors"
	"testing"

	"github.com/looprig/inference"
)

func descriptorFromOptions(t *testing.T, options []Option) DefinitionDescriptor {
	t.Helper()
	definition, err := Define(options...)
	if err != nil {
		t.Fatalf("Define() error = %v", err)
	}
	return definition.Descriptor()
}

func TestDefinitionDescriptorValidate(t *testing.T) {
	t.Parallel()
	current := descriptorFromOptions(t, validCurrentOptions())
	named := descriptorFromOptions(t, validNamedOptions(&testClient{}, validModel("named")))
	zeroPromptHash := current
	zeroPromptHash.PromptSHA256 = [32]byte{}
	currentNamedKey := current
	currentNamedKey.NamedModelKey = inference.ModelKey{Provider: "test", Model: "named"}
	currentNamedRevision := current
	currentNamedRevision.NamedModelPolicyRevision = "named-policy"
	namedMissingKey := named
	namedMissingKey.NamedModelKey = inference.ModelKey{}
	namedMissingRevision := named
	namedMissingRevision.NamedModelPolicyRevision = ""
	minimum := current
	minimum.TimeoutNanos = 1
	minimum.Limits = Limits{InputBytes: 1, OutputBytes: 1}
	maximum := current
	maximum.Limits = Limits{InputBytes: maxPayloadBytes, OutputBytes: maxPayloadBytes}
	overInput := maximum
	overInput.Limits.InputBytes++
	overOutput := maximum
	overOutput.Limits.OutputBytes++
	tests := []struct {
		name    string
		value   DefinitionDescriptor
		wantErr bool
	}{
		{name: "valid current", value: current},
		{name: "valid named", value: named},
		{name: "minimum boundary", value: minimum},
		{name: "maximum payload boundary", value: maximum},
		{name: "zero descriptor", value: DefinitionDescriptor{}, wantErr: true},
		{name: "blank name", value: withDescriptorName(current, " \t"), wantErr: true},
		{name: "reserved name", value: withDescriptorName(current, "_looprig.private"), wantErr: true},
		{name: "invalid participation", value: withDescriptorParticipation(current, ParticipationUnknown), wantErr: true},
		{name: "invalid model source", value: withDescriptorSource(current, ModelSourceUnknown), wantErr: true},
		{name: "zero prompt hash", value: zeroPromptHash, wantErr: true},
		{name: "blank prompt revision", value: withDescriptorPromptRevision(current, " "), wantErr: true},
		{name: "blank policy revision", value: withDescriptorPolicyRevision(current, " "), wantErr: true},
		{name: "zero timeout", value: withDescriptorTimeout(current, 0), wantErr: true},
		{name: "negative timeout", value: withDescriptorTimeout(current, -1), wantErr: true},
		{name: "zero input limit", value: withDescriptorLimits(current, Limits{OutputBytes: 1}), wantErr: true},
		{name: "zero output limit", value: withDescriptorLimits(current, Limits{InputBytes: 1}), wantErr: true},
		{name: "input max plus one", value: overInput, wantErr: true},
		{name: "output max plus one", value: overOutput, wantErr: true},
		{name: "current with named key", value: currentNamedKey, wantErr: true},
		{name: "current with named revision", value: currentNamedRevision, wantErr: true},
		{name: "named missing key", value: namedMissingKey, wantErr: true},
		{name: "named missing model policy", value: namedMissingRevision, wantErr: true},
	}
	for _, tt := range tests {
		testCase := tt
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			err := testCase.value.Validate()
			if (err != nil) != testCase.wantErr {
				t.Fatalf("DefinitionDescriptor.Validate() error = %v, wantErr %v", err, testCase.wantErr)
			}
			if testCase.wantErr {
				var definitionErr *DefinitionError
				if !errors.As(err, &definitionErr) {
					t.Fatalf("error = %T %v, want *DefinitionError", err, err)
				}
			}
		})
	}
}

func TestNameValidateMatchesDefinitionContract(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		value   Name
		wantErr bool
	}{
		{name: "plain name", value: "compact"},
		{name: "surrounding whitespace is preserved and valid", value: "  compact  "},
		{name: "empty", value: "", wantErr: true},
		{name: "whitespace only", value: "   ", wantErr: true},
		{name: "reserved after trimming", value: "  _looprig.private", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if err := tt.value.Validate(); (err != nil) != tt.wantErr {
				t.Errorf("Name.Validate() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func withDescriptorName(value DefinitionDescriptor, name Name) DefinitionDescriptor {
	value.Name = name
	return value
}

func withDescriptorParticipation(value DefinitionDescriptor, participation Participation) DefinitionDescriptor {
	value.Participation = participation
	return value
}

func withDescriptorSource(value DefinitionDescriptor, source ModelSource) DefinitionDescriptor {
	value.ModelSource = source
	return value
}

func withDescriptorPromptRevision(value DefinitionDescriptor, revision string) DefinitionDescriptor {
	value.PromptRevision = revision
	return value
}

func withDescriptorPolicyRevision(value DefinitionDescriptor, revision string) DefinitionDescriptor {
	value.PolicyRevision = revision
	return value
}

func withDescriptorTimeout(value DefinitionDescriptor, timeout int64) DefinitionDescriptor {
	value.TimeoutNanos = timeout
	return value
}

func withDescriptorLimits(value DefinitionDescriptor, limits Limits) DefinitionDescriptor {
	value.Limits = limits
	return value
}
