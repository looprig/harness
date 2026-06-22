package llm_test

import (
	"errors"
	"testing"

	"github.com/ciram-co/looprig/pkg/llm"
)

func TestProviderRequiresKey(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		provider llm.Provider
		want     bool
		wantErr  bool
	}{
		{name: "lmstudio no key", provider: llm.ProviderLMStudio, want: false, wantErr: false},
		{name: "phala requires key", provider: llm.ProviderPhala, want: true, wantErr: false},
		{name: "chutes requires key", provider: llm.ProviderChutes, want: true, wantErr: false},
		{name: "unknown errors", provider: llm.Provider("bogus"), want: false, wantErr: true},
		{name: "empty errors", provider: llm.Provider(""), want: false, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := tt.provider.RequiresKey()
			if (err != nil) != tt.wantErr {
				t.Fatalf("RequiresKey() err = %v, wantErr %v", err, tt.wantErr)
			}
			if got != tt.want {
				t.Errorf("RequiresKey() = %v, want %v", got, tt.want)
			}
			if tt.wantErr {
				var ve *llm.ValidationError
				if !errors.As(err, &ve) {
					t.Errorf("error is %T, want *llm.ValidationError", err)
				}
			}
		})
	}
}
