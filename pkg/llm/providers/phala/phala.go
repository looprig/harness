// Package phala is the Phala confidential-inference provider: it owns the gateway
// base-URL default and a typed constructor (New) that wires the reusable,
// provider-agnostic aci attestation protocol into an attested llm.LLM. aci
// enforces whatever acceptance Policy it is handed; this package ships no pinned
// trust anchors — the caller supplies the acceptance Policy with their own,
// externally verified pins (app-id, build provenance, KMS root). aci never imports
// this package (that would cycle); the dependency is one-way, phala → aci.
package phala

import (
	"github.com/looprig/harness/pkg/llm"
	"github.com/looprig/harness/pkg/llm/aci"
	"github.com/looprig/harness/pkg/llm/auth"
)

// defaultBaseURL is the canonical Phala inference host, used when New is given "".
// PROVISIONAL — confirm the production host with ops before release (design §4.2).
const defaultBaseURL = "https://inference.phala.com"

// New builds an attested Phala confidential-inference client: it composes the
// reusable aci attestation protocol with the caller-supplied acceptance Policy
// (the caller pins its own, externally verified trust anchors). baseURL is the
// gateway origin (e.g. https://inference.phala.com); an empty baseURL self-defaults
// to the canonical Phala host (defaultBaseURL). key is the bearer credential and is
// REQUIRED — the
// typed auth.APIKey parameter encodes that at compile time, and New fails closed on
// an empty string with a typed *llm.AuthRequiredError before any network call
// (checked BEFORE the base-URL default, so a missing key always wins). It then
// forwards to aci.New, which fails closed with *UnpinnedPolicyError on a policy that
// pins no acceptance set and did not opt out via aci.UnpinnedPolicy(). On success it
// returns the llm.LLM aci implements.
func New(baseURL string, key auth.APIKey, p aci.Policy) (llm.LLM, error) {
	if key == "" {
		return nil, &llm.AuthRequiredError{Provider: llm.ProviderPhala, Kind: llm.AuthAPIKey}
	}
	if baseURL == "" {
		baseURL = defaultBaseURL
	}
	return aci.New(baseURL, string(key), p)
}
