// Package auto is the composition root that selects and wires a concrete llm.LLM
// for a validated Model. It is the single place that imports every provider, so
// business logic depends only on the llm.LLM interface — never on a concrete
// provider — and every credential/attestation decision is made here, once. It maps
// a Provider to its client and enforces the provider's fail-closed auth contract
// before any network object is constructed.
package auto

import (
	"fmt"

	"github.com/ciram-co/looprig/pkg/llm"
	"github.com/ciram-co/looprig/pkg/llm/auth"
	"github.com/ciram-co/looprig/pkg/llm/codec/anthropicapi"
	"github.com/ciram-co/looprig/pkg/llm/codec/gemini"
	"github.com/ciram-co/looprig/pkg/llm/codec/openaiapi"
	"github.com/ciram-co/looprig/pkg/llm/providers/chutes"
	geminiprovider "github.com/ciram-co/looprig/pkg/llm/providers/gemini"
	"github.com/ciram-co/looprig/pkg/llm/providers/phala"
	"github.com/ciram-co/looprig/pkg/llm/transport"
)

// SigV4NotConstructibleError is returned by New for a provider whose required
// credential kind is AuthSigV4 (currently Bedrock). auto.New's only credential
// input is an auth.APIKey, which cannot carry AWS SigV4 credentials, so such a
// provider must be constructed directly via its own constructor (named by Use,
// e.g. "bedrock.New"). Fail-closed and directive — never a silent nil client.
// This is why auto does NOT import the bedrock package: it dispatches to an error,
// not to a constructor it cannot feed.
type SigV4NotConstructibleError struct {
	Provider llm.Provider
	Use      string
}

func (e *SigV4NotConstructibleError) Error() string {
	return fmt.Sprintf("provider %q requires AWS SigV4 credentials that auto.New cannot supply; construct it directly via %s", e.Provider, e.Use)
}

// New validates model, enforces the provider's fail-closed auth requirement, then
// constructs the concrete provider client. Ordered:
//  1. Model.Validate — a self-contradictory or unknown-provider model yields a
//     *llm.ValidationError before anything else.
//  2. Provider.RequiredAuth — an unknown provider fails closed with a
//     *llm.ValidationError (never a permissive default).
//  3. A provider that requires an API key but is given none yields a
//     *llm.AuthRequiredError — fail-closed, before any network object exists.
//  4. Dispatch on Provider to the concrete client.
//
// No live I/O happens here; the returned llm.LLM performs its own per-request
// guards (binding, Validate, auth) when Invoke/Stream is called.
func New(model llm.Model, key auth.APIKey) (llm.LLM, error) {
	if err := model.Validate(); err != nil {
		return nil, err
	}
	kind, err := model.Provider.RequiredAuth()
	if err != nil {
		return nil, err
	}
	if kind == llm.AuthAPIKey && key == "" {
		return nil, &llm.AuthRequiredError{Provider: model.Provider, Kind: kind}
	}
	switch model.Provider {
	case llm.ProviderPhala:
		return phala.New(model.BaseURL, key, phala.DefaultPolicy())
	case llm.ProviderChutes:
		return chutes.New(model.BaseURL, string(key)), nil
	case llm.ProviderLMStudio:
		// LM Studio can speak either dialect (supportsAPIFormat admits both); genericHTTP
		// selects the codec by the model's declared APIFormat and fails closed on any
		// format with no codec, rather than silently mis-encoding. A local endpoint needs
		// no credentials.
		return genericHTTP(model, auth.None())
	case llm.ProviderOpenRouter:
		// OpenRouter is an OpenAI-compatible aggregation gateway behind a Bearer key. The
		// fail-closed empty-key guard above (RequiredAuth → AuthAPIKey) already rejected a
		// missing key, so key is present here; wrap it as Bearer auth.
		return genericHTTP(model, auth.Key(key))
	case llm.ProviderGoogle:
		// Google's Gemini generateContent API is not plain codec-over-HTTP (per-model
		// ":generateContent" path + an x-goog-api-key header), so it uses the bespoke
		// providers/gemini client rather than genericHTTP. The empty-key guard above
		// (RequiredAuth → AuthAPIKey) already rejected a missing key; gemini.New re-checks
		// and fails closed on empty regardless.
		return geminiprovider.New(key)
	case llm.ProviderBedrock:
		// Bedrock's RequiredAuth is AuthSigV4, so the empty-APIKey guard above does not
		// fire and control reaches here. auto.New's only credential is an auth.APIKey,
		// which cannot carry AWS SigV4 credentials, so a Bedrock client cannot be built
		// here; direct the caller to bedrock.New (which takes auth.SigV4Credentials + a
		// region). Fail-closed with a directive typed error, not a silent nil.
		return nil, &SigV4NotConstructibleError{Provider: llm.ProviderBedrock, Use: "bedrock.New"}
	default:
		// Defensive: RequiredAuth above already rejects any provider not handled
		// here, so this is unreachable for a validated model — but a permissive
		// fall-through would fail open, so deny by default.
		return nil, &llm.ValidationError{Field: "Provider", Reason: "unsupported provider"}
	}
}

// genericHTTP builds the generic transport-backed client for a provider that speaks a
// plain codec-over-HTTP endpoint. It selects the wire codec by the model's declared
// APIFormat (failing closed if none is implemented) and injects the caller-supplied
// authenticator, so one construction serves both an unauthenticated local endpoint
// (LM Studio, auth.None) and a Bearer-key gateway (OpenRouter, auth.Key) — the auth
// decision stays at the composition root, not in the transport.
func genericHTTP(model llm.Model, a llm.Authenticator) (llm.LLM, error) {
	codec, err := codecFor(model.APIFormat)
	if err != nil {
		return nil, err
	}
	return transport.New(codec, transport.Endpoint{
		Provider: model.Provider,
		BaseURL:  model.BaseURL,
		ChatPath: transport.DefaultChatPath,
	}, a), nil
}

// codecFor selects the wire codec for a generic (transport-backed) provider by its
// declared APIFormat. Model.Validate already admits every APIFormat the SDK knows,
// and a provider may legitimately support a format auto cannot yet encode; codecFor
// is the fail-closed boundary that turns "no codec implemented" into a typed
// *llm.ValidationError at construction rather than a silent wrong-dialect encode.
// Adding a new dialect is one new case here.
func codecFor(f llm.APIFormat) (llm.Codec, error) {
	switch f {
	case llm.APIFormatOpenAI:
		return openaiapi.Codec{}, nil
	case llm.APIFormatAnthropic:
		return anthropicapi.Codec{}, nil
	case llm.APIFormatGemini:
		return gemini.Codec{}, nil
	default:
		return nil, &llm.ValidationError{Field: "APIFormat", Reason: "no codec implemented for this API format yet"}
	}
}
