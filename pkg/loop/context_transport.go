package loop

import (
	contextcount "github.com/looprig/inference/contextcount"
	model "github.com/looprig/inference/model"
)

// ContextTransport is one admitted (wire transport -> trust posture) pair a
// loop definition allows a live model switch to move to.
type ContextTransport struct {
	Provider   model.ProviderName
	APIFormat  model.APIFormat
	BaseURL    string
	Capability contextcount.InferenceCapability
}

// contextTransportKey projects the transport-identifying fields of a model,
// ignoring fields (Name, Sampling, Caps, Limits, Origin) that do not change
// the wire transport or trust posture.
type contextTransportKey struct {
	Provider  model.ProviderName
	APIFormat model.APIFormat
	BaseURL   string
}

// transportKeyOf projects the transport identity of a model.
func transportKeyOf(m model.Model) contextTransportKey {
	return contextTransportKey{Provider: m.Provider, APIFormat: m.APIFormat, BaseURL: m.BaseURL}
}

// lookupTransport reports the admitted capability for m's transport identity
// within set, if any member matches.
func lookupTransport(set []ContextTransport, m model.Model) (contextcount.InferenceCapability, bool) {
	key := transportKeyOf(m)
	for _, t := range set {
		if (contextTransportKey{Provider: t.Provider, APIFormat: t.APIFormat, BaseURL: t.BaseURL}) == key {
			return t.Capability, true
		}
	}
	return contextcount.InferenceCapability{}, false
}
