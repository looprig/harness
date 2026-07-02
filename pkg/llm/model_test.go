package llm_test

import (
	"errors"
	"testing"

	"github.com/ciram-co/looprig/pkg/llm"
)

func f64ptr(v float64) *float64 { return &v }
func intptr(v int) *int         { return &v }

// TestModel_Validate exercises the boundary rules: known provider + supported
// APIFormat pair, non-empty Name, and the HTTPS-only BaseURL rule with its
// narrow localhost-only HTTP exception.
func TestModel_Validate(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		model   llm.Model
		wantErr bool
	}{
		{
			name:    "valid phala openai https",
			model:   llm.Model{Provider: llm.ProviderPhala, APIFormat: llm.APIFormatOpenAI, BaseURL: "https://api.phala.network/v1", Name: "zai-org/GLM-4.6"},
			wantErr: false,
		},
		{
			name:    "valid chutes openai https",
			model:   llm.Model{Provider: llm.ProviderChutes, APIFormat: llm.APIFormatOpenAI, BaseURL: "https://api.chutes.ai", Name: "moonshotai/Kimi-K2.6-TEE"},
			wantErr: false,
		},
		{
			name:    "valid lmstudio openai http localhost",
			model:   llm.Model{Provider: llm.ProviderLMStudio, APIFormat: llm.APIFormatOpenAI, BaseURL: "http://localhost:1234", Name: "qwen"},
			wantErr: false,
		},
		{
			name:    "valid lmstudio anthropic https",
			model:   llm.Model{Provider: llm.ProviderLMStudio, APIFormat: llm.APIFormatAnthropic, BaseURL: "https://lm.example.test", Name: "claude-local"},
			wantErr: false,
		},
		{
			name:    "boundary http 127.0.0.1 loopback allowed",
			model:   llm.Model{Provider: llm.ProviderLMStudio, APIFormat: llm.APIFormatOpenAI, BaseURL: "http://127.0.0.1:1234", Name: "qwen"},
			wantErr: false,
		},
		{
			name:    "boundary http uppercase LOCALHOST allowed",
			model:   llm.Model{Provider: llm.ProviderLMStudio, APIFormat: llm.APIFormatOpenAI, BaseURL: "http://LOCALHOST:1234", Name: "qwen"},
			wantErr: false,
		},
		{
			name:    "boundary http ipv6 loopback ::1 allowed",
			model:   llm.Model{Provider: llm.ProviderLMStudio, APIFormat: llm.APIFormatOpenAI, BaseURL: "http://[::1]:1234", Name: "qwen"},
			wantErr: false,
		},
		{
			name:    "custom origin validates identically (valid)",
			model:   llm.Model{Provider: llm.ProviderChutes, APIFormat: llm.APIFormatOpenAI, BaseURL: "https://api.chutes.ai", Name: "m", Origin: llm.OriginCustom},
			wantErr: false,
		},
		{
			name:    "error unknown provider",
			model:   llm.Model{Provider: llm.Provider("bogus"), APIFormat: llm.APIFormatOpenAI, BaseURL: "https://x.example.test", Name: "m"},
			wantErr: true,
		},
		{
			name:    "error empty provider",
			model:   llm.Model{Provider: "", APIFormat: llm.APIFormatOpenAI, BaseURL: "https://x.example.test", Name: "m"},
			wantErr: true,
		},
		{
			name:    "error unsupported pair phala anthropic",
			model:   llm.Model{Provider: llm.ProviderPhala, APIFormat: llm.APIFormatAnthropic, BaseURL: "https://api.phala.network/v1", Name: "m"},
			wantErr: true,
		},
		{
			name:    "error unsupported pair chutes bedrock",
			model:   llm.Model{Provider: llm.ProviderChutes, APIFormat: llm.APIFormatBedrockConverse, BaseURL: "https://api.chutes.ai", Name: "m"},
			wantErr: true,
		},
		{
			name:    "error unsupported pair lmstudio bedrock",
			model:   llm.Model{Provider: llm.ProviderLMStudio, APIFormat: llm.APIFormatBedrockConverse, BaseURL: "http://localhost:1234", Name: "m"},
			wantErr: true,
		},
		{
			name:    "error empty name",
			model:   llm.Model{Provider: llm.ProviderChutes, APIFormat: llm.APIFormatOpenAI, BaseURL: "https://api.chutes.ai", Name: ""},
			wantErr: true,
		},
		{
			name:    "error empty base url",
			model:   llm.Model{Provider: llm.ProviderChutes, APIFormat: llm.APIFormatOpenAI, BaseURL: "", Name: "m"},
			wantErr: true,
		},
		{
			name:    "error http to remote host",
			model:   llm.Model{Provider: llm.ProviderChutes, APIFormat: llm.APIFormatOpenAI, BaseURL: "http://api.chutes.ai", Name: "m"},
			wantErr: true,
		},
		{
			name:    "error base url with userinfo credentials",
			model:   llm.Model{Provider: llm.ProviderPhala, APIFormat: llm.APIFormatOpenAI, BaseURL: "https://user:pass@evil.example.com/v1", Name: "m"},
			wantErr: true,
		},
		{
			name:    "error http to non-loopback ip",
			model:   llm.Model{Provider: llm.ProviderLMStudio, APIFormat: llm.APIFormatOpenAI, BaseURL: "http://127.0.0.2:1234", Name: "m"},
			wantErr: true,
		},
		{
			name:    "error base url not a url",
			model:   llm.Model{Provider: llm.ProviderChutes, APIFormat: llm.APIFormatOpenAI, BaseURL: "://not-a-url", Name: "m"},
			wantErr: true,
		},
		{
			name:    "error base url no scheme",
			model:   llm.Model{Provider: llm.ProviderChutes, APIFormat: llm.APIFormatOpenAI, BaseURL: "api.chutes.ai", Name: "m"},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := tt.model.Validate()
			if (err != nil) != tt.wantErr {
				t.Fatalf("Validate() error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				var ve *llm.ValidationError
				if !errors.As(err, &ve) {
					t.Errorf("Validate() error is %T, want *llm.ValidationError", err)
				}
			}
		})
	}
}

// TestCustomModel_Defaults confirms CustomModel sets only the four wire fields
// and leaves everything else at its fail-safe zero value: OriginCustom, all
// capabilities false / MaxContext 0, and an empty Sampling.
func TestCustomModel_Defaults(t *testing.T) {
	t.Parallel()
	m := llm.CustomModel(llm.ProviderChutes, llm.APIFormatOpenAI, "https://api.chutes.ai", "moonshotai/Kimi")

	if m.Provider != llm.ProviderChutes {
		t.Errorf("Provider = %q, want %q", m.Provider, llm.ProviderChutes)
	}
	if m.APIFormat != llm.APIFormatOpenAI {
		t.Errorf("APIFormat = %q, want %q", m.APIFormat, llm.APIFormatOpenAI)
	}
	if m.BaseURL != "https://api.chutes.ai" {
		t.Errorf("BaseURL = %q, want https://api.chutes.ai", m.BaseURL)
	}
	if m.Name != "moonshotai/Kimi" {
		t.Errorf("Name = %q, want moonshotai/Kimi", m.Name)
	}
	if m.Origin != llm.OriginCustom {
		t.Errorf("Origin = %v, want OriginCustom (fail-safe default)", m.Origin)
	}
	if m.Caps.AcceptsImages || m.Caps.Tools || m.Caps.Thinking {
		t.Errorf("Caps bool flags = %+v, want all false by default", m.Caps)
	}
	if m.Caps.MaxContext != 0 {
		t.Errorf("Caps.MaxContext = %d, want 0 by default", m.Caps.MaxContext)
	}
	if m.Sampling.Temperature != nil || m.Sampling.TopP != nil || m.Sampling.MaxTokens != nil ||
		m.Sampling.Stop != nil || m.Sampling.Effort != llm.EffortNone {
		t.Errorf("Sampling = %+v, want zero value by default", m.Sampling)
	}
}

// TestCustomModel_Options confirms each ModelOption opts a capability in.
func TestCustomModel_Options(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		opts  []llm.ModelOption
		check func(t *testing.T, m llm.Model)
	}{
		{
			name: "with max context",
			opts: []llm.ModelOption{llm.WithMaxContext(200_000)},
			check: func(t *testing.T, m llm.Model) {
				if m.Caps.MaxContext != 200_000 {
					t.Errorf("MaxContext = %d, want 200000", m.Caps.MaxContext)
				}
			},
		},
		{
			name: "with tools",
			opts: []llm.ModelOption{llm.WithTools()},
			check: func(t *testing.T, m llm.Model) {
				if !m.Caps.Tools {
					t.Error("Caps.Tools = false, want true")
				}
			},
		},
		{
			name: "with images",
			opts: []llm.ModelOption{llm.WithImages()},
			check: func(t *testing.T, m llm.Model) {
				if !m.Caps.AcceptsImages {
					t.Error("Caps.AcceptsImages = false, want true")
				}
			},
		},
		{
			name: "with thinking",
			opts: []llm.ModelOption{llm.WithThinking()},
			check: func(t *testing.T, m llm.Model) {
				if !m.Caps.Thinking {
					t.Error("Caps.Thinking = false, want true")
				}
			},
		},
		{
			name: "with sampling",
			opts: []llm.ModelOption{llm.WithSampling(llm.Sampling{Temperature: f64ptr(0.5), MaxTokens: intptr(128), Effort: llm.EffortHigh})},
			check: func(t *testing.T, m llm.Model) {
				if m.Sampling.Temperature == nil || *m.Sampling.Temperature != 0.5 {
					t.Errorf("Sampling.Temperature = %v, want 0.5", m.Sampling.Temperature)
				}
				if m.Sampling.MaxTokens == nil || *m.Sampling.MaxTokens != 128 {
					t.Errorf("Sampling.MaxTokens = %v, want 128", m.Sampling.MaxTokens)
				}
				if m.Sampling.Effort != llm.EffortHigh {
					t.Errorf("Sampling.Effort = %q, want high", m.Sampling.Effort)
				}
			},
		},
		{
			name: "combined options",
			opts: []llm.ModelOption{llm.WithTools(), llm.WithImages(), llm.WithThinking(), llm.WithMaxContext(64_000)},
			check: func(t *testing.T, m llm.Model) {
				if !m.Caps.Tools || !m.Caps.AcceptsImages || !m.Caps.Thinking || m.Caps.MaxContext != 64_000 {
					t.Errorf("combined Caps = %+v, want all set", m.Caps)
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			m := llm.CustomModel(llm.ProviderLMStudio, llm.APIFormatOpenAI, "http://localhost:1234", "qwen", tt.opts...)
			tt.check(t, m)
		})
	}
}

// TestCustomModel_WithSamplingNoAlias guards that WithSampling deep-copies its
// argument so a later mutation of the caller's Sampling cannot reach the Model.
func TestCustomModel_WithSamplingNoAlias(t *testing.T) {
	t.Parallel()
	s := llm.Sampling{Temperature: f64ptr(0.5), Stop: []string{"</s>"}}
	m := llm.CustomModel(llm.ProviderLMStudio, llm.APIFormatOpenAI, "http://localhost:1234", "qwen", llm.WithSampling(s))

	*s.Temperature = 0.99
	s.Stop[0] = "MUTATED"

	if m.Sampling.Temperature == nil || *m.Sampling.Temperature != 0.5 {
		t.Errorf("Model.Sampling.Temperature aliased caller state: got %v, want 0.5", m.Sampling.Temperature)
	}
	if len(m.Sampling.Stop) != 1 || m.Sampling.Stop[0] != "</s>" {
		t.Errorf("Model.Sampling.Stop aliased caller state: got %v, want [</s>]", m.Sampling.Stop)
	}
}

// TestCustomModel_Validates confirms a custom model still passes through the same
// boundary rules: a well-formed one validates, an http-remote one is rejected.
func TestCustomModel_Validates(t *testing.T) {
	t.Parallel()
	ok := llm.CustomModel(llm.ProviderLMStudio, llm.APIFormatOpenAI, "http://localhost:1234", "qwen")
	if err := ok.Validate(); err != nil {
		t.Errorf("Validate() on valid custom model = %v, want nil", err)
	}

	bad := llm.CustomModel(llm.ProviderChutes, llm.APIFormatOpenAI, "http://api.chutes.ai", "m")
	if err := bad.Validate(); err == nil {
		t.Error("Validate() on http-remote custom model = nil, want error")
	}
}
