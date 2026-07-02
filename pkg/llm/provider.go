package llm

// RequiresKey reports whether the provider needs an API key, and errors on an
// unknown provider so a newly added one must be classified here before it can be
// used. Hosted providers (phala, chutes, openrouter) require a key; a local LM
// Studio endpoint does not. A bare default-false would fail open — the bug this
// method exists to prevent.
func (p Provider) RequiresKey() (bool, error) {
	switch p {
	case ProviderLMStudio:
		return false, nil
	case ProviderPhala, ProviderChutes, ProviderOpenRouter:
		return true, nil
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
	case ProviderPhala, ProviderChutes, ProviderOpenRouter:
		return AuthAPIKey, nil
	default:
		return "", &ValidationError{Field: "Provider", Reason: "unknown provider; auth policy undefined"}
	}
}
