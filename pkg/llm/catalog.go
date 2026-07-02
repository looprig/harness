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
