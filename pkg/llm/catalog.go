package llm

// ChutesKimiK2 returns the Moonshot Kimi K2 model definition served through
// Chutes' TEE-attested endpoint. Chutes resolves the model name to a chute UUID
// via /v1/models at request time, so Name is the value sent on every request.
// BaseURL is the e2e/evidence apiBase, which chutes.New does NOT default — it
// must be explicit. Returned by value (not an exported var) so callers cannot
// mutate shared catalog state. Kimi K2 is text-only, so AcceptsImages stays false.
func ChutesKimiK2() Model {
	return Model{
		Provider:  ProviderChutes,
		APIFormat: APIFormatOpenAI,
		BaseURL:   "https://api.chutes.ai",
		Name:      "moonshotai/Kimi-K2.6-TEE",
		Origin:    OriginCatalog,
		Caps: Capabilities{
			MaxContext: 128_000,
			Tools:      true,
			Thinking:   true,
		},
	}
}

// LMStudioLocal returns a Model for a local LM Studio server at its default
// loopback endpoint. LM Studio speaks the OpenAI-compatible dialect and needs no
// credentials (Provider.RequiredAuth → AuthNone). Unlike the former lmstudio.New,
// the generic transport client demands an explicit, Validate-passing BaseURL, so
// the default localhost endpoint is materialized here — the http:// loopback host
// is permitted by Validate's loopback exception. Capabilities are conservative:
// tool-calling is commonly supported by local OpenAI-compatible servers, while
// image input and hidden thinking are model-specific and left false. Returned by
// value so callers cannot mutate shared catalog state.
func LMStudioLocal(name string) Model {
	return Model{
		Provider:  ProviderLMStudio,
		APIFormat: APIFormatOpenAI,
		BaseURL:   "http://localhost:1234/v1",
		Name:      name,
		Origin:    OriginCatalog,
		Caps: Capabilities{
			Tools: true,
		},
	}
}

// GLM46Phala returns the zai-org/GLM-4.6 model definition served through Phala's
// TEE-attested OpenAI-compatible gateway. Returned by value so callers cannot
// mutate shared catalog state.
func GLM46Phala() Model {
	return Model{
		Provider:  ProviderPhala,
		APIFormat: APIFormatOpenAI,
		BaseURL:   "https://api.phala.network/v1",
		Name:      "zai-org/GLM-4.6",
		Origin:    OriginCatalog,
		Caps: Capabilities{
			MaxContext: 200_000,
			Tools:      true,
			Thinking:   true,
		},
	}
}
