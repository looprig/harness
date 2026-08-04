package loop

import (
	"errors"
	"testing"

	contextcount "github.com/looprig/inference/contextcount"
	model "github.com/looprig/inference/model"
)

func TestContextTransportKeyOf(t *testing.T) {
	base := model.Model{
		Provider:  model.ProviderName("anthropic"),
		APIFormat: model.APIFormatAnthropic,
		BaseURL:   "https://api.anthropic.com",
		Name:      "claude-sonnet",
		Sampling:  model.Sampling{Effort: model.EffortHigh},
	}

	tests := []struct {
		name      string
		a         model.Model
		b         model.Model
		wantEqual bool
	}{
		{
			name:      "identical models produce equal keys",
			a:         base,
			b:         base,
			wantEqual: true,
		},
		{
			name: "differing Name produces equal key",
			a:    base,
			b: func() model.Model {
				m := base
				m.Name = "claude-opus"
				return m
			}(),
			wantEqual: true,
		},
		{
			name: "differing Sampling.Effort produces equal key",
			a:    base,
			b: func() model.Model {
				m := base
				m.Sampling = model.Sampling{Effort: model.EffortLow}
				return m
			}(),
			wantEqual: true,
		},
		{
			name: "differing Provider produces different key",
			a:    base,
			b: func() model.Model {
				m := base
				m.Provider = model.ProviderName("openai")
				return m
			}(),
			wantEqual: false,
		},
		{
			name: "differing APIFormat produces different key",
			a:    base,
			b: func() model.Model {
				m := base
				m.APIFormat = model.APIFormatOpenAI
				return m
			}(),
			wantEqual: false,
		},
		{
			name: "differing BaseURL produces different key",
			a:    base,
			b: func() model.Model {
				m := base
				m.BaseURL = "https://api.other.example"
				return m
			}(),
			wantEqual: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			gotA := transportKeyOf(tt.a)
			gotB := transportKeyOf(tt.b)
			if (gotA == gotB) != tt.wantEqual {
				t.Fatalf("transportKeyOf(a) == transportKeyOf(b) = %v, want %v (a=%v b=%v)", gotA == gotB, tt.wantEqual, gotA, gotB)
			}
		})
	}
}

func TestLookupTransport(t *testing.T) {
	anthropicModel := model.Model{
		Provider:  model.ProviderName("anthropic"),
		APIFormat: model.APIFormatAnthropic,
		BaseURL:   "https://api.anthropic.com",
		Name:      "claude-sonnet",
	}
	openAIModel := model.Model{
		Provider:  model.ProviderName("openai"),
		APIFormat: model.APIFormatOpenAI,
		BaseURL:   "https://api.openai.com",
		Name:      "gpt-5",
	}

	anthropicCapability := contextcount.InferenceCapability{
		Provider:  contextcount.ProviderID("anthropic"),
		Transport: contextcount.InferenceTransportTLS,
	}

	tests := []struct {
		name     string
		set      []ContextTransport
		model    model.Model
		wantCap  contextcount.InferenceCapability
		wantFind bool
	}{
		{
			name:     "empty set never matches",
			set:      nil,
			model:    anthropicModel,
			wantCap:  contextcount.InferenceCapability{},
			wantFind: false,
		},
		{
			name: "member model is found",
			set: []ContextTransport{
				{
					Provider:   anthropicModel.Provider,
					APIFormat:  anthropicModel.APIFormat,
					BaseURL:    anthropicModel.BaseURL,
					Capability: anthropicCapability,
				},
			},
			model:    anthropicModel,
			wantCap:  anthropicCapability,
			wantFind: true,
		},
		{
			name: "non-member model is not found",
			set: []ContextTransport{
				{
					Provider:   anthropicModel.Provider,
					APIFormat:  anthropicModel.APIFormat,
					BaseURL:    anthropicModel.BaseURL,
					Capability: anthropicCapability,
				},
			},
			model:    openAIModel,
			wantCap:  contextcount.InferenceCapability{},
			wantFind: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			gotCap, gotFind := lookupTransport(tt.set, tt.model)
			if gotFind != tt.wantFind {
				t.Fatalf("lookupTransport() found = %v, want %v", gotFind, tt.wantFind)
			}
			if gotCap != tt.wantCap {
				t.Fatalf("lookupTransport() capability = %+v, want %+v", gotCap, tt.wantCap)
			}
		})
	}
}

func TestWithContextTransports_Singleton(t *testing.T) {
	t.Parallel()
	counter := &policyCounter{capability: exactCounterCapability()}
	base := testModel()
	transports := []ContextTransport{
		{Provider: base.Provider, APIFormat: base.APIFormat, BaseURL: base.BaseURL, Capability: localInferenceCapability()},
	}
	opts := append(contextDefinitionOptions(counter, localInferenceCapability(), manualCompactionPolicy()),
		WithContextTransports(transports...), WithContextTransports(transports...))
	_, err := Define(opts...)
	var target *DefinitionError
	if !errors.As(err, &target) || target.Kind != DefinitionDuplicateOption {
		t.Fatalf("Define() error = %T %v, want DefinitionDuplicateOption", err, err)
	}
}

func TestWithContextTransports_RequiresBaseMember(t *testing.T) {
	t.Parallel()
	base := testModel()
	baseTransport := ContextTransport{Provider: base.Provider, APIFormat: base.APIFormat, BaseURL: base.BaseURL, Capability: localInferenceCapability()}
	otherTransport := ContextTransport{Provider: "other", APIFormat: model.APIFormatAnthropic, BaseURL: "https://api.other.example", Capability: localInferenceCapability()}
	mismatchedCapability := baseTransport
	mismatchedCapability.Capability = contextcount.InferenceCapability{Transport: contextcount.InferenceTransportTLS, Retention: contextcount.RetentionNone}

	tests := []struct {
		name       string
		transports []ContextTransport
		wantErr    bool
	}{
		{name: "base member present with matching capability", transports: []ContextTransport{baseTransport}},
		{name: "missing base member", transports: []ContextTransport{otherTransport}, wantErr: true},
		{name: "base member capability mismatch", transports: []ContextTransport{mismatchedCapability}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			counter := &policyCounter{capability: exactCounterCapability()}
			opts := append(contextDefinitionOptions(counter, localInferenceCapability(), manualCompactionPolicy()), WithContextTransports(tt.transports...))
			_, err := Define(opts...)
			if (err != nil) != tt.wantErr {
				t.Fatalf("Define() error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				var target *DefinitionError
				if !errors.As(err, &target) || target.Kind != DefinitionInvalidContextTransport {
					t.Fatalf("Define() error = %T %v, want DefinitionInvalidContextTransport", err, err)
				}
			}
		})
	}
}

// TestWithContextTransports_RequiresContextCounter proves the fix for the gap
// found during Task 1.2's code review: declaring WithContextTransports with no
// WithContextCounter must fail Define (not silently discard the declared
// transports and succeed).
func TestWithContextTransports_RequiresContextCounter(t *testing.T) {
	t.Parallel()
	base := testModel()
	transports := []ContextTransport{
		{Provider: base.Provider, APIFormat: base.APIFormat, BaseURL: base.BaseURL, Capability: localInferenceCapability()},
	}
	_, err := Define(WithName("agent"), WithInference(&fakeLLM{}, base), WithContextTransports(transports...))
	var target *DefinitionError
	if !errors.As(err, &target) || target.Kind != DefinitionMissingContextCounter {
		t.Fatalf("Define() error = %T %v, want DefinitionMissingContextCounter", err, err)
	}
}

func TestWithContextTransports_InvalidMemberCapability(t *testing.T) {
	t.Parallel()
	base := testModel()
	baseTransport := ContextTransport{Provider: base.Provider, APIFormat: base.APIFormat, BaseURL: base.BaseURL, Capability: localInferenceCapability()}
	// Zero-value Capability has Transport == InferenceTransportUnknown, which
	// InferenceCapability.Validate() rejects outright.
	invalidTransport := ContextTransport{Provider: "second-provider", APIFormat: model.APIFormatAnthropic, BaseURL: "https://api.second.example"}
	counter := &policyCounter{capability: exactCounterCapability()}
	opts := append(contextDefinitionOptions(counter, localInferenceCapability(), manualCompactionPolicy()),
		WithContextTransports(baseTransport, invalidTransport))
	_, err := Define(opts...)
	var target *DefinitionError
	if !errors.As(err, &target) || target.Kind != DefinitionInvalidContextTransport {
		t.Fatalf("Define() error = %T %v, want DefinitionInvalidContextTransport", err, err)
	}
	var capErr *contextcount.CapabilityValidationError
	if !errors.As(err, &capErr) {
		t.Fatalf("Define() cause = %T, want *contextcount.CapabilityValidationError", err)
	}
}

func TestWithContextTransports_DuplicateMembers(t *testing.T) {
	t.Parallel()
	base := testModel()
	// The two members share (Provider, APIFormat, BaseURL) but deliberately
	// carry DIFFERENT Capability values, so this regression-guards that
	// duplicate detection keys on contextTransportKey's three fields only —
	// not on full ContextTransport struct equality, which would let two
	// members disagreeing on trust posture for the same wire endpoint slip
	// through undetected.
	duplicateKeyDifferentCapability := ContextTransport{
		Provider: base.Provider, APIFormat: base.APIFormat, BaseURL: base.BaseURL,
		Capability: contextcount.InferenceCapability{Transport: contextcount.InferenceTransportTLS, Retention: contextcount.RetentionNone},
	}
	baseTransport := ContextTransport{Provider: base.Provider, APIFormat: base.APIFormat, BaseURL: base.BaseURL, Capability: localInferenceCapability()}
	counter := &policyCounter{capability: exactCounterCapability()}
	opts := append(contextDefinitionOptions(counter, localInferenceCapability(), manualCompactionPolicy()),
		WithContextTransports(baseTransport, duplicateKeyDifferentCapability))
	_, err := Define(opts...)
	var target *DefinitionError
	if !errors.As(err, &target) || target.Kind != DefinitionDuplicateContextTransport {
		t.Fatalf("Define() error = %T %v, want DefinitionDuplicateContextTransport", err, err)
	}
}

func TestWithContextTransports_IncompatibleMemberCounter(t *testing.T) {
	t.Parallel()
	// A non-provider-neutral CounterCapability, built the same way the
	// existing "incompatible counter" fake is built in
	// compaction_policy_test.go: starting from the neutral exact capability
	// and overriding Transport/Provider/SecurityIdentity so
	// providerNeutralCounter is false and CounterTransportSeparateEndpoint's
	// rules apply.
	separate := exactCounterCapability()
	separate.Provider = "second-provider"
	separate.Transport = contextcount.CounterTransportSeparateEndpoint
	separate.SecurityIdentity = contextcount.SecurityIdentity{9}

	base := testModel()
	baseCapability := contextcount.InferenceCapability{
		Transport: contextcount.InferenceTransportTLS, Provider: "second-provider",
		SecurityIdentity: contextcount.SecurityIdentity{9}, Retention: contextcount.RetentionNone,
	}
	baseTransport := ContextTransport{Provider: base.Provider, APIFormat: base.APIFormat, BaseURL: base.BaseURL, Capability: baseCapability}
	// A second, non-base transport whose Capability (local transport) is
	// incompatible with the separate-endpoint counter above.
	incompatibleTransport := ContextTransport{
		Provider: "second-provider", APIFormat: model.APIFormatAnthropic, BaseURL: "https://api.second.example",
		Capability: localInferenceCapability(),
	}

	counter := &policyCounter{capability: separate}
	opts := append(contextDefinitionOptions(counter, baseCapability, manualCompactionPolicy()),
		WithContextTransports(baseTransport, incompatibleTransport))
	_, err := Define(opts...)
	var target *DefinitionError
	if !errors.As(err, &target) || target.Kind != DefinitionIncompatibleContextCounter {
		t.Fatalf("Define() error = %T %v, want DefinitionIncompatibleContextCounter", err, err)
	}
	var compatErr *contextcount.CounterCompatibilityError
	if !errors.As(err, &compatErr) {
		t.Fatalf("Define() cause = %T, want *contextcount.CounterCompatibilityError", err)
	}
}

// TestWithContextTransports_ModeBindingAcrossDeclaredTransports is the
// regression proving the capability the whole feature exists to unlock: a
// predeclared mode's model can now live on a SECOND declared transport, not
// just the base transport, while a mode on an undeclared third transport
// still fails.
func TestWithContextTransports_ModeBindingAcrossDeclaredTransports(t *testing.T) {
	t.Parallel()
	base := testModel()
	baseTransport := ContextTransport{Provider: base.Provider, APIFormat: base.APIFormat, BaseURL: base.BaseURL, Capability: localInferenceCapability()}

	secondModel := base
	secondModel.Provider = "second-provider"
	secondModel.BaseURL = "https://api.second.example"
	secondCapability := contextcount.InferenceCapability{
		Transport: contextcount.InferenceTransportTLS, Provider: "second-provider",
		SecurityIdentity: contextcount.SecurityIdentity{7}, Retention: contextcount.RetentionNone,
	}
	secondTransport := ContextTransport{Provider: secondModel.Provider, APIFormat: secondModel.APIFormat, BaseURL: secondModel.BaseURL, Capability: secondCapability}

	thirdModel := base
	thirdModel.Provider = "undeclared-provider"
	thirdModel.BaseURL = "https://api.undeclared.example"

	counter := &policyCounter{capability: exactCounterCapability()}
	buildOpts := func(modeModel model.Model) []Option {
		return append(contextDefinitionOptions(counter, localInferenceCapability(), manualCompactionPolicy()),
			WithContextTransports(baseTransport, secondTransport),
			WithModes(Mode{Name: "alternate", Model: modeModel}), WithInitialMode("alternate"))
	}

	if _, err := Define(buildOpts(secondModel)...); err != nil {
		t.Fatalf("Define() with mode on second declared transport: %v", err)
	}

	_, err := Define(buildOpts(thirdModel)...)
	var definitionErr *DefinitionError
	if !errors.As(err, &definitionErr) || definitionErr.Kind != DefinitionInvalidModeBinding {
		t.Fatalf("Define() error = %T %v, want DefinitionInvalidModeBinding", err, err)
	}
	var notDeclared *ContextTransportNotDeclaredError
	if !errors.As(err, &notDeclared) {
		t.Fatalf("Define() cause = %T, want *ContextTransportNotDeclaredError", err)
	}
}
