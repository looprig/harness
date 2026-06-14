package llm

// Model is a named, secret-free model definition: which model, how to reach it,
// and default sampling. It deliberately omits APIKey (a secret) and System (a
// per-agent concern). Materialize a full ModelSpec with Spec.
//
// Sampling is intentionally limited to Temperature and MaxTokens in v1; the
// remaining ModelSpec sampling fields (TopP, Stop, ThinkingBudget,
// ReasoningEffort) are left at their zero values by Spec. Extend both Model and
// Spec together if a future model needs them.
type Model struct {
	Provider    Provider
	BaseURL     string
	Name        string
	Temperature *float64
	MaxTokens   *int
}

// Spec materializes a ModelSpec from this definition, injecting the secret API
// key and the caller's system prompt. Pointer-valued sampling fields are deep
// copied so a returned spec never aliases the definition's state: a caller that
// mutates *spec.Temperature cannot reach back into a shared Model.
func (m Model) Spec(apiKey, system string) ModelSpec {
	return ModelSpec{
		Provider:    m.Provider,
		BaseURL:     m.BaseURL,
		APIKey:      apiKey,
		Model:       m.Name,
		System:      system,
		Temperature: cloneFloat64Ptr(m.Temperature),
		MaxTokens:   cloneIntPtr(m.MaxTokens),
	}
}

// cloneFloat64Ptr returns a fresh pointer to a copy of *p, or nil when p is nil.
// Concrete (not generic) to honor the repo rule against `any` outside
// serialization/plugin boundaries.
func cloneFloat64Ptr(p *float64) *float64 {
	if p == nil {
		return nil
	}
	v := *p
	return &v
}

// cloneIntPtr returns a fresh pointer to a copy of *p, or nil when p is nil.
func cloneIntPtr(p *int) *int {
	if p == nil {
		return nil
	}
	v := *p
	return &v
}
