package swe

import (
	"os"
	"strings"

	"github.com/ciram-co/looprig/pkg/llm"
	"github.com/ciram-co/looprig/pkg/llm/auto"
)

// model is the named model every agent in the SWE-Swarm runs on. P1 reuses Kimi
// K2 (a strong agentic-coding model already in the catalog, text-only), matching
// the coding agent's choice; swapping models is a one-line change here. Read-only
// after init: do not reassign or mutate it — the parallel fake-client tests read
// it concurrently.
var model = llm.ChutesKimiK2()

// envAPIKey is the only value read from the environment. The value is the NAME of
// an env var, not a secret; the #nosec annotation documents that gosec's G101
// "hardcoded credentials" heuristic (which matches on the identifier name) is a
// false positive here.
const envAPIKey = "LLM_API_KEY" // #nosec G101 -- env var name, not a credential

// newModelFactory builds the swarm's ModelFactory: a closure that materializes a
// full llm.ModelSpec for any system prompt by injecting the shared model identity
// + the (already-read) API key. The swarm owns provider/model/sampling; agents
// pass only their finished system prompt and never see the key. The key is closed
// over verbatim — never normalized; credential material is passed as-is. Splitting
// this out from buildClient gives the model_test a key-injection seam with no env
// read or network call.
func newModelFactory(apiKey string) ModelFactory {
	return func(systemPrompt string) llm.ModelSpec {
		return model.Spec(apiKey, systemPrompt)
	}
}

// readAPIKey is the credential boundary: it resolves whether the configured
// model's provider requires a key (failing secure on an unclassified provider),
// reads LLM_API_KEY, and fails loud with a typed *MissingEnvError if a required
// key is absent. env is a boundary, so a whitespace-only value is treated as
// missing — the failure is loud at startup, not deferred to provider-call time.
// The key is returned verbatim (the TrimSpace is a presence check, not a
// sanitizer) so the single read+pass of credential material lives in one spot.
func readAPIKey() (string, error) {
	needsKey, err := model.Provider.RequiresKey()
	if err != nil {
		return "", err // unclassified provider — fail secure
	}
	apiKey := os.Getenv(envAPIKey)
	if needsKey && strings.TrimSpace(apiKey) == "" {
		return "", &MissingEnvError{Var: envAPIKey}
	}
	return apiKey, nil
}

// buildClient is the env+provider construction boundary shared by swe.New: it
// reads the API key (fail-loud via readAPIKey), builds + validates the single
// shared provider client via auto.New, and returns the ModelFactory bound to the
// same key. The client is built from a spec with an EMPTY system prompt — the
// provider client is system-agnostic (the per-agent system prompt is sent every
// turn via loop.Config.Model, materialized by the factory), so the empty-system
// spec is only used to validate + dispatch on the provider. On any failure it
// returns nil client + nil factory (fail secure).
func buildClient() (llm.LLM, ModelFactory, error) {
	apiKey, err := readAPIKey()
	if err != nil {
		return nil, nil, err
	}
	// Empty system: the provider client carries only provider/baseURL/key; the
	// system prompt is a per-turn concern the factory bakes into each agent's spec.
	client, err := auto.New(model.Spec(apiKey, ""))
	if err != nil {
		return nil, nil, err
	}
	return client, newModelFactory(apiKey), nil
}
