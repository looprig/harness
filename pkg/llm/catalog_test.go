package llm_test

import (
	"testing"

	"github.com/looprig/harness/pkg/llm"
)

// TestCatalogModels verifies each hand-authored catalog row has the expected
// wire fields, is marked OriginCatalog, requires a key, and passes Validate.
func TestCatalogModels(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name         string
		model        llm.Model
		wantProvider llm.Provider
		wantFormat   llm.APIFormat
		wantBaseURL  string
		wantName     string
		wantTools    bool
		wantThinking bool
		wantImages   bool
	}{
		{
			name:         "chutes kimi k2",
			model:        llm.ChutesKimiK2(),
			wantProvider: llm.ProviderChutes,
			wantFormat:   llm.APIFormatOpenAI,
			wantBaseURL:  "https://api.chutes.ai",
			wantName:     "moonshotai/Kimi-K2.6-TEE",
			wantTools:    true,
			wantThinking: true,
			wantImages:   false, // Kimi K2 is text-only
		},
		{
			name:         "glm 4.6 phala",
			model:        llm.GLM46Phala(),
			wantProvider: llm.ProviderPhala,
			wantFormat:   llm.APIFormatOpenAI,
			wantBaseURL:  "https://api.phala.network/v1",
			wantName:     "zai-org/GLM-4.6",
			wantTools:    true,
			wantThinking: true,
			wantImages:   false,
		},
		{
			name:         "gemini 2.5 flash",
			model:        llm.GeminiFlash(),
			wantProvider: llm.ProviderGoogle,
			wantFormat:   llm.APIFormatGemini,
			wantBaseURL:  "https://generativelanguage.googleapis.com/v1beta",
			wantName:     "gemini-2.5-flash",
			wantTools:    true,
			wantThinking: true,
			wantImages:   true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			m := tt.model
			if m.Provider != tt.wantProvider {
				t.Errorf("Provider = %q, want %q", m.Provider, tt.wantProvider)
			}
			if m.APIFormat != tt.wantFormat {
				t.Errorf("APIFormat = %q, want %q", m.APIFormat, tt.wantFormat)
			}
			if m.BaseURL != tt.wantBaseURL {
				t.Errorf("BaseURL = %q, want %q", m.BaseURL, tt.wantBaseURL)
			}
			if m.Name != tt.wantName {
				t.Errorf("Name = %q, want %q", m.Name, tt.wantName)
			}
			if m.Origin != llm.OriginCatalog {
				t.Errorf("Origin = %v, want catalog", m.Origin)
			}
			if m.Caps.Tools != tt.wantTools {
				t.Errorf("Caps.Tools = %v, want %v", m.Caps.Tools, tt.wantTools)
			}
			if m.Caps.Thinking != tt.wantThinking {
				t.Errorf("Caps.Thinking = %v, want %v", m.Caps.Thinking, tt.wantThinking)
			}
			if m.Caps.AcceptsImages != tt.wantImages {
				t.Errorf("Caps.AcceptsImages = %v, want %v", m.Caps.AcceptsImages, tt.wantImages)
			}

			needsKey, err := m.Provider.RequiresKey()
			if err != nil || !needsKey {
				t.Errorf("RequiresKey() = (%v, %v), want (true, nil)", needsKey, err)
			}

			if err := m.Validate(); err != nil {
				t.Errorf("Validate() on catalog row = %v, want nil", err)
			}
		})
	}
}

// TestLMStudioLocal covers the local LM Studio row separately from the key-requiring
// rows above: it is credential-free (RequiredAuth → AuthNone) and — critically for
// the generic transport client that replaced lmstudio.New — passes Validate with its
// explicit http:// loopback BaseURL (permitted by Validate's loopback exception).
func TestLMStudioLocal(t *testing.T) {
	t.Parallel()
	m := llm.LMStudioLocal("qwen")
	if m.Provider != llm.ProviderLMStudio {
		t.Errorf("Provider = %q, want %q", m.Provider, llm.ProviderLMStudio)
	}
	if m.APIFormat != llm.APIFormatOpenAI {
		t.Errorf("APIFormat = %q, want %q", m.APIFormat, llm.APIFormatOpenAI)
	}
	if m.BaseURL != "http://localhost:1234/v1" {
		t.Errorf("BaseURL = %q, want %q", m.BaseURL, "http://localhost:1234/v1")
	}
	if m.Name != "qwen" {
		t.Errorf("Name = %q, want %q", m.Name, "qwen")
	}
	if m.Origin != llm.OriginCatalog {
		t.Errorf("Origin = %v, want catalog", m.Origin)
	}
	kind, err := m.Provider.RequiredAuth()
	if err != nil || kind != llm.AuthNone {
		t.Errorf("RequiredAuth() = (%v, %v), want (none, nil)", kind, err)
	}
	if err := m.Validate(); err != nil {
		t.Errorf("Validate() on LM Studio row = %v, want nil", err)
	}
}

// TestClaudeOnBedrockCatalog covers the Bedrock row separately from the API-key
// rows: it is region-routed (empty BaseURL, permitted by Validate for Bedrock),
// authenticates with SigV4 (RequiredAuth → AuthSigV4, never a bearer key),
// declares the Anthropic dialect, is tool- and image-capable, and passes Validate.
// name (the Bedrock model id, colon and all) passes through verbatim.
func TestClaudeOnBedrockCatalog(t *testing.T) {
	t.Parallel()
	const modelID = "anthropic.claude-3-5-sonnet-20241022-v2:0"
	m := llm.ClaudeOnBedrock(modelID)
	if m.Provider != llm.ProviderBedrock {
		t.Errorf("Provider = %q, want %q", m.Provider, llm.ProviderBedrock)
	}
	if m.APIFormat != llm.APIFormatAnthropic {
		t.Errorf("APIFormat = %q, want %q", m.APIFormat, llm.APIFormatAnthropic)
	}
	if m.BaseURL != "" {
		t.Errorf("BaseURL = %q, want empty (region-routed)", m.BaseURL)
	}
	if m.Name != modelID {
		t.Errorf("Name = %q, want %q", m.Name, modelID)
	}
	if m.Origin != llm.OriginCatalog {
		t.Errorf("Origin = %v, want catalog", m.Origin)
	}
	if !m.Caps.Tools {
		t.Errorf("Caps.Tools = %v, want true", m.Caps.Tools)
	}
	if !m.Caps.AcceptsImages {
		t.Errorf("Caps.AcceptsImages = %v, want true", m.Caps.AcceptsImages)
	}
	kind, err := m.Provider.RequiredAuth()
	if err != nil || kind != llm.AuthSigV4 {
		t.Errorf("RequiredAuth() = (%v, %v), want (sigv4, nil)", kind, err)
	}
	if err := m.Validate(); err != nil {
		t.Errorf("Validate() on Bedrock row = %v, want nil (empty BaseURL permitted for Bedrock)", err)
	}
}

// TestOpenRouterCatalog covers the OpenRouter row: it is OpenAI-compatible, requires
// an API key (RequiredAuth → AuthAPIKey — Bearer), advertises tool-calling, and passes
// Validate against its https BaseURL. name is passed through verbatim as the model.
func TestOpenRouterCatalog(t *testing.T) {
	t.Parallel()
	m := llm.OpenRouter("anthropic/claude-3.5-sonnet")
	if m.Provider != llm.ProviderOpenRouter {
		t.Errorf("Provider = %q, want %q", m.Provider, llm.ProviderOpenRouter)
	}
	if m.APIFormat != llm.APIFormatOpenAI {
		t.Errorf("APIFormat = %q, want %q", m.APIFormat, llm.APIFormatOpenAI)
	}
	if m.BaseURL != "https://openrouter.ai/api/v1" {
		t.Errorf("BaseURL = %q, want %q", m.BaseURL, "https://openrouter.ai/api/v1")
	}
	if m.Name != "anthropic/claude-3.5-sonnet" {
		t.Errorf("Name = %q, want %q", m.Name, "anthropic/claude-3.5-sonnet")
	}
	if m.Origin != llm.OriginCatalog {
		t.Errorf("Origin = %v, want catalog", m.Origin)
	}
	if !m.Caps.Tools {
		t.Errorf("Caps.Tools = %v, want true", m.Caps.Tools)
	}
	kind, err := m.Provider.RequiredAuth()
	if err != nil || kind != llm.AuthAPIKey {
		t.Errorf("RequiredAuth() = (%v, %v), want (apikey, nil)", kind, err)
	}
	if err := m.Validate(); err != nil {
		t.Errorf("Validate() on OpenRouter row = %v, want nil", err)
	}
}
