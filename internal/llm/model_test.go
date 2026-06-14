package llm_test

import (
	"testing"

	"github.com/inventivepotter/urvi/internal/llm"
)

func mkF64(v float64) *float64 { return &v }
func mkInt(v int) *int         { return &v }

func eqF64Ptr(a, b *float64) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}
func eqIntPtr(a, b *int) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

func TestModelSpec(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		model  llm.Model
		apiKey string
		system string
		want   llm.ModelSpec
	}{
		{
			name:   "full fields",
			model:  llm.Model{Provider: llm.ProviderChutes, BaseURL: "https://api.chutes.ai", Name: "m", Temperature: mkF64(0.5), MaxTokens: mkInt(128)},
			apiKey: "secret",
			system: "sys",
			want:   llm.ModelSpec{Provider: llm.ProviderChutes, BaseURL: "https://api.chutes.ai", APIKey: "secret", Model: "m", System: "sys", Temperature: mkF64(0.5), MaxTokens: mkInt(128)},
		},
		{
			name:   "nil sampling pointers stay nil",
			model:  llm.Model{Provider: llm.ProviderLMStudio, Name: "local"},
			apiKey: "",
			system: "s",
			want:   llm.ModelSpec{Provider: llm.ProviderLMStudio, Model: "local", System: "s"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := tt.model.Spec(tt.apiKey, tt.system)
			if got.Provider != tt.want.Provider || got.BaseURL != tt.want.BaseURL ||
				got.APIKey != tt.want.APIKey || got.Model != tt.want.Model || got.System != tt.want.System {
				t.Errorf("Spec() scalar mismatch:\n got  %+v\n want %+v", got, tt.want)
			}
			if !eqF64Ptr(got.Temperature, tt.want.Temperature) {
				t.Errorf("Temperature pointer value mismatch")
			}
			if !eqIntPtr(got.MaxTokens, tt.want.MaxTokens) {
				t.Errorf("MaxTokens pointer value mismatch")
			}
		})
	}
}

// TestModelSpecClonesPointers guards both cloneFloat64Ptr and cloneIntPtr:
// mutating the returned spec's pointees must not reach a second spec or the Model.
func TestModelSpecClonesPointers(t *testing.T) {
	t.Parallel()
	m := llm.Model{Provider: llm.ProviderChutes, Name: "m", Temperature: mkF64(0.5), MaxTokens: mkInt(128)}
	s1 := m.Spec("k", "sys")
	s2 := m.Spec("k", "sys")

	*s1.Temperature = 0.99
	*s1.MaxTokens = 999

	if *s2.Temperature != 0.5 {
		t.Errorf("s2.Temperature mutated via s1: got %v want 0.5", *s2.Temperature)
	}
	if *s2.MaxTokens != 128 {
		t.Errorf("s2.MaxTokens mutated via s1: got %v want 128", *s2.MaxTokens)
	}
	if *m.Temperature != 0.5 {
		t.Errorf("model.Temperature mutated via s1: got %v want 0.5", *m.Temperature)
	}
	if *m.MaxTokens != 128 {
		t.Errorf("model.MaxTokens mutated via s1: got %v want 128", *m.MaxTokens)
	}
}
