package llm

// RequiresKey reports whether the provider needs an API key, and errors on an
// unknown provider so a newly added one must be classified here before it can be
// used. Hosted key providers (phala, chutes, openrouter, google) require a key; a
// local LM Studio endpoint does not. A bare default-false would fail open — the
// bug this method exists to prevent. This is the legacy boolean superseded by
// RequiredAuth (which is the real gate and can express non-key auth like SigV4).
func (p Provider) RequiresKey() (bool, error) {
	switch p {
	case ProviderLMStudio:
		return false, nil
	case ProviderPhala, ProviderChutes, ProviderOpenRouter, ProviderGoogle:
		return true, nil
	case ProviderBedrock:
		// Bedrock authenticates with SigV4 credentials, not an API key; this legacy
		// boolean cannot express that. RequiredAuth() is the real gate (AuthSigV4).
		return false, nil
	default:
		return false, &ValidationError{Field: "Provider", Reason: "unknown provider; API-key policy undefined"}
	}
}

// supportsAPIFormat reports whether provider p is known to speak wire dialect f.
// It is fail-closed: an unknown provider supports no formats, so a newly added
// provider must be classified here before any Model naming it can Validate.
func (p Provider) supportsAPIFormat(f APIFormat) bool {
	switch p {
	case ProviderPhala, ProviderChutes, ProviderOpenRouter:
		return f == APIFormatOpenAI
	case ProviderLMStudio:
		return f == APIFormatOpenAI || f == APIFormatAnthropic
	case ProviderBedrock:
		// Anthropic-on-Bedrock speaks the Anthropic Messages dialect (the
		// implemented codec); the native Bedrock Converse dialect is reserved for a
		// future codec but is a legitimate Bedrock format, so both are admitted here.
		return f == APIFormatAnthropic || f == APIFormatBedrockConverse
	case ProviderGoogle:
		// Google's generateContent backend speaks only the Gemini dialect.
		return f == APIFormatGemini
	default:
		return false
	}
}

// allowsEmptyBaseURL reports whether an empty Model.BaseURL is acceptable for the
// provider — true when the base is resolvable to a canonical endpoint: Bedrock is
// region-routed (no base), and every other current provider has a default endpoint
// the SDK supplies when BaseURL is empty. Fail-closed: an unknown/future provider
// with no default returns false, so Validate keeps requiring an explicit base for it.
func (p Provider) allowsEmptyBaseURL() bool {
	switch p {
	case ProviderBedrock, ProviderChutes, ProviderPhala, ProviderOpenRouter, ProviderLMStudio, ProviderGoogle:
		return true
	default:
		return false
	}
}

// RequiredAuth reports which credential kind the provider needs, erroring on an unknown provider
// so a newly added one must be classified here before use. Multi-auth-ready successor to
// RequiresKey; fail-closed by the same rationale (a permissive default would fail open).
func (p Provider) RequiredAuth() (AuthKind, error) {
	switch p {
	case ProviderLMStudio:
		return AuthNone, nil
	case ProviderPhala, ProviderChutes, ProviderOpenRouter, ProviderGoogle:
		return AuthAPIKey, nil
	case ProviderBedrock:
		// Bedrock authenticates with AWS SigV4, not a bearer API key; auto.New cannot
		// supply SigV4 credentials, so a Bedrock client is built directly via bedrock.New.
		return AuthSigV4, nil
	default:
		return "", &ValidationError{Field: "Provider", Reason: "unknown provider; auth policy undefined"}
	}
}
