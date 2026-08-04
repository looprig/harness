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
// the wire transport or trust posture. This is a deliberate product decision,
// not an oversight: Sampling.Effort in particular is never part of transport
// or trust identity — it is a per-request sampling parameter, validated
// separately by each model's own declared effort set, and switching effort
// never crosses a security/retention boundary the way switching transport
// does. Name varies freely within one transport (many models, one endpoint).
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

// ContextTransportNotDeclaredError reports a candidate model whose transport
// is not a member of a loop definition's declared ContextTransport set.
type ContextTransportNotDeclaredError struct {
	Provider  model.ProviderName
	APIFormat model.APIFormat
	BaseURL   string
}

func (e *ContextTransportNotDeclaredError) Error() string {
	return "loop: context model transport is not a declared ContextTransport"
}

// validateContextTransportMembership reports whether m's transport identity is
// a member of transports. An empty transports set is treated as "not yet
// populated" and is permissive (nil): until Task 1.3 wires the real declared
// (or synthesized) set into Define, the mode-binding call site that uses this
// helper has nothing meaningful to check against. Given a non-empty set, it
// performs the real lookupTransport-backed membership check.
func validateContextTransportMembership(transports []ContextTransport, m model.Model) error {
	if len(transports) == 0 {
		return nil
	}
	if _, ok := lookupTransport(transports, m); !ok {
		return &ContextTransportNotDeclaredError{Provider: m.Provider, APIFormat: m.APIFormat, BaseURL: m.BaseURL}
	}
	return nil
}

// validateContextTransportUnchanged is TEMPORARY interim (Task 1.2/1.3) fixed-
// single-transport check backing Definition/BoundDefinition.ValidateContextModel.
// It preserves today's exact behavior (candidate must share bound's transport
// identity) under the new ContextTransportNotDeclaredError shape.
//
// TODO(task-1.4): delete this function. Task 1.4 replaces this call site's
// body wholesale with a real lookupTransport-based set-membership check once
// definitionState carries its own frozen, possibly multi-member
// contextTransports. This function must not survive Task 1.4.
func validateContextTransportUnchanged(bound, candidate model.Model) error {
	if transportKeyOf(bound) == transportKeyOf(candidate) {
		return nil
	}
	return &ContextTransportNotDeclaredError{Provider: candidate.Provider, APIFormat: candidate.APIFormat, BaseURL: candidate.BaseURL}
}
