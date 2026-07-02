package llm

import "testing"

func TestProviderAllowsEmptyBaseURL(t *testing.T) {
	tests := []struct {
		name string
		p    Provider
		want bool
	}{
		{"bedrock region-routed", ProviderBedrock, true},
		{"chutes self-defaults", ProviderChutes, true},
		{"phala self-defaults", ProviderPhala, true},
		{"openrouter self-defaults", ProviderOpenRouter, true},
		{"lmstudio self-defaults", ProviderLMStudio, true},
		{"google self-defaults", ProviderGoogle, true},
		{"unknown fails closed", Provider("nope"), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := tt.p.allowsEmptyBaseURL(); got != tt.want {
				t.Errorf("allowsEmptyBaseURL() = %v, want %v", got, tt.want)
			}
		})
	}
}
