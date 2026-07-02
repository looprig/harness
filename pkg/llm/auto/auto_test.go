package auto

import (
	"errors"
	"testing"

	"github.com/ciram-co/looprig/pkg/llm"
	"github.com/ciram-co/looprig/pkg/llm/aci"
	"github.com/ciram-co/looprig/pkg/llm/auth"
	"github.com/ciram-co/looprig/pkg/llm/codec/anthropicapi"
	"github.com/ciram-co/looprig/pkg/llm/codec/gemini"
	"github.com/ciram-co/looprig/pkg/llm/codec/openaiapi"
	"github.com/ciram-co/looprig/pkg/llm/providers/chutes"
	geminiprovider "github.com/ciram-co/looprig/pkg/llm/providers/gemini"
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
		wantAuthReq bool   // when wantErr: expect *llm.AuthRequiredError, else *llm.ValidationError
		wantField   string // when set (ValidationError path): assert ValidationError.Field
	}{
		{name: "phala with key", model: llm.GLM46Phala(), key: "k"},
		{name: "chutes with key", model: llm.ChutesKimiK2(), key: "k"},
		{name: "openrouter with key", model: llm.OpenRouter("x"), key: "sk-or-key"},
		{name: "google with key", model: llm.GeminiFlash(), key: "AIza-k"},
		{name: "lmstudio without key (AuthNone)", model: llm.LMStudioLocal("qwen"), key: ""},
		{name: "lmstudio ignores a supplied key", model: llm.LMStudioLocal("qwen"), key: "k"},
		{name: "phala empty key fails closed", model: llm.GLM46Phala(), key: "", wantErr: true, wantAuthReq: true},
		{name: "chutes empty key fails closed", model: llm.ChutesKimiK2(), key: "", wantErr: true, wantAuthReq: true},
		{name: "openrouter empty key fails closed", model: llm.OpenRouter("x"), key: "", wantErr: true, wantAuthReq: true},
		{name: "google empty key fails closed", model: llm.GeminiFlash(), key: "", wantErr: true, wantAuthReq: true},
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
		{
			// lmstudio legitimately supports the anthropic dialect (Validate passes).
			// Phase 1 fail-closed here (no anthropic codec); Phase 2 wired the
			// anthropicapi codec into codecFor, so this now resolves a real codec and
			// succeeds instead of erroring.
			name:  "lmstudio+anthropic now succeeds (anthropic codec wired)",
			model: llm.CustomModel(llm.ProviderLMStudio, llm.APIFormatAnthropic, "http://localhost:1234", "m"),
			key:   "",
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
				if tt.wantField != "" && ve.Field != tt.wantField {
					t.Errorf("ValidationError.Field = %q, want %q", ve.Field, tt.wantField)
				}
				return
			}
			if got == nil {
				t.Fatal("New() returned nil llm, want non-nil client")
			}
		})
	}
}

// TestNewBedrockDirectsToConstructor confirms the SigV4 dispatch decision: a
// Bedrock model reaches New's dispatch (its RequiredAuth is AuthSigV4, so the
// empty-APIKey guard does NOT fire — no AuthRequiredError confusion) and returns a
// *SigV4NotConstructibleError directing the caller to bedrock.New, with no client.
// auto.New's only credential is an auth.APIKey, which cannot carry SigV4 creds.
func TestNewBedrockDirectsToConstructor(t *testing.T) {
	t.Parallel()

	// An empty key must NOT surface as an AuthRequiredError here: bedrock's auth
	// kind is SigV4, not APIKey, so the Phase-1 empty-APIKey guard is skipped.
	got, err := New(llm.ClaudeOnBedrock("anthropic.claude-3-5-sonnet-20241022-v2:0"), "")
	if got != nil {
		t.Fatalf("New() returned non-nil client (%T) for a SigV4 provider", got)
	}
	var sigErr *SigV4NotConstructibleError
	if !errors.As(err, &sigErr) {
		t.Fatalf("err = %T, want *SigV4NotConstructibleError", err)
	}
	if sigErr.Provider != llm.ProviderBedrock {
		t.Errorf("SigV4NotConstructibleError.Provider = %q, want %q", sigErr.Provider, llm.ProviderBedrock)
	}
	if sigErr.Use != "bedrock.New" {
		t.Errorf("SigV4NotConstructibleError.Use = %q, want %q", sigErr.Use, "bedrock.New")
	}

	// It must specifically NOT be an AuthRequiredError (the empty-APIKey path).
	var are *llm.AuthRequiredError
	if errors.As(err, &are) {
		t.Error("bedrock empty-key returned *llm.AuthRequiredError; SigV4 providers must skip the empty-APIKey guard")
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
		{
			name:  "openrouter wires the generic transport client",
			model: llm.OpenRouter("x"), key: "sk-or-key",
			is:   func(l llm.LLM) bool { _, ok := l.(*transport.Client); return ok },
			want: "*transport.Client",
		},
		{
			name:  "google wires the bespoke gemini client",
			model: llm.GeminiFlash(), key: "AIza-k",
			is:   func(l llm.LLM) bool { _, ok := l.(*geminiprovider.Client); return ok },
			want: "*geminiprovider.Client",
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

// TestCodecFor pins the codec-selection registry: each wire dialect auto can encode
// resolves to its concrete codec, and a format with no codec yet fails closed with a
// *llm.ValidationError (Field "APIFormat") rather than silently mis-encoding. This is
// the internal seam that makes lmstudio+anthropic and OpenRouter+openai work.
func TestCodecFor(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		format  llm.APIFormat
		is      func(llm.Codec) bool
		want    string
		wantErr bool
	}{
		{
			name:   "openai",
			format: llm.APIFormatOpenAI,
			is:     func(c llm.Codec) bool { _, ok := c.(openaiapi.Codec); return ok },
			want:   "openaiapi.Codec",
		},
		{
			name:   "anthropic",
			format: llm.APIFormatAnthropic,
			is:     func(c llm.Codec) bool { _, ok := c.(anthropicapi.Codec); return ok },
			want:   "anthropicapi.Codec",
		},
		{
			name:   "gemini",
			format: llm.APIFormatGemini,
			is:     func(c llm.Codec) bool { _, ok := c.(gemini.Codec); return ok },
			want:   "gemini.Codec",
		},
		{name: "bedrock-converse has no codec yet", format: llm.APIFormatBedrockConverse, wantErr: true},
		{name: "unknown format fails closed", format: llm.APIFormat("bogus"), wantErr: true},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := codecFor(tt.format)
			if (err != nil) != tt.wantErr {
				t.Fatalf("codecFor(%q) err = %v, wantErr %v", tt.format, err, tt.wantErr)
			}
			if tt.wantErr {
				if got != nil {
					t.Fatalf("codecFor(%q) returned non-nil codec (%T) alongside an error", tt.format, got)
				}
				var ve *llm.ValidationError
				if !errors.As(err, &ve) {
					t.Fatalf("codecFor err = %T, want *llm.ValidationError", err)
				}
				if ve.Field != "APIFormat" {
					t.Errorf("ValidationError.Field = %q, want %q", ve.Field, "APIFormat")
				}
				return
			}
			if !tt.is(got) {
				t.Fatalf("codecFor(%q) = %T, want %s", tt.format, got, tt.want)
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

func TestDefaultGenericBaseURL(t *testing.T) {
	tests := []struct {
		name string
		p    llm.Provider
		want string
	}{
		{"openrouter", llm.ProviderOpenRouter, "https://openrouter.ai/api/v1"},
		{"lmstudio", llm.ProviderLMStudio, "http://localhost:1234/v1"},
		{"no default for others", llm.ProviderChutes, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := defaultGenericBaseURL(tt.p); got != tt.want {
				t.Errorf("defaultGenericBaseURL(%s) = %q, want %q", tt.p, got, tt.want)
			}
		})
	}
}
