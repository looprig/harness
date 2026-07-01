package auto

import (
	"errors"
	"testing"

	"github.com/ciram-co/looprig/pkg/llm"
	"github.com/ciram-co/looprig/pkg/llm/aci"
)

func temp(f float64) *float64 { return &f }

func TestNew(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		spec    llm.ModelSpec
		wantErr bool
		wantLLM bool
	}{
		{name: "lmstudio", spec: llm.ModelSpec{Provider: llm.ProviderLMStudio, BaseURL: "http://x"}, wantLLM: true},
		{name: "phala", spec: llm.ModelSpec{Provider: llm.ProviderPhala, BaseURL: "http://x", APIKey: "k"}, wantLLM: true},
		{name: "chutes", spec: llm.ModelSpec{Provider: llm.ProviderChutes, BaseURL: "http://x", APIKey: "k"}, wantLLM: true},
		{name: "unknown provider", spec: llm.ModelSpec{Provider: "nope"}, wantErr: true},
		{name: "empty provider", spec: llm.ModelSpec{}, wantErr: true},
		{name: "invalid spec rejected before dispatch",
			spec:    llm.ModelSpec{Provider: llm.ProviderLMStudio, ThinkingBudget: 1, Temperature: temp(0.5)},
			wantErr: true},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := New(tt.spec)
			if (err != nil) != tt.wantErr {
				t.Fatalf("New() err = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				var ve *llm.ValidationError
				if !errors.As(err, &ve) {
					t.Fatalf("err = %T, want *llm.ValidationError", err)
				}
				return
			}
			if (got != nil) != tt.wantLLM {
				t.Fatalf("New() llm = %v, wantLLM %v", got, tt.wantLLM)
			}
		})
	}
}

// TestNewPhalaReturnsACIClient pins the Phala provider to the confidential-inference
// aci.Client (built via aci.New with aci.DefaultPhalaPolicy). The table above only
// asserts non-nil; this asserts the concrete type so the wiring can't silently
// regress to another client.
func TestNewPhalaReturnsACIClient(t *testing.T) {
	t.Parallel()
	got, err := New(llm.ModelSpec{Provider: llm.ProviderPhala, BaseURL: "http://x", APIKey: "k"})
	if err != nil {
		t.Fatalf("New() err = %v, want nil", err)
	}
	if _, ok := got.(*aci.Client); !ok {
		t.Fatalf("New() llm = %T, want *aci.Client", got)
	}
}
