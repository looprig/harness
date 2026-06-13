package auto

import (
	"github.com/inventivepotter/urvi/internal/llm"
	"github.com/inventivepotter/urvi/internal/llm/openaiapi/chutes"
	"github.com/inventivepotter/urvi/internal/llm/openaiapi/lmstudio"
	"github.com/inventivepotter/urvi/internal/llm/openaiapi/phala"
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
		return phala.New(spec.BaseURL, spec.APIKey), nil
	case llm.ProviderChutes:
		return chutes.New(spec.BaseURL, spec.APIKey), nil
	default:
		return nil, &llm.ValidationError{Field: "Provider", Reason: "unknown or empty"}
	}
}
