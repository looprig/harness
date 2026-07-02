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

// OpenRouter returns a Model for OpenRouter's OpenAI-compatible aggregation gateway.
// OpenRouter fronts many upstream models behind one OpenAI-format API and one Bearer
// key (Provider.RequiredAuth → AuthAPIKey), so APIFormat is APIFormatOpenAI and name
// is the OpenRouter model slug (e.g. "anthropic/claude-3.5-sonnet") sent verbatim on
// every request. Capabilities are conservative — tool-calling is broadly available,
// while image input and hidden thinking are model-specific and left false. Returned
// by value so callers cannot mutate shared catalog state.
func OpenRouter(name string) Model {
	return Model{
		Provider:  ProviderOpenRouter,
		APIFormat: APIFormatOpenAI,
		BaseURL:   "https://openrouter.ai/api/v1",
		Name:      name,
		Origin:    OriginCatalog,
		Caps: Capabilities{
			Tools: true,
		},
	}
}

// ClaudeOnBedrock returns a Model for Anthropic Claude served through AWS Bedrock
// Runtime. Bedrock is region-routed: the endpoint host is derived from the AWS
// region by bedrock.New, so BaseURL is empty (permitted for ProviderBedrock by
// Model.Validate). name is the Bedrock model id sent in the request path (e.g.
// "anthropic.claude-3-5-sonnet-20241022-v2:0", whose ":" the SigV4 signer encodes
// into the canonical URI). APIFormat is Anthropic (the implemented codec); the
// body is the Anthropic Messages body minus "model" plus "anthropic_version",
// which bedrock.New's client produces. Credentials are AWS SigV4 (RequiredAuth ->
// AuthSigV4), never a bearer key. Capabilities are conservative: Claude on Bedrock
// is tool- and image-capable; hidden thinking is model-version-specific and left
// off (fail-safe — the codec then never emits a thinking field). Returned by value
// so callers cannot mutate shared catalog state.
func ClaudeOnBedrock(name string) Model {
	return Model{
		Provider:  ProviderBedrock,
		APIFormat: APIFormatAnthropic,
		BaseURL:   "", // region-routed; bedrock.New derives the endpoint from the region
		Name:      name,
		Origin:    OriginCatalog,
		Caps: Capabilities{
			MaxContext:    200_000,
			Tools:         true,
			AcceptsImages: true,
		},
	}
}

// GeminiFlash returns the Google Gemini 2.5 Flash model served through Google's
// generateContent API. Provider is ProviderGoogle (auth is an x-goog-api-key
// header, RequiredAuth → AuthAPIKey) and APIFormat is APIFormatGemini — the two
// axes are kept distinct: the backend is "google", the wire dialect is "gemini".
// Name is the model id sent in the request path (…/models/<name>:generateContent).
// BaseURL is the v1beta generateContent root, which the bespoke providers/gemini
// client binds. Gemini 2.5 Flash is tool-, image-, and thinking-capable with a
// ~1M-token context. Returned by value so callers cannot mutate shared catalog
// state.
func GeminiFlash() Model {
	return Model{
		Provider:  ProviderGoogle,
		APIFormat: APIFormatGemini,
		BaseURL:   "https://generativelanguage.googleapis.com/v1beta",
		Name:      "gemini-2.5-flash",
		Origin:    OriginCatalog,
		Caps: Capabilities{
			MaxContext:    1_000_000,
			Tools:         true,
			AcceptsImages: true,
			Thinking:      true,
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
