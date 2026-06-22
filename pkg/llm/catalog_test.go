package llm_test

import (
	"testing"

	"github.com/ciram-co/looprig/pkg/llm"
)

func TestChutesKimiK2(t *testing.T) {
	t.Parallel()
	m := llm.ChutesKimiK2()

	if m.Provider != llm.ProviderChutes {
		t.Errorf("Provider = %q, want %q", m.Provider, llm.ProviderChutes)
	}
	if m.BaseURL != "https://api.chutes.ai" {
		t.Errorf("BaseURL = %q, want https://api.chutes.ai (chutes apiBase is not defaulted)", m.BaseURL)
	}
	if m.Name != "moonshotai/Kimi-K2.6-TEE" {
		t.Errorf("Name = %q, want moonshotai/Kimi-K2.6-TEE", m.Name)
	}

	needsKey, err := m.Provider.RequiresKey()
	if err != nil || !needsKey {
		t.Errorf("RequiresKey() = (%v, %v), want (true, nil)", needsKey, err)
	}

	// Kimi K2 is a text-only model: it must not advertise image support.
	if m.AcceptsImages {
		t.Errorf("AcceptsImages = true, want false (Kimi K2 is text-only)")
	}
}
