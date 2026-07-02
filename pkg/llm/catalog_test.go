package llm_test

import (
	"testing"

	"github.com/ciram-co/looprig/pkg/llm"
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
