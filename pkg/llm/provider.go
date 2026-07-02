package llm

// RequiresKey reports whether the provider needs an API key, and errors on an
// unknown provider so a newly added one must be classified here before it can be
// used. Hosted, attested providers (phala, chutes) require a key; a local LM
// Studio endpoint does not. A bare default-false would fail open — the bug this
// method exists to prevent.
func (p Provider) RequiresKey() (bool, error) {
	switch p {
	case ProviderLMStudio:
		return false, nil
	case ProviderPhala, ProviderChutes:
		return true, nil
	default:
		return false, &ValidationError{Field: "Provider", Reason: "unknown provider; API-key policy undefined"}
	}
}

// RequiredAuth reports which credential kind the provider needs, erroring on an unknown provider
// so a newly added one must be classified here before use. Multi-auth-ready successor to
// RequiresKey; fail-closed by the same rationale (a permissive default would fail open).
func (p Provider) RequiredAuth() (AuthKind, error) {
	switch p {
	case ProviderLMStudio:
		return AuthNone, nil
	case ProviderPhala, ProviderChutes:
		return AuthAPIKey, nil
	default:
		return "", &ValidationError{Field: "Provider", Reason: "unknown provider; auth policy undefined"}
	}
}
