package transport

import (
	"errors"
	"testing"

	"github.com/looprig/harness/pkg/llm"
	"github.com/looprig/harness/pkg/llm/auth"
	"github.com/looprig/harness/pkg/llm/codec/openaiapi"
)

// TestCheckBindingEmptyBase locks the "empty request base means the bound
// endpoint" contract: an empty Model.BaseURL binds to the Client's endpoint, a
// matching base is fine, but a non-empty base that disagrees — or a different
// provider — still fails closed with a *llm.ModelMismatchError.
func TestCheckBindingEmptyBase(t *testing.T) {
	t.Parallel()

	c := New(openaiapi.Codec{}, Endpoint{Provider: llm.ProviderOpenRouter, BaseURL: "https://openrouter.ai/api/v1"}, auth.None())
	tests := []struct {
		name    string
		model   llm.Model
		wantErr bool
	}{
		{"empty base binds to endpoint", llm.Model{Provider: llm.ProviderOpenRouter, BaseURL: ""}, false},
		{"matching base ok", llm.Model{Provider: llm.ProviderOpenRouter, BaseURL: "https://openrouter.ai/api/v1"}, false},
		{"conflicting base fails", llm.Model{Provider: llm.ProviderOpenRouter, BaseURL: "https://evil.example"}, true},
		{"wrong provider fails", llm.Model{Provider: llm.ProviderChutes, BaseURL: ""}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := c.checkBinding(tt.model)
			if (err != nil) != tt.wantErr {
				t.Fatalf("checkBinding() err=%v wantErr=%v", err, tt.wantErr)
			}
			if tt.wantErr {
				var mme *llm.ModelMismatchError
				if !errors.As(err, &mme) {
					t.Fatalf("checkBinding() err=%T, want *llm.ModelMismatchError", err)
				}
			}
		})
	}
}
