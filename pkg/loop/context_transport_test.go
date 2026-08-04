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
