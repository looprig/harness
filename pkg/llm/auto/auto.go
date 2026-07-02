// Package auto is the composition root that selects and wires a concrete llm.LLM
// for a validated Model. It is the single place that imports every provider, so
// business logic depends only on the llm.LLM interface — never on a concrete
// provider — and every credential/attestation decision is made here, once. It maps
// a Provider to its client and enforces the provider's fail-closed auth contract
// before any network object is constructed.
package auto

import (
	"github.com/ciram-co/looprig/pkg/llm"
	"github.com/ciram-co/looprig/pkg/llm/auth"
	"github.com/ciram-co/looprig/pkg/llm/codec/openaiapi"
	"github.com/ciram-co/looprig/pkg/llm/providers/chutes"
	"github.com/ciram-co/looprig/pkg/llm/providers/phala"
	"github.com/ciram-co/looprig/pkg/llm/transport"
)

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
		return transport.New(openaiapi.Codec{}, transport.Endpoint{
			Provider: model.Provider,
			BaseURL:  model.BaseURL,
			ChatPath: transport.DefaultChatPath,
		}, auth.None()), nil
	default:
		// Defensive: RequiredAuth above already rejects any provider not handled
		// here, so this is unreachable for a validated model — but a permissive
		// fall-through would fail open, so deny by default.
		return nil, &llm.ValidationError{Field: "Provider", Reason: "unsupported provider"}
	}
}
