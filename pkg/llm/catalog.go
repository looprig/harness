package llm

// ChutesKimiK2 returns the Moonshot Kimi K2 model definition served through
// Chutes' TEE-attested endpoint. Chutes resolves the model name to a chute UUID
// via /v1/models at request time, so Name is the value sent on every request.
// BaseURL is the e2e/evidence apiBase, which chutes.New does NOT default — it
// must be explicit. Returned by value (not an exported var) so callers cannot
// mutate shared catalog state.
func ChutesKimiK2() Model {
	return Model{
		Provider: ProviderChutes,
		BaseURL:  "https://api.chutes.ai",
		Name:     "moonshotai/Kimi-K2.6-TEE",
	}
}
