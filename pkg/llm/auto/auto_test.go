package auto

import (
	"errors"
	"testing"

	"github.com/ciram-co/looprig/pkg/llm"
	"github.com/ciram-co/looprig/pkg/llm/aci"
	"github.com/ciram-co/looprig/pkg/llm/auth"
	"github.com/ciram-co/looprig/pkg/llm/providers/chutes"
	"github.com/ciram-co/looprig/pkg/llm/transport"
)

// TestNew exercises the dispatch + fail-closed auth contract: valid models build a
// non-nil client, an unknown/self-contradictory model is rejected before dispatch
// with a *llm.ValidationError, and a key-requiring provider given no key fails
// closed with a *llm.AuthRequiredError. LM Studio (AuthNone) succeeds with no key.
func TestNew(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		// model is built from catalog rows / CustomModel so Validate passes on the
		// happy cases; the error cases deliberately fail an earlier ordered guard.
		model       llm.Model
		key         auth.APIKey
		wantErr     bool
		wantAuthReq bool // when wantErr: expect *llm.AuthRequiredError, else *llm.ValidationError
	}{
		{name: "phala with key", model: llm.GLM46Phala(), key: "k"},
		{name: "chutes with key", model: llm.ChutesKimiK2(), key: "k"},
		{name: "lmstudio without key (AuthNone)", model: llm.LMStudioLocal("qwen"), key: ""},
		{name: "lmstudio ignores a supplied key", model: llm.LMStudioLocal("qwen"), key: "k"},
		{name: "phala empty key fails closed", model: llm.GLM46Phala(), key: "", wantErr: true, wantAuthReq: true},
		{name: "chutes empty key fails closed", model: llm.ChutesKimiK2(), key: "", wantErr: true, wantAuthReq: true},
		{
			name:    "unknown provider rejected before dispatch",
			model:   llm.Model{Provider: "nope", APIFormat: llm.APIFormatOpenAI, BaseURL: "https://x.example.test", Name: "m"},
			key:     "k",
			wantErr: true,
		},
		{name: "empty model rejected", model: llm.Model{}, key: "k", wantErr: true},
		{
			name:    "self-contradictory model rejected before dispatch",
			model:   llm.CustomModel(llm.ProviderPhala, llm.APIFormatAnthropic, "https://api.phala.network/v1", "m"),
			key:     "k",
			wantErr: true,
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := New(tt.model, tt.key)
			if (err != nil) != tt.wantErr {
				t.Fatalf("New() err = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				if got != nil {
					t.Fatalf("New() returned non-nil llm (%T) alongside an error", got)
				}
				if tt.wantAuthReq {
					var are *llm.AuthRequiredError
					if !errors.As(err, &are) {
						t.Fatalf("err = %T, want *llm.AuthRequiredError", err)
					}
					if are.Provider != tt.model.Provider {
						t.Errorf("AuthRequiredError.Provider = %q, want %q", are.Provider, tt.model.Provider)
					}
					if are.Kind != llm.AuthAPIKey {
						t.Errorf("AuthRequiredError.Kind = %q, want %q", are.Kind, llm.AuthAPIKey)
					}
					return
				}
				var ve *llm.ValidationError
				if !errors.As(err, &ve) {
					t.Fatalf("err = %T, want *llm.ValidationError", err)
				}
				return
			}
			if got == nil {
				t.Fatal("New() returned nil llm, want non-nil client")
			}
		})
	}
}

// TestNewConcreteTypes pins each provider to its concrete client so the wiring
// cannot silently regress to a different implementation. Behavior differs by
// provider (phala attests via aci, chutes runs its own e2e client, lmstudio is the
// generic transport client), so the concrete type is the observable contract.
func TestNewConcreteTypes(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		model llm.Model
		key   auth.APIKey
		is    func(llm.LLM) bool
		want  string
	}{
		{
			name:  "phala wires the aci attestation client",
			model: llm.GLM46Phala(), key: "k",
			is:   func(l llm.LLM) bool { _, ok := l.(*aci.Client); return ok },
			want: "*aci.Client",
		},
		{
			name:  "chutes wires the chutes client",
			model: llm.ChutesKimiK2(), key: "k",
			is:   func(l llm.LLM) bool { _, ok := l.(*chutes.Client); return ok },
			want: "*chutes.Client",
		},
		{
			name:  "lmstudio wires the generic transport client",
			model: llm.LMStudioLocal("qwen"), key: "",
			is:   func(l llm.LLM) bool { _, ok := l.(*transport.Client); return ok },
			want: "*transport.Client",
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := New(tt.model, tt.key)
			if err != nil {
				t.Fatalf("New() err = %v, want nil", err)
			}
			if !tt.is(got) {
				t.Fatalf("New() llm = %T, want %s", got, tt.want)
			}
		})
	}
}

// TestNewLMStudioLocal is the task's explicit assertion that the dissolved lmstudio
// package's default endpoint now works via a catalog row + the generic client, with
// no credentials.
func TestNewLMStudioLocal(t *testing.T) {
	t.Parallel()
	got, err := New(llm.LMStudioLocal("m"), "")
	if err != nil {
		t.Fatalf("New(LMStudioLocal, \"\") err = %v, want nil", err)
	}
	if got == nil {
		t.Fatal("New(LMStudioLocal, \"\") = nil, want non-nil client")
	}
}
