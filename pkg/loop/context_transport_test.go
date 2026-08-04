package loop

import (
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
