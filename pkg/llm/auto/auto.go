package auto

import (
	"github.com/ciram-co/looprig/pkg/llm"
	"github.com/ciram-co/looprig/pkg/llm/aci"
	"github.com/ciram-co/looprig/pkg/llm/openaiapi/chutes"
	"github.com/ciram-co/looprig/pkg/llm/openaiapi/lmstudio"
)

// New validates spec and constructs the concrete provider client for spec.Provider.
// An unknown or empty provider yields a *llm.ValidationError, as does a
// self-contradictory spec (validated before dispatch).
func New(spec llm.ModelSpec) (llm.LLM, error) {
	if err := spec.Validate(); err != nil {
		return nil, err
	}
	switch spec.Provider {
	case llm.ProviderLMStudio:
		return lmstudio.New(spec.BaseURL), nil
	case llm.ProviderPhala:
		return aci.New(spec.BaseURL, spec.APIKey, aci.DefaultPhalaPolicy()), nil
	case llm.ProviderChutes:
		return chutes.New(spec.BaseURL, spec.APIKey), nil
	default:
		return nil, &llm.ValidationError{Field: "Provider", Reason: "unknown or empty"}
	}
}
