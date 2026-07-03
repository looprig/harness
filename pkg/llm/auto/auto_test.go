package auto

import (
	"errors"
	"testing"

	"github.com/ciram-co/looprig/pkg/llm"
	"github.com/ciram-co/looprig/pkg/llm/auth"
	"github.com/ciram-co/looprig/pkg/llm/codec/anthropicapi"
	"github.com/ciram-co/looprig/pkg/llm/codec/gemini"
	"github.com/ciram-co/looprig/pkg/llm/codec/openaiapi"
	"github.com/ciram-co/looprig/pkg/llm/providers/chutes"
	geminiprovider "github.com/ciram-co/looprig/pkg/llm/providers/gemini"
	"github.com/ciram-co/looprig/pkg/llm/transport"
)

// The helpers below stand in for the deleted model catalogue: each returns a valid
// Model (OriginCustom) via llm.CustomModel, used purely as a test fixture. They keep
// the repeated model rows DRY across this file's dispatch tables.
func chutesKimiK2Model() llm.Model {
	return llm.CustomModel(llm.ProviderChutes, llm.APIFormatOpenAI, "https://api.chutes.ai", "moonshotai/Kimi-K2.6-TEE", llm.WithMaxContext(128_000), llm.WithTools(), llm.WithThinking())
}

func openRouterModel(name string) llm.Model {
	return llm.CustomModel(llm.ProviderOpenRouter, llm.APIFormatOpenAI, "https://openrouter.ai/api/v1", name, llm.WithTools())
}

func geminiFlashModel() llm.Model {
	return llm.CustomModel(llm.ProviderGoogle, llm.APIFormatGemini, "https://generativelanguage.googleapis.com/v1beta", "gemini-2.5-flash", llm.WithMaxContext(1_000_000), llm.WithTools(), llm.WithImages(), llm.WithThinking())
}

func lmStudioLocalModel(name string) llm.Model {
	return llm.CustomModel(llm.ProviderLMStudio, llm.APIFormatOpenAI, "http://localhost:1234/v1", name, llm.WithTools())
}

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
		{name: "chutes with key", model: chutesKimiK2Model(), key: "k"},
		{name: "openrouter with key", model: openRouterModel("x"), key: "sk-or-key"},
		{name: "google with key", model: geminiFlashModel(), key: "AIza-k"},
		{name: "lmstudio without key (AuthNone)", model: lmStudioLocalModel("qwen"), key: ""},
		{name: "lmstudio ignores a supplied key", model: lmStudioLocalModel("qwen"), key: "k"},
		{name: "phala empty key fails closed", model: llm.CustomModel(llm.ProviderPhala, llm.APIFormatOpenAI, "https://api.phala.network/v1", "zai-org/GLM-4.6", llm.WithMaxContext(200_000), llm.WithTools(), llm.WithThinking()), key: "", wantErr: true, wantAuthReq: true},
		{name: "chutes empty key fails closed", model: chutesKimiK2Model(), key: "", wantErr: true, wantAuthReq: true},
		{name: "openrouter empty key fails closed", model: openRouterModel("x"), key: "", wantErr: true, wantAuthReq: true},
		{name: "google empty key fails closed", model: geminiFlashModel(), key: "", wantErr: true, wantAuthReq: true},
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
	got, err := New(llm.CustomModel(llm.ProviderBedrock, llm.APIFormatAnthropic, "", "anthropic.claude-3-5-sonnet-20241022-v2:0", llm.WithMaxContext(200_000), llm.WithTools(), llm.WithImages()), "")
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

// TestNewPhalaNotConstructible confirms the Policy dispatch decision: a Phala model
// reaches New's dispatch with a present key (its RequiredAuth is AuthAPIKey, so the
// empty-APIKey guard does NOT fire) and returns a *PolicyNotConstructibleError
// directing the caller to phala.New, with no client. auto.New's inputs are
// (model, key) only — it carries no attestation acceptance Policy — so a Phala client
// cannot be built here; a defaulted policy would fail open.
func TestNewPhalaNotConstructible(t *testing.T) {
	t.Parallel()
	m := llm.CustomModel(llm.ProviderPhala, llm.APIFormatOpenAI, "https://inference.phala.com", "zai-org/GLM-4.6")
	got, err := New(m, "sk-live")
	if got != nil {
		t.Fatalf("New() returned non-nil client (%T) for a policy-requiring provider", got)
	}
	var pne *PolicyNotConstructibleError
	if !errors.As(err, &pne) {
		t.Fatalf("want *PolicyNotConstructibleError, got %v", err)
	}
	if pne.Provider != llm.ProviderPhala {
		t.Errorf("PolicyNotConstructibleError.Provider = %q, want %q", pne.Provider, llm.ProviderPhala)
	}
	if pne.Use != "phala.New" {
		t.Errorf("PolicyNotConstructibleError.Use = %q, want %q", pne.Use, "phala.New")
	}

	// It must specifically NOT be an AuthRequiredError: auth is checked before the
	// construct-directly dispatch, and a present key means the empty-APIKey guard is
	// skipped, so the policy dispatch — not an auth failure — is what surfaces here.
	var are *llm.AuthRequiredError
	if errors.As(err, &are) {
		t.Error("phala present-key returned *llm.AuthRequiredError; the policy construct-directly dispatch must surface, not an auth failure")
	}
}

// TestNewConcreteTypes pins each provider to its concrete client so the wiring
// cannot silently regress to a different implementation. Behavior differs by
// provider (chutes runs its own e2e client, lmstudio is the generic transport
// client, google is the bespoke gemini client), so the concrete type is the
// observable contract.
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
			name:  "chutes wires the chutes client",
			model: chutesKimiK2Model(), key: "k",
			is:   func(l llm.LLM) bool { _, ok := l.(*chutes.Client); return ok },
			want: "*chutes.Client",
		},
		{
			name:  "lmstudio wires the generic transport client",
			model: lmStudioLocalModel("qwen"), key: "",
			is:   func(l llm.LLM) bool { _, ok := l.(*transport.Client); return ok },
			want: "*transport.Client",
		},
		{
			name:  "openrouter wires the generic transport client",
			model: openRouterModel("x"), key: "sk-or-key",
			is:   func(l llm.LLM) bool { _, ok := l.(*transport.Client); return ok },
			want: "*transport.Client",
		},
		{
			name:  "google wires the bespoke gemini client",
			model: geminiFlashModel(), key: "AIza-k",
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

// TestNewLMStudioDefaultEndpoint is the explicit assertion that the dissolved
// lmstudio package's default loopback endpoint now works via a CustomModel row + the
// generic client, with no credentials.
func TestNewLMStudioDefaultEndpoint(t *testing.T) {
	t.Parallel()
	got, err := New(lmStudioLocalModel("m"), "")
	if err != nil {
		t.Fatalf("New(lmStudioLocalModel, \"\") err = %v, want nil", err)
	}
	if got == nil {
		t.Fatal("New(lmStudioLocalModel, \"\") = nil, want non-nil client")
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
